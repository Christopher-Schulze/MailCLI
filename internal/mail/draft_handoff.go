package mail

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	handoffClaimSuffix    = ".handoff-claim"
	handoffSnapshotSuffix = ".handoff-snapshots"
)

type storedHandoffAttempt struct {
	Version  int            `json:"version"`
	DraftRef string         `json:"draft_ref"`
	Attempt  HandoffAttempt `json:"attempt"`
}

type DraftHandoffPreparation struct {
	Draft           Draft
	Attempt         HandoffAttempt
	AttachmentPaths []string
}

// DraftHandoffSession owns the draft lease, native attachment snapshots, and
// one durable handoff claim until the caller reaches a terminal lifecycle
// transition. Its methods are intentionally serial: exactly one owner may
// mark dispatch, retain uncertainty, or release the claim.
type DraftHandoffSession struct {
	ref             string
	lease           *draftLease
	draft           Draft
	attempt         HandoffAttempt
	attachmentPaths []string
	closed          bool
}

func newHandoffAttemptID() (string, error) {
	var value [18]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate handoff attempt id: %w", err)
	}
	return "handoff_" + base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func validHandoffAttemptID(value string) bool {
	if !strings.HasPrefix(value, "handoff_") || len(value) != len("handoff_")+24 {
		return false
	}
	for _, character := range value[len("handoff_"):] {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '-' && character != '_' {
			return false
		}
	}
	return true
}

func encodeHandoffAttempt(ref string, attempt HandoffAttempt) ([]byte, error) {
	payload, err := json.MarshalIndent(storedHandoffAttempt{
		Version: 1, DraftRef: ref, Attempt: attempt,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode handoff claim: %w", err)
	}
	return append(payload, '\n'), nil
}

func readHandoffAttempt(ref string, state *draftStorage) (*HandoffAttempt, error) {
	name := ref + handoffClaimSuffix
	info, err := state.lstat(name)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect handoff claim: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maximumDraftStateBytes {
		return nil, errors.New("handoff claim is not a bounded regular file")
	}
	payload, err := readBoundedRegularFile(name, info, maximumDraftStateBytes, state)
	if err != nil {
		return nil, fmt.Errorf("read handoff claim: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var stored storedHandoffAttempt
	if err := decoder.Decode(&stored); err != nil {
		return nil, fmt.Errorf("decode handoff claim: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("handoff claim must contain exactly one JSON object")
	}
	if !validHandoffAttempt(stored, ref) {
		return nil, errors.New("handoff claim is invalid")
	}
	return &stored.Attempt, nil
}

func validHandoffAttempt(stored storedHandoffAttempt, ref string) bool {
	attempt := stored.Attempt
	if stored.Version != 1 || stored.DraftRef != ref || attempt.DraftRef != ref ||
		!validHandoffAttemptID(attempt.ID) || attempt.StartedAt.IsZero() || attempt.UpdatedAt.IsZero() {
		return false
	}
	if attempt.SnapshotsRetained != (len(attempt.Snapshots) > 0) {
		return false
	}
	switch attempt.Outcome {
	case HandoffOutcomePrepared:
		if attempt.DispatchStarted {
			return false
		}
	case HandoffOutcomeDispatched, HandoffOutcomeUnknown:
		if !attempt.DispatchStarted {
			return false
		}
	default:
		return false
	}
	if len(attempt.Snapshots) > MaximumDraftAttachments {
		return false
	}
	var totalSnapshotBytes int64
	for _, snapshot := range attempt.Snapshots {
		if snapshot.Name == "" || filepath.Base(snapshot.Name) != snapshot.Name || snapshot.Size < 0 ||
			snapshot.Size > MaximumDraftAttachmentBytes-totalSnapshotBytes || len(snapshot.SHA256) != 64 {
			return false
		}
		if _, err := hex.DecodeString(snapshot.SHA256); err != nil {
			return false
		}
		totalSnapshotBytes += snapshot.Size
	}
	return true
}

func replaceHandoffAttempt(ref string, attempt HandoffAttempt, state *draftStorage) error {
	if !validHandoffAttempt(storedHandoffAttempt{Version: 1, DraftRef: ref, Attempt: attempt}, ref) {
		return errors.New("handoff claim is invalid")
	}
	name := ref + handoffClaimSuffix
	payload, err := encodeHandoffAttempt(ref, attempt)
	if err != nil {
		return err
	}
	return replacePrivateDraftFile(state, name, payload, "write handoff claim update", "publish handoff claim update")
}

func removeHandoffAttempt(ref string, state *draftStorage) error {
	name := ref + handoffClaimSuffix
	return removeDraftStorageFile(state, name, nil, "handoff claim")
}

func ensurePrivateDirectoryAt(storage *draftStorage, name string) error {
	info, err := storage.lstat(name)
	if os.IsNotExist(err) {
		return storage.apply(draftStorageMkdir, name, "", 0o700)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("path is not a private directory")
	}
	file, _, err := storage.openFile(name, info, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o700); err != nil {
		return errors.Join(err, file.Close())
	}
	return file.Close()
}

func removePersistentHandoffSnapshotRoot(
	ref string,
	attemptID string,
	snapshots []HandoffSnapshot,
	state *draftStorage,
) error {
	if !validHandoffAttemptID(attemptID) {
		return validationError("invalid handoff attempt id")
	}
	snapshotName := filepath.Join(ref+handoffSnapshotSuffix, attemptID)
	for index, snapshot := range snapshots {
		directoryName := filepath.Join(snapshotName, strconv.Itoa(index))
		stagedName := filepath.Join(directoryName, snapshot.Name)
		if err := removeDraftStorageFile(state, stagedName, nil, ""); err != nil {
			return err
		}
		if err := removeDraftStorageFile(state, directoryName, nil, ""); err != nil {
			return err
		}
	}
	if err := removeDraftStorageFile(state, snapshotName, nil, ""); err != nil {
		return err
	}
	return state.apply(draftStorageSync, "", "", 0)
}

func closeHandoffFile(file *os.File, resultErr *error) {
	*resultErr = errors.Join(*resultErr, file.Close())
}

func stageDraftAttachmentAt(
	ctx context.Context,
	storage *draftStorage,
	snapshotName string,
	index int,
	expected DraftAttachment,
) (stagedPath string, resultErr error) {
	source, sourceInfo, err := openHandoffAttachment(expected.Path)
	if err != nil {
		return "", err
	}
	defer closeHandoffFile(source, &resultErr)
	directoryName := filepath.Join(snapshotName, strconv.Itoa(index))
	stagedName := filepath.Join(directoryName, filepath.Base(expected.Path))

	if err := storage.apply(draftStorageMkdir, directoryName, "", 0o700); err != nil {
		return "", fmt.Errorf("create private handoff attachment directory: %w", err)
	}
	destination, destinationIdentity, err := storage.openFile(stagedName, nil, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create private handoff attachment snapshot: %w", err)
	}
	defer closeHandoffFile(destination, &resultErr)
	hash := sha256.New()
	limited := io.LimitReader(source, expected.Size+1)
	copyReader := attachmentFingerprintReader{
		ctx: ctx, reader: limited, afterRead: handoffAttachmentReadHook,
	}
	written, copyErr := io.Copy(destination, io.TeeReader(copyReader, hash))
	if copyErr != nil {
		if errors.Is(copyErr, context.Canceled) || errors.Is(copyErr, context.DeadlineExceeded) {
			return "", copyErr
		}
		return "", handoffAttachmentChanged(expected.Path, fmt.Errorf("copy attachment: %w", copyErr))
	}
	if err := destination.Sync(); err != nil {
		return "", fmt.Errorf("sync private handoff attachment snapshot: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	currentInfo, err := os.Lstat(expected.Path)
	if err != nil {
		return "", handoffAttachmentChanged(expected.Path, fmt.Errorf("recheck attachment: %w", err))
	}
	if !currentInfo.Mode().IsRegular() || !os.SameFile(sourceInfo, currentInfo) {
		return "", handoffAttachmentChanged(expected.Path, errors.New("attachment path changed while it was staged"))
	}
	actualHash := hex.EncodeToString(hash.Sum(nil))
	if written != expected.Size || !strings.EqualFold(actualHash, expected.SHA256) {
		return "", handoffAttachmentChanged(expected.Path, errors.New("attachment size or SHA-256 fingerprint changed"))
	}
	if err := destination.Chmod(0o400); err != nil {
		return "", fmt.Errorf("make private handoff attachment snapshot read-only: %w", err)
	}
	current, err := storage.lstat(stagedName)
	if err != nil || !current.Mode().IsRegular() ||
		current.Mode().Perm() != 0o400 || !os.SameFile(destinationIdentity, current) {
		return "", errors.New("private handoff attachment snapshot changed while it was staged")
	}
	return storage.absolute(stagedName), nil
}

func handoffRetryBlockedError(attemptID string) error {
	return &OperationError{
		Code: "handoff_retry_blocked",
		Message: fmt.Sprintf(
			"draft has unresolved visible compose handoff %s; inspect it and run `mailcli drafts handoff-reconcile --ref DRAFT_REF --attempt %s --outcome opened|failed --confirm` before retrying or cleaning up",
			attemptID, attemptID,
		),
	}
}

func (s *Service) BeginDraftHandoff(ref string) (*DraftHandoffSession, error) {
	return s.BeginDraftHandoffContext(context.Background(), ref)
}

func (s *Service) BeginDraftHandoffContext(ctx context.Context, ref string) (*DraftHandoffSession, error) {
	if err := draftContextError(ctx, "handoff"); err != nil {
		return nil, err
	}
	root, err := s.resolveDraftRoot()
	if err != nil {
		return nil, err
	}
	lockContext, cancel := draftLockContext(ctx)
	defer cancel()
	lease, err := acquireDraftLease(lockContext, root, ref)
	if err != nil {
		return nil, classifyDraftContextError(ctx, err, "handoff")
	}
	storage := lease.storage
	draft, err := readDraftForMutation(lease, root, ref)
	if err != nil {
		_ = lease.release()
		return nil, err
	}
	// Attachment preflight and staging are byte-proportional: the staging
	// budget scales with the stored fingerprint sizes while the caller
	// context keeps governing the lease and draft load phases.
	stagingBytes := int64(0)
	for _, attachment := range draft.Attachments {
		stagingBytes += attachment.Size
	}
	stagingCtx, cancelStaging := context.WithTimeout(ctx, draftOperationBudget(stagingBytes))
	defer cancelStaging()
	if draft.HandoffAttempt != nil && !draft.HandoffAttempt.DispatchStarted && draft.HandoffAttempt.Outcome == HandoffOutcomePrepared {
		cleanupErr := errors.Join(
			removePersistentHandoffSnapshotRoot(ref, draft.HandoffAttempt.ID, draft.HandoffAttempt.Snapshots, storage),
			removeHandoffAttempt(ref, storage),
		)
		if cleanupErr != nil {
			_ = lease.release()
			return nil, cleanupErr
		}
		draft.HandoffAttempt = nil
	}
	if err := validateDraftHandoffContext(stagingCtx, draft); err != nil {
		_ = lease.release()
		return nil, err
	}
	if draft.HandoffAttempt != nil {
		_ = lease.release()
		return nil, handoffRetryBlockedError(draft.HandoffAttempt.ID)
	}
	attemptID, err := newHandoffAttemptID()
	if err != nil {
		_ = lease.release()
		return nil, err
	}
	now := time.Now().UTC()
	attempt := HandoffAttempt{
		ID: attemptID, DraftRef: ref, StartedAt: now, UpdatedAt: now,
		Outcome: HandoffOutcomePrepared, Snapshots: []HandoffSnapshot{},
	}
	name := ref + handoffClaimSuffix
	payload, err := encodeHandoffAttempt(ref, attempt)
	if err != nil {
		_ = lease.release()
		return nil, err
	}
	if _, err := writePrivateDraftFile(storage, name, payload); err != nil {
		_ = lease.release()
		if errors.Is(err, os.ErrExist) {
			return nil, handoffRetryBlockedError(attempt.ID)
		}
		return nil, fmt.Errorf("create handoff claim: %w", err)
	}
	snapshotName := filepath.Join(ref+handoffSnapshotSuffix, attemptID)
	parentName := filepath.Dir(snapshotName)
	var paths []string
	var snapshots []HandoffSnapshot
	if err = ensurePrivateDirectoryAt(storage, parentName); err != nil {
		err = fmt.Errorf("create private handoff snapshot parent: %w", err)
	} else if err = ensurePrivateDirectoryAt(storage, snapshotName); err != nil {
		err = fmt.Errorf("create private handoff snapshot root: %w", err)
	} else {
		paths = make([]string, 0, len(draft.Attachments))
		snapshots = make([]HandoffSnapshot, 0, len(draft.Attachments))
		for index, attachment := range draft.Attachments {
			snapshots = append(snapshots, HandoffSnapshot{
				Name: filepath.Base(attachment.Path), Size: attachment.Size, SHA256: attachment.SHA256,
			})
			path, stageErr := stageDraftAttachmentAt(stagingCtx, storage, snapshotName, index, attachment)
			if stageErr != nil {
				err = stageErr
				break
			}
			paths = append(paths, path)
		}
	}
	if err != nil {
		cleanupErr := errors.Join(
			removePersistentHandoffSnapshotRoot(ref, attemptID, snapshots, storage),
			removeHandoffAttempt(ref, storage),
		)
		return nil, errors.Join(classifyDraftContextError(stagingCtx, err, "handoff"), cleanupErr, lease.release())
	}
	attempt.Snapshots = snapshots
	attempt.SnapshotsRetained = len(snapshots) > 0
	attempt.UpdatedAt = time.Now().UTC()
	if err := replaceHandoffAttempt(ref, attempt, storage); err != nil {
		cleanupErr := errors.Join(
			removePersistentHandoffSnapshotRoot(ref, attemptID, snapshots, storage),
			removeHandoffAttempt(ref, storage),
		)
		return nil, errors.Join(err, cleanupErr, lease.release())
	}
	draft.HandoffAttempt = &attempt
	return &DraftHandoffSession{
		ref: ref, lease: lease, draft: draft, attempt: attempt, attachmentPaths: paths,
	}, nil
}

func validateDraftHandoffContext(ctx context.Context, draft Draft) error {
	if err := draftContextError(ctx, "handoff"); err != nil {
		return err
	}
	if err := rejectClaimedDraft(draft); err != nil {
		return err
	}
	if draft.Kind != DraftKindNew {
		return validationError("visible compose handoff supports new drafts only; reply and forward threading cannot be preserved")
	}
	if draft.From != "" {
		return validationError("visible compose handoff cannot guarantee an explicit from identity; remove from and select it in Mail.app")
	}
	if len(draft.CC) > 0 || len(draft.BCC) > 0 {
		return validationError("visible compose handoff cannot preserve CC or BCC roles; add them in Mail.app")
	}
	if len(draft.To) == 0 {
		return validationError("visible compose handoff requires at least one recipient")
	}
	return preflightDraftAttachmentsContext(ctx, draft.Attachments)
}

func (s *DraftHandoffSession) Preparation() DraftHandoffPreparation {
	paths := append([]string(nil), s.attachmentPaths...)
	return DraftHandoffPreparation{Draft: s.draft, Attempt: s.attempt, AttachmentPaths: paths}
}

func (s *DraftHandoffSession) AttemptID() string {
	return s.attempt.ID
}

func (s *DraftHandoffSession) Dispatched() bool {
	return s.attempt.DispatchStarted
}

func (s *DraftHandoffSession) MarkDispatched(ctx context.Context) error {
	if s.closed {
		return &OperationError{Code: "handoff_session_closed", Message: "handoff session is already closed"}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.attempt.Outcome != HandoffOutcomePrepared || s.attempt.DispatchStarted {
		return errors.New("handoff dispatch state is invalid")
	}
	next := s.attempt
	next.DispatchStarted = true
	next.Outcome = HandoffOutcomeDispatched
	next.UpdatedAt = time.Now().UTC()
	if err := replaceHandoffAttempt(s.ref, next, s.lease.storage); err != nil {
		return err
	}
	s.attempt = next
	return nil
}

func (s *DraftHandoffSession) Finish(outcome HandoffOutcome) (resultErr error) {
	if s.closed {
		return &OperationError{Code: "handoff_session_closed", Message: "handoff session is already closed"}
	}
	if outcome != HandoffOutcomeConfirmedOpened && outcome != HandoffOutcomeConfirmedFailed && outcome != HandoffOutcomeUnknown {
		return validationError("invalid handoff terminal outcome")
	}
	if !s.attempt.DispatchStarted {
		return validationError("handoff cannot finish before native dispatch")
	}
	if outcome == HandoffOutcomeUnknown {
		s.attempt.Outcome = HandoffOutcomeUnknown
		s.attempt.DispatchStarted = true
		s.attempt.SnapshotsRetained = len(s.attempt.Snapshots) > 0
		s.attempt.UpdatedAt = time.Now().UTC()
		resultErr = replaceHandoffAttempt(s.ref, s.attempt, s.lease.storage)
		s.closed = true
		return errors.Join(resultErr, s.lease.release())
	}
	cleanupErr := removePersistentHandoffSnapshotRoot(s.ref, s.attempt.ID, s.attempt.Snapshots, s.lease.storage)
	if cleanupErr == nil {
		cleanupErr = removeHandoffAttempt(s.ref, s.lease.storage)
	}
	if cleanupErr != nil {
		resultErr = errors.Join(
			&OperationError{Code: "handoff_attachment_cleanup_failed", Message: fmt.Sprintf("visible compose outcome is %s, but retained handoff snapshots could not be cleaned: %v", outcome, cleanupErr)},
			s.lease.release(),
		)
		s.closed = true
		return resultErr
	}
	s.closed = true
	return s.lease.release()
}

func (s *DraftHandoffSession) CancelBeforeDispatch() error {
	if s.closed {
		return &OperationError{Code: "handoff_session_closed", Message: "handoff session is already closed"}
	}
	if s.attempt.DispatchStarted && s.attempt.Outcome != HandoffOutcomeDispatched {
		return s.Finish(HandoffOutcomeUnknown)
	}
	cleanupErr := errors.Join(
		removePersistentHandoffSnapshotRoot(s.ref, s.attempt.ID, s.attempt.Snapshots, s.lease.storage),
		removeHandoffAttempt(s.ref, s.lease.storage),
	)
	s.closed = true
	return errors.Join(cleanupErr, s.lease.release())
}

func (s *DraftHandoffSession) Close() error {
	if s.closed {
		return nil
	}
	if s.attempt.DispatchStarted {
		return s.Finish(HandoffOutcomeUnknown)
	}
	return s.CancelBeforeDispatch()
}

func (s *Service) ReconcileDraftHandoff(ref string, attemptID string, resolution HandoffResolution) (HandoffReconcileResult, error) {
	return s.ReconcileDraftHandoffContext(context.Background(), ref, attemptID, resolution)
}

func (s *Service) ReconcileDraftHandoffContext(
	ctx context.Context,
	ref string,
	attemptID string,
	resolution HandoffResolution,
) (result HandoffReconcileResult, resultErr error) {
	if err := draftContextError(ctx, "handoff reconcile"); err != nil {
		return result, err
	}
	attemptID = strings.TrimSpace(attemptID)
	if !validHandoffAttemptID(attemptID) {
		return result, validationError("invalid handoff attempt id")
	}
	if resolution != HandoffResolutionOpened && resolution != HandoffResolutionFailed {
		return result, validationError("handoff outcome must be opened or failed")
	}
	root, err := s.resolveDraftRoot()
	if err != nil {
		return result, err
	}
	lockContext, cancel := draftLockContext(ctx)
	defer cancel()
	lease, err := acquireDraftLease(lockContext, root, ref)
	if err != nil {
		return result, classifyDraftContextError(ctx, err, "handoff reconcile")
	}
	storage := lease.storage
	attempt, err := readHandoffAttempt(ref, storage)
	switch {
	case err != nil:
		resultErr = err
	case attempt == nil:
		resultErr = &OperationError{Code: "handoff_not_found", Message: "no retained visible compose handoff exists for this draft"}
	case attempt.ID != attemptID:
		resultErr = &OperationError{Code: "handoff_attempt_mismatch", Message: "handoff attempt does not match the retained draft state"}
	case !attempt.DispatchStarted || (attempt.Outcome != HandoffOutcomeDispatched && attempt.Outcome != HandoffOutcomeUnknown):
		resultErr = &OperationError{Code: "handoff_not_dispatched", Message: "the retained handoff has no unresolved native dispatch to reconcile"}
	default:
		result = HandoffReconcileResult{
			DraftRef: ref, AttemptID: attempt.ID,
			Outcome: mapHandoffResolution(resolution), SnapshotsRetained: attempt.SnapshotsRetained,
		}
		if cleanupErr := removePersistentHandoffSnapshotRoot(ref, attempt.ID, attempt.Snapshots, storage); cleanupErr != nil {
			resultErr = &OperationError{Code: "handoff_attachment_cleanup_failed", Message: fmt.Sprintf("handoff outcome recorded as %s, but snapshots remain: %v", result.Outcome, cleanupErr)}
		} else if cleanupErr := removeHandoffAttempt(ref, storage); cleanupErr != nil {
			result.SnapshotsRetained = false
			resultErr = &OperationError{Code: "handoff_claim_cleanup_failed", Message: fmt.Sprintf("handoff snapshots were removed, but the retained claim remains: %v", cleanupErr)}
		} else {
			result.SnapshotsRetained = false
		}
	}
	return result, errors.Join(resultErr, lease.release())
}

func mapHandoffResolution(resolution HandoffResolution) HandoffOutcome {
	if resolution == HandoffResolutionOpened {
		return HandoffOutcomeConfirmedOpened
	}
	return HandoffOutcomeConfirmedFailed
}
