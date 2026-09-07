package mail

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func (s *Service) resolveDraftRoot() (string, error) {
	root := s.draftRoot
	if root == "" {
		configRoot, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("resolve Application Support directory: %w", err)
		}
		root = filepath.Join(configRoot, "MailCLI", "drafts")
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

func saveClaimPath(root string, ref string) (string, error) {
	if _, err := draftPath(root, ref); err != nil {
		return "", err
	}
	return filepath.Join(root, ref+".save-claim"), nil
}

type draftLease struct {
	lock *draftLockResource
}

func acquireDraftLease(ctx context.Context, root string, ref string) (*draftLease, error) {
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
			return &draftLease{lock: lock}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			closeErr := lock.close()
			return nil, errors.Join(fmt.Errorf("lock draft: %w", err), closeErr)
		}
		select {
		case <-ctx.Done():
			closeErr := lock.close()
			return nil, errors.Join(
				&OperationError{Code: "draft_busy", Message: "draft is busy with another operation"},
				closeErr,
			)
		case <-ticker.C:
		}
	}
}

func (l *draftLease) release() error {
	return errors.Join(
		syscall.Flock(int(l.lock.file.Fd()), syscall.LOCK_UN),
		l.lock.close(),
	)
}

func (l *draftLease) removeLock() error {
	return l.lock.remove()
}

// readDraftForMutation reads a draft while the caller holds its exclusive lease.
// On a missing draft the lock file created by acquireDraftLease is removed through
// that lease's pinned parent and file identity, so a failed mutation leaves no
// orphan lock and cannot clean an attacker-replaced path.
func readDraftForMutation(lease *draftLease, root string, ref string) (Draft, error) {
	draft, err := readDraftFile(root, ref)
	if err != nil {
		var operation *OperationError
		if errors.As(err, &operation) && operation.Code == "not_found" {
			err = errors.Join(err, lease.removeLock())
		}
	}
	return draft, err
}

func resultForAttempt(ref string, attempt SendAttempt, draftRetained bool) SendResult {
	return SendResult{
		DraftRef: ref, AttemptID: attempt.ID, Outcome: attempt.Outcome,
		Accepted:          attempt.AcceptedByMail,
		InvocationStarted: attempt.InvocationStarted, AcceptedByMail: attempt.AcceptedByMail,
		SentStoreObserved: attempt.SentStoreObserved, ObservedMessageRef: attempt.ObservedMessageRef,
		DraftRetained: draftRetained,
	}
}

func replaySendAttempt(lease *draftLease, root string, ref string, attempt SendAttempt) (SendResult, error) {
	result := resultForAttempt(ref, attempt, true)
	result.Replayed = true
	switch attempt.Outcome {
	case SendOutcomeObserved, SendOutcomeSent:
		if err := discardDraftFiles(lease, root, ref); err != nil {
			return result, &OperationError{
				Code: "send_cleanup_failed",
				Message: fmt.Sprintf(
					"sent message was already observed, but local draft cleanup failed: %v", err,
				),
			}
		}
		result.DraftRetained = false
		return result, nil
	case SendOutcomeMirrorPending:
		return result, &OperationError{
			Code: "send_mirror_pending",
			Message: "the message was accepted by SMTP but the Sent mirror is incomplete; " +
				"run 'mailcli drafts reconcile --ref " + ref + "' to finish mirroring; the send itself will not be retried",
		}
	case SendOutcomeAccepted:
		return result, &OperationError{
			Code:    "send_not_observed",
			Message: "Mail.app accepted the send, but Sent does not prove the exact message; the draft is retained and retries are blocked",
		}
	default:
		return result, &OperationError{
			Code:    "send_outcome_unknown",
			Message: "Mail.app send outcome is unknown; the draft is retained and retries are blocked",
		}
	}
}

func discardDraftFiles(lease *draftLease, root string, ref string) error {
	path, err := draftPath(root, ref)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return errors.Join(
				&OperationError{Code: "not_found", Message: "draft not found"},
				lease.removeLock(),
			)
		}
		return fmt.Errorf("discard draft: %w", err)
	}
	if err := syncDirectory(root); err != nil {
		return fmt.Errorf("persist draft removal: %w", err)
	}
	return errors.Join(removeSendAttempt(root, ref), removeDraftSaveAttempt(root, ref), lease.removeLock())
}

func nonNilRecipients(recipients []Recipient) []Recipient {
	if recipients == nil {
		return []Recipient{}
	}
	return recipients
}
