package mail

import (
	"context"
	"errors"
	"fmt"
	stdmail "net/mail"
	"strings"
	"time"
)

type DraftSaveBackend interface {
	ReconcileDraftSave(ctx context.Context, draft Draft, attempt DraftSaveAttempt) (DraftSaveEvidence, error)
}

type ComposeWriteGate interface {
	ComposeWriteSupportError() error
}

func composeWriteSupportError(gateway Gateway) error {
	if gateway == nil {
		return &OperationError{
			Code:    "compose_automation_unsupported",
			Message: "Mail 16 compose scripting is disabled because it cannot preserve reviewed content reliably; use 'drafts send --confirm' for sending and Mail's UI for native draft save",
		}
	}
	capability, ok := gateway.(ComposeWriteGate)
	if !ok {
		return nil
	}
	return capability.ComposeWriteSupportError()
}

func (s *Service) SaveDraft(ctx context.Context, ref string) (result SavedDraft, resultErr error) {
	if err := draftContextError(ctx, "save"); err != nil {
		return SavedDraft{}, err
	}
	root, err := s.resolveDraftRoot()
	if err != nil {
		return SavedDraft{}, err
	}
	lockContext, cancelLock := draftLockContext(ctx)
	defer cancelLock()
	lease, err := acquireDraftLease(lockContext, root, ref)
	if err != nil {
		return SavedDraft{}, classifyDraftContextError(ctx, err, "save")
	}
	defer func() {
		resultErr = errors.Join(classifyDraftContextError(ctx, resultErr, "save"), lease.release())
	}()
	if err := draftContextError(ctx, "save"); err != nil {
		return SavedDraft{}, err
	}
	draft, err := readDraftForMutation(lease, root, ref)
	if err != nil {
		return SavedDraft{}, err
	}
	if err := validateStoredDraftLimits(draft); err != nil {
		return SavedDraft{}, err
	}
	if draft.HandoffAttempt != nil {
		return SavedDraft{}, rejectClaimedDraft(draft)
	}
	if draft.SendAttempt != nil {
		return SavedDraft{}, rejectClaimedDraft(draft)
	}
	backend, durable := s.gateway.(DraftSaveBackend)
	if draft.SaveAttempt != nil {
		if !durable {
			return SavedDraft{}, &OperationError{
				Code:    "draft_save_reconcile_unavailable",
				Message: "the selected Mail backend cannot reconcile the existing native draft-save attempt",
			}
		}
		return reconcileNativeDraftSave(ctx, lease, backend, root, ref, draft, *draft.SaveAttempt)
	}
	if err := composeWriteSupportError(s.gateway); err != nil {
		return SavedDraft{}, err
	}
	if draft.From == "" {
		return SavedDraft{}, validationError("saving a Mail.app draft requires an explicit configured from address")
	}
	if err := s.validateDraftSender(ctx, draft.From); err != nil {
		return SavedDraft{}, err
	}
	if err := verifyDraftAttachmentsContext(ctx, draft.Attachments); err != nil {
		return SavedDraft{}, err
	}
	return s.saveDraftLegacy(ctx, lease, root, ref, draft)
}

func (s *Service) saveDraftLegacy(
	ctx context.Context,
	lease *draftLease,
	root string,
	ref string,
	draft Draft,
) (SavedDraft, error) {
	message, saveErr := s.gateway.SaveDraft(ctx, draft)
	if message.Ref == "" {
		if saveErr != nil {
			return SavedDraft{}, saveErr
		}
		return SavedDraft{}, &OperationError{
			Code: "draft_outcome_unknown", Message: "Mail backend returned no observed native draft",
		}
	}
	result := SavedDraft{LocalDraftRef: ref, Message: message}
	if err := discardDraftFiles(lease, root, ref); err != nil {
		return result, fmt.Errorf("native Mail.app draft saved but local draft cleanup failed: %w", err)
	}
	if saveErr != nil {
		return result, &OperationError{
			Code:    "draft_postflight_failed",
			Message: fmt.Sprintf("native Mail.app draft was observed, but private postflight cleanup failed: %v", saveErr),
		}
	}
	return result, nil
}

func reconcileNativeDraftSave(
	ctx context.Context,
	lease *draftLease,
	backend DraftSaveBackend,
	root string,
	ref string,
	draft Draft,
	attempt DraftSaveAttempt,
) (SavedDraft, error) {
	evidence, err := backend.ReconcileDraftSave(ctx, draft, attempt)
	if err != nil {
		return SavedDraft{}, err
	}
	if evidence.ObservedMessage.Ref == "" {
		return SavedDraft{}, &OperationError{
			Code:    "draft_save_outcome_unknown",
			Message: "Drafts still does not prove the prior native save; the local draft is retained and duplicate saves remain blocked",
		}
	}
	attempt.InvocationStarted = true
	attempt.AcceptedByMail = true
	attempt.ObservedMessageRef = evidence.ObservedMessage.Ref
	if evidence.Materialized != nil {
		attempt.Materialized = cloneSendMaterialization(evidence.Materialized)
	}
	attempt.UpdatedAt = time.Now().UTC()
	if err := replaceDraftSaveAttempt(root, ref, attempt, lease.storage); err != nil {
		return SavedDraft{}, &OperationError{
			Code:    "draft_save_reconcile_state_failed",
			Message: fmt.Sprintf("native draft was observed, but its reconciled state could not be recorded: %v", err),
		}
	}
	return finishObservedDraftSave(lease, root, ref, evidence.ObservedMessage, nil)
}

func finishObservedDraftSave(
	lease *draftLease,
	root string,
	ref string,
	message MessageSummary,
	postflightErr error,
) (SavedDraft, error) {
	result := SavedDraft{LocalDraftRef: ref, Message: message}
	if err := discardDraftFiles(lease, root, ref); err != nil {
		return result, fmt.Errorf("native Mail.app draft saved but local draft cleanup failed: %w", err)
	}
	if postflightErr != nil {
		return result, &OperationError{
			Code:    "draft_postflight_failed",
			Message: fmt.Sprintf("native Mail.app draft was observed, but private postflight cleanup failed: %v", postflightErr),
		}
	}
	return result, nil
}

func (s *Service) validateDraftSender(ctx context.Context, sender string) error {
	if sender == "" {
		return nil
	}
	parsed, err := stdmail.ParseAddress(sender)
	if err != nil {
		return validationError("invalid from address")
	}
	accounts, err := s.gateway.ListAccounts(ctx)
	if err != nil {
		return fmt.Errorf("validate draft sender: %w", err)
	}
	for _, account := range accounts {
		for _, address := range account.EmailAddresses {
			if strings.EqualFold(parsed.Address, address) {
				return nil
			}
		}
	}
	return validationError("from address is not configured in an enabled Mail.app account")
}

func cloneSendMaterialization(value *SendMaterialization) *SendMaterialization {
	if value == nil {
		return nil
	}
	clone := *value
	clone.To = append([]Recipient(nil), value.To...)
	clone.CC = append([]Recipient(nil), value.CC...)
	clone.BCC = append([]Recipient(nil), value.BCC...)
	if value.Body != nil {
		body := *value.Body
		clone.Body = &body
	}
	return &clone
}
