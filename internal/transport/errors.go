package transport

import (
	"errors"
	"fmt"
)

// TransportError is the typed error for all send-transport failures.
type TransportError struct {
	Code    string
	Message string
	Err     error
}

// SubmissionError marks a failure after SMTP accepted the DATA command. The
// server may already have stored the message, so callers must not retry it
// automatically.
type SubmissionError struct {
	Stage string
	Err   error
}

// MutationOutcomeError carries the evidence collected before a mutation's
// final result became unavailable. Callers must reconcile the evidence before
// replaying an operation with an unknown outcome.
type MutationOutcomeError struct {
	Code     string
	Message  string
	Evidence MutationEvidence
	Err      error
}

func (e *MutationOutcomeError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
}

func (e *MutationOutcomeError) Unwrap() error { return e.Err }

func (e *MutationOutcomeError) ErrorCode() string { return e.Code }

func (e *MutationOutcomeError) Is(target error) bool {
	code, ok := target.(transportCode)
	return ok && e.Code == string(code)
}

func (e *SubmissionError) Error() string {
	if e.Err == nil {
		return CodeSMTPSubmissionUnknown + ": SMTP submission outcome is unknown during " + e.Stage
	}
	return fmt.Sprintf("%s: SMTP submission outcome is unknown during %s: %v", CodeSMTPSubmissionUnknown, e.Stage, e.Err)
}

func (e *SubmissionError) Unwrap() error { return e.Err }

func (e *SubmissionError) ErrorCode() string { return CodeSMTPSubmissionUnknown }

func (e *SubmissionError) Is(target error) bool {
	code, ok := target.(transportCode)
	return ok && code == transportCode(CodeSMTPSubmissionUnknown)
}

func (e *TransportError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *TransportError) Unwrap() error { return e.Err }

func (e *TransportError) ErrorCode() string { return e.Code }

func (e *TransportError) Is(target error) bool {
	code, ok := target.(transportCode)
	return ok && e.Code == string(code)
}

// ErrorCode resolves typed codes through the standard wrapped or joined error
// tree. An outcome-uncertain transport code takes precedence over ordinary or
// definitive failures so cleanup diagnostics cannot make an external write
// appear safe to replay. When no uncertainty is present, the first code in
// the standard error traversal is retained for compatibility.
func ErrorCode(err error) string {
	type coder interface{ ErrorCode() string }
	var coded coder
	firstCode := ""
	if errors.As(err, &coded) {
		firstCode = coded.ErrorCode()
	}
	for _, code := range [...]string{
		CodeSMTPSubmissionUnknown,
		CodeIMAPAppendOutcomeUnknown,
		CodeIMAPMoveOutcomeUnknown,
		CodeIMAPAmbiguousMessageID,
		CodeIMAPCopyOutcomeUnknown,
	} {
		if errors.Is(err, transportCode(code)) {
			return code
		}
	}
	return firstCode
}

type transportCode string

func (transportCode) Error() string { return "" }

// Typed error codes for the send transport.
const (
	CodeInvalidAddress           = "invalid_address"
	CodeUnsupportedProvider      = "transport_unsupported_provider"
	CodeSMTPAuthFailed           = "smtp_auth_failed"
	CodeSMTPTLSFailed            = "smtp_tls_failed"
	CodeSMTPRejected             = "smtp_rejected"
	CodeSMTPTimeout              = "smtp_timeout"
	CodeSMTPTransferTimeout      = "smtp_transfer_timeout"
	CodeSMTPSubmissionUnknown    = "smtp_submission_unknown"
	CodeIMAPConnectFailed        = "imap_connect_failed"
	CodeSMTPCredentialsMissing   = "smtp_credentials_missing"
	CodeIMAPAuthFailed           = "imap_auth_failed"
	CodeIMAPSentMailboxNotFound  = "imap_sent_mailbox_not_found"
	CodeIMAPAppendFailed         = "imap_append_failed"
	CodeIMAPAppendOutcomeUnknown = "imap_append_outcome_unknown"
	CodeIMAPTimeout              = "imap_timeout"
	CodeIMAPMailboxNotFound      = "imap_mailbox_not_found"
	CodeIMAPMessageNotFound      = "imap_message_not_found"
	CodeIMAPAmbiguousMailbox     = "imap_ambiguous_mailbox"
	CodeIMAPMutationFailed       = "imap_mutation_failed"
	CodeIMAPFetchFailed          = "imap_fetch_failed"
	CodeIMAPResponseMalformed    = "imap_response_malformed"
	CodeIMAPInvalidValue         = "invalid_imap_value"
	CodeIMAPMessageUIDMismatch   = "imap_message_uid_mismatch"
	CodeIMAPAmbiguousMessageID   = "imap_ambiguous_message_id"
	CodeIMAPMoveOutcomeUnknown   = "imap_move_outcome_unknown"
	CodeIMAPCopyOutcomeUnknown   = "imap_copy_outcome_unknown"
	CodeIMAPMessageUIDUnknown    = "imap_message_uid_unknown"
	CodeIMAPUIDValidityUnknown   = "mailbox_uidvalidity_unknown"
	CodeIMAPRawSourceTooLarge    = "raw_source_too_large"
	CodeLocalOnlyMailbox         = "local_only_mailbox"
	CodeMessageAlreadyTrashed    = "message_already_trashed"
)
