package mail

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	root            string
	ref             string
	lease           *draftLease
	draft           Draft
	attempt         HandoffAttempt
	attachmentPaths []string
	closed          bool
}

func handoffClaimPath(root string, ref string) (string, error) {
	if _, err := draftPath(root, ref); err != nil {
		return "", err
	}
	return filepath.Join(root, ref+handoffClaimSuffix), nil
}

func handoffSnapshotParent(root string, ref string) (string, error) {
	if _, err := draftPath(root, ref); err != nil {
		return "", err
	}
	return filepath.Join(root, ref+handoffSnapshotSuffix), nil
}

func handoffSnapshotRoot(root string, ref string, attemptID string) (string, error) {
	if !validHandoffAttemptID(attemptID) {
		return "", validationError("invalid handoff attempt id")
	}
	parent, err := handoffSnapshotParent(root, ref)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, attemptID), nil
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

func readHandoffAttempt(root string, ref string) (*HandoffAttempt, error) {
	path, err := handoffClaimPath(root, ref)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect handoff claim: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maximumDraftStateBytes {
		return nil, fmt.Errorf("handoff claim is not a bounded regular file")
	}
	payload, err := readBoundedRegularFile(path, info, maximumDraftStateBytes)
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
		return nil, fmt.Errorf("handoff claim must contain exactly one JSON object")
	}
	if !validHandoffAttempt(stored, ref) {
		return nil, fmt.Errorf("handoff claim is invalid")
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

func writeHandoffAttempt(root string, ref string, attempt HandoffAttempt) error {
	path, err := handoffClaimPath(root, ref)
	if err != nil {
		return err
	}
	payload, err := encodeHandoffAttempt(ref, attempt)
	if err != nil {
		return err
	}
	if err := writePrivateFile(path, payload); err != nil {
		if errors.Is(err, os.ErrExist) {
			return handoffRetryBlockedError(attempt.ID)
		}
		return fmt.Errorf("create handoff claim: %w", err)
	}
	return nil
}

func replaceHandoffAttempt(root string, ref string, attempt HandoffAttempt) error {
	if !validHandoffAttempt(storedHandoffAttempt{Version: 1, DraftRef: ref, Attempt: attempt}, ref) {
		return fmt.Errorf("handoff claim is invalid")
	}
	path, err := handoffClaimPath(root, ref)
	if err != nil {
		return err
	}
	payload, err := encodeHandoffAttempt(ref, attempt)
	if err != nil {
		return err
	}
	temporary, err := attachmentTemporaryPath(path)
	if err != nil {
		return err
	}
	defer func() { _ = removeIfPresent(temporary) }()
	if err := writePrivateFile(temporary, payload); err != nil {
		return fmt.Errorf("write handoff claim update: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("publish handoff claim update: %w", err)
	}
	return syncDirectory(root)
}

func removeHandoffAttempt(root string, ref string) error {
	path, err := handoffClaimPath(root, ref)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove handoff claim: %w", err)
	}
	return syncDirectory(root)
}

func ensurePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err := os.Mkdir(path, 0o700); err != nil {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("path is not a private directory")
	}
	return os.Chmod(path, 0o700)
}

func createHandoffSnapshotRoot(root string, ref string, attemptID string) (string, error) {
	parent, err := handoffSnapshotParent(root, ref)
	if err != nil {
		return "", err
	}
	if err := ensurePrivateDirectory(parent); err != nil {
		return "", fmt.Errorf("create private handoff snapshot parent: %w", err)
	}
	snapshotRoot, err := handoffSnapshotRoot(root, ref, attemptID)
	if err != nil {
		return "", err
	}
	if err := ensurePrivateDirectory(snapshotRoot); err != nil {
		return "", fmt.Errorf("create private handoff snapshot root: %w", err)
	}
	return snapshotRoot, nil
}

func removePersistentHandoffSnapshotRoot(root string, ref string, attemptID string) error {
	snapshotRoot, err := handoffSnapshotRoot(root, ref, attemptID)
	if err != nil {
		return err
	}
	if err := removeHandoffSnapshotRoot(snapshotRoot); err != nil {
		return fmt.Errorf("remove handoff snapshots: %w", err)
	}
	return syncDirectory(root)
}

func stageDraftAttachmentsForAttempt(
	ctx context.Context,
	root string,
	ref string,
	attemptID string,
	attachments []DraftAttachment,
) ([]string, []HandoffSnapshot, error) {
	snapshotRoot, err := createHandoffSnapshotRoot(root, ref, attemptID)
	if err != nil {
		return nil, nil, err
	}
	paths := make([]string, 0, len(attachments))
	snapshots := make([]HandoffSnapshot, 0, len(attachments))
	for index, attachment := range attachments {
		path, err := stageDraftAttachment(ctx, snapshotRoot, index, attachment)
		if err != nil {
			return paths, snapshots, err
		}
		paths = append(paths, path)
		snapshots = append(snapshots, HandoffSnapshot{
			Name: filepath.Base(attachment.Path), Size: attachment.Size, SHA256: attachment.SHA256,
		})
	}
	return paths, snapshots, nil
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
	draft, err := readDraftForMutation(lease, root, ref)
	if err != nil {
		_ = lease.release()
		return nil, err
	}
	if draft.HandoffAttempt != nil && !draft.HandoffAttempt.DispatchStarted && draft.HandoffAttempt.Outcome == HandoffOutcomePrepared {
		cleanupErr := errors.Join(
			removePersistentHandoffSnapshotRoot(root, ref, draft.HandoffAttempt.ID),
			removeHandoffAttempt(root, ref),
		)
		if cleanupErr != nil {
			_ = lease.release()
			return nil, cleanupErr
		}
		draft.HandoffAttempt = nil
	}
	if err := validateDraftHandoffContext(ctx, draft); err != nil {
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
	if err := writeHandoffAttempt(root, ref, attempt); err != nil {
		_ = lease.release()
		return nil, err
	}
	paths, snapshots, err := stageDraftAttachmentsForAttempt(ctx, root, ref, attemptID, draft.Attachments)
	if err != nil {
		cleanupErr := errors.Join(removePersistentHandoffSnapshotRoot(root, ref, attemptID), removeHandoffAttempt(root, ref))
		return nil, errors.Join(classifyDraftContextError(ctx, err, "handoff"), cleanupErr, lease.release())
	}
	attempt.Snapshots = snapshots
	attempt.SnapshotsRetained = len(snapshots) > 0
	attempt.UpdatedAt = time.Now().UTC()
	if err := replaceHandoffAttempt(root, ref, attempt); err != nil {
		cleanupErr := errors.Join(removePersistentHandoffSnapshotRoot(root, ref, attemptID), removeHandoffAttempt(root, ref))
		return nil, errors.Join(err, cleanupErr, lease.release())
	}
	draft.HandoffAttempt = &attempt
	return &DraftHandoffSession{
		root: root, ref: ref, lease: lease, draft: draft, attempt: attempt, attachmentPaths: paths,
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
		return fmt.Errorf("handoff dispatch state is invalid")
	}
	next := s.attempt
	next.DispatchStarted = true
	next.Outcome = HandoffOutcomeDispatched
	next.UpdatedAt = time.Now().UTC()
	if err := replaceHandoffAttempt(s.root, s.ref, next); err != nil {
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
		resultErr = replaceHandoffAttempt(s.root, s.ref, s.attempt)
		s.closed = true
		return errors.Join(resultErr, s.lease.release())
	}
	cleanupErr := removePersistentHandoffSnapshotRoot(s.root, s.ref, s.attempt.ID)
	if cleanupErr == nil {
		cleanupErr = removeHandoffAttempt(s.root, s.ref)
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
		removePersistentHandoffSnapshotRoot(s.root, s.ref, s.attempt.ID),
		removeHandoffAttempt(s.root, s.ref),
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
	defer func() { resultErr = errors.Join(resultErr, lease.release()) }()
	attempt, err := readHandoffAttempt(root, ref)
	if err != nil {
		return result, err
	}
	if attempt == nil {
		return result, &OperationError{Code: "handoff_not_found", Message: "no retained visible compose handoff exists for this draft"}
	}
	if attempt.ID != attemptID {
		return result, &OperationError{Code: "handoff_attempt_mismatch", Message: "handoff attempt does not match the retained draft state"}
	}
	if !attempt.DispatchStarted || (attempt.Outcome != HandoffOutcomeDispatched && attempt.Outcome != HandoffOutcomeUnknown) {
		return result, &OperationError{Code: "handoff_not_dispatched", Message: "the retained handoff has no unresolved native dispatch to reconcile"}
	}
	result = HandoffReconcileResult{
		DraftRef: ref, AttemptID: attempt.ID,
		Outcome: mapHandoffResolution(resolution), SnapshotsRetained: attempt.SnapshotsRetained,
	}
	if err := removePersistentHandoffSnapshotRoot(root, ref, attempt.ID); err != nil {
		return result, &OperationError{Code: "handoff_attachment_cleanup_failed", Message: fmt.Sprintf("handoff outcome recorded as %s, but snapshots remain: %v", result.Outcome, err)}
	}
	if err := removeHandoffAttempt(root, ref); err != nil {
		result.SnapshotsRetained = false
		return result, &OperationError{Code: "handoff_claim_cleanup_failed", Message: fmt.Sprintf("handoff snapshots were removed, but the retained claim remains: %v", err)}
	}
	result.SnapshotsRetained = false
	return result, nil
}

func mapHandoffResolution(resolution HandoffResolution) HandoffOutcome {
	if resolution == HandoffResolutionOpened {
		return HandoffOutcomeConfirmedOpened
	}
	return HandoffOutcomeConfirmedFailed
}
