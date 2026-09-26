package mail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"
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

type PruneTemporaryArtifact struct {
	Ref  string `json:"ref"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type PruneDraftsResult struct {
	DryRun                  bool                     `json:"dry_run"`
	Revision                string                   `json:"revision,omitempty"`
	Stable                  *bool                    `json:"stable,omitempty"`
	Candidates              []PruneCandidate         `json:"candidates,omitempty"`
	ExpiredReceipts         []string                 `json:"expired_receipts,omitempty"`
	OrphanArtifacts         []string                 `json:"orphan_artifacts,omitempty"`
	Removed                 []string                 `json:"removed,omitempty"`
	SweptLocks              []string                 `json:"swept_locks,omitempty"`
	SweptArtifacts          []string                 `json:"swept_artifacts,omitempty"`
	Failed                  []PruneFailure           `json:"failed,omitempty"`
	TemporaryArtifacts      []PruneTemporaryArtifact `json:"temporary_artifacts,omitempty"`
	PreservedTemporaries    []string                 `json:"preserved_temporaries,omitempty"`
	PreservedTemporaryCount int                      `json:"preserved_temporary_count,omitempty"`
}

type PruneDraftsRequest struct {
	OlderThan time.Duration
	Confirm   bool
}

const (
	maximumPruneCandidateMetadataBytes   int64 = 64 << 20
	draftPruneDirectoryBatchSize               = 256
	draftTemporaryMinimumAge                   = 10 * time.Minute
	maximumPreservedTemporaryDiagnostics       = 20
)

type draftPruneCandidateSelection struct {
	candidates        []PruneCandidate
	directoryIdentity os.FileInfo
	directoryRevision string
	entryVisits       int64
	metadataBytes     int64
}

func (selection *draftPruneCandidateSelection) appendCandidate(candidate PruneCandidate) error {
	metadataBytes := int64(unsafe.Sizeof(PruneCandidate{})) + int64(len(candidate.Ref)+len(candidate.Subject))
	if metadataBytes > maximumPruneCandidateMetadataBytes-selection.metadataBytes {
		return &OperationError{
			Code:    "prune_candidate_limit_exceeded",
			Message: "draft prune candidates exceed the 64 MiB metadata limit; no drafts were removed",
		}
	}
	selection.candidates = append(selection.candidates, candidate)
	selection.metadataBytes += metadataBytes
	return nil
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
	selection, err := collectPruneCandidates(ctx, root, cutoff)
	if err != nil {
		return PruneDraftsResult{}, classifyDraftContextError(ctx, err, "prune")
	}
	result := PruneDraftsResult{DryRun: !request.Confirm, Candidates: selection.candidates}
	if !request.Confirm {
		result.Revision = selection.directoryRevision
	}
	receiptCandidates, err := listExpiredSendReceipts(root, time.Now().UTC())
	if err != nil {
		return result, err
	}
	orphanRefs, err := listOrphanDraftArtifactRefs(root)
	if err != nil {
		return result, err
	}
	result.TemporaryArtifacts, result.PreservedTemporaries, result.PreservedTemporaryCount, err = listDraftTemporaryArtifacts(ctx, root)
	if err != nil {
		return result, err
	}
	if !request.Confirm {
		if err := draftContextError(ctx, "prune"); err != nil {
			return PruneDraftsResult{}, err
		}
		result.ExpiredReceipts = receiptCandidates
		for _, ref := range orphanRefs {
			busy, err := draftArtifactRefBusy(root, ref)
			if err != nil {
				return result, err
			}
			if busy {
				continue
			}
			if _, err := os.Lstat(filepath.Join(root, ref+".json")); os.IsNotExist(err) {
				result.OrphanArtifacts = append(result.OrphanArtifacts, ref)
			} else if err != nil {
				return result, err
			}
		}
		identity, err := os.Stat(root)
		if err != nil {
			return result, errors.Join(pruneDraftRevisionChangedError(), fmt.Errorf("inspect dry-run draft directory: %w", err))
		}
		if !os.SameFile(selection.directoryIdentity, identity) {
			return result, pruneDraftRevisionChangedError()
		}
		stable := selection.directoryIdentity.ModTime().Equal(identity.ModTime())
		result.Stable = &stable
		return result, nil
	}
	if err := draftContextError(ctx, "prune"); err != nil {
		return PruneDraftsResult{}, err
	}
	if err := verifyPruneDraftRevision(root, selection.directoryRevision); err != nil {
		return PruneDraftsResult{}, err
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

func collectPruneCandidates(ctx context.Context, root string, cutoff time.Time) (selection draftPruneCandidateSelection, resultErr error) {
	pinned, err := os.OpenRoot(root)
	if err != nil {
		return draftPruneCandidateSelection{}, fmt.Errorf("open draft directory: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, pinned.Close()) }()
	directory, err := pinned.Open(".")
	if err != nil {
		return draftPruneCandidateSelection{}, fmt.Errorf("open draft listing: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	state := &draftStorage{rootName: root, root: pinned, directory: directory}
	identity, err := directory.Stat()
	if err != nil {
		return draftPruneCandidateSelection{}, fmt.Errorf("inspect draft directory: %w", err)
	}
	selection.directoryIdentity = identity
	selection.directoryRevision, err = draftListRevision(root, identity)
	if err != nil {
		return draftPruneCandidateSelection{}, err
	}
	if err := draftContextError(ctx, "prune"); err != nil {
		return draftPruneCandidateSelection{}, err
	}
	for {
		if err := draftContextError(ctx, "prune"); err != nil {
			return draftPruneCandidateSelection{}, err
		}
		names, err := directory.Readdirnames(draftPruneDirectoryBatchSize)
		selection.entryVisits += int64(len(names))
		if err != nil && !errors.Is(err, io.EOF) {
			return draftPruneCandidateSelection{}, fmt.Errorf("list draft candidates: %w", err)
		}
		for _, name := range names {
			if !strings.HasPrefix(name, "draft_") || !strings.HasSuffix(name, ".json") {
				continue
			}
			ref := strings.TrimSuffix(name, ".json")
			if !validDraftReference(ref) {
				continue
			}
			draft, err := readDraftSummary(ctx, ref, state)
			if err != nil {
				if ctx.Err() != nil {
					return draftPruneCandidateSelection{}, draftContextError(ctx, "prune")
				}
				continue
			}
			if draft.StateError != "" || !pruneEligible(draft, cutoff) {
				continue
			}
			candidate := PruneCandidate{
				Ref: draft.Ref, Subject: draft.Subject, AgeDays: pruneAgeDays(draft.UpdatedAt),
			}
			if err := selection.appendCandidate(candidate); err != nil {
				return draftPruneCandidateSelection{}, err
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	sort.Slice(selection.candidates, func(left int, right int) bool {
		return selection.candidates[left].Ref < selection.candidates[right].Ref
	})
	return selection, nil
}

func verifyPruneDraftRevision(root string, expectedRevision string) error {
	identity, err := os.Stat(root)
	if err != nil {
		return pruneDraftRevisionChangedError()
	}
	revision, err := draftListRevision(root, identity)
	if err != nil || revision != expectedRevision {
		return pruneDraftRevisionChangedError()
	}
	return nil
}

func pruneDraftRevisionChangedError() error {
	return &OperationError{
		Code:    "prune_state_changed",
		Message: "draft directory changed before prune cleanup; no drafts were removed",
	}
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

// A nonempty ref with known=false is a well-formed but unknown writer suffix.
// Those files are diagnostic evidence, never cleanup authority.
func draftTemporaryRef(name string) (string, bool) {
	const marker = ".mailcli-"
	if !strings.HasPrefix(name, ".") {
		return "", false
	}
	markerIndex := strings.LastIndex(name, marker)
	if markerIndex <= 31 || len(name) < 31 {
		return "", false
	}
	ref := name[1:31]
	suffix := name[markerIndex+len(marker):]
	if !validDraftReference(ref) || len(suffix) != 24 {
		return "", false
	}
	for _, character := range suffix {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return "", false
		}
	}
	switch name[31:markerIndex] {
	case ".json", ".send-spool", ".send-claim", ".save-claim", handoffClaimSuffix:
		return ref, true
	default:
		return ref, false
	}
}

func removeDraftTemporaryFile(storage *draftStorage, name string, expected os.FileInfo) error {
	if _, ok := draftTemporaryRef(name); !ok {
		return validationError("invalid draft temporary name")
	}
	if storage == nil || storage.root == nil || expected == nil || !expected.Mode().IsRegular() {
		return draftLockUnsafeError("draft temporary is not a pinned regular file")
	}
	current, err := storage.lstat(name)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(expected, current) ||
		current.Mode().Perm() != expected.Mode().Perm() || current.Size() != expected.Size() ||
		!current.ModTime().Equal(expected.ModTime()) {
		return draftLockChangedError("draft temporary changed before cleanup")
	}
	return removeDraftStorageFile(storage, name, expected, "draft temporary")
}

// removeDraftTemporaryFiles runs only while holding this draft's exclusive
// lease. The pinned root fixes the parent identity; each file identity is
// checked again immediately before unlink.
func removeDraftTemporaryFiles(storage *draftStorage, ref string) (removed int, resultErr error) {
	if !validDraftReference(ref) {
		return 0, validationError("invalid draft ref")
	}
	if storage == nil || storage.root == nil {
		return 0, draftLockUnsafeError("draft temporary cleanup requires a pinned draft directory")
	}
	directory, err := storage.root.Open(".")
	if err != nil {
		return 0, fmt.Errorf("open pinned draft directory for temporary recovery: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return 0, fmt.Errorf("list draft temporaries: %w", err)
	}
	for _, entry := range entries {
		candidateRef, ok := draftTemporaryRef(entry.Name())
		if !ok || candidateRef != ref {
			continue
		}
		identity, err := entry.Info()
		if err != nil {
			return removed, fmt.Errorf("inspect draft temporary: %w", err)
		}
		if !identity.Mode().IsRegular() {
			return removed, draftLockUnsafeError("draft temporary is not a regular file")
		}
		if !identity.ModTime().Before(time.Now().Add(-draftTemporaryMinimumAge)) {
			continue
		}
		if err := removeDraftTemporaryFile(storage, entry.Name(), identity); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// listOrphanDraftArtifactRefs returns orphan sidecar refs and refs with known
// aged or unsafe temporaries, including live drafts. Terminal send receipts
// are excluded; they carry their own expiry path.
func listOrphanDraftArtifactRefs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("list orphan draft artifacts: %w", err)
	}
	refs := make(map[string]struct{})
	for _, entry := range entries {
		name := entry.Name()
		if ref, ok := draftTemporaryRef(name); ok {
			info, err := entry.Info()
			if err != nil {
				return nil, err
			}
			if info.Mode().IsRegular() && !info.ModTime().Before(time.Now().Add(-draftTemporaryMinimumAge)) {
				continue
			}
			refs[ref] = struct{}{}
			continue
		}
		if !strings.HasPrefix(name, "draft_") {
			continue
		}
		var ref string
		switch {
		case strings.HasSuffix(name, handoffSnapshotSuffix):
			ref = strings.TrimSuffix(name, handoffSnapshotSuffix)
		case strings.HasSuffix(name, ".attachments"):
			ref = strings.TrimSuffix(name, ".attachments")
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

// sweepOrphanDraftArtifacts removes known inactive temporaries for every ref
// and safe sidecars whose draft is gone. Handoff snapshots require a prepared claim;
// dispatched or ambiguous evidence is retained under the same draft lease.
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
	if busy, err := draftArtifactRefBusy(root, ref); err != nil || busy {
		return false, err
	}
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
	var removedTemporaries int
	swept := false
	err = draftContextError(ctx, "prune")
	if err != nil {
		resultErr = err
		goto release
	}
	removedTemporaries, resultErr = removeDraftTemporaryFiles(lease.storage, ref)
	if resultErr != nil {
		goto release
	}
	if _, err = lease.storage.lstat(ref + ".json"); err == nil || !os.IsNotExist(err) {
		swept = err == nil && removedTemporaries > 0
		if err != nil {
			resultErr = err
		}
		goto release
	}
	resultErr = removeOrphanHandoffSnapshotTree(ctx, ref, lease.storage)
	if resultErr != nil {
		goto release
	}
	resultErr = removeOrphanDraftClaims(ctx, lease.storage, ref)
	if resultErr == nil {
		resultErr = removeDraftAttachmentDir(lease.storage, ref)
	}
	if resultErr == nil {
		resultErr = lease.removeLock()
		swept = resultErr == nil
	}
release:
	return swept, errors.Join(resultErr, lease.release())
}

// Existing writers create the lock before exposing any temporary. This
// nonblocking probe skips busy refs; deletion still reacquires the full lease.
func draftArtifactRefBusy(root string, ref string) (bool, error) {
	lock, err := openExistingDraftLockResource(root, ref)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, lock.close()
	}
	if err != nil {
		return false, errors.Join(err, lock.close())
	}
	return false, errors.Join(syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN), lock.close())
}

func listDraftTemporaryArtifacts(ctx context.Context, root string) ([]PruneTemporaryArtifact, []string, int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, 0, err
	}
	var candidates []PruneTemporaryArtifact
	var preserved []string
	var preservedCount int
	cutoff := time.Now().Add(-draftTemporaryMinimumAge)
	for _, entry := range entries {
		if err := draftContextError(ctx, "prune"); err != nil {
			return nil, nil, 0, err
		}
		ref, known := draftTemporaryRef(entry.Name())
		if ref == "" {
			continue
		}
		if !known {
			preservedCount++
			if len(preserved) < maximumPreservedTemporaryDiagnostics {
				preserved = append(preserved, entry.Name())
			}
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, nil, 0, err
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		busy, err := draftArtifactRefBusy(root, ref)
		if err != nil {
			return nil, nil, 0, err
		}
		if !busy {
			candidates = append(candidates, PruneTemporaryArtifact{Ref: ref, Name: entry.Name(), Size: info.Size()})
		}
	}
	return candidates, preserved, preservedCount, nil
}
