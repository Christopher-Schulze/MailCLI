package mail

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite golden fixtures")

const composerTestMessageID = "build-0001@mailcli.local"

func TestBuildMessageGolden(t *testing.T) {
	directory := t.TempDir()
	notesPath := filepath.Join(directory, "notes.txt")
	writeComposerFixture(t, notesPath, "meeting notes\nline two\n")
	resumePath := filepath.Join(directory, "résumé final.txt")
	writeComposerFixture(t, resumePath, "curriculum vitae with ünïcode\n")

	drafts := map[string]Draft{
		"plain-only": {
			Kind: DraftKindNew,
			From: "sender@example.com",
			To:   []Recipient{{Address: "alice@example.com"}},
			Body: "Hello Alice,\n\nHere is the update.\n\nBest,\nSender\n",
		},
		"plain-html": {
			Kind:       DraftKindNew,
			From:       "sender@example.com",
			To:         []Recipient{{Address: "alice@example.com"}},
			Subject:    "Styled update",
			Body:       "Hello Alice,\n\nHere is the update.\n",
			BodyFormat: DraftBodyHTML,
			BodyHTML:   "<p>Hello Alice,</p><p>Here is the <strong>update</strong>.</p>",
		},
		"attachments": {
			Kind:    DraftKindNew,
			From:    "sender@example.com",
			To:      []Recipient{{Name: "Alice", Address: "alice@example.com"}},
			Subject: "Documents attached",
			Body:    "Both documents are attached.\n",
			Attachments: []DraftAttachment{
				{Path: notesPath, Size: 25, SHA256: "unused"},
				{Path: resumePath, Size: 30, SHA256: "unused"},
			},
		},
		"reply-threading": {
			Kind:             DraftKindReply,
			SourceMessageID:  "<original-123@example.com>",
			SourceReferences: "<first-1@example.com> <second-2@example.com>",
			From:             "sender@example.com",
			To:               []Recipient{{Address: "original@example.com"}},
			Subject:          "Re: Original subject",
			Body:             "Thanks, understood.\n",
		},
		"bcc-excluded": {
			Kind:    DraftKindNew,
			From:    "sender@example.com",
			To:      []Recipient{{Address: "alice@example.com"}},
			BCC:     []Recipient{{Name: "Secret", Address: "secret@example.com"}},
			Subject: "Visible to To only",
			Body:    "The BCC recipient must never appear.\n",
		},
	}

	for name, draft := range drafts {
		t.Run(name, func(t *testing.T) {
			message, err := BuildMessage(draft, composerTestMessageID)
			if err != nil {
				t.Fatal(err)
			}
			normalized := normalizeComposerMessage(message)
			goldenPath := filepath.Join("testdata", "golden", name+".golden")
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(goldenPath, []byte(normalized), 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatal(err)
			}
			if normalized != string(want) {
				t.Fatalf("message mismatch:\n--- got ---\n%s\n--- want ---\n%s", normalized, string(want))
			}
		})
	}
}

func TestBuildMessageEdgeCases(t *testing.T) {
	tests := []struct {
		name      string
		draft     func(t *testing.T) Draft
		messageID string
		contains  []string
		absent    []string
	}{
		{
			name:     "empty subject",
			draft:    func(t *testing.T) Draft { return Draft{Kind: DraftKindNew, From: "a@example.com", Body: "hi"} },
			contains: []string{"Subject:\r\n"},
		},
		{
			name: "unicode subject",
			draft: func(t *testing.T) Draft {
				return Draft{Kind: DraftKindNew, From: "a@example.com", Subject: "Réunion ça va", Body: "hi"}
			},
			contains: []string{"Subject: =?UTF-8?q?"},
			absent:   []string{"Réunion"},
		},
		{
			name: "unicode body quoted-printable",
			draft: func(t *testing.T) Draft {
				return Draft{Kind: DraftKindNew, From: "a@example.com", Subject: "s", Body: "Grüße aus München\n"}
			},
			contains: []string{"Content-Transfer-Encoding: quoted-printable", "Gr=C3=BC=C3=9Fe"},
			absent:   []string{"Grüße"},
		},
		{
			name: "recipient with display name",
			draft: func(t *testing.T) Draft {
				return Draft{
					Kind: DraftKindNew, From: "a@example.com",
					To:   []Recipient{{Name: "Zoe Example", Address: "zoe@example.com"}},
					Body: "hi",
				}
			},
			contains: []string{"To: \"Zoe Example\" <zoe@example.com>\r\n"},
		},
		{
			name: "empty bcc omitted",
			draft: func(t *testing.T) Draft {
				return Draft{Kind: DraftKindNew, From: "a@example.com", To: []Recipient{{Address: "b@example.com"}}, BCC: nil, Body: "hi"}
			},
			absent: []string{"Bcc"},
		},
		{
			name: "forward has no threading headers",
			draft: func(t *testing.T) Draft {
				return Draft{
					Kind: DraftKindForward, From: "a@example.com",
					SourceMessageID: "<orig@example.com>", SourceReferences: "<x@example.com>",
					To: []Recipient{{Address: "b@example.com"}}, Subject: "Fwd: thing", Body: "hi",
				}
			},
			absent: []string{"In-Reply-To", "References"},
		},
		{
			name: "reply without prior references",
			draft: func(t *testing.T) Draft {
				return Draft{
					Kind: DraftKindReply, From: "a@example.com", SourceMessageID: "<orig@example.com>",
					To: []Recipient{{Address: "b@example.com"}}, Body: "hi",
				}
			},
			contains: []string{"In-Reply-To: <orig@example.com>\r\n", "References: <orig@example.com>\r\n"},
		},
		{
			name: "long recipient list folds",
			draft: func(t *testing.T) Draft {
				return Draft{
					Kind: DraftKindNew, From: "a@example.com",
					To: []Recipient{
						{Address: "recipient-one@example.com"}, {Address: "recipient-two@example.com"},
						{Address: "recipient-three@example.com"}, {Address: "recipient-four@example.com"},
					},
					Body: "hi",
				}
			},
			contains: []string{"To: <recipient-one@example.com>, <recipient-two@example.com>,\r\n", " <recipient-three@example.com>, <recipient-four@example.com>\r\n"},
		},
		{
			name: "unknown attachment extension falls back to octet-stream",
			draft: func(t *testing.T) Draft {
				path := filepath.Join(t.TempDir(), "payload.xyzabc")
				writeComposerFixture(t, path, "binary-ish")
				return Draft{
					Kind: DraftKindNew, From: "a@example.com", Body: "hi",
					Attachments: []DraftAttachment{{Path: path}},
				}
			},
			contains: []string{"Content-Type: application/octet-stream\r\n", "Content-Disposition: attachment; filename=payload.xyzabc\r\n"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message, err := BuildMessage(test.draft(t), composerTestMessageID)
			if err != nil {
				t.Fatal(err)
			}
			text := string(message)
			for _, want := range test.contains {
				if !strings.Contains(text, want) {
					t.Errorf("message missing %q:\n%s", want, text)
				}
			}
			for _, banned := range test.absent {
				if strings.Contains(text, banned) {
					t.Errorf("message must not contain %q:\n%s", banned, text)
				}
			}
		})
	}
}

func TestBuildMessageStructure(t *testing.T) {
	draft := Draft{
		Kind: DraftKindNew,
		From: "sender@example.com",
		To:   []Recipient{{Address: "alice@example.com"}},
		Body: "body\n",
	}
	message, err := BuildMessage(draft, composerTestMessageID)
	if err != nil {
		t.Fatal(err)
	}
	text := string(message)
	if !strings.HasSuffix(text, "\r\n") {
		t.Errorf("message must end with CRLF, got %q", text[len(text)-8:])
	}
	if bytes.Count(message, []byte("\n")) != bytes.Count(message, []byte("\r\n")) {
		t.Error("message contains bare LF line endings, want CRLF everywhere")
	}
	if !composerBoundaryPattern.MatchString(text) {
		t.Errorf("missing random boundary token:\n%s", text)
	}
	boundaries := composerBoundaryPattern.FindAllString(text, -1)
	seen := map[string]bool{}
	for _, boundary := range boundaries {
		seen[boundary] = true
	}
	if len(seen) != 1 {
		t.Errorf("expected one boundary per message, got %v", seen)
	}
}

func TestBuildMessageAttachmentReadError(t *testing.T) {
	draft := Draft{
		Kind:        DraftKindNew,
		From:        "sender@example.com",
		Body:        "body",
		Attachments: []DraftAttachment{{Path: filepath.Join(t.TempDir(), "missing.bin")}},
	}
	message, err := BuildMessage(draft, composerTestMessageID)
	if err == nil {
		t.Fatalf("BuildMessage() = %q, want error", message)
	}
	var composerErr *ComposerError
	if !errors.As(err, &composerErr) {
		t.Fatalf("error = %T, want *ComposerError", err)
	}
	if composerErr.Message == "" {
		t.Error("ComposerError.Message is empty")
	}
}

func TestBuildMessageRequiresMessageID(t *testing.T) {
	if _, err := BuildMessage(Draft{Kind: DraftKindNew, Body: "hi"}, ""); err == nil {
		t.Error("BuildMessage() with empty message id = nil error, want error")
	}
}

func writeComposerFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

var (
	composerDatePattern     = regexp.MustCompile(`(?m)^Date: .*$`)
	composerMessageIDHeader = regexp.MustCompile(`(?m)^Message-ID: .*$`)
	composerBoundaryPattern = regexp.MustCompile(`=_[0-9a-f]{32}`)
)

// normalizeComposerMessage replaces dynamic values so golden fixtures stay stable.
func normalizeComposerMessage(message []byte) string {
	normalized := composerDatePattern.ReplaceAllString(string(message), "Date: <DATE>")
	normalized = composerMessageIDHeader.ReplaceAllString(normalized, "Message-ID: <MESSAGE-ID>")
	return composerBoundaryPattern.ReplaceAllString(normalized, "<BOUNDARY>")
}
