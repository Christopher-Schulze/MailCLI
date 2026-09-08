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
	if errors.Is(remote, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		state = mail.HydrationStateCanceled
	}
	local := hydrationCause(incompleteMessageCause(message), false)
	return &mail.HydrationDiagnostic{
		State:           state,
		AttemptedSource: "imap",
		Local:           local,
		Remote:          hydrationCause(remote, true),
		Remediation:     hydrationRemediation(state, hydrationErrorCode(remote)),
	}
}

func typedHydrationFailure(err error) error {
	if err == nil || nestedErrorCode(err) != "" {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return operationErrorWithCause(operationCanceledCode, "IMAP hydration was canceled", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return operationErrorWithCause(operationTimeoutCode, "IMAP hydration timed out", err)
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
	case transport.CodeIMAPTimeout, operationTimeoutCode:
		return "IMAP hydration timed out"
	case operationCanceledCode:
		return "IMAP hydration was canceled"
	case transport.CodeIMAPRawSourceTooLarge:
		return "the IMAP message exceeds the 64 MiB hydration limit"
	case transport.CodeIMAPResponseMalformed:
		return "IMAP returned a malformed response"
	case transport.CodeIMAPMailboxNotFound:
		return "the IMAP mailbox is no longer available"
	case transport.CodeIMAPMessageUIDUnknown, transport.CodeIMAPAmbiguousMessageID:
		return "IMAP could not verify the message identity"
	default:
		return "IMAP hydration failed"
	}
}

func hydrationRemediation(state mail.HydrationState, code string) string {
	if state == mail.HydrationStateCanceled {
		return "retry `mailcli messages get --ref REF` when the IMAP operation can finish"
	}
	switch code {
	case transport.CodeIMAPAuthFailed, transport.CodeSMTPCredentialsMissing:
		return "restore the credential with `mailcli send setup --from ADDRESS`, then retry `mailcli messages get --ref REF`"
	case transport.CodeIMAPConnectFailed, transport.CodeIMAPTimeout, operationTimeoutCode:
		return "restore IMAP connectivity, then retry `mailcli messages get --ref REF`"
	case transport.CodeIMAPRawSourceTooLarge:
		return "inspect the message in Mail.app; MailCLI will not bypass the 64 MiB hydration limit"
	default:
		return "resolve the IMAP failure, then retry `mailcli messages get --ref REF`"
	}
}
