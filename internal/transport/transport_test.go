package transport

import (
	"errors"
	"fmt"
	"strings"
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

func TestProviderHostsNormalizesDomainAliases(t *testing.T) {
	tests := []struct {
		name     string
		email    string
		wantSMTP string
		wantIMAP string
	}{
		{name: "gmail uppercase", email: "User@GMAIL.COM", wantSMTP: "smtp.gmail.com", wantIMAP: "imap.gmail.com"},
		{name: "googlemail uppercase", email: "User@GOOGLEMAIL.COM", wantSMTP: "smtp.gmail.com", wantIMAP: "imap.gmail.com"},
		{name: "icloud display name", email: "User <user@ME.COM>", wantSMTP: "smtp.mail.me.com", wantIMAP: "imap.mail.me.com"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			smtpHost, _, imapHost, _, err := ProviderHosts(test.email)
			if err != nil {
				t.Fatalf("ProviderHosts(%q) error = %v", test.email, err)
			}
			if smtpHost != test.wantSMTP || imapHost != test.wantIMAP {
				t.Fatalf("ProviderHosts(%q) = %q, %q; want %q, %q", test.email, smtpHost, imapHost, test.wantSMTP, test.wantIMAP)
			}
		})
	}
}

func TestSupportedProvidersExposeExactAliases(t *testing.T) {
	providers := SupportedProviders()
	if len(providers) != 2 {
		t.Fatalf("SupportedProviders() = %+v, want two providers", providers)
	}
	want := []ProviderSupport{
		{Name: "Gmail", Domains: []string{"gmail.com", "googlemail.com"}},
		{Name: "iCloud", Domains: []string{"icloud.com", "me.com", "mac.com"}},
	}
	for index := range want {
		if providers[index].Name != want[index].Name || fmt.Sprint(providers[index].Domains) != fmt.Sprint(want[index].Domains) {
			t.Fatalf("SupportedProviders()[%d] = %+v, want %+v", index, providers[index], want[index])
		}
	}
	providers[0].Domains[0] = "changed.example"
	if SupportedProviders()[0].Domains[0] != "gmail.com" {
		t.Fatal("SupportedProviders() returned shared domain storage")
	}
}

func TestProviderHostsUnsupportedProvider(t *testing.T) {
	_, _, _, _, err := ProviderHosts("user@YAHOO.COM")
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
	if !strings.Contains(te.Message, "domain: yahoo.com") || !strings.Contains(te.Message, ProviderSupportDescription()) {
		t.Errorf("unsupported provider message = %q", te.Message)
	}
	if _, _, _, _, err := ProviderHosts("user@mail.gmail.com"); ErrorCode(err) != CodeUnsupportedProvider {
		t.Fatalf("subdomain ProviderHosts() error = %v, want %s", err, CodeUnsupportedProvider)
	}
}

func TestProviderHostsInvalidAddress(t *testing.T) {
	tests := []string{
		"no-at-sign",
		"@nodomain.com",
		"user@",
		"user@@gmail.com",
		"user gmail.com",
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

func TestErrorCodeTraversesWrappedAndJoinedChains(t *testing.T) {
	cleanup := errors.New("close accepted spool")
	unknown := &TransportError{Code: CodeIMAPAppendOutcomeUnknown, Message: "final APPEND response lost"}
	tests := []struct {
		name string
		err  error
	}{
		{name: "direct", err: unknown},
		{name: "wrapped", err: fmt.Errorf("mirror failed: %w", unknown)},
		{name: "nested", err: fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", unknown))},
		{name: "joined unknown first", err: errors.Join(unknown, cleanup)},
		{name: "joined cleanup first", err: errors.Join(cleanup, unknown)},
		{name: "wrapped joined", err: fmt.Errorf("reconcile: %w", errors.Join(cleanup, unknown))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ErrorCode(test.err); got != CodeIMAPAppendOutcomeUnknown {
				t.Fatalf("ErrorCode() = %q, want %q: %v", got, CodeIMAPAppendOutcomeUnknown, test.err)
			}
			if !errors.Is(test.err, cleanup) && strings.Contains(test.name, "joined") {
				t.Fatalf("joined error lost cleanup cause: %v", test.err)
			}
			if !strings.Contains(test.err.Error(), "final APPEND response lost") {
				t.Fatalf("error diagnostics lost transport cause: %v", test.err)
			}
		})
	}
}

func TestErrorCodeUncertaintyWinsOverKnownTransportFailure(t *testing.T) {
	unknown := &TransportError{Code: CodeIMAPAppendOutcomeUnknown, Message: "APPEND outcome unknown"}
	rejected := &TransportError{Code: CodeSMTPRejected, Message: "SMTP rejected"}
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "unknown first", err: errors.Join(unknown, rejected)},
		{name: "rejection first", err: errors.Join(rejected, unknown)},
		{name: "known wrapper", err: &TransportError{Code: CodeIMAPAppendFailed, Message: "cleanup failed", Err: unknown}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ErrorCode(test.err); got != CodeIMAPAppendOutcomeUnknown {
				t.Fatalf("ErrorCode() = %q, want %q: %v", got, CodeIMAPAppendOutcomeUnknown, test.err)
			}
			if !strings.Contains(test.err.Error(), "APPEND outcome unknown") {
				t.Fatalf("error diagnostics lost a cause: %v", test.err)
			}
			if test.name != "known wrapper" && !strings.Contains(test.err.Error(), "SMTP rejected") {
				t.Fatalf("error diagnostics lost rejection cause: %v", test.err)
			}
		})
	}
}

func TestErrorCodeUsesStableUncertaintyPrecedence(t *testing.T) {
	smtpUnknown := &SubmissionError{Stage: "final reply"}
	appendUnknown := &TransportError{Code: CodeIMAPAppendOutcomeUnknown, Message: "APPEND outcome unknown"}
	copyUnknown := &TransportError{Code: CodeIMAPCopyOutcomeUnknown, Message: "COPY outcome unknown"}
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{name: "submission before append", err: errors.Join(smtpUnknown, appendUnknown), want: CodeSMTPSubmissionUnknown},
		{name: "append before submission", err: errors.Join(appendUnknown, smtpUnknown), want: CodeSMTPSubmissionUnknown},
		{name: "append before copy", err: errors.Join(appendUnknown, copyUnknown), want: CodeIMAPAppendOutcomeUnknown},
		{name: "copy before append", err: errors.Join(copyUnknown, appendUnknown), want: CodeIMAPAppendOutcomeUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ErrorCode(test.err); got != test.want {
				t.Fatalf("ErrorCode() = %q, want %q: %v", got, test.want, test.err)
			}
		})
	}
}

func TestErrorCodePreservesKnownCodeWithoutUncertainty(t *testing.T) {
	rejected := &TransportError{Code: CodeSMTPRejected, Message: "SMTP rejected"}
	cleanup := errors.New("close accepted spool")
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "direct", err: rejected},
		{name: "wrapped", err: fmt.Errorf("submission: %w", rejected)},
		{name: "joined", err: errors.Join(cleanup, rejected)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ErrorCode(test.err); got != CodeSMTPRejected {
				t.Fatalf("ErrorCode() = %q, want %q: %v", got, CodeSMTPRejected, test.err)
			}
		})
	}
}

func TestPickSentMailboxPrefersSpecialUseBeforeFallback(t *testing.T) {
	mailboxes := []MailboxInfo{
		{Name: "Sent Messages"},
		{Name: "Archive", Flags: []string{"\\sent"}},
		{Name: "Gesendet"},
	}
	if got := PickSentMailbox(mailboxes); got != "Archive" {
		t.Fatalf("PickSentMailbox() = %q, want special-use Archive", got)
	}
}

func TestPickSentMailboxUsesCaseInsensitiveFallback(t *testing.T) {
	if got := PickSentMailbox([]MailboxInfo{{Name: "gEsEnDeT"}}); got != "gEsEnDeT" {
		t.Fatalf("PickSentMailbox() = %q, want fallback mailbox", got)
	}
	if got := PickSentMailbox(nil); got != "" {
		t.Fatalf("PickSentMailbox(nil) = %q, want empty", got)
	}
}

func TestPickTrashMailboxPrefersSpecialUseBeforeFallback(t *testing.T) {
	mailboxes := []MailboxInfo{
		{Name: "Trash"},
		{Name: "Deleted", Flags: []string{"\\TRASH"}},
		{Name: "Papierkorb"},
	}
	if got := PickTrashMailbox(mailboxes); got != "Deleted" {
		t.Fatalf("PickTrashMailbox() = %q, want special-use Deleted", got)
	}
}

func TestPickTrashMailboxUsesLocalizedFallbacks(t *testing.T) {
	for _, name := range []string{"trash", "Deleted Messages", "[Gmail]/Papierkorb", "INBOX.Trash"} {
		t.Run(name, func(t *testing.T) {
			if got := PickTrashMailbox([]MailboxInfo{{Name: name}}); got != name {
				t.Fatalf("PickTrashMailbox(%q) = %q, want fallback", name, got)
			}
		})
	}
	if got := PickTrashMailbox(nil); got != "" {
		t.Fatalf("PickTrashMailbox(nil) = %q, want empty", got)
	}
}

func TestSubmissionErrorPreservesUnknownOutcome(t *testing.T) {
	inner := fmt.Errorf("connection closed")
	err := &SubmissionError{Stage: "final reply", Err: inner}
	if got := err.ErrorCode(); got != CodeSMTPSubmissionUnknown {
		t.Fatalf("ErrorCode() = %q, want %q", got, CodeSMTPSubmissionUnknown)
	}
	if got := err.Unwrap(); got != inner {
		t.Fatalf("Unwrap() = %v, want %v", got, inner)
	}
	if got := err.Error(); got != "smtp_submission_unknown: SMTP submission outcome is unknown during final reply: connection closed" {
		t.Fatalf("Error() = %q, want detailed outcome", got)
	}
	if got := (&SubmissionError{Stage: "DATA"}).Error(); got != "smtp_submission_unknown: SMTP submission outcome is unknown during DATA" {
		t.Fatalf("Error() without cause = %q, want concise outcome", got)
	}
}
