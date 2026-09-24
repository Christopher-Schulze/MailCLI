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

func TestResolveMailboxPathMatchesCanonicalSegmentEquivalence(t *testing.T) {
	composed := "Caf\u00e9"
	decomposed := "Cafe\u0301"
	mailboxes := []MailboxInfo{{
		Name: "Projects." + composed, WireName: "Projects." + composed,
		DisplayName: composed, DisplayPath: []string{"Projects", composed},
		Delimiter: ".", Encoding: MailboxEncodingModifiedUTF7,
	}}
	got, err := ResolveMailboxPath(mailboxes, []string{"Projects", decomposed})
	if err != nil || got != "Projects."+composed {
		t.Fatalf("canonical mailbox path = %q, %v; want composed server wire name", got, err)
	}
}

func TestResolveMailboxPathPrefersExactAndRejectsNormalizedAmbiguity(t *testing.T) {
	composed := "Caf\u00e9"
	decomposed := "Cafe\u0301"
	mailboxes := []MailboxInfo{
		{
			Name: "wire-nfd", WireName: "wire-nfd", DisplayName: decomposed,
			DisplayPath: []string{decomposed}, Delimiter: ".",
		},
		{
			Name: "wire-nfc", WireName: "wire-nfc", DisplayName: composed,
			DisplayPath: []string{composed}, Delimiter: ".",
		},
	}
	if got, err := ResolveMailboxPath(mailboxes, []string{composed}); err != nil || got != "wire-nfc" {
		t.Fatalf("exact NFC mailbox = %q, %v; want exact identity wire-nfc", got, err)
	}

	_, err := ResolveMailboxPath([]MailboxInfo{
		{
			Name: "first", WireName: "first", DisplayName: "\u01fa",
			DisplayPath: []string{"\u01fa"}, Delimiter: ".",
		},
		{
			Name: "second", WireName: "second", DisplayName: "A\u030a\u0301",
			DisplayPath: []string{"A\u030a\u0301"}, Delimiter: ".",
		},
	}, []string{"\u00c5\u0301"})
	if ErrorCode(err) != CodeIMAPAmbiguousMailbox || !strings.Contains(err.Error(), "first, second") {
		t.Fatalf("canonical collision error = %v, want sorted ambiguous candidates", err)
	}
}

func TestResolveMailboxPathPreservesSegmentsAndRejectsCompatibilityFolding(t *testing.T) {
	for _, test := range []struct {
		name    string
		mailbox MailboxInfo
		path    []string
	}{
		{
			name: "delimiter segments stay distinct",
			mailbox: MailboxInfo{
				Name: "Caf\u00e9/Records", WireName: "Caf\u00e9/Records",
				DisplayName: "Caf\u00e9/Records", DisplayPath: []string{"Caf\u00e9/Records"},
				Delimiter: "/",
			},
			path: []string{"Caf\u00e9", "Records"},
		},
		{
			name: "compatibility characters stay distinct",
			mailbox: MailboxInfo{
				Name: "wire-ligature", WireName: "wire-ligature",
				DisplayName: "\ufb01le", DisplayPath: []string{"\ufb01le"}, Delimiter: ".",
			},
			path: []string{"file"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ResolveMailboxPath([]MailboxInfo{test.mailbox}, test.path); ErrorCode(err) != CodeIMAPMailboxNotFound {
				t.Fatalf("ResolveMailboxPath() error = %v, want %s", err, CodeIMAPMailboxNotFound)
			}
		})
	}
}

func TestResolveMailboxPathNormalizesRoleAliasesAndFallbackNames(t *testing.T) {
	decomposedAlias := "Entwu\u0308rfe"
	tests := []struct {
		name     string
		mailbox  MailboxInfo
		wantWire string
	}{
		{
			name: "special use alias",
			mailbox: MailboxInfo{
				Name: "server-drafts", WireName: "server-drafts", DisplayName: "Drafts",
				DisplayPath: []string{"System", "Drafts"}, Delimiter: ".",
				Flags: []string{"\\Drafts"},
			},
			wantWire: "server-drafts",
		},
		{
			name: "localized fallback name",
			mailbox: MailboxInfo{
				Name: "localized-drafts", WireName: "localized-drafts", DisplayName: decomposedAlias,
				DisplayPath: []string{"System", decomposedAlias}, Delimiter: ".",
			},
			wantWire: "localized-drafts",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveMailboxPath([]MailboxInfo{test.mailbox}, []string{decomposedAlias})
			if err != nil || got != test.wantWire {
				t.Fatalf("ResolveMailboxPath() = %q, %v; want %q", got, err, test.wantWire)
			}
		})
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
