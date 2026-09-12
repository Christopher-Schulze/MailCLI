package mail

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
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
	OrphanArtifacts []string         `json:"orphan_artifacts,omitempty"`
	Removed         []string         `json:"removed,omitempty"`
	SweptLocks      []string         `json:"swept_locks,omitempty"`
	SweptArtifacts  []string         `json:"swept_artifacts,omitempty"`
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
	cutoff := time.Now().Add(-request.OlderThan)
	result := PruneDraftsResult{DryRun: !request.Confirm}
	listRequest := ListDraftsRequest{Limit: MaximumDraftListLimit}
	for {
		page, err := s.ListDrafts(ctx, listRequest)
		if err != nil {
			return PruneDraftsResult{}, err
		}
		for _, draft := range page.Drafts {
			if !pruneEligible(draft, cutoff) {
				continue
			}
			result.Candidates = append(result.Candidates, PruneCandidate{
				Ref: draft.Ref, Subject: draft.Subject, AgeDays: pruneAgeDays(draft.UpdatedAt),
			})
		}
		listRequest.Cursor = page.Pagination.NextCursor
		if listRequest.Cursor == "" {
			break
		}
	}
	receiptCandidates, err := listExpiredSendReceipts(root, time.Now().UTC())
	if err != nil {
		return result, err
	}
	orphanRefs, err := listOrphanDraftArtifactRefs(root)
	if err != nil {
		return result, err
	}
	if !request.Confirm {
		result.ExpiredReceipts = receiptCandidates
		result.OrphanArtifacts = orphanRefs
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
	sweptArtifacts, artifactFailures, err := sweepOrphanDraftArtifacts(ctx, root)
	result.SweptArtifacts = sweptArtifacts
	result.Failed = append(result.Failed, artifactFailures...)
	if err != nil {
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

func pruneExpiredSendReceiptOnce(ctx context.Context, root string, ref string, now time.Time) error {
	lockContext, cancel := draftLockContext(ctx)
	defer cancel()
	lease, err := acquireDraftLease(lockContext, root, ref)
	if err != nil {
		return classifyDraftContextError(ctx, err, "prune")
	}
	var resultErr error
	var receipt *SendReceipt
	var attempt *SendAttempt
	var draftName string
	err = draftContextError(ctx, "prune")
	if err != nil {
		resultErr = err
		goto release
	}
	receipt, err = readSendReceipt(root, ref, lease.storage)
	if err != nil || receipt == nil || now.Before(receipt.ExpiresAt) {
		resultErr = err
		goto release
	}
	attempt, err = readSendAttempt(root, ref, lease.storage)
	if err != nil {
		resultErr = err
		goto release
	}
	if attempt != nil && !terminalSendOutcome(attempt.Outcome) {
		goto release
	}
	draftName = ref + ".json"
	if _, err = lease.storage.lstat(draftName); err == nil || !os.IsNotExist(err) {
		goto release
	}
	if err = removeDraftClaims(root, ref, lease.storage); err != nil {
		resultErr = err
		goto release
	}
	if err = removeDraftStorageFile(lease.storage, ref+".send-receipt", nil, "send receipt"); err != nil {
		resultErr = err
		goto release
	}
	resultErr = lease.removeLock()
release:
	return errors.Join(resultErr, lease.release())
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
		sweptLock, err := sweepOrphanDraftLock(root, ref, lockFile)
		if err != nil {
			failures = append(failures, PruneFailure{Ref: ref, Error: err.Error()})
			continue
		}
		if sweptLock {
			swept = append(swept, ref)
		}
	}
	return swept, failures, nil
}

func pruneDraftOnce(ctx context.Context, root string, ref string, cutoff time.Time) error {
	lockContext, cancel := draftLockContext(ctx)
	defer cancel()
	lease, err := acquireDraftLease(lockContext, root, ref)
	if err != nil {
		return classifyDraftContextError(ctx, err, "prune")
	}
	var resultErr error
	var draft Draft
	var operation *OperationError
	err = draftContextError(ctx, "prune")
	if err != nil {
		resultErr = err
		goto release
	}
	draft, err = readDraftForMutation(lease, root, ref)
	if err != nil {
		if errors.As(err, &operation) && operation.Code == "not_found" {
			goto release
		}
		resultErr = err
		goto release
	}
	if !pruneEligible(draftSummaryFrom(draft), cutoff) {
		resultErr = &OperationError{Code: "prune_state_changed", Message: "draft changed since listing; skipped"}
		goto release
	}
	err = draftContextError(ctx, "prune")
	if err != nil {
		resultErr = err
		goto release
	}
	err = discardDraftFiles(lease, root, ref)
	if err != nil {
		if errors.As(err, &operation) && operation.Code == "not_found" {
			goto release
		}
		resultErr = err
	}
release:
	return errors.Join(resultErr, lease.release())
}

// listOrphanDraftArtifactRefs returns refs that own send/save/handoff claim,
// spool, or snapshot files while their draft JSON is absent. Terminal send
// receipts are excluded; they carry their own expiry path.
func listOrphanDraftArtifactRefs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("list orphan draft artifacts: %w", err)
	}
	refs := make(map[string]struct{})
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "draft_") {
			continue
		}
		var ref string
		switch {
		case strings.HasSuffix(name, handoffSnapshotSuffix):
			ref = strings.TrimSuffix(name, handoffSnapshotSuffix)
		case entry.IsDir():
			continue
		case strings.HasSuffix(name, ".send-claim"):
			ref = strings.TrimSuffix(name, ".send-claim")
		case strings.HasSuffix(name, ".send-spool"):
			ref = strings.TrimSuffix(name, ".send-spool")
		case strings.HasSuffix(name, ".save-claim"):
			ref = strings.TrimSuffix(name, ".save-claim")
		case strings.HasSuffix(name, handoffClaimSuffix):
			ref = strings.TrimSuffix(name, handoffClaimSuffix)
		default:
			continue
		}
		draftFile, err := draftPath(root, ref)
		if err != nil {
			continue
		}
		if _, err := os.Lstat(draftFile); err == nil || !os.IsNotExist(err) {
			continue
		}
		refs[ref] = struct{}{}
	}
	out := make([]string, 0, len(refs))
	for ref := range refs {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out, nil
}

// sweepOrphanDraftArtifacts removes claim, spool, and handoff snapshot files
// whose draft is gone, using the same lease evidence as draft mutation: a
// busy lock means a live operation, so the ref is skipped rather than swept.
func sweepOrphanDraftArtifacts(ctx context.Context, root string) ([]string, []PruneFailure, error) {
	refs, err := listOrphanDraftArtifactRefs(root)
	if err != nil {
		return nil, nil, err
	}
	var swept []string
	var failures []PruneFailure
	for _, ref := range refs {
		if err := draftContextError(ctx, "prune"); err != nil {
			return swept, failures, err
		}
		sweptRef, err := pruneOrphanDraftArtifactsOnce(ctx, root, ref)
		if err != nil {
			var operation *OperationError
			if errors.As(err, &operation) && operation.Code == "draft_operation_canceled" {
				return swept, failures, err
			}
			failures = append(failures, PruneFailure{Ref: ref, Error: err.Error()})
			continue
		}
		if sweptRef {
			swept = append(swept, ref)
		}
	}
	return swept, failures, nil
}

func pruneOrphanDraftArtifactsOnce(ctx context.Context, root string, ref string) (bool, error) {
	lockContext, cancel := draftLockContext(ctx)
	defer cancel()
	lease, err := acquireDraftLease(lockContext, root, ref)
	if err != nil {
		var operation *OperationError
		if errors.As(err, &operation) && operation.Code == "draft_busy" {
			return false, nil
		}
		return false, classifyDraftContextError(ctx, err, "prune")
	}
	var resultErr error
	swept := false
	err = draftContextError(ctx, "prune")
	if err != nil {
		resultErr = err
		goto release
	}
	if _, err = lease.storage.lstat(ref + ".json"); err == nil || !os.IsNotExist(err) {
		goto release
	}
	resultErr = removeOrphanHandoffSnapshotTree(ctx, ref, lease.storage)
	if resultErr != nil {
		goto release
	}
	resultErr = removeOrphanDraftClaims(ctx, lease.storage, ref)
	if resultErr == nil {
		resultErr = lease.removeLock()
		swept = resultErr == nil
	}
release:
	return swept, errors.Join(resultErr, lease.release())
}
