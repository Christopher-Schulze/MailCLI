package mailstore

import (
	"context"
	"errors"
	"strings"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

const (
	localContentIncompleteCode = "content_incomplete"
	operationCanceledCode      = "operation_canceled"
	operationTimeoutCode       = "operation_timeout"
)

func incompleteMessageCause(message mail.Message) error {
	code := localContentIncompleteCode
	detail := "local message content is incomplete"
	if message.ContentSource == "emlx_partial" {
		code = "raw_source_partial"
		detail = "local EMLX source is partial"
	}
	if len(message.MissingParts) > 0 {
		detail += "; missing parts: " + strings.Join(message.MissingParts, ", ")
	}
	return operationError(code, detail)
}

func messageHydrationDiagnostic(
	ctx context.Context,
	message mail.Message,
	remote error,
) *mail.HydrationDiagnostic {
	state := mail.HydrationStateFailed
	if transport.ErrorCode(remote) == transport.CodeIMAPCanceled ||
		errors.Is(remote, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		state = mail.HydrationStateCanceled
	}
	local := hydrationCause(incompleteMessageCause(message), false)
	remediation := hydrationRemediation(state, hydrationErrorCode(remote))
	if transport.IsTLSVerificationFailure(remote) {
		remediation = "correct the IMAP certificate trust or configured hostname before retrying; do not disable TLS verification"
	}
	return &mail.HydrationDiagnostic{
		State:           state,
		AttemptedSource: "imap",
		Local:           local,
		Remote:          hydrationCause(remote, true),
		Remediation:     remediation,
	}
}

func typedHydrationFailure(err error) error {
	if err == nil {
		return err
	}
	if transport.IsTLSVerificationFailure(err) {
		return operationErrorWithCause(transport.ErrorCode(err), "IMAP TLS certificate verification failed", err)
	}
	code := nestedErrorCode(err)
	if code == operationCanceledCode || code == operationTimeoutCode {
		return err
	}
	if code == transport.CodeIMAPCanceled || code == transport.CodeIMAPDisconnected {
		return operationErrorWithCause(code, safeRemoteHydrationMessage(code), err)
	}
	if errors.Is(err, context.Canceled) {
		return operationErrorWithCause(operationCanceledCode, "IMAP hydration was canceled; no external mutation was attempted", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return operationErrorWithCause(operationTimeoutCode, "IMAP hydration timed out; no external mutation was attempted", err)
	}
	if code := nestedErrorCode(err); code != "" {
		if code == transport.CodeIMAPTimeout {
			return operationErrorWithCause(code, "IMAP hydration timed out; no external mutation was attempted", err)
		}
		return err
	}
	return err
}

func hydrationCause(err error, remote bool) *mail.HydrationCause {
	if err == nil {
		return nil
	}
	code := hydrationErrorCode(err)
	message := ""
	if remote {
		message = safeRemoteHydrationMessage(code)
		if transport.IsTLSVerificationFailure(err) {
			message = "IMAP TLS certificate verification failed"
		}
	} else {
		var typed *Error
		if errors.As(err, &typed) {
			message = typed.Message
		} else {
			message = "local message content is incomplete"
		}
	}
	return &mail.HydrationCause{Code: code, Message: message}
}

func hydrationErrorCode(err error) string {
	if code := nestedErrorCode(err); code != "" {
		return code
	}
	if errors.Is(err, context.Canceled) {
		return operationCanceledCode
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return operationTimeoutCode
	}
	return "hydration_failed"
}

func safeRemoteHydrationMessage(code string) string {
	switch code {
	case transport.CodeIMAPAuthFailed:
		return "IMAP authentication failed"
	case transport.CodeSMTPCredentialsMissing:
		return "IMAP credentials are unavailable"
	case transport.CodeIMAPConnectFailed:
		return "IMAP connection failed"
	case transport.CodeIMAPCanceled, operationCanceledCode:
		return "IMAP hydration was canceled; no external mutation was attempted"
	case transport.CodeIMAPDisconnected:
		return "IMAP connection was lost during hydration; no external mutation was attempted"
	case transport.CodeIMAPTimeout, operationTimeoutCode:
		return "IMAP hydration timed out; no external mutation was attempted"
	case transport.CodeIMAPRawSourceTooLarge:
		return "the IMAP message exceeds the 64 MiB hydration limit"
	case transport.CodeIMAPResponseMalformed:
		return "IMAP returned a malformed response"
	case transport.CodeIMAPMailboxNotFound:
		return "the IMAP mailbox is no longer available"
	case transport.CodeIMAPAmbiguousMailbox:
		return "the IMAP mailbox identity is ambiguous"
	case transport.CodeIMAPMessageNotFound:
		return "the message is no longer present in the resolved IMAP mailbox"
	case transport.CodeIMAPMessageUIDUnknown, transport.CodeIMAPUIDValidityUnknown, transport.CodeIMAPMessageUIDMismatch:
		return "IMAP could not verify the message identity"
	case "mailbox_uidvalidity_changed":
		return "the IMAP mailbox identity changed since the message reference was resolved"
	case transport.CodeIMAPAmbiguousMessageID:
		return "IMAP message identity is ambiguous"
	case transport.CodeIMAPInvalidValue:
		return "IMAP rejected an invalid message identity or mailbox value"
	case transport.CodeIMAPCommandRejected:
		return "IMAP rejected a read command during hydration"
	case transport.CodeIMAPMutationFailed:
		return "IMAP identity discovery failed"
	case transport.CodeIMAPFetchFailed:
		return "IMAP could not fetch the message"
	case transport.CodeIMAPResourceLimitExceeded:
		return "the IMAP response exceeded a bounded hydration limit"
	default:
		return "IMAP hydration failed"
	}
}

func hydrationRemediation(state mail.HydrationState, code string) string {
	if state == mail.HydrationStateCanceled || code == transport.CodeIMAPCanceled || code == operationCanceledCode {
		return ""
	}
	switch code {
	case transport.CodeIMAPAuthFailed, transport.CodeSMTPCredentialsMissing:
		return "restore the credential with `mailcli send setup --from ADDRESS`, then retry `mailcli messages get --ref REF`"
	case transport.CodeIMAPConnectFailed, transport.CodeIMAPDisconnected, transport.CodeIMAPTimeout, operationTimeoutCode:
		return "restore IMAP connectivity, then retry `mailcli messages get --ref REF`"
	case transport.CodeIMAPRawSourceTooLarge:
		return "inspect the message in Mail.app; MailCLI will not bypass the 64 MiB hydration limit"
	case transport.CodeIMAPMailboxNotFound:
		return "restore the IMAP mailbox or refresh its local mapping, then retry `mailcli messages get --ref REF`"
	case transport.CodeIMAPAmbiguousMailbox:
		return "resolve duplicate IMAP mailbox names or special-use assignments, refresh the mailbox list, then retry `mailcli messages get --ref REF`"
	case transport.CodeIMAPMessageNotFound, transport.CodeIMAPMessageUIDUnknown, transport.CodeIMAPUIDValidityUnknown,
		transport.CodeIMAPMessageUIDMismatch, transport.CodeIMAPAmbiguousMessageID, "mailbox_uidvalidity_changed":
		return "refresh the local catalog and use a message reference with verified IMAP UID and UIDVALIDITY before retrying"
	case transport.CodeIMAPInvalidValue:
		return "refresh the local catalog and retry with a valid message reference and mailbox mapping"
	case transport.CodeIMAPCommandRejected:
		return "inspect error.imap_rejection, correct the IMAP server-side cause, then retry `mailcli messages get --ref REF`"
	case transport.CodeIMAPMutationFailed:
		return "inspect IMAP operator configuration and the retained cause before retrying hydration"
	case transport.CodeIMAPFetchFailed:
		return "inspect the FETCH cause; retry only when it identifies a transient network failure"
	case transport.CodeIMAPResponseMalformed:
		return "inspect the IMAP server response and refresh the message reference before retrying"
	case transport.CodeIMAPResourceLimitExceeded:
		return "inspect the oversized IMAP mailbox or response; do not retry the same bounded response unchanged"
	default:
		return "resolve the IMAP failure, then retry `mailcli messages get --ref REF`"
	}
}
