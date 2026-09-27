package mail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
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

type pruneDirectoryEntryKind uint8

const (
	pruneEntryDraftJSON pruneDirectoryEntryKind = 1 << iota
	pruneEntryReceipt
	pruneEntryArtifact
	pruneEntryLock
	pruneEntryKnownTemporary
	pruneEntryUnknownTemporary
)

type draftPruneDirectoryEntry struct {
	name                     string
	ref                      string
	kind                     pruneDirectoryEntryKind
	identity                 os.FileInfo
	candidateSubject         string
	candidateAgeDays         int
	candidate                bool
	receiptCandidate         bool
	temporaryCandidate       bool
	orphanTemporaryCandidate bool
}

type draftPruneDirectoryReader interface {
	Readdirnames(int) ([]string, error)
}

type draftPruneInventory struct {
	entries              []draftPruneDirectoryEntry
	directoryIdentity    os.FileInfo
	directoryRevision    string
	entryVisits          int64
	classificationVisits int64
	metadataBytes        int64
}

const (
	pruneRetainedStringOverheadBytes int64 = 16
	pruneDirectoryBatchScratchBytes        = int64(draftPruneDirectoryBatchSize)*int64(unsafe.Sizeof("")) + int64(unsafe.Sizeof([]string{}))
	pruneDirectoryFixedMetadataBytes       = int64(unsafe.Sizeof(draftPruneInventory{})) + int64(unsafe.Sizeof(PruneDraftsResult{})) + pruneDirectoryBatchScratchBytes
)

func pruneMetadataLimitError() error {
	return &OperationError{
		Code:    "prune_candidate_limit_exceeded",
		Message: "draft prune classification exceeds the 64 MiB metadata limit; no drafts were removed",
	}
}

func (inventory *draftPruneInventory) reserveMetadata(bytes int64) error {
	if bytes < 0 || bytes > maximumPruneCandidateMetadataBytes-inventory.metadataBytes {
		return pruneMetadataLimitError()
	}
	inventory.metadataBytes += bytes
	return nil
}

func retainedPruneStringBytes(value string) int64 {
	if value == "" {
		return 0
	}
	return int64(len(value)) + pruneRetainedStringOverheadBytes
}

func retainedPruneFileInfoBytes(info os.FileInfo, root string, name string) int64 {
	infoType := reflect.TypeOf(info)
	if infoType == nil {
		return 0
	}
	infoBytes := infoType.Size()
	if infoType.Kind() == reflect.Pointer {
		infoBytes = infoType.Elem().Size()
	}
	return int64(infoBytes) + int64(len(root)+len(name)) + pruneRetainedStringOverheadBytes
}

func appendPruneMetadata[T any](inventory *draftPruneInventory, values []T, value T, retainedStringBytes int64) ([]T, error) {
	newCapacity := cap(values)
	if len(values) == cap(values) {
		if newCapacity == 0 {
			newCapacity = 16
		} else {
			newCapacity *= 2
		}
	}
	var zero T
	capacityBytes := int64(0)
	if newCapacity > cap(values) {
		capacityBytes = int64(newCapacity-cap(values)) * int64(unsafe.Sizeof(zero))
	}
	if err := inventory.reserveMetadata(capacityBytes + retainedStringBytes); err != nil {
		return values, err
	}
	if newCapacity > cap(values) {
		grown := make([]T, len(values), newCapacity)
		copy(grown, values)
		values = grown
	}
	return append(values, value), nil
}

func (inventory *draftPruneInventory) retainRef(entry *draftPruneDirectoryEntry, ref string) error {
	if err := inventory.reserveMetadata(retainedPruneStringBytes(ref)); err != nil {
		return err
	}
	entry.ref = ref
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
	return executeDraftPrune(ctx, root, cutoff, request.Confirm)
}

func executeDraftPrune(ctx context.Context, root string, cutoff time.Time, confirm bool) (PruneDraftsResult, error) {
	inventory, err := collectDraftPruneInventory(ctx, root, cutoff)
	if err != nil {
		return PruneDraftsResult{}, classifyDraftContextError(ctx, err, "prune")
	}
	result, err := prepareDraftPruneResult(ctx, root, inventory, !confirm)
	if err != nil {
		return PruneDraftsResult{}, classifyDraftContextError(ctx, err, "prune")
	}
	if !confirm {
		return finalizeDraftPruneDryRun(ctx, root, inventory, result)
	}
	return confirmDraftPrune(ctx, root, cutoff, inventory, result)
}

func finalizeDraftPruneDryRun(
	ctx context.Context,
	root string,
	inventory *draftPruneInventory,
	result PruneDraftsResult,
) (PruneDraftsResult, error) {
	result.Revision = inventory.directoryRevision
	if err := draftContextError(ctx, "prune"); err != nil {
		return PruneDraftsResult{}, err
	}
	identity, err := os.Stat(root)
	if err != nil {
		return result, errors.Join(pruneDraftRevisionChangedError(), fmt.Errorf("inspect dry-run draft directory: %w", err))
	}
	if !os.SameFile(inventory.directoryIdentity, identity) {
		return result, pruneDraftRevisionChangedError()
	}
	stable := inventory.directoryIdentity.ModTime().Equal(identity.ModTime())
	result.Stable = &stable
	return result, nil
}

func confirmDraftPrune(
	ctx context.Context,
	root string,
	cutoff time.Time,
	inventory *draftPruneInventory,
	result PruneDraftsResult,
) (PruneDraftsResult, error) {
	if err := draftContextError(ctx, "prune"); err != nil {
		return PruneDraftsResult{}, err
	}
	if err := verifyPruneDraftRevision(root, inventory.directoryRevision); err != nil {
		return PruneDraftsResult{}, err
	}
	var err error
	result, err = pruneListedDrafts(ctx, root, cutoff, result)
	if err != nil {
		return result, err
	}
	result, err = pruneListedReceipts(ctx, root, result)
	if err != nil {
		return result, err
	}
	result, err = sweepPruneInventory(ctx, root, inventory, result)
	if err != nil {
		return result, err
	}
	if len(result.Failed) > 0 {
		return result, &OperationError{Code: "prune_failed", Message: "one or more drafts could not be pruned"}
	}
	return result, nil
}

func pruneListedDrafts(ctx context.Context, root string, cutoff time.Time, result PruneDraftsResult) (PruneDraftsResult, error) {
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
	return result, nil
}

func pruneListedReceipts(ctx context.Context, root string, result PruneDraftsResult) (PruneDraftsResult, error) {
	receiptCandidates := result.ExpiredReceipts
	result.ExpiredReceipts = result.ExpiredReceipts[:0]
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
	return result, nil
}

func sweepPruneInventory(
	ctx context.Context,
	root string,
	inventory *draftPruneInventory,
	result PruneDraftsResult,
) (PruneDraftsResult, error) {
	if err := draftContextError(ctx, "prune"); err != nil {
		return result, err
	}
	sweptArtifacts, artifactFailures, err := sweepOrphanDraftArtifacts(ctx, root, inventory)
	result.SweptArtifacts = sweptArtifacts
	result.Failed = append(result.Failed, artifactFailures...)
	if err != nil {
		return result, err
	}
	swept, failures, err := sweepOrphanDraftLocks(root, inventory)
	result.SweptLocks = swept
	result.Failed = append(result.Failed, failures...)
	if err != nil {
		return result, err
	}
	return result, nil
}

func collectDraftPruneInventory(ctx context.Context, root string, cutoff time.Time) (inventory *draftPruneInventory, resultErr error) {
	pinned, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open draft directory: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, pinned.Close()) }()
	directory, err := pinned.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open draft listing: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	state := &draftStorage{rootName: root, root: pinned, directory: directory}
	identity, err := directory.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect draft directory: %w", err)
	}
	revision, err := draftListRevision(root, identity)
	if err != nil {
		return nil, err
	}
	return collectDraftPruneInventoryFromReader(ctx, root, cutoff, time.Now().UTC(), state, identity, revision, directory)
}

func collectDraftPruneInventoryFromReader(
	ctx context.Context,
	root string,
	cutoff time.Time,
	now time.Time,
	state *draftStorage,
	identity os.FileInfo,
	revision string,
	reader draftPruneDirectoryReader,
) (inventory *draftPruneInventory, resultErr error) {
	inventory = &draftPruneInventory{directoryIdentity: identity, directoryRevision: revision}
	fixedBytes := pruneDirectoryFixedMetadataBytes + retainedPruneStringBytes(revision) + retainedPruneFileInfoBytes(identity, root, ".")
	if err := inventory.reserveMetadata(fixedBytes); err != nil {
		return nil, err
	}
	if err := draftContextError(ctx, "prune"); err != nil {
		return nil, err
	}
	for {
		if err := draftContextError(ctx, "prune"); err != nil {
			return nil, err
		}
		names, err := reader.Readdirnames(draftPruneDirectoryBatchSize)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("list draft directory: %w", err)
		}
		if err := classifyDraftPruneBatch(ctx, root, cutoff, now, state, inventory, names); err != nil {
			return nil, err
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	sort.Slice(inventory.entries, func(left int, right int) bool {
		if inventory.entries[left].ref != inventory.entries[right].ref {
			return inventory.entries[left].ref < inventory.entries[right].ref
		}
		return inventory.entries[left].name < inventory.entries[right].name
	})
	return inventory, nil
}

func classifyDraftPruneBatch(
	ctx context.Context,
	root string,
	cutoff time.Time,
	now time.Time,
	state *draftStorage,
	inventory *draftPruneInventory,
	names []string,
) error {
	inventory.entryVisits += int64(len(names))
	batchNameBytes := int64(0)
	for _, name := range names {
		batchNameBytes += retainedPruneStringBytes(name)
	}
	if err := inventory.reserveMetadata(batchNameBytes); err != nil {
		return err
	}
	for _, name := range names {
		if err := appendAndClassifyDraftPruneEntry(ctx, root, cutoff, now, state, inventory, name); err != nil {
			return err
		}
	}
	return nil
}

func appendAndClassifyDraftPruneEntry(
	ctx context.Context,
	root string,
	cutoff time.Time,
	now time.Time,
	state *draftStorage,
	inventory *draftPruneInventory,
	name string,
) error {
	entry := draftPruneDirectoryEntry{name: name}
	entries, err := appendPruneMetadata(inventory, inventory.entries, entry, 0)
	if err != nil {
		return err
	}
	inventory.entries = entries
	current := &inventory.entries[len(inventory.entries)-1]
	if err := classifyDraftPruneEntry(ctx, root, cutoff, now, state, inventory, current); err != nil {
		return err
	}
	inventory.classificationVisits++
	return nil
}

func classifyDraftPruneEntry(
	ctx context.Context,
	root string,
	cutoff time.Time,
	now time.Time,
	state *draftStorage,
	inventory *draftPruneInventory,
	entry *draftPruneDirectoryEntry,
) error {
	if err := draftContextError(ctx, "prune"); err != nil {
		return err
	}
	if ref, known := draftTemporaryRef(entry.name); ref != "" {
		return classifyDraftPruneTemporary(root, now, state, inventory, entry, ref, known)
	}
	if !strings.HasPrefix(entry.name, "draft_") {
		return nil
	}
	switch {
	case strings.HasSuffix(entry.name, ".json"):
		return classifyDraftPruneJSON(ctx, cutoff, state, inventory, entry)
	case strings.HasSuffix(entry.name, ".send-receipt"):
		return classifyDraftPruneReceipt(root, now, state, inventory, entry)
	case strings.HasSuffix(entry.name, ".lock"):
		return classifyDraftPruneLock(inventory, entry)
	default:
		return classifyDraftPruneArtifact(state, inventory, entry)
	}
}

func classifyDraftPruneTemporary(
	root string,
	now time.Time,
	state *draftStorage,
	inventory *draftPruneInventory,
	entry *draftPruneDirectoryEntry,
	ref string,
	known bool,
) error {
	if err := inventory.retainRef(entry, ref); err != nil {
		return err
	}
	if known {
		entry.kind |= pruneEntryKnownTemporary
	} else {
		entry.kind |= pruneEntryUnknownTemporary
		return nil
	}
	identity, err := state.lstat(entry.name)
	if err != nil {
		return fmt.Errorf("inspect draft temporary during prune: %w", err)
	}
	if err := inventory.reserveMetadata(retainedPruneFileInfoBytes(identity, root, entry.name)); err != nil {
		return err
	}
	entry.identity = identity
	staleOrUnsafe := !identity.Mode().IsRegular() || identity.ModTime().Before(now.Add(-draftTemporaryMinimumAge))
	entry.orphanTemporaryCandidate = staleOrUnsafe
	if !identity.Mode().IsRegular() || !staleOrUnsafe {
		return nil
	}
	busy, err := draftArtifactRefBusy(root, ref)
	if err != nil {
		return err
	}
	entry.temporaryCandidate = !busy
	return nil
}

func classifyDraftPruneJSON(
	ctx context.Context,
	cutoff time.Time,
	state *draftStorage,
	inventory *draftPruneInventory,
	entry *draftPruneDirectoryEntry,
) error {
	entry.kind = pruneEntryDraftJSON
	ref := strings.TrimSuffix(entry.name, ".json")
	if !validDraftReference(ref) {
		return nil
	}
	if err := inventory.retainRef(entry, ref); err != nil {
		return err
	}
	draft, err := readDraftSummary(ctx, ref, state)
	if err != nil {
		if ctx.Err() != nil {
			return draftContextError(ctx, "prune")
		}
		return nil
	}
	if draft.StateError != "" || !pruneEligible(draft, cutoff) {
		return nil
	}
	if err := inventory.reserveMetadata(retainedPruneStringBytes(draft.Subject)); err != nil {
		return err
	}
	entry.candidate, entry.candidateSubject = true, draft.Subject
	entry.candidateAgeDays = pruneAgeDays(draft.UpdatedAt)
	return nil
}

func classifyDraftPruneReceipt(
	root string,
	now time.Time,
	state *draftStorage,
	inventory *draftPruneInventory,
	entry *draftPruneDirectoryEntry,
) error {
	ref := strings.TrimSuffix(entry.name, ".send-receipt")
	if !validDraftReference(ref) {
		return nil
	}
	entry.kind = pruneEntryReceipt
	if err := inventory.retainRef(entry, ref); err != nil {
		return err
	}
	receipt, err := readSendReceipt(root, ref, state)
	if err != nil || receipt == nil || now.Before(receipt.ExpiresAt) {
		return nil
	}
	attempt, err := readSendAttempt(root, ref, state)
	if err != nil || attempt != nil && !terminalSendOutcome(attempt.Outcome) {
		return nil
	}
	if _, err := state.lstat(ref + ".json"); err == nil || !os.IsNotExist(err) {
		return nil
	}
	entry.receiptCandidate = true
	return nil
}

func classifyDraftPruneLock(inventory *draftPruneInventory, entry *draftPruneDirectoryEntry) error {
	ref := strings.TrimSuffix(entry.name, ".lock")
	if !validDraftReference(ref) {
		return nil
	}
	entry.kind = pruneEntryLock
	return inventory.retainRef(entry, ref)
}

func classifyDraftPruneArtifact(state *draftStorage, inventory *draftPruneInventory, entry *draftPruneDirectoryEntry) error {
	ref, checkDirectory, ok := draftPruneArtifactRef(entry.name)
	if !ok {
		return nil
	}
	if checkDirectory {
		identity, err := state.lstat(entry.name)
		if err == nil && identity.IsDir() {
			return nil
		}
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("inspect draft artifact during prune: %w", err)
		}
	}
	entry.kind = pruneEntryArtifact
	return inventory.retainRef(entry, ref)
}

func draftPruneArtifactRef(name string) (string, bool, bool) {
	if !strings.HasPrefix(name, "draft_") {
		return "", false, false
	}
	for _, suffix := range []string{handoffSnapshotSuffix, ".attachments"} {
		if strings.HasSuffix(name, suffix) {
			ref := strings.TrimSuffix(name, suffix)
			return ref, false, validDraftReference(ref)
		}
	}
	for _, suffix := range []string{".send-claim", ".send-spool", ".save-claim", handoffClaimSuffix} {
		if strings.HasSuffix(name, suffix) {
			ref := strings.TrimSuffix(name, suffix)
			return ref, true, validDraftReference(ref)
		}
	}
	return "", false, false
}

func prepareDraftPruneResult(
	ctx context.Context,
	root string,
	inventory *draftPruneInventory,
	dryRun bool,
) (PruneDraftsResult, error) {
	result := PruneDraftsResult{DryRun: dryRun}
	for index := range inventory.entries {
		if err := draftContextError(ctx, "prune"); err != nil {
			return PruneDraftsResult{}, err
		}
		if err := appendDraftPruneEntryResults(inventory, &result, &inventory.entries[index]); err != nil {
			return PruneDraftsResult{}, err
		}
	}
	if dryRun {
		if err := appendDraftPruneOrphanResults(ctx, root, inventory, &result); err != nil {
			return PruneDraftsResult{}, err
		}
	}
	return result, nil
}

func appendDraftPruneEntryResults(inventory *draftPruneInventory, result *PruneDraftsResult, entry *draftPruneDirectoryEntry) error {
	if entry.candidate {
		candidate := PruneCandidate{Ref: entry.ref, Subject: entry.candidateSubject, AgeDays: entry.candidateAgeDays}
		values, err := appendPruneMetadata(inventory, result.Candidates, candidate, 0)
		if err != nil {
			return err
		}
		result.Candidates, entry.candidateSubject = values, ""
	}
	if entry.receiptCandidate {
		values, err := appendPruneMetadata(inventory, result.ExpiredReceipts, entry.ref, 0)
		if err != nil {
			return err
		}
		result.ExpiredReceipts = values
	}
	if entry.temporaryCandidate {
		artifact := PruneTemporaryArtifact{Ref: entry.ref, Name: entry.name, Size: entry.identity.Size()}
		values, err := appendPruneMetadata(inventory, result.TemporaryArtifacts, artifact, 0)
		if err != nil {
			return err
		}
		result.TemporaryArtifacts = values
	}
	return appendDraftPruneUnknownTemporary(inventory, result, entry)
}

func appendDraftPruneUnknownTemporary(
	inventory *draftPruneInventory,
	result *PruneDraftsResult,
	entry *draftPruneDirectoryEntry,
) error {
	if entry.kind&pruneEntryUnknownTemporary == 0 {
		return nil
	}
	result.PreservedTemporaryCount++
	if len(result.PreservedTemporaries) >= maximumPreservedTemporaryDiagnostics {
		return nil
	}
	values, err := appendPruneMetadata(inventory, result.PreservedTemporaries, entry.name, 0)
	if err == nil {
		result.PreservedTemporaries = values
	}
	return err
}

func appendDraftPruneOrphanResults(
	ctx context.Context,
	root string,
	inventory *draftPruneInventory,
	result *PruneDraftsResult,
) error {
	for start := 0; start < len(inventory.entries); {
		if err := draftContextError(ctx, "prune"); err != nil {
			return err
		}
		end := nextDraftPruneRefGroup(inventory.entries, start)
		ref := inventory.entries[start].ref
		if ref != "" {
			if err := appendDraftPruneOrphanResult(root, ref, inventory.entries[start:end], inventory, result); err != nil {
				return err
			}
		}
		start = end
	}
	return nil
}

func appendDraftPruneOrphanResult(
	root string,
	ref string,
	entries []draftPruneDirectoryEntry,
	inventory *draftPruneInventory,
	result *PruneDraftsResult,
) error {
	staleTemporary := hasOrphanPruneTemporary(entries)
	if !staleTemporary && !hasPruneEntryKind(entries, pruneEntryArtifact) {
		return nil
	}
	draftPath := filepath.Join(root, ref+".json")
	if !staleTemporary {
		if _, err := os.Lstat(draftPath); err == nil || !os.IsNotExist(err) {
			return nil
		}
	}
	busy, err := draftArtifactRefBusy(root, ref)
	if err != nil || busy {
		return err
	}
	if _, err := os.Lstat(draftPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	values, err := appendPruneMetadata(inventory, result.OrphanArtifacts, ref, 0)
	if err == nil {
		result.OrphanArtifacts = values
	}
	return err
}

func nextDraftPruneRefGroup(entries []draftPruneDirectoryEntry, start int) int {
	end := start + 1
	for end < len(entries) && entries[end].ref == entries[start].ref {
		end++
	}
	return end
}

func hasOrphanPruneTemporary(entries []draftPruneDirectoryEntry) bool {
	for _, entry := range entries {
		if entry.orphanTemporaryCandidate {
			return true
		}
	}
	return false
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

func sweepOrphanDraftLocks(root string, inventory *draftPruneInventory) ([]string, []PruneFailure, error) {
	var swept []string
	var failures []PruneFailure
	for start := 0; start < len(inventory.entries); {
		end := nextDraftPruneRefGroup(inventory.entries, start)
		ref := inventory.entries[start].ref
		if ref != "" && hasPruneEntryKind(inventory.entries[start:end], pruneEntryLock) {
			sweptLock, err := sweepOrphanDraftLockRef(root, ref)
			if err != nil {
				failures = append(failures, PruneFailure{Ref: ref, Error: err.Error()})
			} else if sweptLock {
				swept = append(swept, ref)
			}
		}
		start = end
	}
	return swept, failures, nil
}

func sweepOrphanDraftLockRef(root string, ref string) (bool, error) {
	if _, err := draftPath(root, ref); err != nil {
		return false, nil
	}
	lockPath := filepath.Join(root, ref+".lock")
	lockIdentity, err := os.Lstat(lockPath)
	if os.IsNotExist(err) || err == nil && lockIdentity.IsDir() {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(filepath.Join(root, ref+".json")); err == nil || !os.IsNotExist(err) {
		return false, nil
	}
	lockFile, err := openExistingDraftLockResource(root, ref)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return sweepOrphanDraftLock(root, ref, lockFile)
}

func hasPruneEntryKind(entries []draftPruneDirectoryEntry, kind pruneDirectoryEntryKind) bool {
	for _, entry := range entries {
		if entry.kind&kind != 0 {
			return true
		}
	}
	return false
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

// removeDraftTemporaryFiles uses only names classified by the root inventory.
// The pinned root and retained file identity are rechecked immediately before unlink.
func removeDraftTemporaryFiles(storage *draftStorage, ref string, entries []draftPruneDirectoryEntry) (removed int, resultErr error) {
	if !validDraftReference(ref) {
		return 0, validationError("invalid draft ref")
	}
	if storage == nil || storage.root == nil {
		return 0, draftLockUnsafeError("draft temporary cleanup requires a pinned draft directory")
	}
	for _, entry := range entries {
		if entry.kind&pruneEntryKnownTemporary == 0 || entry.ref != ref {
			continue
		}
		if entry.identity == nil || !entry.identity.Mode().IsRegular() {
			return removed, draftLockUnsafeError("draft temporary is not a regular file")
		}
		if !entry.identity.ModTime().Before(time.Now().Add(-draftTemporaryMinimumAge)) {
			continue
		}
		if err := removeDraftTemporaryFile(storage, entry.name, entry.identity); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// sweepOrphanDraftArtifacts removes known inactive temporaries for every ref
// and safe sidecars whose draft is gone. Handoff snapshots require a prepared claim;
// dispatched or ambiguous evidence is retained under the same draft lease.
func sweepOrphanDraftArtifacts(ctx context.Context, root string, inventory *draftPruneInventory) ([]string, []PruneFailure, error) {
	var swept []string
	var failures []PruneFailure
	for start := 0; start < len(inventory.entries); {
		end := nextDraftPruneRefGroup(inventory.entries, start)
		ref := inventory.entries[start].ref
		if ref == "" {
			start = end
			continue
		}
		group := inventory.entries[start:end]
		if !shouldSweepPruneArtifactGroup(root, ref, group) {
			start = end
			continue
		}
		if err := draftContextError(ctx, "prune"); err != nil {
			return swept, failures, err
		}
		sweptRef, err := pruneOrphanDraftArtifactsOnce(ctx, root, ref, group)
		if err != nil {
			var operation *OperationError
			if errors.As(err, &operation) && operation.Code == "draft_operation_canceled" {
				return swept, failures, err
			}
			failures = append(failures, PruneFailure{Ref: ref, Error: err.Error()})
			start = end
			continue
		}
		if sweptRef {
			swept = append(swept, ref)
		}
		start = end
	}
	return swept, failures, nil
}

func shouldSweepPruneArtifactGroup(root string, ref string, entries []draftPruneDirectoryEntry) bool {
	draftMissing := false
	if _, err := os.Lstat(filepath.Join(root, ref+".json")); os.IsNotExist(err) {
		draftMissing = true
	}
	for _, entry := range entries {
		if entry.orphanTemporaryCandidate || draftMissing && entry.kind&pruneEntryArtifact != 0 {
			return true
		}
	}
	return false
}

func pruneOrphanDraftArtifactsOnce(ctx context.Context, root string, ref string, entries []draftPruneDirectoryEntry) (bool, error) {
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
	swept, resultErr := cleanOrphanDraftArtifactsUnderLease(ctx, ref, lease, entries)
	return swept, errors.Join(resultErr, lease.release())
}

func cleanOrphanDraftArtifactsUnderLease(
	ctx context.Context,
	ref string,
	lease *draftLease,
	entries []draftPruneDirectoryEntry,
) (bool, error) {
	if err := draftContextError(ctx, "prune"); err != nil {
		return false, err
	}
	removedTemporaries, err := removeDraftTemporaryFiles(lease.storage, ref, entries)
	if err != nil {
		return false, err
	}
	if _, err := lease.storage.lstat(ref + ".json"); err == nil || !os.IsNotExist(err) {
		if err != nil {
			return false, err
		}
		return removedTemporaries > 0, nil
	}
	if err := removeOrphanHandoffSnapshotTree(ctx, ref, lease.storage); err != nil {
		return false, err
	}
	if err := removeOrphanDraftClaims(ctx, lease.storage, ref); err != nil {
		return false, err
	}
	if err := removeDraftAttachmentDir(lease.storage, ref); err != nil {
		return false, err
	}
	if err := lease.removeLock(); err != nil {
		return false, err
	}
	return true, nil
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
