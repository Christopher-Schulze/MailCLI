package mail

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

func (s *Service) CreateDraft(request CreateDraftRequest) (Draft, error) {
	return s.CreateDraftContext(context.Background(), request)
}

func (s *Service) CreateDraftContext(ctx context.Context, request CreateDraftRequest) (Draft, error) {
	if err := draftContextError(ctx, "create"); err != nil {
		return Draft{}, err
	}
	draft, err := prepareDraftWithAttachmentsObserverContext(ctx, request, nil, s.contentObserver)
	if err != nil {
		return Draft{}, classifyDraftContextError(ctx, err, "create")
	}
	if err := draftContextError(ctx, "create"); err != nil {
		return Draft{}, err
	}
	root, err := s.resolveDraftRoot()
	if err != nil {
		return Draft{}, err
	}
	if err := refreshDraftRevision(&draft); err != nil {
		return Draft{}, err
	}
	if err := writeDraftFile(root, draft); err != nil {
		return Draft{}, err
	}
	return draft, nil
}

func (s *Service) GetDraft(ref string) (Draft, error) {
	root, err := s.resolveDraftRoot()
	if err != nil {
		return Draft{}, err
	}
	return readDraftFileWithObserver(root, ref, s.contentObserver)
}

// GetSendReceipt returns the compact terminal proof for a consumed draft.
// Expired receipts are treated as absent; unresolved send claims are never
// represented by this method.
func (s *Service) GetSendReceipt(ref string) (SendReceipt, error) {
	root, err := s.resolveDraftRoot()
	if err != nil {
		return SendReceipt{}, err
	}
	receipt, err := readActiveSendReceipt(root, ref)
	if err != nil {
		var operation *OperationError
		if errors.As(err, &operation) && operation.Code == "send_receipt_expired" {
			return SendReceipt{}, &OperationError{Code: "not_found", Message: "send receipt expired"}
		}
		return SendReceipt{}, err
	}
	if receipt == nil {
		return SendReceipt{}, &OperationError{Code: "not_found", Message: "send receipt not found"}
	}
	return *receipt, nil
}

func (s *Service) PrepareDraftHandoff(ref string) (Draft, error) {
	return s.PrepareDraftHandoffContext(context.Background(), ref)
}

func (s *Service) PrepareDraftHandoffContext(ctx context.Context, ref string) (Draft, error) {
	if err := draftContextError(ctx, "handoff"); err != nil {
		return Draft{}, err
	}
	draft, err := s.GetDraft(ref)
	if err != nil {
		return Draft{}, err
	}
	if err := draftContextError(ctx, "handoff"); err != nil {
		return Draft{}, err
	}
	if err := validateDraftHandoffContext(ctx, draft); err != nil {
		return Draft{}, classifyDraftContextError(ctx, err, "handoff")
	}
	return draft, nil
}

func (s *Service) ListDrafts() ([]DraftSummary, error) {
	root, err := s.resolveDraftRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("list drafts: %w", err)
	}
	drafts := make([]DraftSummary, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "draft_") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		ref := strings.TrimSuffix(entry.Name(), ".json")
		draft, err := loadDraftDocument(root, ref)
		if err == nil {
			err = attachDraftAttempts(root, ref, &draft)
		}
		if err != nil {
			var operation *OperationError
			if errors.Is(err, os.ErrNotExist) || (errors.As(err, &operation) && operation.Code == "not_found") {
				continue
			}
			drafts = append(drafts, DraftSummary{
				Ref:        ref,
				StateError: err.Error(),
			})
			continue
		}
		drafts = append(drafts, draftSummaryFrom(draft))
	}
	sort.Slice(drafts, func(left int, right int) bool {
		return drafts[left].UpdatedAt.After(drafts[right].UpdatedAt)
	})
	return drafts, nil
}

// draftSummaryFrom maps a loaded draft to its list summary. Body content
// never leaves this function: the summary carries counts and state only.
func draftSummaryFrom(draft Draft) DraftSummary {
	return DraftSummary{
		Ref:             draft.Ref,
		Kind:            draft.Kind,
		AccountRef:      draft.AccountRef,
		Subject:         draft.Subject,
		From:            draft.From,
		To:              draft.To,
		CC:              draft.CC,
		CreatedAt:       draft.CreatedAt,
		UpdatedAt:       draft.UpdatedAt,
		BodyFormat:      draft.BodyFormat,
		AttachmentCount: len(draft.Attachments),
		EverSent:        draft.SendAttempt != nil,
		SendAttempt:     draftSendAttemptSummaryFrom(draft.SendAttempt),
		SaveAttempt:     draftSaveAttemptSummaryFrom(draft.SaveAttempt),
		HandoffAttempt:  draftHandoffAttemptSummaryFrom(draft.HandoffAttempt),
	}
}

func draftSendAttemptSummaryFrom(attempt *SendAttempt) *DraftSendAttemptSummary {
	if attempt == nil {
		return nil
	}
	summary := &DraftSendAttemptSummary{
		ID:                  attempt.ID,
		DraftRevision:       attempt.DraftRevision,
		StartedAt:           attempt.StartedAt,
		UpdatedAt:           attempt.UpdatedAt,
		MessageID:           attempt.MessageID,
		EnvelopeFingerprint: attempt.EnvelopeFingerprint,
		MIMEFingerprint:     attempt.MIMEFingerprint,
		Outcome:             attempt.Outcome,
		InvocationStarted:   attempt.InvocationStarted,
		AcceptedByMail:      attempt.AcceptedByMail,
		SubmissionAccepted:  submissionAcceptedForAttempt(*attempt),
		SentStoreObserved:   attempt.SentStoreObserved,
		SentCopyObserved:    attempt.SentStoreObserved,
		ObservedMessageRef:  attempt.ObservedMessageRef,
		ObservationBaseline: cloneSendObservationBaseline(attempt.ObservationBaseline),
	}
	if attempt.Transport != nil {
		transport := *attempt.Transport
		summary.Transport = &transport
	}
	return summary
}

func draftSaveAttemptSummaryFrom(attempt *DraftSaveAttempt) *DraftSaveAttemptSummary {
	if attempt == nil {
		return nil
	}
	return &DraftSaveAttemptSummary{
		ID:                  attempt.ID,
		StartedAt:           attempt.StartedAt,
		UpdatedAt:           attempt.UpdatedAt,
		InvocationStarted:   attempt.InvocationStarted,
		AcceptedByMail:      attempt.AcceptedByMail,
		ObservedMessageRef:  attempt.ObservedMessageRef,
		ObservationBaseline: cloneSendObservationBaseline(attempt.ObservationBaseline),
	}
}

func draftHandoffAttemptSummaryFrom(attempt *HandoffAttempt) *DraftHandoffAttemptSummary {
	if attempt == nil {
		return nil
	}
	var snapshotBytes int64
	for _, snapshot := range attempt.Snapshots {
		snapshotBytes += snapshot.Size
	}
	return &DraftHandoffAttemptSummary{
		ID: attempt.ID, StartedAt: attempt.StartedAt, UpdatedAt: attempt.UpdatedAt,
		Outcome: attempt.Outcome, DispatchStarted: attempt.DispatchStarted,
		SnapshotsRetained: attempt.SnapshotsRetained, SnapshotCount: len(attempt.Snapshots),
		SnapshotBytes: snapshotBytes,
	}
}

func (s *Service) UpdateDraft(request UpdateDraftRequest) (result Draft, resultErr error) {
	return s.UpdateDraftContext(context.Background(), request)
}

func (s *Service) UpdateDraftContext(ctx context.Context, request UpdateDraftRequest) (result Draft, resultErr error) {
	if err := draftContextError(ctx, "update"); err != nil {
		return Draft{}, err
	}
	root, err := s.resolveDraftRoot()
	if err != nil {
		return Draft{}, err
	}
	lockContext, cancel := draftLockContext(ctx)
	defer cancel()
	lease, err := acquireDraftLease(lockContext, root, request.Ref)
	if err != nil {
		return Draft{}, classifyDraftContextError(ctx, err, "update")
	}
	defer func() { resultErr = errors.Join(resultErr, lease.release()) }()
	current, err := readDraftForMutation(lease, root, request.Ref)
	if err != nil {
		return Draft{}, err
	}
	if err := draftContextError(ctx, "update"); err != nil {
		return Draft{}, err
	}
	if err := requireDraftRevision(request.Ref, request.ExpectedRevision, current.Revision); err != nil {
		return Draft{}, err
	}
	if err := rejectClaimedDraft(current); err != nil {
		return Draft{}, err
	}
	replacement, err := prepareDraftWithAttachmentsObserverContext(ctx, CreateDraftRequest{
		Kind: current.Kind, SourceRef: current.SourceRef,
		ReplyAll: current.ReplyAll, SourceMessageID: current.SourceMessageID,
		SourceReferences: current.SourceReferences, Input: request.Input,
	}, current.Attachments, s.contentObserver)
	if err != nil {
		return Draft{}, classifyDraftContextError(ctx, err, "update")
	}
	if err := draftContextError(ctx, "update"); err != nil {
		return Draft{}, err
	}
	replacement.Ref = current.Ref
	replacement.CreatedAt = current.CreatedAt
	if err := refreshDraftRevision(&replacement); err != nil {
		return Draft{}, err
	}
	if err := writeDraftFile(root, replacement, lease.storage); err != nil {
		return Draft{}, err
	}
	return replacement, nil
}

func (s *Service) DiscardDraft(ref string) (resultErr error) {
	return s.DiscardDraftContext(context.Background(), ref)
}

func (s *Service) DiscardDraftContext(ctx context.Context, ref string) error {
	if err := draftContextError(ctx, "discard"); err != nil {
		return err
	}
	root, err := s.resolveDraftRoot()
	if err != nil {
		return err
	}
	lockContext, cancel := draftLockContext(ctx)
	defer cancel()
	lease, err := acquireDraftLease(lockContext, root, ref)
	if err != nil {
		return classifyDraftContextError(ctx, err, "discard")
	}
	if err := draftContextError(ctx, "discard"); err != nil {
		return errors.Join(err, lease.release())
	}
	return errors.Join(discardDraftFiles(lease, root, ref), lease.release())
}
