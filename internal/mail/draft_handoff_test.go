package mail

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDraftHandoffTerminalPathsRemoveSnapshotParent(t *testing.T) {
	for _, outcome := range []string{"cancel", "finish", "reconcile", "discard"} {
		for _, attachments := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/attachments_%t", outcome, attachments), func(t *testing.T) {
				root := filepath.Join(t.TempDir(), "drafts")
				service := NewServiceWithDraftRoot(nil, root)
				input := DraftInput{To: []Recipient{{Address: "ada@example.com"}}, Body: "body"}
				if attachments {
					path := filepath.Join(t.TempDir(), "attachment.txt")
					if err := os.WriteFile(path, []byte("attachment bytes"), 0o600); err != nil {
						t.Fatal(err)
					}
					input.Attachments = []string{path}
				}
				draft, err := service.CreateDraft(CreateDraftRequest{Input: input})
				if err != nil {
					t.Fatal(err)
				}
				session, err := service.BeginDraftHandoff(draft.Ref)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := session.Close(); err != nil {
						t.Error(err)
					}
				})
				switch outcome {
				case "cancel", "discard":
					err = session.CancelBeforeDispatch()
				case "finish":
					if err = session.MarkDispatched(context.Background()); err == nil {
						err = session.Finish(HandoffOutcomeConfirmedOpened)
					}
				case "reconcile":
					if err = session.MarkDispatched(context.Background()); err == nil {
						err = session.Finish(HandoffOutcomeUnknown)
					}
					if err == nil {
						_, err = service.ReconcileDraftHandoff(draft.Ref, session.AttemptID(), HandoffResolutionFailed)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				if outcome == "discard" {
					if err := service.DiscardDraft(draft.Ref); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := os.Lstat(filepath.Join(root, draft.Ref+handoffSnapshotSuffix)); !os.IsNotExist(err) {
					t.Fatalf("snapshot parent remains: %v", err)
				}
				if result, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: 24 * time.Hour, Confirm: true}); err != nil || len(result.Failed) != 0 {
					t.Fatalf("prune = %+v, %v", result, err)
				}
			})
		}
	}
}

func TestDraftHandoffUnknownAttemptRetainsEvidenceUntilReconciliation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(nil, root)
	attachment := filepath.Join(t.TempDir(), "attachment.txt")
	if err := os.WriteFile(attachment, []byte("retained bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		To: []Recipient{{Address: "ada@example.com"}}, Body: "Body", Attachments: []string{attachment},
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	session, err := service.BeginDraftHandoff(draft.Ref)
	if err != nil {
		t.Fatalf("BeginDraftHandoff() error = %v", err)
	}
	preparation := session.Preparation()
	if len(preparation.AttachmentPaths) != 1 {
		t.Fatalf("handoff preparation paths = %v", preparation.AttachmentPaths)
	}
	if err := session.MarkDispatched(context.Background()); err != nil {
		t.Fatalf("MarkDispatched() error = %v", err)
	}
	if err := session.Finish(HandoffOutcomeUnknown); err != nil {
		t.Fatalf("Finish(unknown) error = %v", err)
	}
	retained, err := service.GetDraft(draft.Ref)
	if err != nil {
		t.Fatalf("GetDraft() error = %v", err)
	}
	if retained.HandoffAttempt == nil || retained.HandoffAttempt.ID != preparation.Attempt.ID ||
		retained.HandoffAttempt.Outcome != HandoffOutcomeUnknown || !retained.HandoffAttempt.SnapshotsRetained {
		t.Fatalf("retained handoff state = %+v", retained.HandoffAttempt)
	}
	if _, err := os.Stat(preparation.AttachmentPaths[0]); err != nil {
		t.Fatalf("retained snapshot stat error = %v", err)
	}
	if _, err := service.BeginDraftHandoff(draft.Ref); errorCode(err) != "handoff_retry_blocked" {
		t.Fatalf("BeginDraftHandoff() error = %v, want handoff_retry_blocked", err)
	}
	if _, err := service.ReconcileDraftHandoff(draft.Ref, preparation.Attempt.ID, HandoffResolutionOpened); err != nil {
		t.Fatalf("ReconcileDraftHandoff() error = %v", err)
	}
	if _, err := os.Stat(preparation.AttachmentPaths[0]); !os.IsNotExist(err) {
		t.Fatalf("snapshot after reconciliation stat error = %v", err)
	}
	reconciled, err := service.GetDraft(draft.Ref)
	if err != nil {
		t.Fatalf("GetDraft() after reconciliation error = %v", err)
	}
	if reconciled.HandoffAttempt != nil {
		t.Fatalf("handoff state after reconciliation = %+v", reconciled.HandoffAttempt)
	}
}

func TestDraftHandoffCancellationBeforeDispatchCleansEvidence(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(nil, root)
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		To: []Recipient{{Address: "ada@example.com"}}, Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	session, err := service.BeginDraftHandoff(draft.Ref)
	if err != nil {
		t.Fatalf("BeginDraftHandoff() error = %v", err)
	}
	attemptID := session.AttemptID()
	if err := session.CancelBeforeDispatch(); err != nil {
		t.Fatalf("CancelBeforeDispatch() error = %v", err)
	}
	if _, err := service.GetDraft(draft.Ref); err != nil {
		t.Fatalf("GetDraft() error = %v", err)
	}
	next, err := service.BeginDraftHandoff(draft.Ref)
	if err != nil {
		t.Fatalf("BeginDraftHandoff() after cancellation error = %v", err)
	}
	if err := next.CancelBeforeDispatch(); err != nil {
		t.Fatalf("CancelBeforeDispatch() after retry error = %v", err)
	}
	if _, err := service.ReconcileDraftHandoff(draft.Ref, attemptID, HandoffResolutionFailed); errorCode(err) != "handoff_not_found" {
		t.Fatalf("ReconcileDraftHandoff() error = %v, want handoff_not_found", err)
	}
}

func TestDraftHandoffReconcileRejectsPreparedAttempt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(nil, root)
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		To: []Recipient{{Address: "ada@example.com"}}, Body: "Body",
	}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.BeginDraftHandoff(draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.lease.release(); err != nil {
		t.Fatal(err)
	}
	session.closed = true
	if _, err := service.ReconcileDraftHandoff(draft.Ref, session.AttemptID(), HandoffResolutionOpened); errorCode(err) != "handoff_not_dispatched" {
		t.Fatalf("ReconcileDraftHandoff() error = %v, want handoff_not_dispatched", err)
	}
}

func TestDraftHandoffReconcileValidatesAttemptID(t *testing.T) {
	service := NewServiceWithDraftRoot(nil, filepath.Join(t.TempDir(), "drafts"))
	if _, err := service.ReconcileDraftHandoff("draft_123456789012345678901234567", "bad", HandoffResolutionFailed); errorCode(err) != "invalid_argument" {
		t.Fatalf("ReconcileDraftHandoff() error = %v, want invalid_argument", err)
	}
}

func TestDraftHandoffRecoversPartialPreparedStagingAfterCrash(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(nil, root)
	attachment := filepath.Join(t.TempDir(), "attachment.txt")
	if err := os.WriteFile(attachment, []byte("complete attachment payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		To: []Recipient{{Address: "ada@example.com"}}, Body: "Body", Attachments: []string{attachment},
	}})
	if err != nil {
		t.Fatal(err)
	}
	attemptID := "handoff_123456789012345678901234"
	writePreparedHandoffClaimFixture(t, root, draft.Ref, attemptID, nil)
	partialDirectory := filepath.Join(root, draft.Ref+handoffSnapshotSuffix, attemptID, "0")
	if err := os.MkdirAll(partialDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	partialPath := filepath.Join(partialDirectory, filepath.Base(attachment))
	if err := os.WriteFile(partialPath, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := service.BeginDraftHandoff(draft.Ref)
	if err != nil {
		t.Fatalf("BeginDraftHandoff() after prepared crash fixture error = %v", err)
	}
	if session.AttemptID() == attemptID {
		t.Fatal("retry reused the recovered handoff attempt ID")
	}
	if _, err := os.Lstat(partialPath); !os.IsNotExist(err) {
		t.Fatalf("partial snapshot survived recovery: %v", err)
	}
	if err := session.CancelBeforeDispatch(); err != nil {
		t.Fatalf("CancelBeforeDispatch() error = %v", err)
	}
}

func TestDraftHandoffPreparedRecoveryPreservesSymlinkedStaging(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(nil, root)
	attachment := filepath.Join(t.TempDir(), "attachment.txt")
	if err := os.WriteFile(attachment, []byte("complete attachment payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		To: []Recipient{{Address: "ada@example.com"}}, Body: "Body", Attachments: []string{attachment},
	}})
	if err != nil {
		t.Fatal(err)
	}
	attemptID := "handoff_123456789012345678901234"
	writePreparedHandoffClaimFixture(t, root, draft.Ref, attemptID, nil)
	indexDirectory := filepath.Join(root, draft.Ref+handoffSnapshotSuffix, attemptID, "0")
	if err := os.MkdirAll(indexDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(sentinel, []byte("external bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	stagedPath := filepath.Join(indexDirectory, filepath.Base(attachment))
	if err := os.Symlink(sentinel, stagedPath); err != nil {
		t.Fatal(err)
	}
	claimPath := filepath.Join(root, draft.Ref+handoffClaimSuffix)
	claimBefore, err := os.ReadFile(claimPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.BeginDraftHandoff(draft.Ref); err == nil {
		t.Fatal("BeginDraftHandoff() accepted symlinked prepared staging")
	}
	if info, err := os.Lstat(stagedPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("staging symlink changed: %v, %v", info, err)
	}
	if claimAfter, err := os.ReadFile(claimPath); err != nil || string(claimAfter) != string(claimBefore) {
		t.Fatalf("prepared claim changed: %q, %v", claimAfter, err)
	}
	if payload, err := os.ReadFile(sentinel); err != nil || string(payload) != "external bytes" {
		t.Fatalf("symlink target changed: %q, %v", payload, err)
	}
}

func TestDraftHandoffCancelPreservesClaimAfterCleanupRefusal(t *testing.T) {
	for _, unsafe := range []string{"nonempty", "symlink", "mode"} {
		t.Run(unsafe, func(t *testing.T) {
			root := t.TempDir()
			service := NewServiceWithDraftRoot(nil, root)
			draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{To: []Recipient{{Address: "ada@example.com"}}, Body: "body"}})
			if err != nil {
				t.Fatal(err)
			}
			session, err := service.BeginDraftHandoff(draft.Ref)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if !session.closed {
					if err := session.lease.release(); err != nil {
						t.Error(err)
					}
				}
			})
			parent := filepath.Join(root, draft.Ref+handoffSnapshotSuffix)
			claim := filepath.Join(root, draft.Ref+handoffClaimSuffix)
			before, err := os.ReadFile(claim)
			if err != nil {
				t.Fatal(err)
			}
			switch unsafe {
			case "nonempty":
				if err := os.WriteFile(filepath.Join(parent, "foreign"), []byte("foreign bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(parent, parent+".retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), parent); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(parent, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := session.CancelBeforeDispatch(); err == nil {
				t.Fatal("unsafe cancellation cleanup succeeded")
			}
			if after, err := os.ReadFile(claim); err != nil || string(after) != string(before) {
				t.Fatalf("owning claim changed: %q, %v", after, err)
			}
			if _, err := os.Lstat(parent); err != nil {
				t.Fatalf("unsafe parent removed: %v", err)
			}
			if err := service.DiscardDraft(draft.Ref); errorCode(err) != "handoff_retry_blocked" {
				t.Fatalf("claim no longer blocks discard: %v", err)
			}
		})
	}
}

func TestDraftHandoffCancelAfterDispatchRetainsUncertainty(t *testing.T) {
	service := NewServiceWithDraftRoot(nil, t.TempDir())
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{To: []Recipient{{Address: "ada@example.com"}}, Body: "body"}})
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
	if err := session.CancelBeforeDispatch(); err != nil {
		t.Fatal(err)
	}
	loaded, err := service.GetDraft(draft.Ref)
	if err != nil || loaded.HandoffAttempt == nil || !loaded.HandoffAttempt.DispatchStarted || loaded.HandoffAttempt.Outcome != HandoffOutcomeUnknown {
		t.Fatalf("dispatch evidence lost: %+v, %v", loaded.HandoffAttempt, err)
	}
	if _, err := os.Lstat(filepath.Join(service.draftRoot, draft.Ref+handoffSnapshotSuffix)); err != nil {
		t.Fatal(err)
	}
	if err := service.DiscardDraft(draft.Ref); errorCode(err) != "handoff_retry_blocked" {
		t.Fatalf("uncertain handoff discarded: %v", err)
	}
}

func TestDraftHandoffCancelRemovesParentWithAbsentEmptyAttempt(t *testing.T) {
	service := NewServiceWithDraftRoot(nil, t.TempDir())
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{To: []Recipient{{Address: "ada@example.com"}}, Body: "body"}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.BeginDraftHandoff(draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(service.draftRoot, draft.Ref+handoffSnapshotSuffix)
	if err := os.Remove(filepath.Join(parent, session.AttemptID())); err != nil {
		t.Fatal(err)
	}
	if err := session.CancelBeforeDispatch(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(parent); !os.IsNotExist(err) {
		t.Fatalf("parent remains: %v", err)
	}
}
