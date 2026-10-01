package mail

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestDraftContextErrorDistinguishesDeadline(t *testing.T) {
	for _, test := range []struct {
		name     string
		deadline bool
		want     string
		cause    error
	}{
		{"caller cancellation", false, "draft_operation_canceled", context.Canceled},
		{"operation deadline", true, "draft_operation_timeout", context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if test.deadline {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			} else {
				cancel()
			}
			defer cancel()
			err := draftContextError(ctx, "list")
			if errorCode(err) != test.want || !errors.Is(err, test.cause) {
				t.Fatalf("context error=%v, code=%s, want=%s", err, errorCode(err), test.want)
			}
		})
	}
	if err := draftContextError(context.Background(), "list"); err != nil {
		t.Fatalf("live context failed: %v", err)
	}
}

func TestDraftContextClassificationRetainsSpecificCauses(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"untyped deadline", context.DeadlineExceeded, "draft_operation_timeout"},
		{"outer deadline while busy", &OperationError{Code: "draft_busy", Message: "busy"}, "draft_operation_timeout"},
		{"specific operation", &OperationError{Code: "draft_state_error", Message: "invalid", Err: context.DeadlineExceeded}, "draft_state_error"},
		{"submission uncertainty", &transport.SubmissionError{Stage: "final reply", Err: context.DeadlineExceeded}, transport.CodeSMTPSubmissionUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := classifyDraftContextError(ctx, test.err, "send")
			if errorCode(got) != test.want || !errors.Is(got, test.err) {
				t.Fatalf("classified=%v, code=%s, want=%s, cause retained=%t", got, errorCode(got), test.want, errors.Is(got, test.err))
			}
		})
	}
	plain := errors.New("independent failure")
	if got := classifyDraftContextError(ctx, plain, "list"); got != plain {
		t.Fatalf("unrelated error replaced: %v", got)
	}
}

func TestDraftDeadlineWaitRetainsOtherOwnersLock(t *testing.T) {
	for _, outerDeadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "lease budget", true: "operation budget"}[outerDeadline], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "drafts")
			ref := "draft_abcdefghijklmnopqrstuvwx"
			command, release := startDraftLockHelper(t, root, ref)
			defer func() {
				if err := release.Close(); err != nil {
					t.Error(err)
				}
				if err := command.Wait(); err != nil {
					t.Error(err)
				}
			}()
			operationCtx := context.Background()
			lockCtx, cancel := context.WithTimeout(operationCtx, 25*time.Millisecond)
			defer cancel()
			want := "draft_busy"
			if outerDeadline {
				operationCtx, want = lockCtx, "draft_operation_timeout"
			}
			lease, err := acquireDraftLease(lockCtx, root, ref)
			err = classifyDraftContextError(operationCtx, err, "send")
			if lease != nil || errorCode(err) != want {
				t.Fatalf("lease=%v, error=%v, want=%s", lease, err, want)
			}
			if outerDeadline && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("outer deadline cause lost")
			}
			path, err := draftLockPath(root, ref)
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(path, os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			lockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(lockErr, syscall.EWOULDBLOCK) {
				t.Fatalf("other owner's lock was released: %v", lockErr)
			}
		})
	}
}

func TestDraftPruneDeadlineRetainsCompletedEvidence(t *testing.T) {
	for _, receipts := range []bool{false, true} {
		t.Run(map[bool]string{false: "draft candidates", true: "receipt candidates"}[receipts], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "drafts")
			ref := "draft_abcdefghijklmnopqrstuvwx"
			command, release := startDraftLockHelper(t, root, ref)
			defer func() {
				if err := release.Close(); err != nil {
					t.Error(err)
				}
				if err := command.Wait(); err != nil {
					t.Error(err)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			result := PruneDraftsResult{Removed: []string{"already-removed"}}
			var err error
			if receipts {
				result.ExpiredReceipts = []string{ref}
				result, err = pruneListedReceipts(ctx, root, result)
			} else {
				result.Candidates = []PruneCandidate{{Ref: ref}}
				result, err = pruneListedDrafts(ctx, root, time.Now(), result)
			}
			if errorCode(err) != "draft_operation_timeout" || !errors.Is(err, context.DeadlineExceeded) || len(result.Failed) != 0 || len(result.Removed) != 1 || result.Removed[0] != "already-removed" {
				t.Fatalf("prune lost timeout or prior effects: result=%+v, error=%v", result, err)
			}
		})
	}
}
