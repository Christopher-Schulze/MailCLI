package transport

import "fmt"

// TransportError is the typed error for all send-transport failures.
type TransportError struct {
	Code    string
	Message string
	Err     error
}

func (e *TransportError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *TransportError) Unwrap() error { return e.Err }

func (e *TransportError) ErrorCode() string { return e.Code }

// ErrorCode returns the typed code of a transport error, or "" for other errors.
func ErrorCode(err error) string {
	type coder interface{ ErrorCode() string }
	if c, ok := err.(coder); ok {
		return c.ErrorCode()
	}
	return ""
}

// Typed error codes for the send transport.
const (
	CodeInvalidAddress          = "invalid_address"
	CodeUnsupportedProvider     = "transport_unsupported_provider"
	CodeSMTPAuthFailed          = "smtp_auth_failed"
	CodeSMTPTLSFailed           = "smtp_tls_failed"
	CodeSMTPRejected            = "smtp_rejected"
	CodeSMTPTimeout             = "smtp_timeout"
	CodeIMAPConnectFailed       = "imap_connect_failed"
	CodeSMTPCredentialsMissing  = "smtp_credentials_missing"
	CodeIMAPAuthFailed          = "imap_auth_failed"
	CodeIMAPSentMailboxNotFound = "imap_sent_mailbox_not_found"
	CodeIMAPAppendFailed        = "imap_append_failed"
	CodeIMAPTimeout             = "imap_timeout"
)
