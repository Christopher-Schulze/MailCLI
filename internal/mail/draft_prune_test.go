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
	if _, err := beginSendAttempt(root, draft.Ref, "", ""); err != nil {
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

func TestPruneSweepsOrphanHandoffArtifacts(t *testing.T) {
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
	result, err := service.PruneDrafts(
		PruneDraftsRequest{OlderThan: 30 * 24 * time.Hour, Confirm: true},
	)
	if err != nil {
		t.Fatalf("PruneDrafts() error = %v", err)
	}
	if len(result.SweptArtifacts) != 1 || result.SweptArtifacts[0] != draft.Ref {
		t.Fatalf("SweptArtifacts = %v, want [%s]", result.SweptArtifacts, draft.Ref)
	}
	for _, name := range []string{
		draft.Ref + handoffClaimSuffix,
		draft.Ref + handoffSnapshotSuffix,
		draft.Ref + ".lock",
	} {
		if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("orphan %s still present: %v", name, err)
		}
	}
}

func TestPruneKeepsArtifactsWhileDraftExists(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&draftGateway{}, root)
	draft := createSendTestDraft(t, service)
	if _, err := beginSendAttempt(root, draft.Ref, "", ""); err != nil {
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
	if _, err := beginSendAttempt(root, draft.Ref, "", ""); err != nil {
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
	if _, err := beginSendAttempt(root, draft.Ref, "", ""); err != nil {
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
