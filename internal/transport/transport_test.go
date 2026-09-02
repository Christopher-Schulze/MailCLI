package transport

import (
	"fmt"
	"testing"
)

func TestProviderHostsGmail(t *testing.T) {
	tests := []struct {
		domain string
		smtpH  string
		smtpP  int
		imapH  string
		imapP  int
	}{
		{"gmail.com", "smtp.gmail.com", 587, "imap.gmail.com", 993},
		{"googlemail.com", "smtp.gmail.com", 587, "imap.gmail.com", 993},
		{"icloud.com", "smtp.mail.me.com", 587, "imap.mail.me.com", 993},
		{"me.com", "smtp.mail.me.com", 587, "imap.mail.me.com", 993},
		{"mac.com", "smtp.mail.me.com", 587, "imap.mail.me.com", 993},
	}
	for _, tt := range tests {
		t.Run(tt.domain, func(t *testing.T) {
			email := "user@" + tt.domain
			smtpH, smtpP, imapH, imapP, err := ProviderHosts(email)
			if err != nil {
				t.Fatalf("ProviderHosts(%q) error = %v", email, err)
			}
			if smtpH != tt.smtpH {
				t.Errorf("smtpHost = %q, want %q", smtpH, tt.smtpH)
			}
			if smtpP != tt.smtpP {
				t.Errorf("smtpPort = %d, want %d", smtpP, tt.smtpP)
			}
			if imapH != tt.imapH {
				t.Errorf("imapHost = %q, want %q", imapH, tt.imapH)
			}
			if imapP != tt.imapP {
				t.Errorf("imapPort = %d, want %d", imapP, tt.imapP)
			}
		})
	}
}

func TestProviderHostsCaseInsensitive(t *testing.T) {
	smtpH, _, imapH, _, err := ProviderHosts("User@GMAIL.COM")
	if err != nil {
		t.Fatalf("ProviderHosts error = %v", err)
	}
	if smtpH != "smtp.gmail.com" {
		t.Errorf("smtpHost = %q, want smtp.gmail.com", smtpH)
	}
	if imapH != "imap.gmail.com" {
		t.Errorf("imapHost = %q, want imap.gmail.com", imapH)
	}
}

func TestProviderHostsUnsupportedProvider(t *testing.T) {
	_, _, _, _, err := ProviderHosts("user@yahoo.com")
	if err == nil {
		t.Fatal("ProviderHosts error = nil, want unsupported provider error")
	}
	te, ok := err.(*TransportError)
	if !ok {
		t.Fatalf("error type = %T, want *TransportError", err)
	}
	if te.Code != CodeUnsupportedProvider {
		t.Errorf("error code = %q, want %q", te.Code, CodeUnsupportedProvider)
	}
}

func TestProviderHostsInvalidAddress(t *testing.T) {
	tests := []string{
		"no-at-sign",
		"@nodomain.com",
		"user@",
		"",
	}
	for _, email := range tests {
		t.Run(email, func(t *testing.T) {
			_, _, _, _, err := ProviderHosts(email)
			if err == nil {
				t.Fatalf("ProviderHosts(%q) error = nil, want invalid address error", email)
			}
			te, ok := err.(*TransportError)
			if !ok {
				t.Fatalf("error type = %T, want *TransportError", err)
			}
			if te.Code != CodeInvalidAddress {
				t.Errorf("error code = %q, want %q", te.Code, CodeInvalidAddress)
			}
		})
	}
}

func TestTransportErrorError(t *testing.T) {
	tests := []struct {
		name string
		err  *TransportError
		want string
	}{
		{"with message", &TransportError{Code: "test_code", Message: "something failed"}, "test_code: something failed"},
		{"empty message", &TransportError{Code: "test_code", Message: ""}, "test_code: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.err.Error()
			if got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTransportErrorCode(t *testing.T) {
	te := &TransportError{Code: CodeSMTPAuthFailed, Message: "auth failed"}
	if got := te.ErrorCode(); got != CodeSMTPAuthFailed {
		t.Errorf("ErrorCode() = %q, want %q", got, CodeSMTPAuthFailed)
	}
}

func TestTransportErrorUnwrap(t *testing.T) {
	inner := &TransportError{Code: CodeSMTPTimeout, Message: "inner"}
	outer := &TransportError{Code: CodeSMTPRejected, Message: "outer", Err: inner}
	if unwrapped := outer.Unwrap(); unwrapped != inner {
		t.Errorf("Unwrap() = %p, want %p", unwrapped, inner)
	}
}

func TestErrorCodeFreeFunction(t *testing.T) {
	te := &TransportError{Code: CodeSMTPAuthFailed, Message: "auth"}
	if got := ErrorCode(te); got != CodeSMTPAuthFailed {
		t.Errorf("ErrorCode() = %q, want %q", got, CodeSMTPAuthFailed)
	}
	if got := ErrorCode(nil); got != "" {
		t.Errorf("ErrorCode(nil) = %q, want empty", got)
	}
	if got := ErrorCode(fmt.Errorf("plain error")); got != "" {
		t.Errorf("ErrorCode(plain) = %q, want empty", got)
	}
}
