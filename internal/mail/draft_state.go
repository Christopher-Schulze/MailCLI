package mail

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

func (s *Service) resolveDraftRoot() (string, error) {
	root := s.draftRoot
	if root == "" {
		var err error
		root, err = defaultDraftRoot()
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(root) {
		return "", validationError("draft root must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create draft directory: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", fmt.Errorf("restrict draft directory: %w", err)
	}
	return root, nil
}

func defaultDraftRoot() (string, error) {
	configRoot, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve Application Support directory: %w", err)
	}
	return filepath.Join(configRoot, "MailCLI", "drafts"), nil
}

func newDraftReference() (string, error) {
	var bytes [18]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate draft ref: %w", err)
	}
	return "draft_" + base64.RawURLEncoding.EncodeToString(bytes[:]), nil
}

func draftPath(root string, ref string) (string, error) {
	if !strings.HasPrefix(ref, "draft_") || len(ref) != 30 || strings.ContainsAny(ref, "/\\") {
		return "", validationError("invalid draft ref")
	}
	return filepath.Join(root, ref+".json"), nil
}

func draftLockPath(root string, ref string) (string, error) {
	if _, err := draftPath(root, ref); err != nil {
		return "", err
	}
	return filepath.Join(root, ref+".lock"), nil
}

func sendClaimPath(root string, ref string) (string, error) {
	if _, err := draftPath(root, ref); err != nil {
		return "", err
	}
	return filepath.Join(root, ref+".send-claim"), nil
}

func sendReceiptPath(root string, ref string) (string, error) {
	if _, err := draftPath(root, ref); err != nil {
		return "", err
	}
	return filepath.Join(root, ref+".send-receipt"), nil
}

func saveClaimPath(root string, ref string) (string, error) {
	if _, err := draftPath(root, ref); err != nil {
		return "", err
	}
	return filepath.Join(root, ref+".save-claim"), nil
}

type draftLease struct {
	lock        *draftLockResource
	storage     *draftStorage
	releaseOnce sync.Once
	releaseErr  error
}

func acquireDraftLease(ctx context.Context, root string, ref string) (*draftLease, error) {
	if runtime.GOOS == "plan9" || (runtime.GOOS == "js" && runtime.GOARCH == "wasm") {
		return nil, &OperationError{
			Code:    "unsupported_platform",
			Message: "descriptor-backed draft storage is unavailable on this platform",
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock, err := openDraftLockResource(root, ref)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(draftLockPoll)
	defer ticker.Stop()
	for {
		err := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			if ctx.Err() != nil {
				closeErr := errors.Join(syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN), lock.close())
				return nil, errors.Join(&OperationError{Code: "draft_busy", Message: "draft is busy with another operation"}, closeErr)
			}
			var storage *draftStorage
			if runtime.GOOS == "darwin" {
				pinnedRoot, storageErr := os.OpenRoot(fmt.Sprintf("/dev/fd/%d", lock.directory.Fd()))
				if storageErr != nil {
					return nil, errors.Join(fmt.Errorf("open pinned draft directory: %w", storageErr), syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN), lock.close())
				}
				storage = &draftStorage{root: pinnedRoot, rootName: root, directory: lock.directory}
			} else {
				var storageErr error
				storage, storageErr = openPinnedDraftStorage(root, lock, ref)
				if storageErr != nil {
					return nil, errors.Join(storageErr, syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN), lock.close())
				}
			}
			return &draftLease{lock: lock, storage: storage}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(fmt.Errorf("lock draft: %w", err), lock.close())
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(&OperationError{Code: "draft_busy", Message: "draft is busy with another operation"}, lock.close())
		case <-ticker.C:
		}
	}
}

func (l *draftLease) release() error {
	if l == nil {
		return nil
	}
	l.releaseOnce.Do(func() {
		l.releaseErr = errors.Join(
			syscall.Flock(int(l.lock.file.Fd()), syscall.LOCK_UN),
			l.lock.close(),
			l.storage.root.Close(),
		)
	})
	return l.releaseErr
}

func (l *draftLease) removeLock() error {
	if runtime.GOOS == "darwin" {
		return l.lock.remove()
	}
	return removeDraftStorageLock(l.storage, l.lock.name, l.lock.identity)
}

// readDraftForMutation reads a draft while the caller holds its exclusive lease.
// On a missing draft the lock file created by acquireDraftLease is removed through
// that lease's pinned parent and file identity, so a failed mutation leaves no
// orphan lock and cannot clean an attacker-replaced path.
func readDraftForMutation(lease *draftLease, root string, ref string) (Draft, error) {
	draft, err := readDraftFileWithObserver(root, ref, nil, lease.storage)
	if err != nil {
		var operation *OperationError
		if errors.As(err, &operation) && operation.Code == "not_found" {
			err = errors.Join(err, lease.removeLock())
		}
	}
	return draft, err
}

func submissionAcceptedForAttempt(attempt SendAttempt) bool {
	if attempt.Transport == nil {
		return false
	}
	return attempt.Transport.SubmissionAccepted || strings.TrimSpace(attempt.Transport.ServerResponse) != ""
}

func resultForAttempt(ref string, attempt SendAttempt, draftRetained bool) SendResult {
	return SendResult{
		DraftRef: ref, AttemptID: attempt.ID, Outcome: attempt.Outcome,
		Accepted:           attempt.AcceptedByMail,
		SubmissionAccepted: submissionAcceptedForAttempt(attempt),
		InvocationStarted:  attempt.InvocationStarted, AcceptedByMail: attempt.AcceptedByMail,
		SentStoreObserved: attempt.SentStoreObserved, SentCopyObserved: attempt.SentStoreObserved,
		ObservedMessageRef: attempt.ObservedMessageRef,
		DraftRetained:      draftRetained,
	}
}

func finishObservedSend(
	lease *draftLease,
	root string,
	ref string,
	attempt SendAttempt,
	reconciled bool,
) (SendResult, error) {
	result := resultForAttempt(ref, attempt, true)
	result.Reconciled = reconciled
	receipt, err := ensureSendReceipt(root, ref, attempt, lease.storage)
	if err != nil {
		return result, &OperationError{
			Code:    "send_receipt_persist_failed",
			Message: fmt.Sprintf("terminal send evidence could not be retained safely: %v", err),
		}
	}
	result.Receipt = receipt
	if err := discardDraftFiles(lease, root, ref); err != nil {
		return result, &OperationError{
			Code:    "send_cleanup_failed",
			Message: fmt.Sprintf("terminal send evidence was retained, but local draft cleanup failed: %v", err),
		}
	}
	result.DraftRetained = false
	return result, nil
}

func removeDraftClaims(root string, ref string, storage *draftStorage) error {
	return errors.Join(
		removeSendAttempt(root, ref, storage),
		removeDraftStorageFile(storage, ref+".save-claim", nil, "draft-save claim"),
		removeHandoffAttempt(ref, storage),
	)
}

func replaySendAttempt(lease *draftLease, root string, ref string, attempt SendAttempt) (SendResult, error) {
	result := resultForAttempt(ref, attempt, true)
	result.Replayed = true
	switch attempt.Outcome {
	case SendOutcomeObserved, SendOutcomeSent:
		receipt, err := ensureSendReceipt(root, ref, attempt, lease.storage)
		if err != nil {
			return result, &OperationError{
				Code:    "send_receipt_unavailable",
				Message: fmt.Sprintf("terminal send evidence could not be loaded safely: %v", err),
			}
		}
		result.Receipt = receipt
		if err := discardDraftFiles(lease, root, ref); err != nil {
			return result, &OperationError{
				Code: "send_cleanup_failed",
				Message: fmt.Sprintf(
					"the Sent copy was already observed, but local draft cleanup failed: %v", err,
				),
			}
		}
		result.DraftRetained = false
		return result, nil
	case SendOutcomeMirrorPending:
		return result, &OperationError{
			Code: "send_mirror_pending",
			Message: "SMTP submission was accepted, but the Sent copy is not observed; " +
				"recipient delivery is unverified; run 'mailcli drafts reconcile --ref " + ref + "' to finish mirroring; the submission will not be retried",
		}
	case SendOutcomeAccepted:
		return result, &OperationError{
			Code:    "send_not_observed",
			Message: "Mail.app accepted the send request, but SMTP submission and an exact Sent copy are not evidenced; the draft is retained and retries are blocked",
		}
	default:
		return result, &OperationError{
			Code:    "send_outcome_unknown",
			Message: "Mail.app send outcome is unknown; the draft is retained and retries are blocked",
		}
	}
}

func discardDraftFiles(lease *draftLease, root string, ref string) error {
	state := lease.storage
	if attempt, err := readHandoffAttempt(ref, state); err != nil {
		return err
	} else if attempt != nil {
		return handoffRetryBlockedError(attempt.ID)
	}
	name := ref + ".json"
	if err := state.apply(draftStorageRemove, name, "", 0); err != nil {
		if os.IsNotExist(err) {
			return errors.Join(
				&OperationError{Code: "not_found", Message: "draft not found"},
				lease.removeLock(),
			)
		}
		return fmt.Errorf("discard draft: %w", err)
	}
	if err := state.apply(draftStorageSync, "", "", 0); err != nil {
		return fmt.Errorf("persist draft removal: %w", err)
	}
	return errors.Join(removeDraftClaims(root, ref, state), lease.removeLock())
}

func nonNilRecipients(recipients []Recipient) []Recipient {
	if recipients == nil {
		return []Recipient{}
	}
	return recipients
}
