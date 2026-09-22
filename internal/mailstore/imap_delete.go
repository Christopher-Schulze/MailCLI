package mailstore

import (
	"context"
	"strings"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

func rejectAlreadyTrashed(target imapTarget) error {
	if target.trashMailbox == "" || !strings.EqualFold(target.imapMailbox, target.trashMailbox) {
		return nil
	}
	return &transport.TransportError{
		Code:    transport.CodeMessageAlreadyTrashed,
		Message: "message is already in trash; restore it in Mail or use Mail.app to empty the trash",
	}
}

// DeleteMessage deletes a message over IMAP by moving it to the Trash mailbox.
func (c *Client) DeleteMessage(ctx context.Context, request mail.DeleteMessageRequest) (mail.DeleteResult, error) {
	if c.store == nil {
		return mail.DeleteResult{}, c.safeWriteUnavailableError()
	}
	if err := c.rejectUnconfirmedDraftMutation(ctx, request.Ref, request.AllowDraftMutation); err != nil {
		return mail.DeleteResult{}, err
	}

	target, err := c.resolveImapTargetForDelete(ctx, request.Ref)
	if err != nil {
		return mail.DeleteResult{}, err
	}

	imapOp := c.send.ImapClient()
	if imapOp == nil {
		return mail.DeleteResult{}, &transport.TransportError{
			Code:    transport.CodeIMAPMutationFailed,
			Message: "IMAP operator is not configured",
		}
	}

	ev, err := imapOp.DeleteMessage(ctx, target.cfg, target.imapMailbox, target.uid, target.uidvalidity)
	if isUIDValidityChangedError(err) {
		retried, retryErr := c.resolveImapTargetForDelete(ctx, request.Ref)
		if retryErr != nil {
			return mail.DeleteResult{}, retryErr
		}
		if retryErr := rejectAlreadyTrashed(retried); retryErr != nil {
			return mail.DeleteResult{}, retryErr
		}
		ev, err = imapOp.DeleteMessage(ctx, retried.cfg, retried.imapMailbox, retried.uid, retried.uidvalidity)
		target.duplicateMatches = retried.duplicateMatches
	}
	if err != nil && ev.Command == "" {
		return mail.DeleteResult{}, err
	}
	ev.DuplicateMatches = duplicateMatchEvidence(target.duplicateMatches)
	return mail.DeleteResult{
		MessageRef: request.Ref, Deleted: err == nil,
		ServerTruth: &mail.ServerMutationEvidence{
			OperationID: ev.OperationID, Outcome: mail.ServerMutationOutcome(ev.Outcome), SourceAccount: ev.SourceAccount,
			Command: ev.Command, ServerResponse: ev.ServerResponse,
			Mailbox: ev.Mailbox, TargetMailbox: ev.TargetMailbox, UID: ev.UID,
			ExpectedUIDValidity: ev.ExpectedUIDValidity, UIDValidity: ev.UIDValidity,
			DuplicateMatches: ev.DuplicateMatches,
			ExpungeBranch:    ev.ExpungeBranch, ForeignDeletedCount: ev.ForeignDeletedCount,
			DestinationUIDValidity: ev.DestinationUIDValidity, DestinationUID: ev.DestinationUID,
			CopyUIDResponse: ev.CopyUIDResponse, CopyUIDValidity: ev.CopyUIDValidity,
			CopySourceUID: ev.CopySourceUID, CopyDestinationUID: ev.CopyDestinationUID,
			CompletedEffects: append([]string(nil), ev.CompletedEffects...),
			FlagsState:       string(ev.FlagsState),
			ActualFlags:      append([]string(nil), ev.ActualFlags...),
			FlagsSource:      ev.FlagsSource,
		},
	}, err
}
