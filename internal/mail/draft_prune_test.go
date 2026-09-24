package mail

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

func TestDraftMutationRecoversJSONTemporaryUnderLease(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&draftGateway{}, root)
	draft := createSendTestDraft(t, service)
	name := "." + draft.Ref + ".json.mailcli-0123456789abcdef01234567"
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte("partial draft update"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("stale JSON temporary remains: %v", err)
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
				if err := removeDraftJSONTemporaryFiles(lease.storage, draft.Ref); err == nil {
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
				if err := removeDraftJSONTemporaryFile(lease.storage, name, expected); err == nil {
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
