package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	stdmail "net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

const (
	draftLockPoll          = 25 * time.Millisecond
	draftMutationLockWait  = 2 * time.Second
	maximumDraftStateBytes = int64(20 * 1024 * 1024)
)

func (s *Service) CreateDraft(request CreateDraftRequest) (Draft, error) {
	draft, err := prepareDraft(request)
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
	return readDraftFile(root, ref)
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
		summary, err := readDraftSummary(root, ref)
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
		Subject:         draft.Subject,
		From:            draft.From,
		To:              draft.To,
		CC:              draft.CC,
		CreatedAt:       draft.CreatedAt,
		UpdatedAt:       draft.UpdatedAt,
		BodyFormat:      draft.BodyFormat,
		AttachmentCount: len(draft.Attachments),
		EverSent:        draft.SendAttempt != nil,
		SendAttempt:     draft.SendAttempt,
		SaveAttempt:     draft.SaveAttempt,
	}
}

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
	DryRun     bool             `json:"dry_run"`
	Candidates []PruneCandidate `json:"candidates,omitempty"`
	Removed    []string         `json:"removed,omitempty"`
	SweptLocks []string         `json:"swept_locks,omitempty"`
	Failed     []PruneFailure   `json:"failed,omitempty"`
}

type PruneDraftsRequest struct {
	OlderThan time.Duration
	Confirm   bool
}

func pruneEligible(draft DraftSummary, cutoff time.Time) bool {
	return draft.UpdatedAt.Before(cutoff) && draft.SendAttempt == nil && draft.SaveAttempt == nil
}

func pruneAgeDays(updatedAt time.Time) int {
	return int(time.Since(updatedAt).Hours() / 24)
}

// PruneDrafts lists (dry run) or deletes stale never-sent local drafts. Drafts with a
// send or save attempt are reconcilable at-most-once state and are never pruned.
func (s *Service) PruneDrafts(request PruneDraftsRequest) (PruneDraftsResult, error) {
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
	if !request.Confirm {
		return result, nil
	}
	for _, candidate := range result.Candidates {
		if err := pruneDraftOnce(root, candidate.Ref, cutoff); err != nil {
			result.Failed = append(result.Failed, PruneFailure{Ref: candidate.Ref, Error: err.Error()})
			continue
		}
		result.Removed = append(result.Removed, candidate.Ref)
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
		lockFile, err := os.OpenFile(filepath.Join(root, name), os.O_RDWR, 0o600)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			failures = append(failures, PruneFailure{Ref: ref, Error: err.Error()})
			continue
		}
		if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = lockFile.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				continue
			}
			failures = append(failures, PruneFailure{Ref: ref, Error: err.Error()})
			continue
		}
		removeErr := os.Remove(lockFile.Name())
		unlockErr := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		closeErr := lockFile.Close()
		if err := errors.Join(removeErr, unlockErr, closeErr); err != nil {
			failures = append(failures, PruneFailure{Ref: ref, Error: err.Error()})
			continue
		}
		swept = append(swept, ref)
	}
	return swept, failures, nil
}

func pruneDraftOnce(root string, ref string, cutoff time.Time) (resultErr error) {
	lockContext, cancel := context.WithTimeout(context.Background(), draftMutationLockWait)
	defer cancel()
	lease, err := acquireDraftLease(lockContext, root, ref)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lease.release()) }()
	draft, err := readDraftForMutation(root, ref)
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
	if err := discardDraftFiles(root, ref); err != nil {
		var operation *OperationError
		if errors.As(err, &operation) && operation.Code == "not_found" {
			return nil
		}
		return err
	}
	return nil
}

func (s *Service) UpdateDraft(request UpdateDraftRequest) (result Draft, resultErr error) {
	root, err := s.resolveDraftRoot()
	if err != nil {
		return Draft{}, err
	}
	lockContext, cancel := context.WithTimeout(context.Background(), draftMutationLockWait)
	defer cancel()
	lease, err := acquireDraftLease(lockContext, root, request.Ref)
	if err != nil {
		return Draft{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, lease.release()) }()
	current, err := readDraftForMutation(root, request.Ref)
	if err != nil {
		return Draft{}, err
	}
	if err := rejectClaimedDraft(current); err != nil {
		return Draft{}, err
	}
	replacement, err := prepareDraftWithAttachments(CreateDraftRequest{
		Kind: current.Kind, SourceRef: current.SourceRef,
		ReplyAll: current.ReplyAll, SourceMessageID: current.SourceMessageID,
		SourceReferences: current.SourceReferences, Input: request.Input,
	}, current.Attachments)
	if err != nil {
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
	root, err := s.resolveDraftRoot()
	if err != nil {
		return err
	}
	lockContext, cancel := context.WithTimeout(context.Background(), draftMutationLockWait)
	defer cancel()
	lease, err := acquireDraftLease(lockContext, root, ref)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lease.release()) }()
	return discardDraftFiles(root, ref)
}

func (s *Service) SendDraft(ctx context.Context, ref string) (result SendResult, resultErr error) {
	root, err := s.resolveDraftRoot()
	if err != nil {
		return SendResult{}, err
	}
	lease, err := acquireDraftLease(ctx, root, ref)
	if err != nil {
		return SendResult{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, lease.release()) }()
	draft, err := readDraftForMutation(root, ref)
	if err != nil {
		return SendResult{}, err
	}
	if err := validateStoredDraftLimits(draft); err != nil {
		return SendResult{}, err
	}
	if err := validateThreadSource(draft.SourceMessageID, draft.SourceReferences); err != nil {
		return SendResult{}, err
	}
	if len(draft.To)+len(draft.CC)+len(draft.BCC) == 0 {
		return SendResult{}, validationError("sending a draft requires at least one recipient")
	}
	if err := validateStoredDraftAddresses(draft); err != nil {
		return SendResult{}, err
	}
	if draft.SendAttempt != nil {
		return replaySendAttempt(root, ref, *draft.SendAttempt)
	}
	if draft.Kind == DraftKindNew && strings.TrimSpace(draft.From) == "" {
		return SendResult{}, validationError("sending a new draft requires an explicit from address")
	}
	if draft.Kind == DraftKindForward && len(draft.To)+len(draft.CC)+len(draft.BCC) == 0 {
		return SendResult{}, validationError("sending a forward draft requires at least one explicit recipient")
	}
	if err := verifyDraftAttachments(draft.Attachments); err != nil {
		return SendResult{}, err
	}
	if err := s.send.available(); err != nil {
		return SendResult{}, err
	}
	sender, err := sendSender(draft.From)
	if err != nil {
		return SendResult{}, err
	}
	smtpHost, smtpPort, imapHost, imapPort, err := transport.ProviderHosts(sender)
	if err != nil {
		return SendResult{}, err
	}
	messageID, err := newMessageID(sender)
	if err != nil {
		return SendResult{}, err
	}
	envelopeRecipients, err := draftEnvelopeRecipients(draft)
	if err != nil {
		return SendResult{}, err
	}
	message, err := composeDraftSpool(draft, messageID)
	if err != nil {
		return SendResult{}, err
	}
	defer func() {
		if err := message.Remove(); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	password, err := s.send.Credentials.Load(sender)
	if err != nil || password == "" {
		return SendResult{}, missingCredentialsError(sender)
	}
	attempt, err := beginSendAttempt(root, ref, messageID, envelopeFingerprint(draft, messageID))
	if err != nil {
		return SendResult{}, err
	}
	submitEvidence, err := submitComposedMessage(
		ctx,
		s.send.Submitter,
		transport.SubmitConfig{Host: smtpHost, Port: smtpPort, Username: sender, Password: password},
		sender, envelopeRecipients, message,
	)
	if err != nil {
		var submissionErr *transport.SubmissionError
		if errors.As(err, &submissionErr) {
			attempt.InvocationStarted = true
			attempt.Transport = &TransportEvidence{
				MessageID:       attempt.MessageID,
				SubmissionStage: submissionErr.Stage,
			}
			attempt.Outcome = SendOutcomeUnknown
			attempt.UpdatedAt = time.Now().UTC()
			result = resultForAttempt(ref, attempt, true)
			if stateErr := replaceSendAttempt(root, ref, attempt); stateErr != nil {
				return result, &OperationError{
					Code:    "send_state_unknown",
					Message: fmt.Sprintf("SMTP submission outcome is unknown and its local state could not be retained safely: %v", stateErr),
				}
			}
			return result, err
		}
		// The server never accepted the message, so the claim can be
		// released and a later send may retry the submission.
		if cleanupErr := removeSendAttempt(root, ref); cleanupErr != nil {
			result = resultForAttempt(ref, attempt, true)
			return result, &OperationError{
				Code:    "send_state_cleanup_failed",
				Message: fmt.Sprintf("send was rejected, but its local claim could not be cleared: %v", cleanupErr),
			}
		}
		return SendResult{}, err
	}
	attempt.InvocationStarted = true
	attempt.AcceptedByMail = true
	attempt.Transport = &TransportEvidence{
		ServerResponse: submitEvidence.ServerResponse,
		MessageID:      attempt.MessageID,
	}
	attempt.Outcome = SendOutcomeMirrorPending
	attempt.UpdatedAt = time.Now().UTC()
	result = resultForAttempt(ref, attempt, true)
	if err := replaceSendAttempt(root, ref, attempt); err != nil {
		return result, &OperationError{
			Code:    "send_outcome_unknown",
			Message: fmt.Sprintf("the server accepted the message, but its local send state could not be recorded safely: %v", err),
		}
	}
	appendEvidence, err := mirrorComposedMessage(
		ctx,
		s.send.Mirror,
		transport.ImapConfig{Host: imapHost, Port: imapPort, Username: sender, Password: password},
		message,
		attempt.MessageID,
	)
	if err != nil {
		// The submission was accepted, so the send itself is never retried;
		// the claim stays reconcilable and only the mirror may be retried.
		return result, mirrorPendingError(err)
	}
	attempt.SentStoreObserved = true
	attempt.Transport.MirrorMailbox = appendEvidence.Mailbox
	attempt.Transport.MirrorAppended = appendEvidence.Appended
	attempt.Outcome = SendOutcomeSent
	attempt.UpdatedAt = time.Now().UTC()
	result = resultForAttempt(ref, attempt, true)
	if err := replaceSendAttempt(root, ref, attempt); err != nil {
		return result, &OperationError{
			Code:    "send_outcome_unknown",
			Message: fmt.Sprintf("the message was sent and mirrored, but its local send state could not be recorded safely: %v", err),
		}
	}
	if err := discardDraftFiles(root, ref); err != nil {
		return result, &OperationError{
			Code:    "send_cleanup_failed",
			Message: fmt.Sprintf("sent message was mirrored, but local draft cleanup failed: %v", err),
		}
	}
	result.DraftRetained = false
	return result, nil
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

type SendReconciler interface {
	ReconcileSend(ctx context.Context, draft Draft, attempt SendAttempt) (SendEvidence, error)
}

func (s *Service) ReconcileDraft(ctx context.Context, ref string) (result SendResult, resultErr error) {
	root, err := s.resolveDraftRoot()
	if err != nil {
		return SendResult{}, err
	}
	lease, err := acquireDraftLease(ctx, root, ref)
	if err != nil {
		return SendResult{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, lease.release()) }()
	draft, err := readDraftForMutation(root, ref)
	if err != nil {
		return SendResult{}, err
	}
	if draft.SendAttempt == nil {
		return SendResult{}, &OperationError{Code: "send_reconcile_unavailable", Message: "draft has no send attempt to reconcile"}
	}
	attempt := *draft.SendAttempt
	if attempt.Outcome == SendOutcomeObserved || attempt.Outcome == SendOutcomeSent {
		result, err := replaySendAttempt(root, ref, attempt)
		result.Reconciled = true
		return result, err
	}
	if attempt.Outcome == SendOutcomeMirrorPending {
		return s.reconcileMirrorPending(ctx, root, ref, draft, attempt)
	}
	if attempt.ObservationBaseline == nil {
		if attempt.Outcome != SendOutcomeUnknown || attempt.MessageID == "" {
			return resultForReconcile(ref, attempt), &OperationError{
				Code: "send_reconcile_unavailable",
				Message: fmt.Sprintf(
					"send attempt from %s carries no Message-ID and predates durable store observation; it remains blocked and must not be retried",
					attempt.StartedAt.Format(time.RFC3339),
				),
			}
		}
		return s.reconcileUnknownViaImap(ctx, root, ref, draft, attempt)
	}
	reconciler, ok := s.gateway.(SendReconciler)
	if !ok {
		return resultForReconcile(ref, attempt), &OperationError{
			Code: "send_reconcile_unavailable", Message: "the selected Mail backend cannot reconcile send outcomes safely",
		}
	}
	evidence, err := reconciler.ReconcileSend(ctx, draft, attempt)
	if err != nil {
		return resultForReconcile(ref, attempt), err
	}
	if evidence.SentStoreObserved && evidence.ObservedMessageRef != "" {
		return persistReconciledSend(root, ref, attempt, evidence)
	}
	result = resultForReconcile(ref, attempt)
	if attempt.Outcome == SendOutcomeAccepted {
		return result, &OperationError{
			Code:    "send_not_observed",
			Message: "Mail.app accepted the send, but Sent still does not prove the exact message; the draft is retained and retries remain blocked",
		}
	}
	return result, &OperationError{
		Code:    "send_outcome_unknown",
		Message: "the sent store still does not prove this send; the draft is retained and retries remain blocked",
	}
}

func persistReconciledSend(
	root string,
	ref string,
	attempt SendAttempt,
	evidence SendEvidence,
) (SendResult, error) {
	attempt.InvocationStarted = true
	attempt.AcceptedByMail = true
	attempt.SentStoreObserved = true
	attempt.ObservedMessageRef = evidence.ObservedMessageRef
	attempt.Outcome = SendOutcomeObserved
	attempt.UpdatedAt = time.Now().UTC()
	result := resultForReconcile(ref, attempt)
	if err := replaceSendAttempt(root, ref, attempt); err != nil {
		return result, &OperationError{
			Code:    "send_reconcile_state_failed",
			Message: fmt.Sprintf("sent message was observed, but the reconciled state could not be recorded: %v", err),
		}
	}
	if err := discardDraftFiles(root, ref); err != nil {
		return result, &OperationError{
			Code:    "send_cleanup_failed",
			Message: fmt.Sprintf("sent message was observed, but local draft cleanup failed: %v", err),
		}
	}
	result.DraftRetained = false
	return result, nil
}

// reconcileUnknownViaImap resolves a crash-stranded unknown claim: the claim
// carries the Message-ID and envelope fingerprint written before submission,
// so the Sent mailbox can be searched over IMAP without store observation.
// Absence in Sent never proves non-delivery (APPEND may still crash), so the
// claim stays unknown and automatic retries remain blocked.
func (s *Service) reconcileUnknownViaImap(
	ctx context.Context,
	root string,
	ref string,
	draft Draft,
	attempt SendAttempt,
) (SendResult, error) {
	if attempt.EnvelopeFingerprint != envelopeFingerprint(draft, attempt.MessageID) {
		return resultForReconcile(ref, attempt), &OperationError{
			Code:    "send_fingerprint_mismatch",
			Message: "the draft no longer matches the claimed send envelope; reconciliation is blocked and retries remain forbidden",
		}
	}
	if err := s.send.available(); err != nil {
		return resultForReconcile(ref, attempt), err
	}
	imap := s.send.ImapClient()
	if imap == nil {
		return resultForReconcile(ref, attempt), &OperationError{
			Code:    "send_transport_unavailable",
			Message: "direct send transport has no IMAP operator; reconciliation over the Sent mailbox is unavailable",
		}
	}
	sender, err := sendSender(draft.From)
	if err != nil {
		return resultForReconcile(ref, attempt), err
	}
	_, _, imapHost, imapPort, err := transport.ProviderHosts(sender)
	if err != nil {
		return resultForReconcile(ref, attempt), err
	}
	password, err := s.send.Credentials.Load(sender)
	if err != nil || password == "" {
		return resultForReconcile(ref, attempt), missingCredentialsError(sender)
	}
	cfg := transport.ImapConfig{Host: imapHost, Port: imapPort, Username: sender, Password: password}
	mailboxes, err := imap.ListMailboxes(ctx, cfg)
	if err != nil {
		return resultForReconcile(ref, attempt), err
	}
	sentBox := transport.PickSentMailbox(mailboxes)
	if sentBox == "" {
		return resultForReconcile(ref, attempt), unverifiableSendError(attempt, draft, "no Sent mailbox found on the IMAP server")
	}
	uid, uidValidity, matchCount, err := imap.SearchUID(ctx, cfg, sentBox, attempt.MessageID)
	if err != nil {
		var transportErr *transport.TransportError
		if !errors.As(err, &transportErr) || transportErr.Code != transport.CodeIMAPMessageNotFound {
			return resultForReconcile(ref, attempt), err
		}
	}
	if matchCount > 1 {
		return resultForReconcile(ref, attempt), unverifiableSendError(attempt, draft,
			fmt.Sprintf("the Sent mailbox contains %d messages with the claimed Message-ID", matchCount))
	}
	if uid != 0 && matchCount == 1 {
		raw, fetchErr := imap.FetchMessage(ctx, cfg, sentBox, uid, uidValidity, MaximumRawSourceBytes)
		if fetchErr != nil {
			return resultForReconcile(ref, attempt), fetchErr
		}
		if identityErr := verifySentMessageIdentity(raw, draft, attempt.MessageID); identityErr != nil {
			return resultForReconcile(ref, attempt), identityErr
		}
		attempt.InvocationStarted = true
		attempt.AcceptedByMail = true
		attempt.SentStoreObserved = true
		attempt.Outcome = SendOutcomeSent
		attempt.Transport = &TransportEvidence{MessageID: attempt.MessageID, MirrorMailbox: sentBox}
		attempt.UpdatedAt = time.Now().UTC()
		result := resultForReconcile(ref, attempt)
		result.Reconciled = true
		if err := replaceSendAttempt(root, ref, attempt); err != nil {
			return result, &OperationError{
				Code:    "send_reconcile_state_failed",
				Message: fmt.Sprintf("the sent message was located over IMAP, but the reconciled state could not be recorded: %v", err),
			}
		}
		return result, nil
	}
	return resultForReconcile(ref, attempt), unverifiableSendError(attempt, draft, "the Sent mailbox contains no message with the claimed Message-ID")
}

// unverifiableSendError turns an unreconcilable unknown claim into an
// actionable typed error: Message-ID, attempt start, recipients, and manual
// remediation, without ever unlocking automatic retries.
func unverifiableSendError(attempt SendAttempt, draft Draft, finding string) error {
	recipients, err := draftEnvelopeRecipients(draft)
	if err != nil {
		recipients = draftRecipientValues(draft)
	}
	return &OperationError{
		Code: "send_outcome_unverifiable",
		Message: fmt.Sprintf(
			"send outcome unknown: %s (Message-ID %s, started %s, recipients %s); verify the message in the Sent or spam folder manually, or discard the draft to stop reconciliation",
			finding, attempt.MessageID, attempt.StartedAt.Format(time.RFC3339),
			strings.Join(recipients, ", "),
		),
	}
}

func resultForReconcile(ref string, attempt SendAttempt) SendResult {
	result := resultForAttempt(ref, attempt, true)
	result.Reconciled = true
	return result
}

// reconcileMirrorPending finishes a direct send whose SMTP submission was
// accepted but whose Sent-mailbox mirror did not complete. It retries only
// the idempotent IMAP mirror; the SMTP submission is never sent again.
func (s *Service) reconcileMirrorPending(
	ctx context.Context,
	root string,
	ref string,
	draft Draft,
	attempt SendAttempt,
) (result SendResult, resultErr error) {
	result = resultForReconcile(ref, attempt)
	if attempt.Transport == nil || strings.TrimSpace(attempt.Transport.MessageID) == "" {
		return result, &OperationError{
			Code:    "send_reconcile_unavailable",
			Message: "the send attempt carries no Message-ID; the Sent mirror cannot be completed safely",
		}
	}
	if s.send.Mirror == nil || s.send.Credentials == nil {
		return result, &OperationError{
			Code:    "send_transport_unavailable",
			Message: "direct SMTP send is unavailable because no send transport is configured",
		}
	}
	if err := verifyDraftAttachments(draft.Attachments); err != nil {
		return result, err
	}
	sender, err := sendSender(draft.From)
	if err != nil {
		return result, err
	}
	_, _, imapHost, imapPort, err := transport.ProviderHosts(sender)
	if err != nil {
		return result, err
	}
	password, err := s.send.Credentials.Load(sender)
	if err != nil || password == "" {
		return result, missingCredentialsError(sender)
	}
	if imap := s.send.ImapClient(); imap != nil {
		mailboxes, listErr := imap.ListMailboxes(ctx, transport.ImapConfig{Host: imapHost, Port: imapPort, Username: sender, Password: password})
		if listErr != nil {
			return result, mirrorPendingError(listErr)
		}
		sentBox := transport.PickSentMailbox(mailboxes)
		if sentBox == "" {
			return result, &OperationError{Code: "send_reconcile_unavailable", Message: "no Sent mailbox is available to verify the accepted message before mirroring"}
		}
		uid, uidValidity, matchCount, searchErr := imap.SearchUID(
			ctx,
			transport.ImapConfig{Host: imapHost, Port: imapPort, Username: sender, Password: password},
			sentBox,
			attempt.MessageID,
		)
		if searchErr != nil {
			var transportErr *transport.TransportError
			if !errors.As(searchErr, &transportErr) || transportErr.Code != transport.CodeIMAPMessageNotFound {
				return result, mirrorPendingError(searchErr)
			}
		}
		if matchCount > 1 {
			return result, unverifiableSendError(attempt, draft,
				fmt.Sprintf("the Sent mailbox contains %d messages with the claimed Message-ID", matchCount))
		}
		if uid != 0 && matchCount == 1 {
			raw, fetchErr := imap.FetchMessage(ctx,
				transport.ImapConfig{Host: imapHost, Port: imapPort, Username: sender, Password: password},
				sentBox, uid, uidValidity, MaximumRawSourceBytes)
			if fetchErr != nil {
				return result, mirrorPendingError(fetchErr)
			}
			if identityErr := verifySentMessageIdentity(raw, draft, attempt.MessageID); identityErr != nil {
				return result, identityErr
			}
			attempt.SentStoreObserved = true
			attempt.Transport.MirrorMailbox = sentBox
			attempt.Transport.MirrorAppended = false
			attempt.Outcome = SendOutcomeSent
			attempt.UpdatedAt = time.Now().UTC()
			result = resultForReconcile(ref, attempt)
			if stateErr := replaceSendAttempt(root, ref, attempt); stateErr != nil {
				return result, &OperationError{
					Code:    "send_reconcile_state_failed",
					Message: fmt.Sprintf("the existing Sent message was verified, but the reconciled state could not be recorded: %v", stateErr),
				}
			}
			if cleanupErr := discardDraftFiles(root, ref); cleanupErr != nil {
				return result, &OperationError{
					Code:    "send_cleanup_failed",
					Message: fmt.Sprintf("the existing Sent message was verified, but local draft cleanup failed: %v", cleanupErr),
				}
			}
			result.DraftRetained = false
			return result, nil
		}
	}
	message, err := composeDraftSpool(draft, attempt.MessageID)
	if err != nil {
		return result, err
	}
	defer func() {
		if err := message.Remove(); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	appendEvidence, err := mirrorComposedMessage(
		ctx,
		s.send.Mirror,
		transport.ImapConfig{Host: imapHost, Port: imapPort, Username: sender, Password: password},
		message,
		attempt.MessageID,
	)
	if err != nil {
		return result, mirrorPendingError(err)
	}
	attempt.SentStoreObserved = true
	attempt.Transport.MirrorMailbox = appendEvidence.Mailbox
	attempt.Transport.MirrorAppended = appendEvidence.Appended
	attempt.Outcome = SendOutcomeSent
	attempt.UpdatedAt = time.Now().UTC()
	result = resultForReconcile(ref, attempt)
	if err := replaceSendAttempt(root, ref, attempt); err != nil {
		return result, &OperationError{
			Code:    "send_reconcile_state_failed",
			Message: fmt.Sprintf("the sent message was mirrored, but the reconciled state could not be recorded: %v", err),
		}
	}
	if err := discardDraftFiles(root, ref); err != nil {
		return result, &OperationError{
			Code:    "send_cleanup_failed",
			Message: fmt.Sprintf("sent message was mirrored, but local draft cleanup failed: %v", err),
		}
	}
	result.DraftRetained = false
	return result, nil
}

// available rejects sends when the Service was created without a full
// SendTransport instead of panicking on a nil dependency.
func (t SendTransport) available() error {
	if t.Submitter == nil || t.Mirror == nil || t.Credentials == nil {
		return &OperationError{
			Code:    "send_transport_unavailable",
			Message: "direct SMTP send is unavailable because no send transport is configured",
		}
	}
	return nil
}

func composeDraftSpool(draft Draft, messageID string) (*ComposedMessage, error) {
	if err := verifyDraftAttachments(draft.Attachments); err != nil {
		return nil, err
	}
	return ComposeMessageSpool(draft, messageID)
}

func submitComposedMessage(
	ctx context.Context,
	submitter transport.Submitter,
	cfg transport.SubmitConfig,
	from string,
	recipients []string,
	message *ComposedMessage,
) (evidence transport.SubmitEvidence, resultErr error) {
	if streaming, ok := submitter.(transport.StreamingSubmitter); ok {
		reader, err := message.Open()
		if err != nil {
			return transport.SubmitEvidence{}, err
		}
		defer func() {
			if err := reader.Close(); err != nil {
				resultErr = errors.Join(resultErr, err)
			}
		}()
		return streaming.SubmitReader(ctx, cfg, from, recipients, message.MessageID(), reader, message.Size())
	}
	payload, err := readComposedMessage(message)
	if err != nil {
		return transport.SubmitEvidence{}, err
	}
	return submitter.Submit(ctx, cfg, from, recipients, payload)
}

func mirrorComposedMessage(
	ctx context.Context,
	mirror transport.SentMirror,
	cfg transport.ImapConfig,
	message *ComposedMessage,
	messageID string,
) (evidence transport.AppendEvidence, resultErr error) {
	if streaming, ok := mirror.(transport.StreamingSentMirror); ok {
		reader, err := message.Open()
		if err != nil {
			return transport.AppendEvidence{}, err
		}
		defer func() {
			if err := reader.Close(); err != nil {
				resultErr = errors.Join(resultErr, err)
			}
		}()
		return streaming.AppendToSentReader(ctx, cfg, reader, message.Size(), messageID)
	}
	payload, err := readComposedMessage(message)
	if err != nil {
		return transport.AppendEvidence{}, err
	}
	return mirror.AppendToSent(ctx, cfg, payload, messageID)
}

func readComposedMessage(message *ComposedMessage) (payload []byte, resultErr error) {
	reader, err := message.Open()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := reader.Close(); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	return io.ReadAll(reader)
}

// envelopeFingerprint identifies the exact claimed envelope: Message-ID,
// sender, recipients, subject, and body. Draft edits after the claim (or a
// mismatched claim) are detected before reconciliation trusts the claim.
// The fingerprint hashes the immutable draft fields, deliberately NOT the
// composed message bytes: BuildMessage generates fresh random multipart
// boundaries per invocation, so a byte hash would never reproduce at
// reconcile time and would falsely reject legitimate multipart sends.
func envelopeFingerprint(draft Draft, messageID string) string {
	hash := sha256.New()
	parts := []string{messageID, draft.From, draft.Subject}
	for _, recipient := range draft.To {
		parts = append(parts, recipient.Address)
	}
	for _, recipient := range draft.CC {
		parts = append(parts, recipient.Address)
	}
	for _, recipient := range draft.BCC {
		parts = append(parts, recipient.Address)
	}
	parts = append(parts, strings.ReplaceAll(draft.Body, "\r\n", "\n"))
	for _, part := range parts {
		hash.Write([]byte(part))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
func messageIdentityFingerprint(draft Draft, messageID string) string {
	parts := []string{
		strings.TrimSpace(messageID),
		canonicalDraftAddress(draft.From),
		canonicalDraftAddresses(draft.To),
		canonicalDraftAddresses(draft.CC),
		strings.TrimSpace(draft.Subject),
		normalizeMessageBody(draft.Body),
	}
	return hashMessageIdentity(parts)
}

func rawMessageIdentityFingerprint(raw []byte) (string, error) {
	message, err := stdmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate is not a readable RFC 5322 message: %v", err)}
	}
	from, err := canonicalHeaderAddress(message.Header.Get("From"))
	if err != nil {
		return "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate has an invalid From header: %v", err)}
	}
	to, err := canonicalHeaderAddresses(message.Header.Get("To"))
	if err != nil {
		return "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate has an invalid To header: %v", err)}
	}
	cc, err := canonicalHeaderAddresses(message.Header.Get("Cc"))
	if err != nil {
		return "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate has an invalid Cc header: %v", err)}
	}
	body, err := readPlainMessageBody(message.Header, message.Body)
	if err != nil {
		return "", err
	}
	return hashMessageIdentity([]string{
		strings.TrimSpace(message.Header.Get("Message-ID")),
		from,
		to,
		cc,
		decodeMessageHeader(message.Header.Get("Subject")),
		normalizeMessageBody(body),
	}), nil
}

func verifySentMessageIdentity(raw []byte, draft Draft, messageID string) error {
	actual, err := rawMessageIdentityFingerprint(raw)
	if err != nil {
		return err
	}
	if actual != messageIdentityFingerprint(draft, messageID) {
		return &OperationError{
			Code:    "send_identity_mismatch",
			Message: "the Sent candidate has the claimed Message-ID but does not match the draft envelope and body",
		}
	}
	return nil
}

func canonicalDraftAddress(value string) string {
	parsed, err := stdmail.ParseAddress(strings.TrimSpace(value))
	if err != nil {
		return strings.ToLower(strings.TrimSpace(value))
	}
	return strings.ToLower(parsed.Address)
}

func canonicalDraftAddresses(values []Recipient) string {
	addresses := make([]string, 0, len(values))
	for _, value := range values {
		addresses = append(addresses, canonicalDraftAddress(value.Address))
	}
	return strings.Join(addresses, "\x00")
}

func canonicalHeaderAddress(value string) (string, error) {
	parsed, err := stdmail.ParseAddress(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	return strings.ToLower(parsed.Address), nil
}

func canonicalHeaderAddresses(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	parsed, err := stdmail.ParseAddressList(value)
	if err != nil {
		return "", err
	}
	addresses := make([]string, 0, len(parsed))
	for _, address := range parsed {
		addresses = append(addresses, strings.ToLower(address.Address))
	}
	return strings.Join(addresses, "\x00"), nil
}

func hashMessageIdentity(parts []string) string {
	hash := sha256.New()
	for _, part := range parts {
		hash.Write([]byte(part))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func decodeMessageHeader(value string) string {
	decoded, err := (&mime.WordDecoder{}).DecodeHeader(value)
	if err != nil {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(decoded)
}

func normalizeMessageBody(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
}

func readPlainMessageBody(header stdmail.Header, body io.Reader) (string, error) {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		return readDecodedMessagePart(header, body)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return "", &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate multipart body has no boundary"}
	}
	reader := multipart.NewReader(body, boundary)
	for {
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate multipart body is malformed: %v", err)}
		}
		partHeader := stdmail.Header(part.Header)
		partType, partParams, parseErr := mime.ParseMediaType(partHeader.Get("Content-Type"))
		if parseErr == nil && strings.HasPrefix(strings.ToLower(partType), "multipart/") {
			nestedBoundary := partParams["boundary"]
			if nestedBoundary == "" {
				continue
			}
			nested, nestedErr := readPlainMessageBody(partHeader, part)
			if nestedErr == nil {
				return nested, nil
			}
			continue
		}
		if parseErr == nil && strings.EqualFold(partType, "text/plain") {
			return readDecodedMessagePart(partHeader, part)
		}
	}
	return "", &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate has no text/plain body"}
}

func readDecodedMessagePart(header stdmail.Header, body io.Reader) (string, error) {
	limited := io.LimitReader(body, maximumDraftStateBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("reading the Sent candidate body failed: %v", err)}
	}
	if int64(len(data)) > maximumDraftStateBytes {
		return "", &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate body exceeds the reconciliation limit"}
	}
	encoding := strings.ToLower(strings.TrimSpace(header.Get("Content-Transfer-Encoding")))
	switch encoding {
	case "base64":
		decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(data)))
		if err != nil {
			return "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate body has invalid base64: %v", err)}
		}
		return string(decoded), nil
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(data)))
		if err != nil {
			return "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate body has invalid quoted-printable data: %v", err)}
		}
		return string(decoded), nil
	default:
		return string(data), nil
	}
}

func sendSender(from string) (string, error) {
	parsed, err := stdmail.ParseAddress(strings.TrimSpace(from))
	if err != nil || parsed.Address == "" {
		return "", validationError("invalid from address")
	}
	return parsed.Address, nil
}

func missingCredentialsError(sender string) error {
	return &OperationError{
		Code: "smtp_credentials_missing",
		Message: "no app-specific password is stored for " + sender +
			"; run 'mailcli send setup --from " + sender + "' to store one",
	}
}

func mirrorPendingError(err error) error {
	message := "the message was accepted by the SMTP server, but mirroring it into the Sent mailbox failed; " +
		"the draft is retained and the send itself will not be retried"
	code := transport.ErrorCode(err)
	if code == "" {
		code = "send_mirror_pending"
	}
	return &OperationError{Code: code, Message: message + ": " + err.Error()}
}

func newMessageID(sender string) (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate message id: %w", err)
	}
	domain := sender[strings.LastIndex(sender, "@")+1:]
	return "<" + hex.EncodeToString(value[:]) + "@" + domain + ">", nil
}

func validateStoredDraftAddresses(draft Draft) error {
	if strings.TrimSpace(draft.From) != "" {
		if _, err := stdmail.ParseAddress(draft.From); err != nil {
			return validationError("invalid from address")
		}
	}
	seen := make(map[string]struct{}, len(draft.To)+len(draft.CC)+len(draft.BCC))
	for _, group := range [][]Recipient{draft.To, draft.CC, draft.BCC} {
		for _, recipient := range group {
			address := recipient.Address
			if recipient.Name != "" {
				address = (&stdmail.Address{Name: recipient.Name, Address: recipient.Address}).String()
			}
			parsed, err := stdmail.ParseAddress(address)
			if err != nil || parsed.Address == "" {
				return validationError("invalid recipient address")
			}
			normalized := strings.ToLower(parsed.Address)
			if _, duplicate := seen[normalized]; duplicate {
				return validationError("duplicate recipient address")
			}
			seen[normalized] = struct{}{}
		}
	}
	return nil
}

func draftEnvelopeRecipients(draft Draft) ([]string, error) {
	if err := validateStoredDraftAddresses(draft); err != nil {
		return nil, err
	}
	recipients := make([]Recipient, 0, len(draft.To)+len(draft.CC)+len(draft.BCC))
	recipients = append(recipients, draft.To...)
	recipients = append(recipients, draft.CC...)
	recipients = append(recipients, draft.BCC...)
	addresses := make([]string, 0, len(recipients))
	for index, recipient := range recipients {
		parsed, err := stdmail.ParseAddress(recipient.Address)
		if err != nil || parsed.Address == "" {
			return nil, validationError(fmt.Sprintf("invalid recipient address at position %d", index+1))
		}
		addresses = append(addresses, parsed.Address)
	}
	return addresses, nil
}

func draftRecipientValues(draft Draft) []string {
	recipients := make([]Recipient, 0, len(draft.To)+len(draft.CC)+len(draft.BCC))
	recipients = append(recipients, draft.To...)
	recipients = append(recipients, draft.CC...)
	recipients = append(recipients, draft.BCC...)
	values := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		values = append(values, recipient.Address)
	}
	return values
}

// DeliverViaTransport submits a draft over direct SMTP and mirrors it into
// the account's Sent mailbox without any local claim handling. It never
// resubmits after an accepted submission, even when the mirror fails; the
// partial evidence is returned alongside the mirror error. Callers own
// at-most-once semantics.
func DeliverViaTransport(ctx context.Context, send SendTransport, draft Draft) (evidence TransportEvidence, resultErr error) {
	if send.Submitter == nil || send.Mirror == nil || send.Credentials == nil {
		return TransportEvidence{}, &OperationError{
			Code:    "send_transport_unavailable",
			Message: "direct SMTP send is unavailable because no send transport is configured",
		}
	}
	if len(draft.To)+len(draft.CC)+len(draft.BCC) == 0 {
		return TransportEvidence{}, validationError("sending a draft requires at least one recipient")
	}
	if err := validateThreadSource(draft.SourceMessageID, draft.SourceReferences); err != nil {
		return TransportEvidence{}, err
	}
	envelopeRecipients, err := draftEnvelopeRecipients(draft)
	if err != nil {
		return TransportEvidence{}, err
	}
	if err := verifyDraftAttachments(draft.Attachments); err != nil {
		return TransportEvidence{}, err
	}
	sender, err := sendSender(draft.From)
	if err != nil {
		return TransportEvidence{}, err
	}
	smtpHost, smtpPort, imapHost, imapPort, err := transport.ProviderHosts(sender)
	if err != nil {
		return TransportEvidence{}, err
	}
	password, err := send.Credentials.Load(sender)
	if err != nil || password == "" {
		return TransportEvidence{}, missingCredentialsError(sender)
	}
	messageID, err := newMessageID(sender)
	if err != nil {
		return TransportEvidence{}, err
	}
	message, err := composeDraftSpool(draft, messageID)
	if err != nil {
		return TransportEvidence{}, err
	}
	defer func() {
		if err := message.Remove(); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	submitEvidence, err := submitComposedMessage(
		ctx,
		send.Submitter,
		transport.SubmitConfig{Host: smtpHost, Port: smtpPort, Username: sender, Password: password},
		sender, envelopeRecipients, message,
	)
	if err != nil {
		return TransportEvidence{}, err
	}
	evidence = TransportEvidence{
		ServerResponse: submitEvidence.ServerResponse,
		MessageID:      submitEvidence.MessageID,
	}
	appendEvidence, err := mirrorComposedMessage(
		ctx,
		send.Mirror,
		transport.ImapConfig{Host: imapHost, Port: imapPort, Username: sender, Password: password},
		message,
		submitEvidence.MessageID,
	)
	if err != nil {
		return evidence, err
	}
	evidence.MirrorMailbox = appendEvidence.Mailbox
	evidence.MirrorAppended = appendEvidence.Appended
	return evidence, nil
}

func cloneSendObservationBaseline(value *SendObservationBaseline) *SendObservationBaseline {
	if value == nil {
		return nil
	}
	clone := *value
	clone.SentMailboxIDs = append([]int64(nil), value.SentMailboxIDs...)
	return &clone
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

type DraftSaveBackend interface {
	ReconcileDraftSave(ctx context.Context, draft Draft, attempt DraftSaveAttempt) (DraftSaveEvidence, error)
}

func (s *Service) SaveDraft(ctx context.Context, ref string) (result SavedDraft, resultErr error) {
	root, err := s.resolveDraftRoot()
	if err != nil {
		return SavedDraft{}, err
	}
	lease, err := acquireDraftLease(ctx, root, ref)
	if err != nil {
		return SavedDraft{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, lease.release()) }()
	draft, err := readDraftForMutation(root, ref)
	if err != nil {
		return SavedDraft{}, err
	}
	if err := validateStoredDraftLimits(draft); err != nil {
		return SavedDraft{}, err
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
		return reconcileNativeDraftSave(ctx, backend, root, ref, draft, *draft.SaveAttempt)
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
	if err := verifyDraftAttachments(draft.Attachments); err != nil {
		return SavedDraft{}, err
	}
	return s.saveDraftLegacy(ctx, root, ref, draft)
}

func (s *Service) saveDraftLegacy(
	ctx context.Context,
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
	if err := discardDraftFiles(root, ref); err != nil {
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
	if err := replaceDraftSaveAttempt(root, ref, attempt); err != nil {
		return SavedDraft{}, &OperationError{
			Code:    "draft_save_reconcile_state_failed",
			Message: fmt.Sprintf("native draft was observed, but its reconciled state could not be recorded: %v", err),
		}
	}
	return finishObservedDraftSave(root, ref, evidence.ObservedMessage, nil)
}

func finishObservedDraftSave(
	root string,
	ref string,
	message MessageSummary,
	postflightErr error,
) (SavedDraft, error) {
	result := SavedDraft{LocalDraftRef: ref, Message: message}
	if err := discardDraftFiles(root, ref); err != nil {
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

func prepareDraft(request CreateDraftRequest) (Draft, error) {
	return prepareDraftWithAttachments(request, nil)
}

func prepareDraftWithAttachments(request CreateDraftRequest, previous []DraftAttachment) (Draft, error) {
	if request.Kind == "" {
		request.Kind = DraftKindNew
	}
	if request.Kind != DraftKindNew && request.Kind != DraftKindReply && request.Kind != DraftKindForward {
		return Draft{}, validationError("draft kind must be new, reply, or forward")
	}
	if request.Kind != DraftKindNew && request.SourceRef == "" {
		return Draft{}, validationError("reply and forward drafts require a source message ref")
	}
	if request.Kind != DraftKindNew {
		ref, err := mailref.DecodeMessage(request.SourceRef)
		if err != nil || !ref.IsStoreBound() {
			return Draft{}, validationError("reply and forward drafts require a store-bound source message ref")
		}
		if err := validateThreadSource(request.SourceMessageID, request.SourceReferences); err != nil {
			return Draft{}, err
		}
	}
	if request.Kind == DraftKindNew && len(request.Input.To)+len(request.Input.CC)+len(request.Input.BCC) == 0 {
		return Draft{}, validationError("new drafts require at least one recipient")
	}
	if request.Kind == DraftKindForward && len(request.Input.To)+len(request.Input.CC)+len(request.Input.BCC) == 0 {
		return Draft{}, validationError("forward drafts require at least one explicit recipient")
	}
	if err := validateDraftLimits(request.Input); err != nil {
		return Draft{}, err
	}
	if err := validateDraftAddresses(request.Input); err != nil {
		return Draft{}, err
	}
	content, err := prepareDraftContent(request.Input.BodyFormat, request.Input.Body)
	if err != nil {
		return Draft{}, err
	}
	attachments, err := fingerprintAttachmentsWithPrevious(request.Input.Attachments, previous)
	if err != nil {
		return Draft{}, err
	}
	ref, err := newDraftReference()
	if err != nil {
		return Draft{}, err
	}
	now := time.Now().UTC()
	return Draft{
		Ref: ref, Kind: request.Kind, SourceRef: request.SourceRef,
		ReplyAll:        request.ReplyAll,
		SourceMessageID: request.SourceMessageID, SourceReferences: request.SourceReferences,
		From: request.Input.From,
		To:   nonNilRecipients(request.Input.To), CC: nonNilRecipients(request.Input.CC),
		BCC: nonNilRecipients(request.Input.BCC), Subject: request.Input.Subject,
		Body: content.Plain, BodyFormat: content.Format,
		BodySource: content.Source, BodyHTML: content.HTML, Attachments: attachments,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

// validateThreadSource keeps control characters out of the threading headers
// written into drafts (the composer copies them verbatim into the message).
func validateThreadSource(sourceMessageID, sourceReferences string) error {
	if strings.ContainsAny(sourceMessageID, "\r\n") {
		return validationError("source message id contains control characters")
	}
	// Raw check: strings.Fields would treat CR/LF as whitespace and hide them.
	if strings.ContainsAny(sourceReferences, "\r\n") {
		return validationError("source references contain control characters")
	}
	return nil
}

func validateDraftLimits(input DraftInput) error {
	return validateDraftResourceLimits(
		len(input.Subject), len(input.Body), len(input.To)+len(input.CC)+len(input.BCC), len(input.Attachments),
	)
}

func validateStoredDraftLimits(draft Draft) error {
	bodyBytes := max(len(draft.Body), len(draft.BodySource), len(draft.BodyHTML))
	return validateDraftResourceLimits(
		len(draft.Subject), bodyBytes, len(draft.To)+len(draft.CC)+len(draft.BCC), len(draft.Attachments),
	)
}

func validateDraftResourceLimits(subjectBytes int, bodyBytes int, recipients int, attachments int) error {
	if subjectBytes > MaximumDraftSubjectBytes {
		return validationError("draft subject exceeds 64 KiB")
	}
	if bodyBytes > MaximumDraftBodyBytes {
		return validationError("draft body exceeds 4 MiB")
	}
	if recipients > MaximumDraftRecipients {
		return validationError("draft exceeds 200 total recipients")
	}
	if attachments > MaximumDraftAttachments {
		return validationError("draft exceeds 100 attachments")
	}
	return nil
}

func validateDraftAddresses(input DraftInput) error {
	if input.From != "" {
		if _, err := stdmail.ParseAddress(input.From); err != nil {
			return validationError("invalid from address")
		}
	}
	seen := make(map[string]struct{})
	for _, group := range [][]Recipient{input.To, input.CC, input.BCC} {
		for _, recipient := range group {
			address := recipient.Address
			if recipient.Name != "" {
				address = (&stdmail.Address{Name: recipient.Name, Address: recipient.Address}).String()
			}
			parsed, err := stdmail.ParseAddress(address)
			if err != nil {
				return validationError("invalid recipient address")
			}
			normalized := strings.ToLower(parsed.Address)
			if _, duplicate := seen[normalized]; duplicate {
				return validationError("duplicate recipient address")
			}
			seen[normalized] = struct{}{}
		}
	}
	return nil
}

func fingerprintAttachmentsWithPrevious(paths []string, previous []DraftAttachment) ([]DraftAttachment, error) {
	attachments := make([]DraftAttachment, 0, len(paths))
	remaining := MaximumDraftAttachmentBytes
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return nil, validationError("draft attachment paths must be absolute")
		}
		if reused, ok := reuseAttachmentFingerprint(path, previous); ok && reused.Size <= remaining {
			attachments = append(attachments, reused)
			remaining -= reused.Size
			continue
		}
		attachment, err := fingerprintAttachment(path, remaining)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
		remaining -= attachment.Size
	}
	return attachments, nil
}

func reuseAttachmentFingerprint(path string, previous []DraftAttachment) (DraftAttachment, bool) {
	for _, attachment := range previous {
		if attachment.Path != path || attachment.ModTimeNanos == 0 || attachment.SHA256 == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != attachment.Size || info.ModTime().UnixNano() != attachment.ModTimeNanos {
			return DraftAttachment{}, false
		}
		return attachment, true
	}
	return DraftAttachment{}, false
}

func fingerprintAttachment(path string, maximumSize int64) (DraftAttachment, error) {
	file, err := os.Open(path)
	if err != nil {
		return DraftAttachment{}, fmt.Errorf("open draft attachment: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		return DraftAttachment{}, errors.Join(fmt.Errorf("stat draft attachment: %w", err), file.Close())
	}
	if !info.Mode().IsRegular() {
		return DraftAttachment{}, errors.Join(
			validationError("draft attachment must be a regular file"), file.Close(),
		)
	}
	if info.Size() < 0 || info.Size() > maximumSize {
		return DraftAttachment{}, errors.Join(
			validationError("draft attachments exceed 512 MiB total"), file.Close(),
		)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return DraftAttachment{}, errors.Join(fmt.Errorf("hash draft attachment: %w", err), file.Close())
	}
	if err := file.Close(); err != nil {
		return DraftAttachment{}, fmt.Errorf("close draft attachment: %w", err)
	}
	return DraftAttachment{
		Path: path, Size: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil)),
		ModTimeNanos: info.ModTime().UnixNano(),
	}, nil
}

func verifyDraftAttachments(attachments []DraftAttachment) error {
	if len(attachments) > MaximumDraftAttachments {
		return validationError("draft exceeds 100 attachments")
	}
	remaining := MaximumDraftAttachmentBytes
	for _, expected := range attachments {
		if expected.Size < 0 || expected.Size > remaining {
			return validationError("draft attachments exceed 512 MiB total")
		}
		actual, err := fingerprintAttachment(expected.Path, remaining)
		if err != nil {
			return err
		}
		if actual.Size != expected.Size || actual.SHA256 != expected.SHA256 {
			return validationError("draft attachment " + filepath.Base(expected.Path) + " changed after review; update the draft before sending")
		}
		remaining -= actual.Size
	}
	return nil
}

func verifyAndLoadAttachments(draft Draft) ([]composerAttachment, error) {
	if len(draft.Attachments) > MaximumDraftAttachments {
		return nil, validationError("draft exceeds 100 attachments")
	}
	remaining := MaximumDraftAttachmentBytes
	loaded := make([]composerAttachment, 0, len(draft.Attachments))
	for _, expected := range draft.Attachments {
		if expected.Size < 0 || expected.Size > remaining {
			return nil, validationError("draft attachments exceed 512 MiB total")
		}
		attachment, actual, err := loadVerifiedAttachment(expected, remaining)
		if err != nil {
			return nil, err
		}
		if actual.Size != expected.Size || actual.SHA256 != expected.SHA256 {
			return nil, validationError("draft attachment " + filepath.Base(expected.Path) + " changed after review; update the draft before sending")
		}
		loaded = append(loaded, attachment)
		remaining -= actual.Size
	}
	return loaded, nil
}

func loadVerifiedAttachment(
	expected DraftAttachment,
	maximumSize int64,
) (loaded composerAttachment, actual DraftAttachment, resultErr error) {
	name := filepath.Base(expected.Path)
	file, err := os.Open(expected.Path)
	if err != nil {
		return composerAttachment{}, DraftAttachment{}, &ComposerError{Message: "read draft attachment " + name, Err: err}
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close draft attachment: %w", closeErr))
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return composerAttachment{}, DraftAttachment{}, fmt.Errorf("stat draft attachment %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return composerAttachment{}, DraftAttachment{}, validationError("draft attachment must be a regular file")
	}
	if info.Size() < 0 || info.Size() > maximumSize {
		return composerAttachment{}, DraftAttachment{}, validationError("draft attachments exceed 512 MiB total")
	}
	hash := sha256.New()
	data := make([]byte, int(info.Size()))
	if _, err := io.ReadFull(io.TeeReader(io.LimitReader(file, maximumSize), hash), data); err != nil {
		return composerAttachment{}, DraftAttachment{}, &ComposerError{Message: "read draft attachment " + name, Err: err}
	}
	finalInfo, err := file.Stat()
	if err != nil {
		return composerAttachment{}, DraftAttachment{}, fmt.Errorf("stat draft attachment %s after read: %w", name, err)
	}
	if finalInfo.Size() != info.Size() {
		return composerAttachment{}, DraftAttachment{}, validationError("draft attachment " + name + " changed while sending")
	}
	actual = DraftAttachment{Path: expected.Path, Size: int64(len(data)), SHA256: hex.EncodeToString(hash.Sum(nil))}
	return composerAttachmentFromData(expected.Path, data), actual, nil
}

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
	file *os.File
}

func acquireDraftLease(ctx context.Context, root string, ref string) (*draftLease, error) {
	path, err := draftLockPath(root, ref)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open draft lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, errors.Join(fmt.Errorf("restrict draft lock: %w", err), file.Close())
	}
	ticker := time.NewTicker(draftLockPoll)
	defer ticker.Stop()
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &draftLease{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			closeErr := file.Close()
			return nil, errors.Join(fmt.Errorf("lock draft: %w", err), closeErr)
		}
		select {
		case <-ctx.Done():
			closeErr := file.Close()
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
		syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN),
		l.file.Close(),
	)
}

func rejectClaimedDraft(draft Draft) error {
	if draft.SendAttempt != nil {
		return &OperationError{
			Code: "send_retry_blocked",
			Message: fmt.Sprintf(
				"draft has send attempt %s with outcome %s; inspect it and discard explicitly instead of retrying",
				draft.SendAttempt.ID, draft.SendAttempt.Outcome,
			),
		}
	}
	if draft.SaveAttempt != nil {
		return &OperationError{
			Code: "draft_save_retry_blocked",
			Message: fmt.Sprintf(
				"draft has native save attempt %s; run drafts save again only to reconcile it, or discard explicitly",
				draft.SaveAttempt.ID,
			),
		}
	}
	return nil
}

func beginSendAttempt(root string, ref string, messageID, envelopeFingerprint string) (SendAttempt, error) {
	return beginSendAttemptWithBaseline(root, ref, nil, messageID, envelopeFingerprint)
}

func beginSendAttemptWithBaseline(
	root string,
	ref string,
	baseline *SendObservationBaseline,
	messageID string,
	envelopeFingerprint string,
) (SendAttempt, error) {
	id, err := newSendAttemptID()
	if err != nil {
		return SendAttempt{}, err
	}
	now := time.Now().UTC()
	attempt := SendAttempt{
		ID: id, StartedAt: now, UpdatedAt: now, Outcome: SendOutcomeUnknown,
		MessageID: messageID, EnvelopeFingerprint: envelopeFingerprint,
		ObservationBaseline: cloneSendObservationBaseline(baseline),
	}
	path, err := sendClaimPath(root, ref)
	if err != nil {
		return SendAttempt{}, err
	}
	payload, err := encodeSendAttempt(ref, attempt)
	if err != nil {
		return SendAttempt{}, err
	}
	if err := writePrivateFile(path, payload); err != nil {
		if errors.Is(err, os.ErrExist) {
			return SendAttempt{}, &OperationError{
				Code:    "send_retry_blocked",
				Message: "draft already has a send attempt; inspect it and discard explicitly instead of retrying",
			}
		}
		return SendAttempt{}, fmt.Errorf("create send claim: %w", err)
	}
	return attempt, nil
}

func newSendAttemptID() (string, error) {
	var value [18]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate send attempt id: %w", err)
	}
	return "send_" + base64.RawURLEncoding.EncodeToString(value[:]), nil
}

type storedSendAttempt struct {
	Version  int         `json:"version"`
	DraftRef string      `json:"draft_ref"`
	Attempt  SendAttempt `json:"attempt"`
}

func encodeSendAttempt(ref string, attempt SendAttempt) ([]byte, error) {
	payload, err := json.MarshalIndent(storedSendAttempt{
		Version: 1, DraftRef: ref, Attempt: attempt,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode send claim: %w", err)
	}
	return append(payload, '\n'), nil
}

func readSendAttempt(root string, ref string) (*SendAttempt, error) {
	path, err := sendClaimPath(root, ref)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect send claim: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maximumDraftStateBytes {
		return nil, fmt.Errorf("send claim is not a bounded regular file")
	}
	payload, err := readBoundedRegularFile(path, info, maximumDraftStateBytes)
	if err != nil {
		return nil, fmt.Errorf("read send claim: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var stored storedSendAttempt
	if err := decoder.Decode(&stored); err != nil {
		return nil, fmt.Errorf("decode send claim: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("send claim must contain exactly one JSON object")
	}
	if !validSendAttempt(stored, ref) {
		return nil, fmt.Errorf("send claim is invalid")
	}
	return &stored.Attempt, nil
}

func validSendAttempt(stored storedSendAttempt, ref string) bool {
	attempt := stored.Attempt
	if stored.Version != 1 || stored.DraftRef != ref || attempt.ID == "" ||
		attempt.StartedAt.IsZero() || attempt.UpdatedAt.IsZero() {
		return false
	}
	if !validObservationBaseline(attempt.ObservationBaseline) {
		return false
	}
	if !validSendMaterialization(attempt.Materialized) {
		return false
	}
	switch attempt.Outcome {
	case SendOutcomeUnknown:
		return !attempt.SentStoreObserved && !attempt.AcceptedByMail
	case SendOutcomeAccepted:
		return attempt.InvocationStarted && attempt.AcceptedByMail && !attempt.SentStoreObserved
	case SendOutcomeObserved:
		return attempt.InvocationStarted && attempt.SentStoreObserved
	case SendOutcomeSent:
		return attempt.InvocationStarted && attempt.AcceptedByMail && attempt.SentStoreObserved &&
			validTransportEvidence(attempt.Transport)
	case SendOutcomeMirrorPending:
		return attempt.InvocationStarted && attempt.AcceptedByMail && !attempt.SentStoreObserved &&
			validTransportEvidence(attempt.Transport)
	default:
		return false
	}
}

func validTransportEvidence(value *TransportEvidence) bool {
	return value != nil && strings.TrimSpace(value.MessageID) != ""
}

func validSendMaterialization(value *SendMaterialization) bool {
	if value == nil {
		return true
	}
	if value.AttachmentCount < 0 || strings.TrimSpace(value.From) == "" ||
		len(value.To)+len(value.CC)+len(value.BCC) == 0 {
		return false
	}
	if value.Body != nil && int64(len(*value.Body)) > maximumDraftStateBytes {
		return false
	}
	return validateDraftAddresses(DraftInput{
		From: value.From, To: value.To, CC: value.CC, BCC: value.BCC,
	}) == nil
}

func validObservationBaseline(value *SendObservationBaseline) bool {
	if value == nil {
		return true
	}
	if value.StoreUUID == "" || value.MaximumRowID < 0 || value.CapturedUnix < 1 || len(value.SentMailboxIDs) == 0 {
		return false
	}
	seen := make(map[int64]struct{}, len(value.SentMailboxIDs))
	for _, identifier := range value.SentMailboxIDs {
		if identifier < 1 {
			return false
		}
		if _, exists := seen[identifier]; exists {
			return false
		}
		seen[identifier] = struct{}{}
	}
	return true
}

func replaceSendAttempt(root string, ref string, attempt SendAttempt) (resultErr error) {
	path, err := sendClaimPath(root, ref)
	if err != nil {
		return err
	}
	payload, err := encodeSendAttempt(ref, attempt)
	if err != nil {
		return err
	}
	temporary, err := attachmentTemporaryPath(path)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, removeIfPresent(temporary))
	}()
	if err := writePrivateFile(temporary, payload); err != nil {
		return fmt.Errorf("write send claim update: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("publish send claim update: %w", err)
	}
	return syncDirectory(root)
}

func removeSendAttempt(root string, ref string) error {
	path, err := sendClaimPath(root, ref)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove send claim: %w", err)
	}
	return syncDirectory(root)
}

type storedDraftSaveAttempt struct {
	Version  int              `json:"version"`
	DraftRef string           `json:"draft_ref"`
	Attempt  DraftSaveAttempt `json:"attempt"`
}

func beginDraftSaveAttempt(
	root string,
	ref string,
	baseline *SendObservationBaseline,
) (DraftSaveAttempt, error) {
	id, err := newDraftSaveAttemptID()
	if err != nil {
		return DraftSaveAttempt{}, err
	}
	now := time.Now().UTC()
	attempt := DraftSaveAttempt{
		ID: id, StartedAt: now, UpdatedAt: now,
		ObservationBaseline: cloneSendObservationBaseline(baseline),
	}
	path, err := saveClaimPath(root, ref)
	if err != nil {
		return DraftSaveAttempt{}, err
	}
	payload, err := encodeDraftSaveAttempt(ref, attempt)
	if err != nil {
		return DraftSaveAttempt{}, err
	}
	if err := writePrivateFile(path, payload); err != nil {
		if errors.Is(err, os.ErrExist) {
			return DraftSaveAttempt{}, &OperationError{
				Code: "draft_save_retry_blocked", Message: "draft already has a native save attempt",
			}
		}
		return DraftSaveAttempt{}, fmt.Errorf("create draft-save claim: %w", err)
	}
	return attempt, nil
}

func newDraftSaveAttemptID() (string, error) {
	var value [18]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate draft-save attempt id: %w", err)
	}
	return "save_" + base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func encodeDraftSaveAttempt(ref string, attempt DraftSaveAttempt) ([]byte, error) {
	payload, err := json.MarshalIndent(storedDraftSaveAttempt{
		Version: 1, DraftRef: ref, Attempt: attempt,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode draft-save claim: %w", err)
	}
	payload = append(payload, '\n')
	if int64(len(payload)) > maximumDraftStateBytes {
		return nil, validationError("draft-save claim exceeds 20 MiB")
	}
	return payload, nil
}

func readDraftSaveAttempt(root string, ref string) (*DraftSaveAttempt, error) {
	path, err := saveClaimPath(root, ref)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect draft-save claim: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maximumDraftStateBytes {
		return nil, fmt.Errorf("draft-save claim is not a bounded regular file")
	}
	payload, err := readBoundedRegularFile(path, info, maximumDraftStateBytes)
	if err != nil {
		return nil, fmt.Errorf("read draft-save claim: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var stored storedDraftSaveAttempt
	if err := decoder.Decode(&stored); err != nil {
		return nil, fmt.Errorf("decode draft-save claim: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("draft-save claim must contain exactly one JSON object")
	}
	if !validDraftSaveAttempt(stored, ref) {
		return nil, fmt.Errorf("draft-save claim is invalid")
	}
	return &stored.Attempt, nil
}

func validDraftSaveAttempt(stored storedDraftSaveAttempt, ref string) bool {
	attempt := stored.Attempt
	if stored.Version != 1 || stored.DraftRef != ref || attempt.ID == "" ||
		attempt.StartedAt.IsZero() || attempt.UpdatedAt.IsZero() ||
		attempt.ObservationBaseline == nil || !validObservationBaseline(attempt.ObservationBaseline) ||
		!validSendMaterialization(attempt.Materialized) {
		return false
	}
	if attempt.AcceptedByMail && !attempt.InvocationStarted {
		return false
	}
	return attempt.ObservedMessageRef == "" || attempt.InvocationStarted && attempt.AcceptedByMail
}

func replaceDraftSaveAttempt(root string, ref string, attempt DraftSaveAttempt) (resultErr error) {
	path, err := saveClaimPath(root, ref)
	if err != nil {
		return err
	}
	payload, err := encodeDraftSaveAttempt(ref, attempt)
	if err != nil {
		return err
	}
	temporary, err := attachmentTemporaryPath(path)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, removeIfPresent(temporary)) }()
	if err := writePrivateFile(temporary, payload); err != nil {
		return fmt.Errorf("write draft-save claim update: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("publish draft-save claim update: %w", err)
	}
	return syncDirectory(root)
}

func removeDraftSaveAttempt(root string, ref string) error {
	path, err := saveClaimPath(root, ref)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove draft-save claim: %w", err)
	}
	return syncDirectory(root)
}

func removeDraftLockFile(root string, ref string) error {
	path, err := draftLockPath(root, ref)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove draft lock: %w", err)
	}
	return nil
}

// readDraftForMutation reads a draft while the caller holds its exclusive lease.
// On a missing draft the lock file created by acquireDraftLease is removed, so a
// failed mutation leaves no orphan lock; the held flock makes the unlink race-free.
func readDraftForMutation(root string, ref string) (Draft, error) {
	draft, err := readDraftFile(root, ref)
	if err != nil {
		var operation *OperationError
		if errors.As(err, &operation) && operation.Code == "not_found" {
			err = errors.Join(err, removeDraftLockFile(root, ref))
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

func replaySendAttempt(root string, ref string, attempt SendAttempt) (SendResult, error) {
	result := resultForAttempt(ref, attempt, true)
	result.Replayed = true
	switch attempt.Outcome {
	case SendOutcomeObserved, SendOutcomeSent:
		if err := discardDraftFiles(root, ref); err != nil {
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

func discardDraftFiles(root string, ref string) error {
	path, err := draftPath(root, ref)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return errors.Join(
				&OperationError{Code: "not_found", Message: "draft not found"},
				removeDraftLockFile(root, ref),
			)
		}
		return fmt.Errorf("discard draft: %w", err)
	}
	if err := syncDirectory(root); err != nil {
		return fmt.Errorf("persist draft removal: %w", err)
	}
	return errors.Join(removeSendAttempt(root, ref), removeDraftSaveAttempt(root, ref), removeDraftLockFile(root, ref))
}

func writeDraftFile(root string, draft Draft) (resultErr error) {
	path, err := draftPath(root, draft.Ref)
	if err != nil {
		return err
	}
	draft.SendAttempt = nil
	draft.SaveAttempt = nil
	payload, err := json.MarshalIndent(draft, "", "  ")
	if err != nil {
		return fmt.Errorf("encode draft: %w", err)
	}
	payload = append(payload, '\n')
	if int64(len(payload)) > maximumDraftStateBytes {
		return validationError("draft state exceeds 20 MiB")
	}
	temporary, err := attachmentTemporaryPath(path)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, removeIfPresent(temporary))
	}()
	if err := writePrivateFile(temporary, payload); err != nil {
		return fmt.Errorf("write draft: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("publish draft: %w", err)
	}
	return syncDirectory(root)
}

func writePrivateFile(path string, payload []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create private file: %w", err)
	}
	if _, err := file.Write(payload); err != nil {
		return errors.Join(
			fmt.Errorf("write private file: %w", err), file.Close(), removeIfPresent(path),
		)
	}
	if err := file.Sync(); err != nil {
		return errors.Join(
			fmt.Errorf("sync private file: %w", err), file.Close(), removeIfPresent(path),
		)
	}
	if err := file.Close(); err != nil {
		return errors.Join(fmt.Errorf("close private file: %w", err), removeIfPresent(path))
	}
	return syncDirectory(filepath.Dir(path))
}

func readDraftFile(root string, ref string) (Draft, error) {
	draft, err := loadDraftDocument(root, ref)
	if err != nil {
		return Draft{}, wrapDraftStateError(root, ref, err)
	}
	if err := validateStoredDraftContent(draft); err != nil {
		return Draft{}, wrapDraftStateError(root, ref, fmt.Errorf("validate draft content: %w", err))
	}
	if err := attachDraftAttempts(root, ref, &draft); err != nil {
		return Draft{}, wrapDraftStateError(root, ref, err)
	}
	return draft, nil
}

func wrapDraftStateError(root, ref string, err error) error {
	var operation *OperationError
	if errors.As(err, &operation) && operation.Code == "not_found" {
		return err
	}
	return &OperationError{
		Code: "draft_state_error",
		Message: fmt.Sprintf(
			"draft %s has invalid state in %s: %v; delete the corrupt state file or discard the draft",
			ref, filepath.Base(filepath.Join(root, ref+".json")), err,
		),
	}
}

// readDraftSummary loads the list view of a draft: same envelope
// discipline as readDraftFile but no canonical body validation and no
// Markdown/HTML re-render, so listing stays cheap and a draft with a
// corrupt body still appears (inspect keeps the full gate).
func readDraftSummary(root string, ref string) (DraftSummary, error) {
	draft, err := loadDraftDocument(root, ref)
	if err != nil {
		return DraftSummary{}, err
	}
	if err := attachDraftAttempts(root, ref, &draft); err != nil {
		return DraftSummary{}, err
	}
	return draftSummaryFrom(draft), nil
}

// loadDraftDocument decodes one draft file: bounded regular file, exactly
// one JSON object with strict fields, reference match. No content
// validation and no attempt sidecars; callers add what their path needs.
func loadDraftDocument(root string, ref string) (Draft, error) {
	path, err := draftPath(root, ref)
	if err != nil {
		return Draft{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Draft{}, &OperationError{Code: "not_found", Message: "draft not found"}
		}
		return Draft{}, fmt.Errorf("inspect draft: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maximumDraftStateBytes {
		return Draft{}, fmt.Errorf("draft is not a bounded regular file")
	}
	payload, err := readBoundedRegularFile(path, info, maximumDraftStateBytes)
	if err != nil {
		return Draft{}, fmt.Errorf("read draft: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var draft Draft
	if err := decoder.Decode(&draft); err != nil {
		return Draft{}, fmt.Errorf("decode draft: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Draft{}, fmt.Errorf("draft must contain exactly one JSON object")
	}
	if draft.Ref != ref {
		return Draft{}, fmt.Errorf("draft reference does not match its file")
	}
	if draft.BodyFormat == "" {
		draft.BodyFormat = DraftBodyPlain
	}
	return draft, nil
}

// attachDraftAttempts loads the send/save attempt sidecars onto a decoded
// draft, enforcing the same claim-conflict rule on every read path.
func attachDraftAttempts(root string, ref string, draft *Draft) error {
	draft.SendAttempt = nil
	attempt, err := readSendAttempt(root, ref)
	if err != nil {
		return err
	}
	draft.SendAttempt = attempt
	draft.SaveAttempt = nil
	saveAttempt, err := readDraftSaveAttempt(root, ref)
	if err != nil {
		return err
	}
	draft.SaveAttempt = saveAttempt
	if draft.SendAttempt != nil && draft.SaveAttempt != nil {
		return fmt.Errorf("draft has conflicting send and save claims")
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open state directory: %w", err)
	}
	if err := directory.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync state directory: %w", err), directory.Close())
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close state directory: %w", err)
	}
	return nil
}

func readBoundedRegularFile(path string, expected os.FileInfo, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) || opened.Size() > maximum {
		return nil, errors.Join(fmt.Errorf("file identity changed while opening"), file.Close())
	}
	payload, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if int64(len(payload)) > maximum {
		return nil, fmt.Errorf("file exceeds maximum size")
	}
	return payload, nil
}

func nonNilRecipients(recipients []Recipient) []Recipient {
	if recipients == nil {
		return []Recipient{}
	}
	return recipients
}
