package transport

import (
	"strings"
	"testing"
)

func TestResolveMailboxPathPrecedenceAndAmbiguity(t *testing.T) {
	tests := []struct {
		name       string
		mailboxes  []MailboxInfo
		path       []string
		want       string
		wantCode   string
		wantPhrase string
	}{
		{
			name: "exact path outranks special use",
			mailboxes: []MailboxInfo{
				{Name: "Archive", Flags: []string{"\\Sent"}},
				{Name: "Gesendet", Flags: []string{"\\Sent"}},
			},
			path: []string{"Archive"}, want: "Archive",
		},
		{
			name: "dot separated exact path",
			mailboxes: []MailboxInfo{
				{Name: "INBOX.Archive"}, {Name: "Archive", Flags: []string{"\\Archive"}},
			},
			path: []string{"INBOX", "Archive"}, want: "INBOX.Archive",
		},
		{
			name: "conflicting exact separators are ambiguous",
			mailboxes: []MailboxInfo{
				{Name: "A/B"}, {Name: "A.B"},
			},
			path: []string{"A", "B"}, wantCode: CodeIMAPAmbiguousMailbox,
			wantPhrase: "A.B, A/B",
		},
		{
			name: "case insensitive special use",
			mailboxes: []MailboxInfo{
				{Name: "Archive", Flags: []string{"\\SENT"}},
			},
			path: []string{"sent"}, want: "Archive",
		},
		{
			name:      "localized exact name",
			mailboxes: []MailboxInfo{{Name: "gEsEnDeT"}},
			path:      []string{"Gesendet"}, want: "gEsEnDeT",
		},
		{
			name: "ambiguous special use",
			mailboxes: []MailboxInfo{
				{Name: "z-sent", Flags: []string{"\\Sent"}},
				{Name: "a-sent", Flags: []string{"\\Sent"}},
			},
			path: []string{"Sent"}, wantCode: CodeIMAPAmbiguousMailbox, wantPhrase: "a-sent, z-sent",
		},
		{
			name:      "ambiguous heuristic leaf",
			mailboxes: []MailboxInfo{{Name: "A/Custom"}, {Name: "B.Custom"}},
			path:      []string{"Custom"}, wantCode: CodeIMAPAmbiguousMailbox,
			wantPhrase: "A/Custom, B.Custom",
		},
		{
			name:      "missing mailbox",
			mailboxes: []MailboxInfo{{Name: "INBOX"}},
			path:      []string{"Archive"}, wantCode: CodeIMAPMailboxNotFound,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveMailboxPath(test.mailboxes, test.path)
			if got != test.want {
				t.Fatalf("ResolveMailboxPath() = %q, want %q; error = %v", got, test.want, err)
			}
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("ResolveMailboxPath() error = %v", err)
				}
				return
			}
			if ErrorCode(err) != test.wantCode {
				t.Fatalf("ResolveMailboxPath() error code = %q, want %q: %v", ErrorCode(err), test.wantCode, err)
			}
			if test.wantPhrase != "" && !strings.Contains(err.Error(), test.wantPhrase) {
				t.Fatalf("ResolveMailboxPath() error = %v, want candidates %q", err, test.wantPhrase)
			}
		})
	}
}

func TestResolveMailboxPathAmbiguityEvidenceIsOrderIndependent(t *testing.T) {
	forward := []MailboxInfo{
		{Name: "Gesendet", Flags: []string{"\\Sent"}},
		{Name: "Archive", Flags: []string{"\\Sent"}},
	}
	reverse := []MailboxInfo{forward[1], forward[0]}
	_, firstErr := ResolveMailboxPath(forward, []string{"Sent"})
	_, secondErr := ResolveMailboxPath(reverse, []string{"Sent"})
	if ErrorCode(firstErr) != CodeIMAPAmbiguousMailbox || ErrorCode(secondErr) != CodeIMAPAmbiguousMailbox {
		t.Fatalf("ambiguity codes = %q, %q; want %q", ErrorCode(firstErr), ErrorCode(secondErr), CodeIMAPAmbiguousMailbox)
	}
	if firstErr.Error() != secondErr.Error() {
		t.Fatalf("ambiguity errors differ by LIST order: %q versus %q", firstErr, secondErr)
	}
}

func TestResolveSpecialMailboxRejectsAmbiguousCandidates(t *testing.T) {
	mailboxes := []MailboxInfo{
		{Name: "Trash", Flags: []string{"\\Trash"}},
		{Name: "Papierkorb", Flags: []string{"\\Trash"}},
	}
	if _, err := ResolveTrashMailbox(mailboxes); ErrorCode(err) != CodeIMAPAmbiguousMailbox {
		t.Fatalf("ResolveTrashMailbox() error = %v, want %s", err, CodeIMAPAmbiguousMailbox)
	}
	if _, err := ResolveSentMailbox([]MailboxInfo{
		{Name: "Sent Messages"}, {Name: "Gesendet"},
	}); ErrorCode(err) != CodeIMAPAmbiguousMailbox {
		t.Fatalf("ResolveSentMailbox() error = %v, want %s", err, CodeIMAPAmbiguousMailbox)
	}
}

func TestResolveMailboxPathUsesServerHierarchyAndExactCase(t *testing.T) {
	mailboxes := []MailboxInfo{
		{
			Name: "A.B", WireName: "A.B", DisplayName: "A.B",
			DisplayPath: []string{"A", "B"}, Delimiter: ".", Encoding: MailboxEncodingModifiedUTF7,
		},
		{
			Name: "A/B", WireName: "A/B", DisplayName: "A/B",
			DisplayPath: []string{"A/B"}, Encoding: MailboxEncodingModifiedUTF7,
		},
		{
			Name: "Sent", WireName: "Sent", DisplayName: "Sent",
			DisplayPath: []string{"Sent"}, Delimiter: ".", Encoding: MailboxEncodingModifiedUTF7,
		},
		{
			Name: "sent", WireName: "sent", DisplayName: "sent",
			DisplayPath: []string{"sent"}, Delimiter: ".", Encoding: MailboxEncodingModifiedUTF7,
		},
		{
			Name: "Custom", WireName: "Custom", DisplayName: "Custom",
			DisplayPath: []string{"Custom"}, Delimiter: ".", Encoding: MailboxEncodingModifiedUTF7,
		},
		{
			Name: "custom", WireName: "custom", DisplayName: "custom",
			DisplayPath: []string{"custom"}, Delimiter: ".", Encoding: MailboxEncodingModifiedUTF7,
		},
	}
	if got, err := ResolveMailboxPath(mailboxes, []string{"A", "B"}); err != nil || got != "A.B" {
		t.Fatalf("dot hierarchy = %q, %v; want A.B", got, err)
	}
	if got, err := ResolveMailboxPath(mailboxes, []string{"A/B"}); err != nil || got != "A/B" {
		t.Fatalf("flat mailbox = %q, %v; want A/B", got, err)
	}
	if got, err := ResolveMailboxPath(mailboxes, []string{"sent"}); err != nil || got != "sent" {
		t.Fatalf("case-distinct lowercase mailbox = %q, %v; want sent", got, err)
	}
	if got, err := ResolveMailboxPath(mailboxes, []string{"custom"}); err != nil || got != "custom" {
		t.Fatalf("case-distinct custom mailbox = %q, %v; want custom", got, err)
	}
	if got, err := ResolveMailboxPath(mailboxes, []string{"Sent"}); err != nil || got != "Sent" {
		t.Fatalf("case-distinct Sent mailbox = %q, %v; want Sent", got, err)
	}
}

func TestResolveMailboxPathDoesNotInventHierarchyForNILDelimiter(t *testing.T) {
	mailboxes := []MailboxInfo{{
		Name: "A/B", WireName: "A/B", DisplayName: "A/B", DisplayPath: []string{"A/B"},
		Delimiter: "", Encoding: MailboxEncodingModifiedUTF7,
	}}
	if _, err := ResolveMailboxPath(mailboxes, []string{"A", "B"}); ErrorCode(err) != CodeIMAPMailboxNotFound {
		t.Fatalf("NIL delimiter synthetic hierarchy error = %v, want %s", err, CodeIMAPMailboxNotFound)
	}
}

func TestResolveSpecialUseKeepsCaseDistinctWireNames(t *testing.T) {
	mailboxes := []MailboxInfo{
		{
			Name: "Sent", WireName: "Sent", DisplayName: "Sent", DisplayPath: []string{"Sent"},
			Delimiter: ".", Encoding: MailboxEncodingModifiedUTF7, Flags: []string{"\\Sent"},
		},
		{
			Name: "sent", WireName: "sent", DisplayName: "sent", DisplayPath: []string{"sent"},
			Delimiter: ".", Encoding: MailboxEncodingModifiedUTF7,
		},
	}
	if got, err := ResolveSentMailbox(mailboxes); err != nil || got != "Sent" {
		t.Fatalf("special-use Sent = %q, %v; want Sent", got, err)
	}
	if got, err := ResolveMailboxPath(mailboxes, []string{"sent"}); err != nil || got != "sent" {
		t.Fatalf("explicit lowercase Sent = %q, %v; want sent", got, err)
	}
}
