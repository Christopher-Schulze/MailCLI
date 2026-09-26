package transport

import (
	"fmt"
	"strings"
	"testing"
)

func TestMutationMailboxRejectsCanonicalCollisions(t *testing.T) {
	composed, decomposed := "Caf\u00e9", "Cafe\u0301"
	for _, test := range []struct {
		name      string
		paths     [][]string
		requested []string
		want      string
		ambiguous bool
	}{
		{name: "exact NFC", paths: [][]string{{composed}, {decomposed}}, requested: []string{composed}, ambiguous: true},
		{name: "exact NFD", paths: [][]string{{decomposed}, {composed}}, requested: []string{decomposed}, ambiguous: true},
		{name: "shortened Gmail display alias", paths: [][]string{{"[Gmail]", composed}, {decomposed}}, requested: []string{composed}, ambiguous: true},
		{name: "hierarchical", paths: [][]string{{"Projects", composed}, {"Projects", decomposed}}, requested: []string{"Projects", composed}, ambiguous: true},
		{name: "unique canonical", paths: [][]string{{composed}}, requested: []string{decomposed}, want: "wire-0"},
		{name: "case distinct", paths: [][]string{{"Custom"}, {"custom"}}, requested: []string{"Custom"}, want: "wire-0"},
		{name: "compatibility distinct", paths: [][]string{{"file"}, {"\ufb01le"}}, requested: []string{"file"}, want: "wire-0"},
		{name: "segments distinct", paths: [][]string{{composed + "/Records"}, {decomposed, "Records"}}, requested: []string{composed + "/Records"}, want: "wire-0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			boxes := make([]MailboxInfo, len(test.paths))
			for index, path := range test.paths {
				boxes[index] = MailboxInfo{WireName: fmt.Sprintf("wire-%d", index), DisplayPath: path}
			}
			read, err := ResolveMailboxPath(boxes, test.requested)
			if err != nil || read != "wire-0" {
				t.Fatalf("read = %q, %v; want byte-exact or unique canonical wire-0", read, err)
			}
			got, err := ResolveMailboxPathForMutation(boxes, test.requested)
			if !test.ambiguous {
				if err != nil || got != test.want {
					t.Fatalf("mutation = %q, %v; want %q", got, err, test.want)
				}
				return
			}
			if got != "" || !IsAmbiguousMailbox(err) || !strings.Contains(err.Error(), "wire-0, wire-1") {
				t.Fatalf("mutation = %q, %v; want sorted ambiguous wire identities", got, err)
			}
			boxes[0], boxes[1] = boxes[1], boxes[0]
			_, reverseErr := ResolveMailboxPathForMutation(boxes, test.requested)
			if reverseErr == nil || reverseErr.Error() != err.Error() {
				t.Fatalf("LIST order changed ambiguity: %v versus %v", err, reverseErr)
			}
		})
	}
}

func TestMutationMailboxAmbiguityEvidenceIsBounded(t *testing.T) {
	for _, count := range []int{10, 12} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			boxes := make([]MailboxInfo, count)
			for index := range boxes {
				boxes[index] = MailboxInfo{WireName: fmt.Sprintf("candidate-%02d", index), DisplayPath: []string{"Caf\u00e9"}}
			}
			_, err := ResolveMailboxPathForMutation(boxes, []string{"Cafe\u0301"})
			if !IsAmbiguousMailbox(err) {
				t.Fatalf("ambiguity = %v", err)
			}
			for index := 0; index < 10; index++ {
				if !strings.Contains(err.Error(), fmt.Sprintf("candidate-%02d", index)) {
					t.Fatalf("missing bounded candidate %d: %v", index, err)
				}
			}
			if strings.Contains(err.Error(), "candidate-10") || strings.Contains(err.Error(), "candidate-11") ||
				(count > 10 && !strings.Contains(err.Error(), "and 2 more")) {
				t.Fatalf("unbounded or missing overflow evidence: %v", err)
			}
		})
	}
}

func TestResolveTrashMailboxRejectsCanonicalUnflaggedTwin(t *testing.T) {
	boxes := []MailboxInfo{
		{WireName: "wire-nfc", DisplayPath: []string{"Gel\u00f6scht"}, Flags: []string{"\\Trash"}},
		{WireName: "wire-nfd", DisplayPath: []string{"Gelo\u0308scht"}},
	}
	if got, err := ResolveTrashMailbox(boxes); got != "" || !IsAmbiguousMailbox(err) {
		t.Fatalf("Trash = %q, %v; want canonical collision refusal", got, err)
	}
	if got, err := ResolveTrashMailbox(boxes[:1]); err != nil || got != "wire-nfc" {
		t.Fatalf("unique Trash = %q, %v", got, err)
	}
}

func TestResolveTrashMailboxRejectsCanonicalGmailAliasTwin(t *testing.T) {
	boxes := []MailboxInfo{
		{WireName: "wire-trash", DisplayPath: []string{"[Gmail]", "Gel\u00f6scht"}, Flags: []string{"\\Trash"}},
		{WireName: "wire-twin", DisplayPath: []string{"Gelo\u0308scht"}},
	}
	if got, err := ResolveTrashMailbox(boxes); got != "" || !IsAmbiguousMailbox(err) ||
		!strings.Contains(err.Error(), "wire-trash, wire-twin") {
		t.Fatalf("Trash = %q, %v; want canonical collision through the shortened Gmail alias", got, err)
	}
}

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
