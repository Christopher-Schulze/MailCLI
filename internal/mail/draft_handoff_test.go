package mail

import (
	"context"
	"encoding/json"
	"errors"
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
			var original storedHandoffAttempt
			if err := json.Unmarshal(before, &original); err != nil {
				t.Fatal(err)
			}
			retained, err := service.GetDraft(draft.Ref)
			if err != nil || retained.HandoffAttempt == nil || retained.HandoffAttempt.ID != original.Attempt.ID || retained.HandoffAttempt.Outcome != HandoffOutcomeCanceled || retained.HandoffAttempt.DispatchStarted {
				t.Fatalf("owning canceled claim lost: %+v, %v", retained.HandoffAttempt, err)
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

func newHandoffCleanupFixture(t *testing.T) (*Service, Draft, *DraftHandoffSession) {
	t.Helper()
	service := NewServiceWithDraftRoot(nil, t.TempDir())
	var attachments []string
	for _, name := range []string{"first", "second"} {
		path := filepath.Join(t.TempDir(), name+".txt")
		if err := os.WriteFile(path, []byte(name+" verified bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		attachments = append(attachments, path)
	}
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		To: []Recipient{{Address: "recipient@example.com"}}, Body: "retained body", Attachments: attachments,
	}})
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
	return service, draft, session
}

func TestHandoffTerminalCleanupPersistsOutcomeBeforeDeletion(t *testing.T) {
	for _, action := range []string{"finish opened", "finish failed", "cancel", "reconcile opened", "reconcile failed"} {
		t.Run(action, func(t *testing.T) {
			service, draft, session := newHandoffCleanupFixture(t)
			paths := session.Preparation().AttachmentPaths
			foreign := filepath.Join(filepath.Dir(paths[0]), "unrelated")
			if err := os.WriteFile(foreign, []byte("foreign bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			outcome, resolution := HandoffOutcomeConfirmedFailed, HandoffResolutionFailed
			var err error
			if action == "cancel" {
				outcome, err = HandoffOutcomeCanceled, session.CancelBeforeDispatch()
			} else {
				if action == "finish opened" || action == "reconcile opened" {
					outcome, resolution = HandoffOutcomeConfirmedOpened, HandoffResolutionOpened
				}
				if err := session.MarkDispatched(context.Background()); err != nil {
					t.Fatal(err)
				}
				if action == "reconcile opened" || action == "reconcile failed" {
					if err := session.Finish(HandoffOutcomeUnknown); err != nil {
						t.Fatal(err)
					}
					_, err = service.ReconcileDraftHandoff(draft.Ref, session.AttemptID(), resolution)
				} else {
					err = session.Finish(outcome)
				}
			}
			if err == nil {
				t.Fatal("foreign snapshot entry did not refuse cleanup")
			}
			retained, err := service.GetDraft(draft.Ref)
			if err != nil || retained.HandoffAttempt == nil || retained.HandoffAttempt.ID != session.AttemptID() || retained.HandoffAttempt.Outcome != outcome || !retained.HandoffAttempt.SnapshotsRetained {
				t.Fatalf("confirmed outcome was lost: %+v, %v", retained.HandoffAttempt, err)
			}
			claim := filepath.Join(service.draftRoot, draft.Ref+handoffClaimSuffix)
			before, err := os.ReadFile(claim)
			if err != nil {
				t.Fatal(err)
			}
			conflict := HandoffResolutionOpened
			if resolution == HandoffResolutionOpened {
				conflict = HandoffResolutionFailed
			}
			if _, err := service.ReconcileDraftHandoff(draft.Ref, session.AttemptID(), conflict); err == nil {
				t.Fatal("cleanup changed an already confirmed outcome")
			}
			if after, err := os.ReadFile(claim); err != nil || string(after) != string(before) {
				t.Fatalf("conflicting resolution changed claim: %q, %v", after, err)
			}
			if got, err := os.ReadFile(foreign); err != nil || string(got) != "foreign bytes" {
				t.Fatalf("foreign entry changed: %q, %v", got, err)
			}
			if err := os.Rename(foreign, filepath.Join(t.TempDir(), "retained-foreign")); err != nil {
				t.Fatal(err)
			}
			result, err := service.ReconcileDraftHandoff(draft.Ref, session.AttemptID(), resolution)
			if err != nil || result.Outcome != outcome || result.SnapshotsRetained {
				t.Fatalf("cleanup retry = %+v, %v", result, err)
			}
			clean, err := service.GetDraft(draft.Ref)
			if err != nil || clean.HandoffAttempt != nil || clean.Revision != draft.Revision || clean.Body != draft.Body {
				t.Fatalf("cleanup changed draft: %+v, %v", clean, err)
			}
			for _, path := range paths {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("snapshot remains: %v", err)
				}
			}
		})
	}
}

type handoffCleanupBoundaryContext struct {
	context.Context
	path      string
	cancel    context.CancelFunc
	triggered bool
	ready     func() bool
	action    func()
}

func (ctx *handoffCleanupBoundaryContext) Err() error {
	_, err := os.Lstat(ctx.path)
	ready := os.IsNotExist(err)
	if ctx.ready != nil {
		ready = ctx.ready()
	}
	if ready && !ctx.triggered {
		ctx.triggered = true
		if ctx.action != nil {
			ctx.action()
		} else {
			ctx.cancel()
		}
	}
	return ctx.Context.Err()
}

func TestPreparedHandoffCleanupResumesAfterOwnedDeletion(t *testing.T) {
	for _, boundary := range []string{"first file", "first index", "last file", "last index", "attempt", "parent"} {
		t.Run(boundary, func(t *testing.T) {
			service, draft, session := newHandoffCleanupFixture(t)
			paths := session.Preparation().AttachmentPaths
			attempt := &session.attempt
			attempt.Snapshots, attempt.SnapshotsRetained = nil, false
			if err := replaceHandoffAttempt(draft.Ref, *attempt, session.lease.storage); err != nil {
				t.Fatal(err)
			}
			checkpoints := map[string]string{
				"first file": paths[0], "first index": filepath.Dir(paths[0]),
				"last file": paths[1], "last index": filepath.Dir(paths[1]),
				"attempt": filepath.Dir(filepath.Dir(paths[0])),
				"parent":  filepath.Join(service.draftRoot, draft.Ref+handoffSnapshotSuffix),
			}
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &handoffCleanupBoundaryContext{Context: base, path: checkpoints[boundary], cancel: cancel}
			err := recoverPreparedHandoffStaging(ctx, draft.Ref, attempt.ID, draft.Attachments, session.lease.storage)
			if !ctx.triggered || !errors.Is(err, context.Canceled) {
				t.Fatalf("cleanup boundary triggered=%t error=%v", ctx.triggered, err)
			}
			if err := session.lease.release(); err != nil {
				t.Fatal(err)
			}
			session.closed = true
			next, err := service.BeginDraftHandoff(draft.Ref)
			if err != nil {
				t.Fatalf("prepared recovery after %s: %v", boundary, err)
			}
			if next.AttemptID() == attempt.ID {
				t.Fatal("cleanup replay reused the old attempt")
			}
			if err := next.CancelBeforeDispatch(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHandoffConfirmedCleanupResumesAtEveryBoundary(t *testing.T) {
	for _, outcome := range []HandoffOutcome{HandoffOutcomeConfirmedOpened, HandoffOutcomeConfirmedFailed, HandoffOutcomeCanceled} {
		for _, boundary := range []string{"first file", "first index", "last file", "last index", "attempt", "parent", "claim"} {
			t.Run(fmt.Sprintf("%s/%s", outcome, boundary), func(t *testing.T) {
				service, draft, session := newHandoffCleanupFixture(t)
				paths := session.Preparation().AttachmentPaths
				// Force the public terminal transition to persist its outcome
				// while a real foreign entry refuses the first cleanup.
				foreign := filepath.Join(filepath.Dir(paths[0]), "unrelated")
				if err := os.WriteFile(foreign, []byte("retained"), 0o600); err != nil {
					t.Fatal(err)
				}
				var err error
				if outcome == HandoffOutcomeCanceled {
					err = session.CancelBeforeDispatch()
				} else {
					if err := session.MarkDispatched(context.Background()); err != nil {
						t.Fatal(err)
					}
					err = session.Finish(outcome)
				}
				if err == nil {
					t.Fatal("initial cleanup unexpectedly succeeded")
				}
				if err := os.Rename(foreign, filepath.Join(t.TempDir(), "foreign")); err != nil {
					t.Fatal(err)
				}
				checkpoints := map[string]string{
					"first file": paths[0], "first index": filepath.Dir(paths[0]),
					"last file": paths[1], "last index": filepath.Dir(paths[1]),
					"attempt": filepath.Dir(filepath.Dir(paths[0])),
					"parent":  filepath.Join(service.draftRoot, draft.Ref+handoffSnapshotSuffix),
				}
				base, cancel := context.WithCancel(context.Background())
				defer cancel()
				ctx := &handoffCleanupBoundaryContext{Context: base, path: checkpoints[boundary], cancel: cancel}
				if boundary == "claim" {
					ctx.ready = func() bool {
						payload, err := os.ReadFile(filepath.Join(service.draftRoot, draft.Ref+handoffClaimSuffix))
						var stored storedHandoffAttempt
						return err == nil && json.Unmarshal(payload, &stored) == nil && !stored.Attempt.SnapshotsRetained
					}
				}
				resolution := HandoffResolutionFailed
				if outcome == HandoffOutcomeConfirmedOpened {
					resolution = HandoffResolutionOpened
				}
				result, err := service.ReconcileDraftHandoffContext(ctx, draft.Ref, session.AttemptID(), resolution)
				if !ctx.triggered || !errors.Is(err, context.Canceled) || result.Outcome != outcome {
					t.Fatalf("boundary triggered=%t result=%+v error=%v", ctx.triggered, result, err)
				}
				if (boundary == "parent" || boundary == "claim") && result.SnapshotsRetained {
					t.Fatalf("removed snapshot root was reported retained: %+v", result)
				}
				retained, err := service.GetDraft(draft.Ref)
				if err != nil || retained.HandoffAttempt == nil || retained.HandoffAttempt.Outcome != outcome {
					t.Fatalf("interruption lost known outcome: %+v, %v", retained.HandoffAttempt, err)
				}
				result, err = service.ReconcileDraftHandoff(draft.Ref, session.AttemptID(), resolution)
				if err != nil || result.Outcome != outcome || result.SnapshotsRetained {
					t.Fatalf("resume after %s = %+v, %v", boundary, result, err)
				}
			})
		}
	}
}

func TestPreparedCompleteHandoffCleanupResumesWithMissingRoots(t *testing.T) {
	for _, boundary := range []string{"index", "attempt", "parent"} {
		t.Run(boundary, func(t *testing.T) {
			service, draft, session := newHandoffCleanupFixture(t)
			path := session.Preparation().AttachmentPaths[0]
			checkpoints := map[string]string{
				"index": filepath.Dir(path), "attempt": filepath.Dir(filepath.Dir(path)),
				"parent": filepath.Join(service.draftRoot, draft.Ref+handoffSnapshotSuffix),
			}
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &handoffCleanupBoundaryContext{Context: base, path: checkpoints[boundary], cancel: cancel}
			if err := recoverPreparedHandoffStaging(ctx, draft.Ref, session.AttemptID(), draft.Attachments, session.lease.storage); !ctx.triggered || !errors.Is(err, context.Canceled) {
				t.Fatalf("full prepared boundary triggered=%t error=%v", ctx.triggered, err)
			}
			if err := session.lease.release(); err != nil {
				t.Fatal(err)
			}
			session.closed = true
			next, err := service.BeginDraftHandoff(draft.Ref)
			if err != nil {
				t.Fatalf("full prepared recovery after %s: %v", boundary, err)
			}
			if err := next.CancelBeforeDispatch(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHandoffCleanupPreservesTerminalEvidenceOnIOFailure(t *testing.T) {
	for _, fault := range []string{"directory sync", "claim replacement"} {
		t.Run(fault, func(t *testing.T) {
			service, draft, session := newHandoffCleanupFixture(t)
			foreign := filepath.Join(filepath.Dir(session.Preparation().AttachmentPaths[0]), "unrelated")
			if err := os.WriteFile(foreign, []byte("retained"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := session.MarkDispatched(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := session.Finish(HandoffOutcomeConfirmedOpened); err == nil {
				t.Fatal("initial cleanup unexpectedly succeeded")
			}
			if err := os.Rename(foreign, filepath.Join(t.TempDir(), "foreign")); err != nil {
				t.Fatal(err)
			}
			lease, err := acquireDraftLease(context.Background(), service.draftRoot, draft.Ref)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := lease.release(); err != nil {
					t.Error(err)
				}
			})
			attempt, err := readHandoffAttempt(draft.Ref, lease.storage)
			if err != nil || attempt == nil {
				t.Fatalf("terminal attempt=%+v error=%v", attempt, err)
			}
			originalDirectory := lease.storage.directory
			ctx := &handoffCleanupBoundaryContext{Context: context.Background()}
			claimPath := filepath.Join(service.draftRoot, draft.Ref+handoffClaimSuffix)
			retainedClaim := filepath.Join(t.TempDir(), "retained-claim")
			sentinel := filepath.Join(t.TempDir(), "sentinel")
			if err := os.WriteFile(sentinel, []byte("foreign claim target"), 0o600); err != nil {
				t.Fatal(err)
			}
			if fault == "directory sync" {
				closed, err := os.Open(service.draftRoot)
				if err != nil {
					t.Fatal(err)
				}
				if err := closed.Close(); err != nil {
					t.Fatal(err)
				}
				ctx.path = filepath.Join(service.draftRoot, draft.Ref+handoffSnapshotSuffix, attempt.ID)
				ctx.action = func() { lease.storage.directory = closed }
			} else {
				ctx.ready = func() bool {
					current, err := readHandoffAttempt(draft.Ref, lease.storage)
					return err == nil && current != nil && !current.SnapshotsRetained
				}
				ctx.action = func() {
					if err := os.Rename(claimPath, retainedClaim); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(sentinel, claimPath); err != nil {
						t.Fatal(err)
					}
				}
			}
			err = cleanupTerminalHandoffAttempt(ctx, draft.Ref, attempt, lease.storage, nil)
			lease.storage.directory = originalDirectory
			if !ctx.triggered || err == nil {
				t.Fatalf("I/O fault triggered=%t error=%v", ctx.triggered, err)
			}
			if fault == "claim replacement" {
				if errorCode(err) != "draft_lock_changed" {
					t.Fatalf("changed terminal claim accepted: %v", err)
				}
				if info, err := os.Lstat(claimPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("foreign claim link changed: %v, %v", info, err)
				}
				payload, err := os.ReadFile(retainedClaim)
				var stored storedHandoffAttempt
				if err != nil || json.Unmarshal(payload, &stored) != nil || stored.Attempt.Outcome != HandoffOutcomeConfirmedOpened || stored.Attempt.SnapshotsRetained {
					t.Fatalf("confirmed cleanup evidence lost: %q, %v", payload, err)
				}
			} else {
				if errorCode(err) != "handoff_attachment_cleanup_failed" || !errors.Is(err, os.ErrClosed) {
					t.Fatalf("real closed-descriptor sync failure lost: %v", err)
				}
				if err := lease.release(); err != nil {
					t.Fatal(err)
				}
				if result, err := service.ReconcileDraftHandoff(draft.Ref, session.AttemptID(), HandoffResolutionOpened); err != nil || result.SnapshotsRetained || result.Outcome != HandoffOutcomeConfirmedOpened {
					t.Fatalf("sync-failure recovery = %+v, %v", result, err)
				}
			}
			if got, err := os.ReadFile(sentinel); err != nil || string(got) != "foreign claim target" {
				t.Fatalf("foreign claim target changed: %q, %v", got, err)
			}
		})
	}
}
