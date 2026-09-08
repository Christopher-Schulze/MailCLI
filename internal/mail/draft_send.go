package mail

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"mailcli/internal/transport"
)

func (s *Service) SendDraft(ctx context.Context, ref string) (result SendResult, resultErr error) {
	if err := draftContextError(ctx, "send"); err != nil {
		return SendResult{}, err
	}
	root, err := s.resolveDraftRoot()
	if err != nil {
		return SendResult{}, err
	}
	lockContext, cancelLock := draftLockContext(ctx)
	defer cancelLock()
	lease, err := acquireDraftLease(lockContext, root, ref)
	if err != nil {
		return SendResult{}, classifyDraftContextError(ctx, err, "send")
	}
	defer func() {
		resultErr = errors.Join(classifyDraftContextError(ctx, resultErr, "send"), lease.release())
	}()
	if err := draftContextError(ctx, "send"); err != nil {
		return SendResult{}, err
	}
	draft, err := readDraftForMutation(lease, root, ref)
	if err != nil {
		var operation *OperationError
		if errors.As(err, &operation) && operation.Code == "not_found" {
			receipt, receiptErr := readActiveSendReceipt(root, ref)
			if receiptErr == nil && receipt != nil {
				result := resultForReceipt(*receipt)
				if cleanupErr := errors.Join(removeDraftClaims(root, ref), lease.removeLock()); cleanupErr != nil {
					return result, &OperationError{
						Code:    "send_cleanup_failed",
						Message: fmt.Sprintf("terminal send evidence was found, but stale local claims could not be removed: %v", cleanupErr),
					}
				}
				return result, nil
			}
			if receiptErr != nil {
				var receiptOperation *OperationError
				if !errors.As(receiptErr, &receiptOperation) || receiptOperation.Code != "not_found" {
					return SendResult{}, receiptErr
				}
			}
		}
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
		return replaySendAttempt(lease, root, ref, *draft.SendAttempt)
	}
	if receipt, receiptErr := readActiveSendReceipt(root, ref); receiptErr != nil {
		var operation *OperationError
		if !errors.As(receiptErr, &operation) || operation.Code != "not_found" {
			return SendResult{}, receiptErr
		}
	} else if receipt != nil {
		result := resultForReceipt(*receipt)
		if cleanupErr := discardDraftFiles(lease, root, ref); cleanupErr != nil {
			return result, &OperationError{
				Code:    "send_cleanup_failed",
				Message: fmt.Sprintf("terminal send evidence was found, but the duplicate local draft could not be removed: %v", cleanupErr),
			}
		}
		result.DraftRetained = false
		return result, nil
	}
	if draft.Kind == DraftKindNew && strings.TrimSpace(draft.From) == "" {
		return SendResult{}, validationError("sending a new draft requires an explicit from address")
	}
	if draft.Kind == DraftKindForward && len(draft.To)+len(draft.CC)+len(draft.BCC) == 0 {
		return SendResult{}, validationError("sending a forward draft requires at least one explicit recipient")
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
	message, err := composeDraftSpool(ctx, draft, messageID)
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
	mimeFingerprint, err := draftMIMEFingerprint(draft)
	if err != nil {
		return SendResult{}, err
	}
	attempt, err := beginSendAttemptWithMIMEFingerprint(
		root,
		ref,
		nil,
		messageID,
		envelopeFingerprint(draft, messageID),
		mimeFingerprint,
	)
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
	if err := persistMirrorAttemptBeforeDispatch(root, ref, &attempt); err != nil {
		return resultForAttempt(ref, attempt, true), &OperationError{
			Code:    "send_state_unknown",
			Message: fmt.Sprintf("the server accepted the message, but its Sent mirror state could not be armed safely: %v", err),
		}
	}
	result = resultForAttempt(ref, attempt, true)
	appendEvidence, err := mirrorComposedMessage(
		ctx,
		s.send.Mirror,
		transport.ImapConfig{Host: imapHost, Port: imapPort, Username: sender, Password: password},
		message,
		attempt.MessageID,
	)
	if err != nil {
		attempt.Transport.MirrorOutcomeUnknown = mirrorOutcomeUnknown(err)
		attempt.UpdatedAt = time.Now().UTC()
		if stateErr := replaceSendAttempt(root, ref, attempt); stateErr != nil {
			result = resultForAttempt(ref, attempt, true)
			return result, &OperationError{
				Code:    "send_state_unknown",
				Message: fmt.Sprintf("Sent mirroring failed, but its local outcome could not be recorded safely: %v", stateErr),
			}
		}
		result = resultForAttempt(ref, attempt, true)
		// The submission was accepted, so the send itself is never retried;
		// the claim stays reconcilable and only the mirror may be retried.
		return result, mirrorPendingError(err)
	}
	attempt.SentStoreObserved = true
	attempt.Transport.MirrorMailbox = appendEvidence.Mailbox
	attempt.Transport.MirrorUIDValidity = appendEvidence.UIDValidity
	attempt.Transport.MirrorUID = appendEvidence.UID
	attempt.Transport.MirrorAppended = appendEvidence.Appended
	attempt.Transport.MirrorOutcomeUnknown = false
	attempt.Outcome = SendOutcomeSent
	attempt.UpdatedAt = time.Now().UTC()
	result = resultForAttempt(ref, attempt, true)
	if err := replaceSendAttempt(root, ref, attempt); err != nil {
		return result, &OperationError{
			Code:    "send_outcome_unknown",
			Message: fmt.Sprintf("the message was sent and mirrored, but its local send state could not be recorded safely: %v", err),
		}
	}
	return finishObservedSend(lease, root, ref, attempt, false)
}

func (s *Service) adoptObservedSentMessage(
	ctx context.Context,
	lease *draftLease,
	root string,
	ref string,
	draft Draft,
	attempt SendAttempt,
	imap transport.ImapOperator,
	cfg transport.ImapConfig,
	sentBox string,
	uid uint32,
	uidValidity uint32,
	matchCount int,
) (SendResult, error) {
	result := resultForReconcile(ref, attempt)
	if matchCount > 1 {
		return result, unverifiableSendError(attempt, draft,
			fmt.Sprintf("the Sent mailbox contains %d messages with the claimed Message-ID", matchCount))
	}
	if uid == 0 || matchCount != 1 {
		return result, &OperationError{
			Code:    "send_reconcile_unavailable",
			Message: "the Sent Message-ID match did not include a usable UID",
		}
	}
	raw, fetchErr := imap.FetchMessage(ctx, cfg, sentBox, uid, uidValidity, MaximumRawSourceBytes)
	if fetchErr != nil {
		return result, mirrorPendingError(fetchErr)
	}
	if identityErr := verifySentMessageIdentity(raw, draft, attempt.MessageID, attempt.MIMEFingerprint); identityErr != nil {
		return result, identityErr
	}
	attempt.SentStoreObserved = true
	attempt.Transport.MirrorMailbox = sentBox
	attempt.Transport.MirrorUIDValidity = uidValidity
	attempt.Transport.MirrorUID = uid
	attempt.Transport.MirrorAppended = false
	attempt.Transport.MirrorAttempted = true
	attempt.Transport.MirrorOutcomeUnknown = false
	attempt.Outcome = SendOutcomeSent
	attempt.UpdatedAt = time.Now().UTC()
	result = resultForReconcile(ref, attempt)
	if stateErr := replaceSendAttempt(root, ref, attempt); stateErr != nil {
		return result, &OperationError{
			Code:    "send_reconcile_state_failed",
			Message: fmt.Sprintf("the existing Sent message was verified, but the reconciled state could not be recorded: %v", stateErr),
		}
	}
	return finishObservedSend(lease, root, ref, attempt, true)
}

func mirrorOutcomeUnknownError(attempt SendAttempt) error {
	return &OperationError{
		Code: "send_mirror_outcome_unknown",
		Message: fmt.Sprintf(
			"the Sent APPEND outcome for %s is unknown and no matching message is currently provable; automatic APPEND retry is forbidden",
			attempt.MessageID,
		),
	}
}

type SendReconciler interface {
	ReconcileSend(ctx context.Context, draft Draft, attempt SendAttempt) (SendEvidence, error)
}

func (s *Service) ReconcileDraft(ctx context.Context, ref string) (result SendResult, resultErr error) {
	if err := draftContextError(ctx, "reconcile"); err != nil {
		return SendResult{}, err
	}
	root, err := s.resolveDraftRoot()
	if err != nil {
		return SendResult{}, err
	}
	lockContext, cancelLock := draftLockContext(ctx)
	defer cancelLock()
	lease, err := acquireDraftLease(lockContext, root, ref)
	if err != nil {
		return SendResult{}, classifyDraftContextError(ctx, err, "reconcile")
	}
	defer func() { resultErr = errors.Join(resultErr, lease.release()) }()
	if err := draftContextError(ctx, "reconcile"); err != nil {
		return SendResult{}, err
	}
	draft, err := readDraftForMutation(lease, root, ref)
	if err != nil {
		var operation *OperationError
		if errors.As(err, &operation) && operation.Code == "not_found" {
			receipt, receiptErr := readActiveSendReceipt(root, ref)
			if receiptErr == nil && receipt != nil {
				result := resultForReceipt(*receipt)
				result.Reconciled = true
				if cleanupErr := errors.Join(removeDraftClaims(root, ref), lease.removeLock()); cleanupErr != nil {
					return result, &OperationError{
						Code:    "send_cleanup_failed",
						Message: fmt.Sprintf("terminal send evidence was found, but stale local claims could not be removed: %v", cleanupErr),
					}
				}
				return result, nil
			}
			if receiptErr != nil {
				var receiptOperation *OperationError
				if !errors.As(receiptErr, &receiptOperation) || receiptOperation.Code != "not_found" {
					return SendResult{}, receiptErr
				}
			}
		}
		return SendResult{}, err
	}
	if draft.SendAttempt == nil {
		if receipt, receiptErr := readActiveSendReceipt(root, ref); receiptErr != nil {
			return SendResult{}, receiptErr
		} else if receipt != nil {
			result := resultForReceipt(*receipt)
			result.Reconciled = true
			if cleanupErr := discardDraftFiles(lease, root, ref); cleanupErr != nil {
				return result, &OperationError{
					Code:    "send_cleanup_failed",
					Message: fmt.Sprintf("terminal send evidence was found, but the duplicate local draft could not be removed: %v", cleanupErr),
				}
			}
			result.DraftRetained = false
			return result, nil
		}
		return SendResult{}, &OperationError{Code: "send_reconcile_unavailable", Message: "draft has no send attempt to reconcile"}
	}
	attempt := *draft.SendAttempt
	if attempt.Outcome == SendOutcomeObserved || attempt.Outcome == SendOutcomeSent {
		result, err := replaySendAttempt(lease, root, ref, attempt)
		result.Reconciled = true
		return result, err
	}
	if attempt.Outcome == SendOutcomeMirrorPending {
		return s.reconcileMirrorPending(ctx, lease, root, ref, draft, attempt)
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
		return s.reconcileUnknownViaImap(ctx, lease, root, ref, draft, attempt)
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
		return persistReconciledSend(lease, root, ref, attempt, evidence)
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
	lease *draftLease,
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
	return finishObservedSend(lease, root, ref, attempt, true)
}

// reconcileUnknownViaImap resolves a crash-stranded unknown claim: the claim
// carries the Message-ID and envelope fingerprint written before submission,
// so the Sent mailbox can be searched over IMAP without store observation.
// Absence in Sent never proves non-delivery (APPEND may still crash), so the
// claim stays unknown and automatic retries remain blocked.
func (s *Service) reconcileUnknownViaImap(
	ctx context.Context,
	lease *draftLease,
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
	if strings.TrimSpace(attempt.MIMEFingerprint) == "" {
		return resultForReconcile(ref, attempt), &OperationError{
			Code:    "send_identity_unverifiable",
			Message: "the retained send claim has no versioned MIME fingerprint; reconciliation cannot prove the exact content",
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
	sentBox, resolveErr := transport.ResolveSentMailbox(mailboxes)
	if resolveErr != nil {
		if transport.ErrorCode(resolveErr) == transport.CodeIMAPSentMailboxNotFound {
			return resultForReconcile(ref, attempt), unverifiableSendError(attempt, draft, "no Sent mailbox found on the IMAP server")
		}
		return resultForReconcile(ref, attempt), resolveErr
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
		if identityErr := verifySentMessageIdentity(raw, draft, attempt.MessageID, attempt.MIMEFingerprint); identityErr != nil {
			return resultForReconcile(ref, attempt), identityErr
		}
		attempt.InvocationStarted = true
		attempt.AcceptedByMail = true
		attempt.SentStoreObserved = true
		attempt.Outcome = SendOutcomeSent
		attempt.Transport = &TransportEvidence{
			MessageID: attempt.MessageID, MirrorMailbox: sentBox,
			MirrorUIDValidity: uidValidity, MirrorUID: uid,
		}
		attempt.UpdatedAt = time.Now().UTC()
		result := resultForReconcile(ref, attempt)
		result.Reconciled = true
		if err := replaceSendAttempt(root, ref, attempt); err != nil {
			return result, &OperationError{
				Code:    "send_reconcile_state_failed",
				Message: fmt.Sprintf("the sent message was located over IMAP, but the reconciled state could not be recorded: %v", err),
			}
		}
		return finishObservedSend(lease, root, ref, attempt, true)
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
// accepted but whose Sent-mailbox mirror did not complete. It retries only a
// mirror with a known failed APPEND; an unknown APPEND outcome is searched and
// verified but never replayed.
func (s *Service) reconcileMirrorPending(
	ctx context.Context,
	lease *draftLease,
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
	if strings.TrimSpace(attempt.MIMEFingerprint) == "" {
		return result, &OperationError{
			Code:    "send_identity_unverifiable",
			Message: "the retained send claim has no versioned MIME fingerprint; Sent adoption and mirror retry are blocked",
		}
	}
	outcomeUnknown := attempt.Transport.MirrorOutcomeUnknown
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
	imap := s.send.ImapClient()
	sentConfig := transport.ImapConfig{Host: imapHost, Port: imapPort, Username: sender, Password: password}
	sentBox := ""
	if imap != nil {
		mailboxes, listErr := imap.ListMailboxes(ctx, sentConfig)
		if listErr != nil {
			return result, mirrorPendingError(listErr)
		}
		var resolveErr error
		sentBox, resolveErr = transport.ResolveSentMailbox(mailboxes)
		if resolveErr != nil {
			if transport.ErrorCode(resolveErr) == transport.CodeIMAPSentMailboxNotFound {
				return result, &OperationError{Code: "send_reconcile_unavailable", Message: "no Sent mailbox is available to verify the accepted message before mirroring"}
			}
			return result, resolveErr
		}
		uid, uidValidity, matchCount, searchErr := imap.SearchUID(
			ctx,
			sentConfig,
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
			return s.adoptObservedSentMessage(
				ctx,
				lease,
				root,
				ref,
				draft,
				attempt,
				imap,
				sentConfig,
				sentBox,
				uid,
				uidValidity,
				matchCount,
			)
		}
	}
	if outcomeUnknown {
		return result, mirrorOutcomeUnknownError(attempt)
	}
	message, err := composeDraftSpool(ctx, draft, attempt.MessageID)
	if err != nil {
		return result, err
	}
	defer func() {
		if err := message.Remove(); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if err := persistMirrorAttemptBeforeDispatch(root, ref, &attempt); err != nil {
		return resultForReconcile(ref, attempt), &OperationError{
			Code:    "send_reconcile_state_failed",
			Message: fmt.Sprintf("the Sent mirror attempt could not be armed safely: %v", err),
		}
	}
	appendEvidence, err := mirrorComposedMessage(
		ctx,
		s.send.Mirror,
		sentConfig,
		message,
		attempt.MessageID,
	)
	if err != nil {
		attempt.Transport.MirrorOutcomeUnknown = mirrorOutcomeUnknown(err)
		attempt.UpdatedAt = time.Now().UTC()
		result = resultForReconcile(ref, attempt)
		if stateErr := replaceSendAttempt(root, ref, attempt); stateErr != nil {
			return result, &OperationError{
				Code:    "send_reconcile_state_failed",
				Message: fmt.Sprintf("Sent mirroring failed, but its outcome could not be recorded safely: %v", stateErr),
			}
		}
		return result, mirrorPendingError(err)
	}
	if imap != nil {
		uid, uidValidity, matchCount, searchErr := imap.SearchUID(
			ctx,
			sentConfig,
			sentBox,
			attempt.MessageID,
		)
		if searchErr != nil {
			var transportErr *transport.TransportError
			if !errors.As(searchErr, &transportErr) || transportErr.Code != transport.CodeIMAPMessageNotFound {
				return result, mirrorPendingError(searchErr)
			}
		}
		if matchCount > 0 {
			return s.adoptObservedSentMessage(
				ctx,
				lease,
				root,
				ref,
				draft,
				attempt,
				imap,
				sentConfig,
				sentBox,
				uid,
				uidValidity,
				matchCount,
			)
		}
		return result, mirrorOutcomeUnknownError(attempt)
	}
	attempt.SentStoreObserved = true
	attempt.Transport.MirrorMailbox = appendEvidence.Mailbox
	attempt.Transport.MirrorUIDValidity = appendEvidence.UIDValidity
	attempt.Transport.MirrorUID = appendEvidence.UID
	attempt.Transport.MirrorAppended = appendEvidence.Appended
	attempt.Transport.MirrorOutcomeUnknown = false
	attempt.Outcome = SendOutcomeSent
	attempt.UpdatedAt = time.Now().UTC()
	result = resultForReconcile(ref, attempt)
	if err := replaceSendAttempt(root, ref, attempt); err != nil {
		return result, &OperationError{
			Code:    "send_reconcile_state_failed",
			Message: fmt.Sprintf("the sent message was mirrored, but the reconciled state could not be recorded: %v", err),
		}
	}
	return finishObservedSend(lease, root, ref, attempt, true)
}

func persistMirrorAttemptBeforeDispatch(root string, ref string, attempt *SendAttempt) error {
	if attempt == nil || attempt.Transport == nil {
		return fmt.Errorf("send attempt has no transport evidence")
	}
	mirrorAttemptID, err := newMirrorAttemptID()
	if err != nil {
		return err
	}
	attempt.Transport.MirrorAttemptID = mirrorAttemptID
	attempt.Transport.MirrorAttempted = true
	attempt.Transport.MirrorOutcomeUnknown = true
	attempt.UpdatedAt = time.Now().UTC()
	return replaceSendAttempt(root, ref, *attempt)
}

func mirrorOutcomeUnknown(err error) bool {
	code := transport.ErrorCode(err)
	return code == transport.CodeIMAPAppendOutcomeUnknown || code == transport.CodeIMAPAmbiguousMessageID
}

// envelopeFingerprint identifies the exact claimed envelope: Message-ID,
// sender, recipients, subject, and body. Draft edits after the claim (or a
// mismatched claim) are detected before reconciliation trusts the claim.
// The fingerprint hashes the immutable draft fields, deliberately NOT the
// composed message bytes: message composition generates fresh random multipart
// boundaries per invocation, so a byte hash would never reproduce at reconcile
// time and would falsely reject legitimate multipart sends.
