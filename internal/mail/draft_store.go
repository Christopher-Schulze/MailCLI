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
	draft, err := prepareDraftWithObserver(request, s.contentObserver)
	if err != nil {
		return Draft{}, err
	}
	root, err := s.resolveDraftRoot()
	if err != nil {
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
	draft, err := s.GetDraft(ref)
	if err != nil {
		return Draft{}, err
	}
	if err := rejectClaimedDraft(draft); err != nil {
		return Draft{}, err
	}
	if draft.Kind != DraftKindNew {
		return Draft{}, validationError("visible compose handoff supports new drafts only; reply and forward threading cannot be preserved")
	}
	if draft.From != "" {
		return Draft{}, validationError("visible compose handoff cannot guarantee an explicit from identity; remove from and select it in Mail.app")
	}
	if len(draft.CC) > 0 || len(draft.BCC) > 0 {
		return Draft{}, validationError("visible compose handoff cannot preserve CC or BCC roles; add them in Mail.app")
	}
	if len(draft.To) == 0 {
		return Draft{}, validationError("visible compose handoff requires at least one recipient")
	}
	if err := verifyDraftAttachments(draft.Attachments); err != nil {
		return Draft{}, err
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
		summary, err := readDraftSummary(root, ref, s.contentObserver)
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
		drafts = append(drafts, summary)
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
	}
}

func draftSendAttemptSummaryFrom(attempt *SendAttempt) *DraftSendAttemptSummary {
	if attempt == nil {
		return nil
	}
	summary := &DraftSendAttemptSummary{
		ID:                  attempt.ID,
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
	if err := rejectClaimedDraft(current); err != nil {
		return Draft{}, err
	}
	replacement, err := prepareDraftWithAttachmentsObserver(CreateDraftRequest{
		Kind: current.Kind, SourceRef: current.SourceRef,
		ReplyAll: current.ReplyAll, SourceMessageID: current.SourceMessageID,
		SourceReferences: current.SourceReferences, Input: request.Input,
	}, current.Attachments, s.contentObserver)
	if err != nil {
		return Draft{}, err
	}
	if err := draftContextError(ctx, "update"); err != nil {
		return Draft{}, err
	}
	replacement.Ref = current.Ref
	replacement.CreatedAt = current.CreatedAt
	if err := writeDraftFile(root, replacement); err != nil {
		return Draft{}, err
	}
	return replacement, nil
}

func (s *Service) DiscardDraft(ref string) (resultErr error) {
	return s.DiscardDraftContext(context.Background(), ref)
}

func (s *Service) DiscardDraftContext(ctx context.Context, ref string) (resultErr error) {
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
	defer func() { resultErr = errors.Join(resultErr, lease.release()) }()
	if err := draftContextError(ctx, "discard"); err != nil {
		return err
	}
	return discardDraftFiles(lease, root, ref)
}
