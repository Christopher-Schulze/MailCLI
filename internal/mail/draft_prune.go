package mail

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"
)

type PruneCandidate struct {
	Ref     string `json:"ref"`
	Subject string `json:"subject"`
	AgeDays int    `json:"age_days"`
}

type PruneFailure struct {
	Ref   string `json:"ref"`
	Error string `json:"error"`
}

type PruneDraftsResult struct {
	DryRun          bool             `json:"dry_run"`
	Candidates      []PruneCandidate `json:"candidates,omitempty"`
	ExpiredReceipts []string         `json:"expired_receipts,omitempty"`
	Removed         []string         `json:"removed,omitempty"`
	SweptLocks      []string         `json:"swept_locks,omitempty"`
	Failed          []PruneFailure   `json:"failed,omitempty"`
}

type PruneDraftsRequest struct {
	OlderThan time.Duration
	Confirm   bool
}

func pruneEligible(draft DraftSummary, cutoff time.Time) bool {
	return draft.UpdatedAt.Before(cutoff) && draft.SendAttempt == nil && draft.SaveAttempt == nil && draft.HandoffAttempt == nil
}

func pruneAgeDays(updatedAt time.Time) int {
	return int(time.Since(updatedAt).Hours() / 24)
}

// PruneDrafts lists (dry run) or deletes stale never-sent local drafts and
// expired terminal send receipts. Drafts with a send or save attempt remain
// reconcilable at-most-once state and are never pruned.
func (s *Service) PruneDrafts(request PruneDraftsRequest) (PruneDraftsResult, error) {
	return s.PruneDraftsContext(context.Background(), request)
}

func (s *Service) PruneDraftsContext(ctx context.Context, request PruneDraftsRequest) (PruneDraftsResult, error) {
	if err := draftContextError(ctx, "prune"); err != nil {
		return PruneDraftsResult{}, err
	}
	if request.OlderThan < 24*time.Hour {
		return PruneDraftsResult{}, validationError(
			"older-than must be at least 1 day; every never-sent draft would be pruned at lower values",
		)
	}
	root, err := s.resolveDraftRoot()
	if err != nil {
		return PruneDraftsResult{}, err
	}
	drafts, err := s.ListDrafts()
	if err != nil {
		return PruneDraftsResult{}, err
	}
	if err := draftContextError(ctx, "prune"); err != nil {
		return PruneDraftsResult{}, err
	}
	cutoff := time.Now().Add(-request.OlderThan)
	result := PruneDraftsResult{DryRun: !request.Confirm}
	for _, draft := range drafts {
		if !pruneEligible(draft, cutoff) {
			continue
		}
		result.Candidates = append(result.Candidates, PruneCandidate{
			Ref: draft.Ref, Subject: draft.Subject, AgeDays: pruneAgeDays(draft.UpdatedAt),
		})
	}
	receiptCandidates, err := listExpiredSendReceipts(root, time.Now().UTC())
	if err != nil {
		return result, err
	}
	if !request.Confirm {
		result.ExpiredReceipts = receiptCandidates
		return result, nil
	}
	for _, candidate := range result.Candidates {
		if err := draftContextError(ctx, "prune"); err != nil {
			return result, err
		}
		if err := pruneDraftOnce(ctx, root, candidate.Ref, cutoff); err != nil {
			var coded interface{ ErrorCode() string }
			if errors.As(err, &coded) && coded.ErrorCode() == "draft_operation_canceled" {
				return result, err
			}
			result.Failed = append(result.Failed, PruneFailure{Ref: candidate.Ref, Error: err.Error()})
			continue
		}
		result.Removed = append(result.Removed, candidate.Ref)
	}
	for _, ref := range receiptCandidates {
		if err := draftContextError(ctx, "prune"); err != nil {
			return result, err
		}
		if err := pruneExpiredSendReceiptOnce(ctx, root, ref, time.Now().UTC()); err != nil {
			var coded interface{ ErrorCode() string }
			if errors.As(err, &coded) && coded.ErrorCode() == "draft_operation_canceled" {
				return result, err
			}
			result.Failed = append(result.Failed, PruneFailure{Ref: ref, Error: err.Error()})
			continue
		}
		result.ExpiredReceipts = append(result.ExpiredReceipts, ref)
	}
	if err := draftContextError(ctx, "prune"); err != nil {
		return result, err
	}
	swept, failures, err := sweepOrphanDraftLocks(root)
	result.SweptLocks = swept
	result.Failed = append(result.Failed, failures...)
	if err != nil {
		return result, err
	}
	if len(result.Failed) > 0 {
		return result, &OperationError{Code: "prune_failed", Message: "one or more drafts could not be pruned"}
	}
	return result, nil
}

func listExpiredSendReceipts(root string, now time.Time) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("list send receipts: %w", err)
	}
	refs := make([]string, 0)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "draft_") || !strings.HasSuffix(name, ".send-receipt") {
			continue
		}
		ref := strings.TrimSuffix(name, ".send-receipt")
		if _, err := draftPath(root, ref); err != nil {
			continue
		}
		receipt, err := readSendReceipt(root, ref)
		if err != nil || receipt == nil || now.Before(receipt.ExpiresAt) {
			continue
		}
		attempt, err := readSendAttempt(root, ref)
		if err != nil {
			continue
		}
		if attempt != nil && !terminalSendOutcome(attempt.Outcome) {
			continue
		}
		draftFile, err := draftPath(root, ref)
		if err != nil {
			continue
		}
		if _, err := os.Lstat(draftFile); err == nil || !os.IsNotExist(err) {
			continue
		}
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs, nil
}

func terminalSendOutcome(outcome SendOutcome) bool {
	return outcome == SendOutcomeObserved || outcome == SendOutcomeSent
}

func pruneExpiredSendReceiptOnce(ctx context.Context, root string, ref string, now time.Time) (resultErr error) {
	lockContext, cancel := draftLockContext(ctx)
	defer cancel()
	lease, err := acquireDraftLease(lockContext, root, ref)
	if err != nil {
		return classifyDraftContextError(ctx, err, "prune")
	}
	defer func() { resultErr = errors.Join(resultErr, lease.release()) }()
	if err := draftContextError(ctx, "prune"); err != nil {
		return err
	}
	receipt, err := readSendReceipt(root, ref)
	if err != nil || receipt == nil || now.Before(receipt.ExpiresAt) {
		return err
	}
	attempt, err := readSendAttempt(root, ref)
	if err != nil {
		return err
	}
	if attempt != nil && !terminalSendOutcome(attempt.Outcome) {
		return nil
	}
	draftFile, err := draftPath(root, ref)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(draftFile); err == nil || !os.IsNotExist(err) {
		return nil
	}
	if err := removeDraftClaims(root, ref); err != nil {
		return err
	}
	if err := removeSendReceipt(root, ref); err != nil {
		return err
	}
	return lease.removeLock()
}

func sweepOrphanDraftLocks(root string) ([]string, []PruneFailure, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, fmt.Errorf("list draft locks: %w", err)
	}
	var swept []string
	var failures []PruneFailure
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "draft_") || !strings.HasSuffix(name, ".lock") {
			continue
		}
		ref := strings.TrimSuffix(name, ".lock")
		if _, err := draftPath(root, ref); err != nil {
			continue
		}
		draftFile, err := draftPath(root, ref)
		if err != nil {
			continue
		}
		if _, err := os.Stat(draftFile); err == nil || !os.IsNotExist(err) {
			continue
		}
		lockFile, err := openExistingDraftLockResource(root, ref)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			failures = append(failures, PruneFailure{Ref: ref, Error: err.Error()})
			continue
		}
		if err := syscall.Flock(int(lockFile.file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = lockFile.close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				continue
			}
			failures = append(failures, PruneFailure{Ref: ref, Error: err.Error()})
			continue
		}
		removeErr := lockFile.remove()
		unlockErr := syscall.Flock(int(lockFile.file.Fd()), syscall.LOCK_UN)
		closeErr := lockFile.close()
		if err := errors.Join(removeErr, unlockErr, closeErr); err != nil {
			failures = append(failures, PruneFailure{Ref: ref, Error: err.Error()})
			continue
		}
		swept = append(swept, ref)
	}
	return swept, failures, nil
}

func pruneDraftOnce(ctx context.Context, root string, ref string, cutoff time.Time) (resultErr error) {
	lockContext, cancel := draftLockContext(ctx)
	defer cancel()
	lease, err := acquireDraftLease(lockContext, root, ref)
	if err != nil {
		return classifyDraftContextError(ctx, err, "prune")
	}
	defer func() { resultErr = errors.Join(resultErr, lease.release()) }()
	if err := draftContextError(ctx, "prune"); err != nil {
		return err
	}
	draft, err := readDraftForMutation(lease, root, ref)
	if err != nil {
		var operation *OperationError
		if errors.As(err, &operation) && operation.Code == "not_found" {
			return nil
		}
		return err
	}
	if !pruneEligible(draftSummaryFrom(draft), cutoff) {
		return &OperationError{Code: "prune_state_changed", Message: "draft changed since listing; skipped"}
	}
	if err := draftContextError(ctx, "prune"); err != nil {
		return err
	}
	if err := discardDraftFiles(lease, root, ref); err != nil {
		var operation *OperationError
		if errors.As(err, &operation) && operation.Code == "not_found" {
			return nil
		}
		return err
	}
	return nil
}
