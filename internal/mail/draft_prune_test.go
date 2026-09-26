package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPruneSweepsOrphanSendClaimAndSpool(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&draftGateway{}, root)
	draft := createSendTestDraft(t, service)
	if _, err := beginSendAttempt(sendAttemptOptions{Root: root, Ref: draft.Ref}); err != nil {
		t.Fatalf("beginSendAttempt() error = %v", err)
	}
	spoolPath := filepath.Join(root, draft.Ref+".send-spool")
	if err := os.WriteFile(spoolPath, []byte("orphaned mime bytes"), 0o600); err != nil {
		t.Fatalf("write spool error = %v", err)
	}
	// Simulate discard dying after the draft JSON removal but before claim cleanup.
	if err := os.Remove(filepath.Join(root, draft.Ref+".json")); err != nil {
		t.Fatalf("remove draft error = %v", err)
	}
	result, err := service.PruneDrafts(
		PruneDraftsRequest{OlderThan: 30 * 24 * time.Hour, Confirm: true},
	)
	if err != nil {
		t.Fatalf("PruneDrafts() error = %v", err)
	}
	if len(result.SweptArtifacts) != 1 || result.SweptArtifacts[0] != draft.Ref {
		t.Fatalf("SweptArtifacts = %v, want [%s]", result.SweptArtifacts, draft.Ref)
	}
	for _, suffix := range []string{".send-claim", ".send-spool", ".lock"} {
		if _, err := os.Lstat(filepath.Join(root, draft.Ref+suffix)); !os.IsNotExist(err) {
			t.Fatalf("orphan %s still present: %v", draft.Ref+suffix, err)
		}
	}
}

func writePreparedHandoffClaimFixture(t *testing.T, root string, ref string, attemptID string, snapshots []HandoffSnapshot) HandoffAttempt {
	t.Helper()
	if snapshots == nil {
		snapshots = []HandoffSnapshot{}
	}
	now := time.Now().UTC()
	attempt := HandoffAttempt{
		ID: attemptID, DraftRef: ref, StartedAt: now, UpdatedAt: now,
		Outcome: HandoffOutcomePrepared, Snapshots: snapshots,
		SnapshotsRetained: len(snapshots) > 0,
	}
	payload, err := encodeHandoffAttempt(ref, attempt)
	if err != nil {
		t.Fatalf("encode prepared handoff fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ref+handoffClaimSuffix), payload, 0o600); err != nil {
		t.Fatalf("write prepared handoff fixture: %v", err)
	}
	return attempt
}

func TestPrunePreservesAmbiguousOrphanHandoffArtifacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&draftGateway{}, root)
	draft := createSendTestDraft(t, service)
	snapshotFile := filepath.Join(
		root, draft.Ref+handoffSnapshotSuffix, "handoff_123456789012345678901234", "0", "staged.eml",
	)
	if err := os.MkdirAll(filepath.Dir(snapshotFile), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(snapshotFile, []byte("staged"), 0o600); err != nil {
		t.Fatalf("write snapshot error = %v", err)
	}
	claimPath := filepath.Join(root, draft.Ref+handoffClaimSuffix)
	if err := os.WriteFile(claimPath, []byte("{corrupt"), 0o600); err != nil {
		t.Fatalf("write handoff claim error = %v", err)
	}
	if err := os.Remove(filepath.Join(root, draft.Ref+".json")); err != nil {
		t.Fatalf("remove draft error = %v", err)
	}
	claimBefore, err := os.ReadFile(claimPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshotBefore, err := os.ReadFile(snapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.PruneDrafts(
		PruneDraftsRequest{OlderThan: 30 * 24 * time.Hour, Confirm: true},
	)
	if err == nil || len(result.Failed) != 1 || result.Failed[0].Ref != draft.Ref || len(result.SweptArtifacts) != 0 {
		t.Fatalf("PruneDrafts() = %+v, %v; want ambiguous evidence preserved", result, err)
	}
	if claimAfter, err := os.ReadFile(claimPath); err != nil || string(claimAfter) != string(claimBefore) {
		t.Fatalf("ambiguous claim changed: %q, %v", claimAfter, err)
	}
	if snapshotAfter, err := os.ReadFile(snapshotFile); err != nil || string(snapshotAfter) != string(snapshotBefore) {
		t.Fatalf("ambiguous snapshot changed: %q, %v", snapshotAfter, err)
	}
}

func TestPruneKeepsArtifactsWhileDraftExists(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&draftGateway{}, root)
	draft := createSendTestDraft(t, service)
	if _, err := beginSendAttempt(sendAttemptOptions{Root: root, Ref: draft.Ref}); err != nil {
		t.Fatalf("beginSendAttempt() error = %v", err)
	}
	result, err := service.PruneDrafts(
		PruneDraftsRequest{OlderThan: 30 * 24 * time.Hour, Confirm: true},
	)
	if err != nil {
		t.Fatalf("PruneDrafts() error = %v", err)
	}
	if len(result.SweptArtifacts) != 0 || len(result.Failed) != 0 {
		t.Fatalf("live-draft artifacts touched: %+v", result)
	}
	if _, err := os.Lstat(filepath.Join(root, draft.Ref+".send-claim")); err != nil {
		t.Fatalf("send claim removed while draft exists: %v", err)
	}
}

func TestPruneDryRunListsOrphanArtifactsWithoutRemoving(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&draftGateway{}, root)
	draft := createSendTestDraft(t, service)
	if _, err := beginSendAttempt(sendAttemptOptions{Root: root, Ref: draft.Ref}); err != nil {
		t.Fatalf("beginSendAttempt() error = %v", err)
	}
	if err := os.Remove(filepath.Join(root, draft.Ref+".json")); err != nil {
		t.Fatalf("remove draft error = %v", err)
	}
	result, err := service.PruneDrafts(
		PruneDraftsRequest{OlderThan: 30 * 24 * time.Hour},
	)
	if err != nil {
		t.Fatalf("PruneDrafts() error = %v", err)
	}
	if !result.DryRun || len(result.OrphanArtifacts) != 1 || result.OrphanArtifacts[0] != draft.Ref {
		t.Fatalf("OrphanArtifacts = %v, want [%s]", result.OrphanArtifacts, draft.Ref)
	}
	if _, err := os.Lstat(filepath.Join(root, draft.Ref+".send-claim")); err != nil {
		t.Fatalf("dry-run removed orphan claim: %v", err)
	}
}

func TestPruneSkipsOrphanArtifactsWhileLocked(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&draftGateway{}, root)
	draft := createSendTestDraft(t, service)
	if _, err := beginSendAttempt(sendAttemptOptions{Root: root, Ref: draft.Ref}); err != nil {
		t.Fatalf("beginSendAttempt() error = %v", err)
	}
	if err := os.Remove(filepath.Join(root, draft.Ref+".json")); err != nil {
		t.Fatalf("remove draft error = %v", err)
	}
	lockContext, cancel := draftLockContext(context.Background())
	defer cancel()
	lease, err := acquireDraftLease(lockContext, root, draft.Ref)
	if err != nil {
		t.Fatalf("acquireDraftLease() error = %v", err)
	}
	defer func() {
		if err := lease.release(); err != nil {
			t.Fatalf("release() error = %v", err)
		}
	}()
	result, err := service.PruneDrafts(
		PruneDraftsRequest{OlderThan: 30 * 24 * time.Hour, Confirm: true},
	)
	if err != nil {
		t.Fatalf("PruneDrafts() error = %v", err)
	}
	for _, ref := range result.SweptArtifacts {
		if strings.EqualFold(ref, draft.Ref) {
			t.Fatalf("locked orphan ref was swept: %+v", result)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, draft.Ref+".send-claim")); err != nil {
		t.Fatalf("claim removed while lock held: %v", err)
	}
}

func TestPruneSweepsPreparedOrphanHandoffAttempt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(nil, root)
	attachment := filepath.Join(t.TempDir(), "attachment.txt")
	if err := os.WriteFile(attachment, []byte("staged attachment"), 0o600); err != nil {
		t.Fatal(err)
	}
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		To: []Recipient{{Address: "ada@example.com"}}, Body: "Body", Attachments: []string{attachment},
	}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.BeginDraftHandoff(draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	prepared := session.Preparation()
	if err := session.lease.release(); err != nil {
		t.Fatal(err)
	}
	session.closed = true
	if err := os.Remove(filepath.Join(root, draft.Ref+".json")); err != nil {
		t.Fatal(err)
	}
	result, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: 24 * time.Hour, Confirm: true})
	if err != nil || len(result.SweptArtifacts) != 1 || result.SweptArtifacts[0] != draft.Ref {
		t.Fatalf("prepared orphan prune = %+v, %v", result, err)
	}
	for _, path := range []string{
		filepath.Join(root, draft.Ref+handoffClaimSuffix),
		filepath.Join(root, draft.Ref+handoffSnapshotSuffix),
		filepath.Join(root, draft.Ref+".lock"),
		prepared.AttachmentPaths[0],
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("prepared orphan path remains %q: %v", path, err)
		}
	}
}

func TestPrunePreservesDispatchedOrphanHandoffAttempt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(nil, root)
	attachment := filepath.Join(t.TempDir(), "attachment.txt")
	if err := os.WriteFile(attachment, []byte("retained attachment"), 0o600); err != nil {
		t.Fatal(err)
	}
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		To: []Recipient{{Address: "ada@example.com"}}, Body: "Body", Attachments: []string{attachment},
	}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.BeginDraftHandoff(draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.MarkDispatched(context.Background()); err != nil {
		t.Fatal(err)
	}
	prepared := session.Preparation()
	stagedPath := prepared.AttachmentPaths[0]
	stagedBefore, err := os.ReadFile(stagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.lease.release(); err != nil {
		t.Fatal(err)
	}
	session.closed = true
	if err := os.Remove(filepath.Join(root, draft.Ref+".json")); err != nil {
		t.Fatal(err)
	}
	result, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: 24 * time.Hour, Confirm: true})
	if err == nil || len(result.Failed) != 1 || result.Failed[0].Ref != draft.Ref || len(result.SweptArtifacts) != 0 {
		t.Fatalf("dispatched orphan prune = %+v, %v; want retained evidence", result, err)
	}
	if stagedAfter, err := os.ReadFile(stagedPath); err != nil || string(stagedAfter) != string(stagedBefore) {
		t.Fatalf("dispatched snapshot changed: %q, %v", stagedAfter, err)
	}
	if _, err := os.Stat(filepath.Join(root, draft.Ref+handoffClaimSuffix)); err != nil {
		t.Fatalf("dispatched claim was removed: %v", err)
	}
}

func TestPruneFindsAndRemovesOrphanDraftJSONTemporary(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&draftGateway{}, root)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ref := "draft_123456789012345678901234"
	validName := "." + ref + ".json.mailcli-0123456789abcdef01234567"
	invalidName := "." + ref + ".json.mailcli-0123456789abcdef0123456g"
	for _, name := range []string{validName, invalidName} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("unpublished draft bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Crash cleanup now requires age as well as exclusive ref ownership.
	ageDraftTemporary(t, filepath.Join(root, validName))
	dryRun, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: 24 * time.Hour})
	if err != nil || !dryRun.DryRun || len(dryRun.OrphanArtifacts) != 1 || dryRun.OrphanArtifacts[0] != ref {
		t.Fatalf("orphan temporary dry run = %+v, %v", dryRun, err)
	}
	result, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: 24 * time.Hour, Confirm: true})
	if err != nil || len(result.SweptArtifacts) != 1 || result.SweptArtifacts[0] != ref {
		t.Fatalf("orphan temporary cleanup = %+v, %v", result, err)
	}
	if _, err := os.Lstat(filepath.Join(root, validName)); !os.IsNotExist(err) {
		t.Fatalf("valid temporary remains: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, invalidName)); err != nil {
		t.Fatalf("unmatched temporary was removed: %v", err)
	}
}

func TestDraftMutationLeavesJSONTemporaryForConfirmedPrune(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&draftGateway{}, root)
	draft := createSendTestDraft(t, service)
	name := "." + draft.Ref + ".json.mailcli-0123456789abcdef01234567"
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte("partial draft update"), 0o600); err != nil {
		t.Fatal(err)
	}
	ageDraftTemporary(t, path)
	lease, err := acquireDraftLease(context.Background(), root, draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readDraftForMutation(lease, root, draft.Ref); err != nil {
		t.Fatalf("readDraftForMutation() error = %v", err)
	}
	if err := lease.release(); err != nil {
		t.Fatal(err)
	}
	for _, confirm := range []bool{false, true} {
		if got, err := os.ReadFile(path); err != nil || string(got) != "partial draft update" {
			t.Fatalf("mutation or dry run changed crash temporary: %q, %v", got, err)
		}
		result, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: 24 * time.Hour, Confirm: confirm})
		if err != nil || len(result.TemporaryArtifacts) != 1 || result.TemporaryArtifacts[0].Name != name {
			t.Fatalf("prune confirm=%t: %+v, %v", confirm, result, err)
		}
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("confirmed prune retained stale JSON temporary: %v", err)
	}
	if stored, err := service.GetDraft(draft.Ref); err != nil || stored.Ref != draft.Ref || stored.Revision != draft.Revision {
		t.Fatalf("temporary recovery changed the stable draft: %+v, %v", stored, err)
	}
}

func TestDraftJSONTemporaryRecoveryPreservesSymlinkAndReplacement(t *testing.T) {
	for _, replacement := range []string{"symlink", "file"} {
		t.Run(replacement, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "drafts")
			service := NewServiceWithDraftRoot(&draftGateway{}, root)
			draft := createSendTestDraft(t, service)
			name := "." + draft.Ref + ".json.mailcli-0123456789abcdef01234567"
			path := filepath.Join(root, name)
			sentinel := filepath.Join(t.TempDir(), "sentinel")
			if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sentinel, []byte("sentinel bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			lease, err := acquireDraftLease(context.Background(), root, draft.Ref)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := lease.release(); err != nil {
					t.Error(err)
				}
			}()
			if replacement == "symlink" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(sentinel, path); err != nil {
					t.Fatal(err)
				}
				if _, err := removeDraftTemporaryFiles(lease.storage, draft.Ref); err == nil {
					t.Fatal("symlink temporary was removed")
				}
				if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("symlink temporary changed: %v, %v", info, err)
				}
			} else {
				expected, err := lease.storage.lstat(name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path, path+".retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := removeDraftTemporaryFile(lease.storage, name, expected); err == nil {
					t.Fatal("replacement temporary was removed")
				}
				if payload, err := os.ReadFile(path); err != nil || string(payload) != "replacement" {
					t.Fatalf("replacement changed: %q, %v", payload, err)
				}
			}
			if payload, err := os.ReadFile(sentinel); err != nil || string(payload) != "sentinel bytes" {
				t.Fatalf("sentinel changed: %q, %v", payload, err)
			}
		})
	}
}

func TestPruneCandidateSelectionVisitsEachGeneratedDirectoryEntryOnce(t *testing.T) {
	for _, count := range []int{10000, 100000} {
		t.Run(fmt.Sprintf("entries-%d", count), func(t *testing.T) {
			root, refs := draftSelectionFixture(t, count)
			selection, err := collectPruneCandidates(context.Background(), root, time.Now())
			if err != nil {
				t.Fatalf("collectPruneCandidates() error = %v", err)
			}
			if selection.entryVisits != int64(count) || selection.entryVisits > 2*int64(count) {
				t.Fatalf("entry visits = %d, want exactly %d and at most %d", selection.entryVisits, count, 2*count)
			}
			if len(selection.candidates) != 0 || selection.metadataBytes != 0 || len(refs) != count {
				t.Fatalf("selection = %+v, fixture refs = %d; incomplete drafts must not become candidates", selection, len(refs))
			}
		})
	}
}

func TestPruneSkipsIncompleteAndCorruptDrafts(t *testing.T) {
	service, refs := createDraftListFixture(t, 1, 32)
	ageDraftFile(t, service.draftRoot, refs[0], 40)
	incompleteRef := "draft_000000000000000000000001"
	corruptRef := "draft_000000000000000000000002"
	fixtures := map[string]string{
		incompleteRef: `{"ref":"draft_000000000000000000000001","subject":`,
		corruptRef:    `{"ref":"draft_000000000000000000000002","subject":42}`,
	}
	for ref, content := range fixtures {
		if err := os.WriteFile(filepath.Join(service.draftRoot, ref+".json"), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s fixture: %v", ref, err)
		}
	}

	result, err := service.PruneDraftsContext(context.Background(), PruneDraftsRequest{
		OlderThan: 30 * 24 * time.Hour, Confirm: true,
	})
	if err != nil || len(result.Candidates) != 1 || result.Candidates[0].Ref != refs[0] ||
		len(result.Removed) != 1 || result.Removed[0] != refs[0] {
		t.Fatalf("PruneDraftsContext() = %+v, %v; want only the validated stale draft", result, err)
	}
	for ref, content := range fixtures {
		actual, err := os.ReadFile(filepath.Join(service.draftRoot, ref+".json"))
		if err != nil || string(actual) != content {
			t.Fatalf("invalid draft %s changed: %q, %v", ref, actual, err)
		}
	}
}

func TestPruneCancellationDuringCandidateCollectionDoesNotDelete(t *testing.T) {
	service, refs := createDraftListFixture(t, 1, 32)
	ageDraftFile(t, service.draftRoot, refs[0], 40)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &draftListBoundaryContext{Context: base, trigger: 4, action: cancel}
	result, err := service.PruneDraftsContext(ctx, PruneDraftsRequest{
		OlderThan: 30 * 24 * time.Hour, Confirm: true,
	})
	if ctx.checks < 2 || errorCode(err) != "draft_operation_canceled" || len(result.Removed) != 0 {
		t.Fatalf("PruneDraftsContext() = %+v, %v after %d checks; want bounded cancellation before deletion", result, err, ctx.checks)
	}
	if _, err := os.Stat(filepath.Join(service.draftRoot, refs[0]+".json")); err != nil {
		t.Fatalf("canceled prune removed the draft: %v", err)
	}
}

func TestPruneDryRunReportsDirectoryStability(t *testing.T) {
	for _, change := range []bool{false, true} {
		t.Run(fmt.Sprint(change), func(t *testing.T) {
			service, refs := createDraftListFixture(t, 1, 32)
			ageDraftFile(t, service.draftRoot, refs[0], 40)
			path := filepath.Join(service.draftRoot, refs[0]+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			identity, err := os.Stat(service.draftRoot)
			if err != nil {
				t.Fatal(err)
			}
			revision, err := draftListRevision(service.draftRoot, identity)
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(service.draftRoot, "unrelated-marker")
			ctx := &draftListBoundaryContext{Context: context.Background(), trigger: 4, action: func() {
				if !change {
					return
				}
				if err := os.WriteFile(marker, []byte("retain marker"), 0o600); err != nil {
					t.Fatal(err)
				}
				future := identity.ModTime().Add(2 * time.Second)
				if err := os.Chtimes(service.draftRoot, future, future); err != nil {
					t.Fatal(err)
				}
			}}
			result, err := service.PruneDraftsContext(ctx, PruneDraftsRequest{OlderThan: 30 * 24 * time.Hour})
			if err != nil || ctx.checks < ctx.trigger || !result.DryRun || result.Revision != revision ||
				result.Stable == nil || *result.Stable == change || len(result.Candidates) != 1 || result.Candidates[0].Ref != refs[0] ||
				len(result.Removed)+len(result.SweptLocks)+len(result.SweptArtifacts) != 0 {
				t.Fatalf("dry-run result = %+v, %v; want original revision %s, stable=%t, no effects", result, err, revision, !change)
			}
			if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
				t.Fatalf("dry run changed draft: %q, %v", after, err)
			}
			if change {
				if after, err := os.ReadFile(marker); err != nil || string(after) != "retain marker" {
					t.Fatalf("dry run changed marker: %q, %v", after, err)
				}
			}
		})
	}
}

func TestPruneDryRunRejectsDirectoryIdentityAndIOChanges(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
	}{
		{name: "replaced"},
		{name: "vanished", cause: os.ErrNotExist},
		{name: "stat failure", cause: syscall.ELOOP},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "drafts")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			identity, err := os.Stat(root)
			if err != nil {
				t.Fatal(err)
			}
			retained := root + "-retained"
			ctx := &draftListBoundaryContext{Context: context.Background(), trigger: 4, action: func() {
				if err := os.Rename(root, retained); err != nil {
					t.Fatal(err)
				}
				switch test.name {
				case "replaced":
					if err := os.Mkdir(root, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(root, identity.ModTime(), identity.ModTime()); err != nil {
						t.Fatal(err)
					}
				case "stat failure":
					if err := os.Symlink(filepath.Base(root), root); err != nil {
						t.Fatal(err)
					}
				}
			}}
			result, err := NewServiceWithDraftRoot(nil, root).PruneDraftsContext(ctx, PruneDraftsRequest{OlderThan: 24 * time.Hour})
			if ctx.checks < ctx.trigger || errorCode(err) != "prune_state_changed" || result.Stable != nil ||
				len(result.Removed)+len(result.SweptLocks)+len(result.SweptArtifacts) != 0 ||
				(test.cause != nil && !errors.Is(err, test.cause)) {
				t.Fatalf("unsafe dry run = %+v, %v after %d checks; want refusal with cause %v", result, err, ctx.checks, test.cause)
			}
			if current, err := os.Stat(retained); err != nil || !os.SameFile(identity, current) {
				t.Fatalf("original directory changed: %v, %v", current, err)
			}
		})
	}
}

func TestPruneDirectoryRevisionChangeBeforeCleanupDoesNotDelete(t *testing.T) {
	service, refs := createDraftListFixture(t, 1, 32)
	ageDraftFile(t, service.draftRoot, refs[0], 40)
	ctx := &draftListBoundaryContext{
		Context: context.Background(),
		trigger: 4,
		action: func() {
			marker := filepath.Join(service.draftRoot, "external-revision-change")
			if err := os.WriteFile(marker, []byte("changed"), 0o600); err != nil {
				t.Errorf("write revision marker: %v", err)
				return
			}
			future := time.Now().Add(2 * time.Second)
			if err := os.Chtimes(service.draftRoot, future, future); err != nil {
				t.Errorf("advance directory revision: %v", err)
			}
		},
	}
	result, err := service.PruneDraftsContext(ctx, PruneDraftsRequest{
		OlderThan: 30 * 24 * time.Hour, Confirm: true,
	})
	if ctx.checks < 2 || errorCode(err) != "prune_state_changed" || len(result.Removed) != 0 {
		t.Fatalf("PruneDraftsContext() = %+v, %v after %d checks; want revision refusal before deletion", result, err, ctx.checks)
	}
	if _, err := os.Stat(filepath.Join(service.draftRoot, refs[0]+".json")); err != nil {
		t.Fatalf("revision-changed prune removed the draft: %v", err)
	}
}

func TestPruneCandidateMetadataLimitReturnsTypedNoDeletionError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(nil, root)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create draft root: %v", err)
	}
	subject := strings.Repeat("s", MaximumDraftSubjectBytes)
	updatedAt := time.Now().Add(-40 * 24 * time.Hour).UTC()
	for index := 0; index < 1024; index++ {
		ref := fmt.Sprintf("draft_%024d", index)
		payload, err := json.Marshal(Draft{Ref: ref, Subject: subject, UpdatedAt: updatedAt})
		if err != nil {
			t.Fatalf("marshal %s fixture: %v", ref, err)
		}
		if err := os.WriteFile(filepath.Join(root, ref+".json"), payload, 0o600); err != nil {
			t.Fatalf("write %s fixture: %v", ref, err)
		}
	}
	firstPath := filepath.Join(root, "draft_000000000000000000000000.json")
	lastPath := filepath.Join(root, "draft_000000000000000000001023.json")
	result, err := service.PruneDraftsContext(context.Background(), PruneDraftsRequest{
		OlderThan: 30 * 24 * time.Hour, Confirm: true,
	})
	if errorCode(err) != "prune_candidate_limit_exceeded" || len(result.Candidates) != 0 || len(result.Removed) != 0 {
		t.Fatalf("PruneDraftsContext() = %+v, %v; want typed refusal before deletion", result, err)
	}
	for _, path := range []string{firstPath, lastPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("candidate file missing after bounded refusal: %v", err)
		}
	}
}

func BenchmarkDraftPruneCandidateSelection(b *testing.B) {
	const count = 10000
	root, _ := draftSelectionFixture(b, count)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		selection, err := collectPruneCandidates(context.Background(), root, time.Now())
		if err != nil || selection.entryVisits != count {
			b.Fatalf("entry visits = %d, error = %v; want %d", selection.entryVisits, err, count)
		}
	}
	b.ReportMetric(float64(count), "entry_visits/op")
}

func ageDraftTemporary(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-2 * draftTemporaryMinimumAge)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestPruneDraftTemporaryWriterSuffixes(t *testing.T) {
	for _, suffix := range []string{".json", ".send-spool", ".send-claim", ".save-claim", handoffClaimSuffix} {
		for _, live := range []bool{false, true} {
			for _, state := range []string{"old", "young", "busy", "symlink", "replacement"} {
				t.Run(fmt.Sprintf("%s/live_%t/%s", suffix, live, state), func(t *testing.T) {
					root := t.TempDir()
					service := NewServiceWithDraftRoot(nil, root)
					ref := "draft_123456789012345678901234"
					finalArtifacts := make(map[string][]byte)
					if live {
						draft := createSendTestDraft(t, service)
						ref = draft.Ref
						finalArtifacts[".send-spool"] = []byte("retained final evidence")
						now := time.Now().UTC()
						claimSuffix := ".send-claim"
						var claim []byte
						var err error
						switch suffix {
						case ".save-claim":
							claimSuffix = suffix
							claim, err = encodeDraftSaveAttempt(ref, DraftSaveAttempt{ID: "save-retained", StartedAt: now, UpdatedAt: now, ObservationBaseline: &SendObservationBaseline{StoreUUID: "store", CapturedUnix: now.Unix(), SentMailboxIDs: []int64{1}}})
						case handoffClaimSuffix:
							claimSuffix = suffix
							claim, err = encodeHandoffAttempt(ref, HandoffAttempt{ID: "handoff_0123456789abcdefghijklmn", DraftRef: ref, StartedAt: now, UpdatedAt: now, Outcome: HandoffOutcomeUnknown, DispatchStarted: true})
						default:
							claim, err = encodeSendAttempt(ref, SendAttempt{ID: "send-retained", StartedAt: now, UpdatedAt: now, Outcome: SendOutcomeUnknown, DraftRevision: draft.Revision})
						}
						if err != nil {
							t.Fatal(err)
						}
						finalArtifacts[claimSuffix] = claim
						for finalSuffix, payload := range finalArtifacts {
							if err := os.WriteFile(filepath.Join(root, ref+finalSuffix), payload, 0o600); err != nil {
								t.Fatal(err)
							}
						}
					}
					name := "." + ref + suffix + ".mailcli-0123456789abcdef01234567"
					path := filepath.Join(root, name)
					payload := []byte("verified unpublished bytes")
					if err := os.WriteFile(path, payload, 0o600); err != nil {
						t.Fatal(err)
					}
					if state != "young" {
						ageDraftTemporary(t, path)
					}
					var lease *draftLease
					if state == "busy" || state == "replacement" {
						var err error
						lease, err = acquireDraftLease(context.Background(), root, ref)
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if err := lease.release(); err != nil {
								t.Error(err)
							}
						})
					}
					if state == "replacement" {
						identity, err := lease.storage.lstat(name)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.Rename(path, path+".original"); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
							t.Fatal(err)
						}
						if err := removeDraftTemporaryFile(lease.storage, name, identity); errorCode(err) != "draft_lock_changed" {
							t.Fatalf("replacement cleanup = %v", err)
						}
						if got, err := os.ReadFile(path); err != nil || string(got) != "replacement" {
							t.Fatalf("replacement changed: %q, %v", got, err)
						}
						return
					}
					if state == "symlink" {
						if err := os.Rename(path, path+".original"); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(path+".original", path); err != nil {
							t.Fatal(err)
						}
					}
					var jsonBefore []byte
					if live {
						var err error
						jsonBefore, err = os.ReadFile(filepath.Join(root, ref+".json"))
						if err != nil {
							t.Fatal(err)
						}
					}
					for _, confirm := range []bool{false, true} {
						result, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: 24 * time.Hour, Confirm: confirm})
						wantFailure := confirm && state == "symlink"
						if (err != nil) != wantFailure {
							t.Fatalf("prune confirm=%t: %+v, %v", confirm, result, err)
						}
						wantCandidates := 0
						if state == "old" {
							wantCandidates = 1
						}
						if len(result.TemporaryArtifacts) != wantCandidates {
							t.Fatalf("temporary candidates=%+v", result.TemporaryArtifacts)
						}
						if wantCandidates == 1 && (result.TemporaryArtifacts[0].Name != name || result.TemporaryArtifacts[0].Ref != ref || result.TemporaryArtifacts[0].Size != int64(len(payload))) {
							t.Fatalf("temporary evidence=%+v", result.TemporaryArtifacts)
						}
						if confirm && state == "old" {
							if _, err := os.Lstat(path); !os.IsNotExist(err) {
								t.Fatalf("old temporary remains: %v", err)
							}
						} else if _, err := os.Lstat(path); err != nil {
							t.Fatalf("retained temporary missing: %v", err)
						}
					}
					if live {
						if got, err := os.ReadFile(filepath.Join(root, ref+".json")); err != nil || string(got) != string(jsonBefore) {
							t.Fatalf("live draft changed: %q, %v", got, err)
						}
						for finalSuffix, payload := range finalArtifacts {
							if got, err := os.ReadFile(filepath.Join(root, ref+finalSuffix)); err != nil || string(got) != string(payload) {
								t.Fatalf("live final artifact %s changed: %q, %v", finalSuffix, got, err)
							}
						}
					}
					if state == "symlink" {
						if got, err := os.ReadFile(path + ".original"); err != nil || string(got) != string(payload) {
							t.Fatalf("symlink target changed: %q, %v", got, err)
						}
					}
				})
			}
		}
	}
}

func TestPruneDraftTemporaryUnknownAndMalformedNames(t *testing.T) {
	root := t.TempDir()
	ref := "draft_123456789012345678901234"
	var names []string
	for _, suffix := range []string{".json", ".send-spool", ".send-claim", ".save-claim", handoffClaimSuffix} {
		names = append(names, ref+suffix+".mailcli-0123456789abcdef01234567", "."+ref+suffix+".mailcli-0123456789abcdef0123456g", "."+ref+suffix+".mailcli-0123456789abcdef012345678", ".bad"+suffix+".mailcli-0123456789abcdef01234567")
	}
	for index := 0; index < maximumPreservedTemporaryDiagnostics+5; index++ {
		names = append(names, fmt.Sprintf(".%s.unknown.mailcli-%024x", ref, index))
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(root, name), []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		ageDraftTemporary(t, filepath.Join(root, name))
	}
	service := NewServiceWithDraftRoot(nil, root)
	for _, confirm := range []bool{false, true} {
		result, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: 24 * time.Hour, Confirm: confirm})
		if err != nil || result.PreservedTemporaryCount != maximumPreservedTemporaryDiagnostics+5 || len(result.PreservedTemporaries) != maximumPreservedTemporaryDiagnostics || len(result.SweptArtifacts) != 0 {
			t.Fatalf("unknown cleanup=%+v, %v", result, err)
		}
		for _, name := range names {
			if got, err := os.ReadFile(filepath.Join(root, name)); err != nil || string(got) != "preserve" {
				t.Fatalf("preserved temporary=%q, %v", got, err)
			}
		}
	}
}
