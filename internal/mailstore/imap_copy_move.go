package mailstore

import (
	"context"
	"errors"
	"fmt"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

// TransferMessage moves or copies a message to another mailbox over IMAP.
func (c *Client) TransferMessage(ctx context.Context, request mail.TransferMessageRequest) (mail.MessageSummary, error) {
	if c.store == nil {
		return mail.MessageSummary{}, c.safeWriteUnavailableError()
	}
	if !request.Copy {
		if err := c.rejectUnconfirmedDraftMutation(ctx, request.Ref, request.AllowDraftMutation); err != nil {
			return mail.MessageSummary{}, err
		}
	}

	target, err := c.resolveImapTargetForMutation(ctx, request.Ref)
	if err != nil {
		return mail.MessageSummary{}, err
	}

	dstRef, err := mailref.DecodeMailbox(request.DestinationMailbox)
	if err != nil {
		return mail.MessageSummary{}, operationError("invalid_reference", "invalid destination mailbox ref: "+err.Error())
	}
	if dstRef.AccountID != target.accountID {
		return mail.MessageSummary{}, &transport.TransportError{
			Code:    transport.CodeIMAPMutationFailed,
			Message: "cross-account IMAP moves/copies are not supported directly",
		}
	}

	imapOp := c.send.ImapClient()
	if imapOp == nil {
		return mail.MessageSummary{}, &transport.TransportError{
			Code:    transport.CodeIMAPMutationFailed,
			Message: "IMAP operator is not configured",
		}
	}

	boxes, err := c.getOrLoadMailboxes(ctx, imapOp, target.cfg, target.cfg.Username)
	if err != nil {
		return mail.MessageSummary{}, err
	}
	dstImapBox, err := mapPathToIMAP(boxes, dstRef.Path)
	if err != nil {
		return mail.MessageSummary{}, err
	}

	copyKey := ""
	if request.Copy {
		copyKey = copyAttemptKey(target, dstImapBox)
		if priorEvidence, exists := c.copyAttempt(copyKey); exists {
			observation, observeErr := observeCopyDestination(
				ctx, imapOp, target.cfg, dstImapBox, target.messageID,
			)
			if observeErr != nil {
				return mail.MessageSummary{}, copyOutcomeUnknownError(
					priorEvidence,
					fmt.Sprintf(
						"COPY operation %s remains unresolved; destination reconciliation failed",
						priorEvidence.OperationID,
					),
					nil,
					observeErr,
				)
			}
			if observation.matchCount > 0 {
				if priorEvidence.DestinationUIDValidity != 0 &&
					priorEvidence.DestinationUIDValidity != observation.uidvalidity {
					return mail.MessageSummary{}, copyOutcomeUnknownError(
						priorEvidence,
						fmt.Sprintf(
							"COPY operation %s cannot adopt destination identity after UIDVALIDITY rollover (%d -> %d)",
							priorEvidence.OperationID, priorEvidence.DestinationUIDValidity, observation.uidvalidity,
						),
						nil,
						nil,
					)
				}
				priorEvidence = evidenceWithCopyDestination(priorEvidence, observation)
			}
			return mail.MessageSummary{}, copyOutcomeUnknownError(
				priorEvidence,
				fmt.Sprintf(
					"COPY operation %s remains unresolved; reconcile mailbox %s before retrying",
					priorEvidence.OperationID, dstImapBox,
				),
				nil,
				nil,
			)
		}
		if _, err := verifyCopyDestinationBeforeDispatch(
			ctx, imapOp, target.cfg, dstImapBox, target.messageID, copyAttemptEvidence(target, dstImapBox),
		); err != nil {
			return mail.MessageSummary{}, err
		}
		c.rememberCopyAttempt(copyKey, copyAttemptEvidence(target, dstImapBox))
	}

	if !request.Copy {
		if err := verifyMoveDestination(
			ctx, imapOp, target.cfg, dstImapBox, target.messageID, nil,
		); err != nil {
			return mail.MessageSummary{}, err
		}
	}

	var ev transport.MutationEvidence
	if request.Copy {
		ev, err = imapOp.CopyMessage(ctx, target.cfg, target.imapMailbox, target.uid, target.uidvalidity, dstImapBox)
	} else {
		ev, err = imapOp.MoveMessage(ctx, target.cfg, target.imapMailbox, target.uid, target.uidvalidity, dstImapBox)
	}
	if isUIDValidityChangedError(err) {
		if request.Copy {
			c.forgetCopyAttempt(copyKey)
		}
		retried, retryErr := c.resolveImapTargetForMutation(ctx, request.Ref)
		if retryErr != nil {
			return mail.MessageSummary{}, retryErr
		}
		if request.Copy {
			copyKey = copyAttemptKey(retried, dstImapBox)
			if _, err := verifyCopyDestinationBeforeDispatch(
				ctx, imapOp, retried.cfg, dstImapBox, retried.messageID, copyAttemptEvidence(retried, dstImapBox),
			); err != nil {
				return mail.MessageSummary{}, err
			}
			c.rememberCopyAttempt(copyKey, copyAttemptEvidence(retried, dstImapBox))
			ev, err = imapOp.CopyMessage(ctx, retried.cfg, retried.imapMailbox, retried.uid, retried.uidvalidity, dstImapBox)
		} else {
			ev, err = imapOp.MoveMessage(ctx, retried.cfg, retried.imapMailbox, retried.uid, retried.uidvalidity, dstImapBox)
		}
		target = retried
	}
	if err != nil {
		if request.Copy {
			ev = mergeCopyEvidence(copyAttemptEvidence(target, dstImapBox), ev)
			if ev.Outcome == transport.MutationOutcomeRejected || ev.Outcome == transport.MutationOutcomeNotStarted {
				c.forgetCopyAttempt(copyKey)
				return mail.MessageSummary{}, err
			}
			if ev.Outcome == "" {
				ev.Outcome = transport.MutationOutcomeUnknown
			}
			observedEvidence, outcomeErr := verifyCopyDestinationAfterDispatch(
				ctx, imapOp, target.cfg, dstImapBox, target.messageID, ev, err,
			)
			if outcomeErr != nil {
				c.rememberCopyAttempt(copyKey, outcomeEvidenceFromError(outcomeErr, observedEvidence))
				return mail.MessageSummary{}, outcomeErr
			}
			observedEvidence.Outcome = transport.MutationOutcomeUnknown
			c.rememberCopyAttempt(copyKey, observedEvidence)
			return mail.MessageSummary{}, copyOutcomeUnknownError(
				observedEvidence,
				fmt.Sprintf(
					"COPY operation %s returned an incomplete result; reconcile the destination before retrying",
					observedEvidence.OperationID,
				),
				err,
				nil,
			)
		}
		if !request.Copy {
			if outcomeErr := verifyMoveDestination(
				ctx, imapOp, target.cfg, dstImapBox, target.messageID, err,
			); outcomeErr != nil {
				err = outcomeErr
			}
		}
		if ev.Command == "" {
			return mail.MessageSummary{}, err
		}
	}
	if request.Copy {
		var outcomeErr error
		ev = mergeCopyEvidence(copyAttemptEvidence(target, dstImapBox), ev)
		ev, outcomeErr = verifyCopyDestinationAfterDispatch(
			ctx, imapOp, target.cfg, dstImapBox, target.messageID, ev, nil,
		)
		if outcomeErr != nil {
			c.rememberCopyAttempt(copyKey, outcomeEvidenceFromError(outcomeErr, ev))
			return mail.MessageSummary{}, outcomeErr
		}
		c.forgetCopyAttempt(copyKey)
	}
	ev.DuplicateMatches = duplicateMatchEvidence(target.duplicateMatches)

	summary := target.summary
	if !request.Copy && err == nil {
		summary.MailboxRef = request.DestinationMailbox
	}
	summary.ServerTruth = &mail.ServerMutationEvidence{
		OperationID:            ev.OperationID,
		Outcome:                mail.ServerMutationOutcome(ev.Outcome),
		SourceAccount:          ev.SourceAccount,
		Command:                ev.Command,
		ServerResponse:         ev.ServerResponse,
		Mailbox:                ev.Mailbox,
		TargetMailbox:          ev.TargetMailbox,
		UID:                    ev.UID,
		ExpectedUIDValidity:    ev.ExpectedUIDValidity,
		UIDValidity:            ev.UIDValidity,
		DuplicateMatches:       ev.DuplicateMatches,
		ExpungeBranch:          ev.ExpungeBranch,
		ForeignDeletedCount:    ev.ForeignDeletedCount,
		DestinationUIDValidity: ev.DestinationUIDValidity,
		DestinationUID:         ev.DestinationUID,
		CopyUIDResponse:        ev.CopyUIDResponse,
		CopyUIDValidity:        ev.CopyUIDValidity,
		CopySourceUID:          ev.CopySourceUID,
		CopyDestinationUID:     ev.CopyDestinationUID,
		CompletedEffects:       append([]string(nil), ev.CompletedEffects...),
		FlagsState:             string(ev.FlagsState),
		ActualFlags:            append([]string(nil), ev.ActualFlags...),
		FlagsSource:            ev.FlagsSource,
	}
	summary.StalenessNote = stalenessExplanation
	if err != nil {
		summary.StalenessNote = "MOVE is incomplete; retained source flags and COPY effects are in server_truth; summary booleans retain local cached values"
	}
	return summary, err
}

func duplicateMatchEvidence(matchCount int) int {
	if matchCount <= 1 {
		return 0
	}
	return matchCount
}

type copyDestinationObservation struct {
	uid         uint32
	uidvalidity uint32
	matchCount  int
}

func (c *Client) copyAttempt(key string) (transport.MutationEvidence, bool) {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	evidence, ok := c.copyAttempts[key]
	return evidence, ok
}

func (c *Client) rememberCopyAttempt(key string, evidence transport.MutationEvidence) {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	if c.copyAttempts == nil {
		c.copyAttempts = make(map[string]transport.MutationEvidence)
	}
	c.copyAttempts[key] = evidence
}

func (c *Client) forgetCopyAttempt(key string) {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	delete(c.copyAttempts, key)
}

func copyAttemptKey(target imapTarget, destination string) string {
	return transport.MutationOperationID(
		"COPY", target.cfg.Username, target.imapMailbox, target.uid, target.uidvalidity, destination,
	)
}

func copyAttemptEvidence(target imapTarget, destination string) transport.MutationEvidence {
	return transport.MutationEvidence{
		OperationID:         copyAttemptKey(target, destination),
		Outcome:             transport.MutationOutcomeAttempted,
		SourceAccount:       target.cfg.Username,
		Command:             "COPY",
		Mailbox:             target.imapMailbox,
		TargetMailbox:       destination,
		UID:                 target.uid,
		UIDValidity:         target.uidvalidity,
		ExpectedUIDValidity: target.uidvalidity,
	}
}

func observeCopyDestination(
	ctx context.Context,
	imapOp transport.ImapOperator,
	cfg transport.ImapConfig,
	dstMailbox string,
	messageID string,
) (copyDestinationObservation, error) {
	if messageID == "" {
		return copyDestinationObservation{}, &transport.TransportError{
			Code:    transport.CodeIMAPCopyOutcomeUnknown,
			Message: "cannot reconcile COPY because the source Message-ID is unavailable",
		}
	}
	uid, uidvalidity, matchCount, err := imapOp.SearchUID(ctx, cfg, dstMailbox, messageID)
	if err != nil {
		if transport.ErrorCode(err) == transport.CodeIMAPMessageNotFound {
			return copyDestinationObservation{}, nil
		}
		return copyDestinationObservation{}, err
	}
	if matchCount == 0 {
		return copyDestinationObservation{}, nil
	}
	if uid == 0 || uidvalidity == 0 {
		return copyDestinationObservation{}, &transport.TransportError{
			Code: transport.CodeIMAPCopyOutcomeUnknown,
			Message: fmt.Sprintf(
				"destination %s returned an unusable COPY identity (UID %d, UIDVALIDITY %d)",
				dstMailbox, uid, uidvalidity,
			),
		}
	}
	return copyDestinationObservation{uid: uid, uidvalidity: uidvalidity, matchCount: matchCount}, nil
}

func evidenceWithCopyDestination(
	evidence transport.MutationEvidence,
	observation copyDestinationObservation,
) transport.MutationEvidence {
	evidence.DestinationUID = observation.uid
	evidence.DestinationUIDValidity = observation.uidvalidity
	return evidence
}

func mergeCopyEvidence(base, actual transport.MutationEvidence) transport.MutationEvidence {
	if actual.OperationID == "" {
		actual.OperationID = base.OperationID
	}
	if actual.Outcome == "" {
		actual.Outcome = base.Outcome
	}
	if actual.SourceAccount == "" {
		actual.SourceAccount = base.SourceAccount
	}
	if actual.Command == "" {
		actual.Command = base.Command
	}
	if actual.Mailbox == "" {
		actual.Mailbox = base.Mailbox
	}
	if actual.TargetMailbox == "" {
		actual.TargetMailbox = base.TargetMailbox
	}
	if actual.UID == 0 {
		actual.UID = base.UID
	}
	if actual.UIDValidity == 0 {
		actual.UIDValidity = base.UIDValidity
	}
	if actual.ExpectedUIDValidity == 0 {
		actual.ExpectedUIDValidity = base.ExpectedUIDValidity
	}
	return actual
}

func copyOutcomeUnknownError(
	evidence transport.MutationEvidence,
	message string,
	previousErr error,
	probeErr error,
) error {
	evidence.Outcome = transport.MutationOutcomeUnknown
	return &transport.MutationOutcomeError{
		Code:     transport.CodeIMAPCopyOutcomeUnknown,
		Message:  message,
		Evidence: evidence,
		Err:      errors.Join(previousErr, probeErr),
	}
}

func outcomeEvidenceFromError(err error, fallback transport.MutationEvidence) transport.MutationEvidence {
	var outcomeErr *transport.MutationOutcomeError
	if errors.As(err, &outcomeErr) {
		return outcomeErr.Evidence
	}
	return fallback
}

func verifyCopyDestinationBeforeDispatch(
	ctx context.Context,
	imapOp transport.ImapOperator,
	cfg transport.ImapConfig,
	dstMailbox string,
	messageID string,
	evidence transport.MutationEvidence,
) (copyDestinationObservation, error) {
	observation, err := observeCopyDestination(ctx, imapOp, cfg, dstMailbox, messageID)
	if err != nil {
		return observation, copyOutcomeUnknownError(
			evidence,
			fmt.Sprintf("cannot safely start COPY for message ID %s to mailbox %s; destination observation failed", messageID, dstMailbox),
			nil,
			err,
		)
	}
	if observation.matchCount == 0 {
		return observation, nil
	}
	observationEvidence := evidenceWithCopyDestination(evidence, observation)
	return observation, copyOutcomeUnknownError(
		observationEvidence,
		fmt.Sprintf(
			"cannot safely start or replay COPY for message ID %s to mailbox %s; destination contains %d matching message(s)",
			messageID, dstMailbox, observation.matchCount,
		),
		nil,
		nil,
	)
}

func verifyCopyDestinationAfterDispatch(
	ctx context.Context,
	imapOp transport.ImapOperator,
	cfg transport.ImapConfig,
	dstMailbox string,
	messageID string,
	evidence transport.MutationEvidence,
	previousErr error,
) (transport.MutationEvidence, error) {
	observation, err := observeCopyDestination(ctx, imapOp, cfg, dstMailbox, messageID)
	if err != nil {
		return evidence, copyOutcomeUnknownError(
			evidence,
			fmt.Sprintf(
				"COPY outcome for message ID %s to mailbox %s is unknown; destination observation failed",
				messageID, dstMailbox,
			),
			previousErr,
			err,
		)
	}
	if observation.matchCount == 0 {
		return evidence, copyOutcomeUnknownError(
			evidence,
			fmt.Sprintf(
				"COPY outcome for message ID %s to mailbox %s is unknown; destination contains no matching message",
				messageID, dstMailbox,
			),
			previousErr,
			nil,
		)
	}
	evidence = evidenceWithCopyDestination(evidence, observation)
	if observation.matchCount > 1 {
		return evidence, copyOutcomeUnknownError(
			evidence,
			fmt.Sprintf(
				"COPY outcome for message ID %s to mailbox %s is ambiguous; destination contains %d matching messages",
				messageID, dstMailbox, observation.matchCount,
			),
			previousErr,
			nil,
		)
	}
	if evidence.CopyUIDValidity != 0 && evidence.CopyUIDValidity != observation.uidvalidity {
		return evidence, copyOutcomeUnknownError(
			evidence,
			fmt.Sprintf(
				"COPY destination UIDVALIDITY changed from %d to %d while reconciling message ID %s",
				evidence.CopyUIDValidity, observation.uidvalidity, messageID,
			),
			previousErr,
			nil,
		)
	}
	if evidence.CopyDestinationUID != 0 && evidence.CopyDestinationUID != observation.uid {
		return evidence, copyOutcomeUnknownError(
			evidence,
			fmt.Sprintf(
				"COPY destination UID changed from %d to %d while reconciling message ID %s",
				evidence.CopyDestinationUID, observation.uid, messageID,
			),
			previousErr,
			nil,
		)
	}
	return evidence, nil
}

func validateResolvedUID(uid uint32, messageID, mailbox string) error {
	if uid != 0 {
		return nil
	}
	return &transport.TransportError{
		Code: transport.CodeIMAPMessageUIDUnknown,
		Message: fmt.Sprintf(
			"IMAP Message-ID search for %s in %s returned UID zero; refusing the operation",
			messageID, mailbox,
		),
	}
}

func verifyMoveDestination(
	ctx context.Context,
	imapOp transport.ImapOperator,
	cfg transport.ImapConfig,
	dstMailbox string,
	messageID string,
	previousErr error,
) error {
	if messageID == "" {
		return &transport.TransportError{
			Code:    transport.CodeIMAPMoveOutcomeUnknown,
			Message: "cannot safely execute or replay MOVE because the message identity is unavailable",
			Err:     previousErr,
		}
	}
	uid, uidvalidity, matchCount, err := imapOp.SearchUID(ctx, cfg, dstMailbox, messageID)
	if err != nil {
		if transport.ErrorCode(err) == transport.CodeIMAPMessageNotFound {
			if previousErr != nil {
				return moveOutcomeUnknownError(dstMailbox, messageID, 0, 0, 0, previousErr, nil)
			}
			return nil
		}
		return moveOutcomeUnknownError(dstMailbox, messageID, 0, 0, 0, previousErr, err)
	}
	return moveOutcomeUnknownError(
		dstMailbox, messageID, uid, uidvalidity, matchCount, previousErr, nil,
	)
}

func moveOutcomeUnknownError(
	dstMailbox string,
	messageID string,
	uid uint32,
	uidvalidity uint32,
	matchCount int,
	previousErr error,
	probeErr error,
) error {
	message := fmt.Sprintf(
		"cannot safely replay MOVE for message ID %s to mailbox %s",
		messageID, dstMailbox,
	)
	if matchCount > 0 {
		message = fmt.Sprintf(
			"%s; destination contains %d matching message(s) at UID %d with UIDVALIDITY %d",
			message, matchCount, uid, uidvalidity,
		)
	} else if probeErr != nil {
		message += "; destination verification failed"
	}
	message += "; reconcile the source and destination before retrying"
	return &transport.TransportError{
		Code:    transport.CodeIMAPMoveOutcomeUnknown,
		Message: message,
		Err:     errors.Join(previousErr, probeErr),
	}
}
