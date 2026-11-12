package mailstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"mailcli/internal/cli"
	"mailcli/internal/mail"
)

func TestMIMETextDecodingCompleteness(t *testing.T) {
	for _, test := range []struct {
		name     string
		kind     string
		body     string
		complete bool
	}{
		{"valid Unicode", "text/plain; charset=utf-8", "Grüße 日本語", true},
		{"literal replacement rune", "text/plain; charset=utf-8", "kept � text", true},
		{"invalid plain", "text/plain; charset=utf-8", "kept \xff text", false},
		{"truncated rune", "text/plain; charset=utf-8", "kept \xe2\x82", false},
		{"invalid HTML", "text/html; charset=utf-8", "<p>kept \xff text</p>", false},
		{"valid HTML replacement rune", "text/html; charset=utf-8", "<p>kept � text</p>", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := "Content-Type: " + test.kind + "\r\n\r\n" + test.body
			for _, retain := range []bool{true, false} {
				document, err := parseMIMEDocumentWithContextAndRetention(
					context.Background(), strings.NewReader(raw), false, false, false, retain,
				)
				if err != nil || document.Complete != test.complete ||
					slices.Contains(document.MissingParts, "mime-decoding") == test.complete {
					t.Fatalf("retain=%t complete=%t missing=%v error=%v", retain, document.Complete, document.MissingParts, err)
				}
				if retain && strings.HasPrefix(test.kind, "text/plain") && document.Content != test.body {
					t.Fatalf("plain evidence changed: %q, want %q", document.Content, test.body)
				}
				if !retain && document.Content != "" {
					t.Fatal("metadata read retained body text")
				}
			}
			_, complete, err := excerptText(context.Background(), []byte(raw))
			if err != nil || complete != test.complete {
				t.Fatalf("excerpt complete=%t error=%v, want complete=%t", complete, err, test.complete)
			}
		})
	}
}

func TestMIMEFlowedTextFidelity(t *testing.T) {
	for _, test := range []struct {
		name       string
		parameters string
		body       string
		want       string
	}{
		{"default fixed", "", "kept \r\nline", "kept \r\nline"},
		{"unknown format fixed", "; format=unknown; delsp=yes", "kept \r\nline", "kept \r\nline"},
		{"flowed default spacing", "; format=flowed", "joined \r\nwords", "joined words"},
		{"unknown DelSp keeps space", "; format=flowed; delsp=unknown", "joined \r\nwords", "joined words"},
		{"DelSp removes one space", "; FORMAT=Flowed; DelSp=YES", "joined  \r\nwords", "joined words"},
		{"DelSp within word", "; format=flowed; delsp=yes", "interoper \r\nability", "interoperability"},
		{"Unicode soft break", "; format=flowed; delsp=yes", "日本 \r\n語", "日本語"},
		{"hard breaks", "; format=flowed", "first\r\n\r\nsecond", "first\n\nsecond"},
		{"quote continuation", "; format=flowed", "> quoted \r\n> words", ">quoted words"},
		{"quote depth wins", "; format=flowed; delsp=yes", "> first \r\n>> second \r\n>> line", ">first\n>>secondline"},
		{"stuffed literal quote", "; format=flowed", " >literal\r\n From sender\r\n  indented", ">literal\nFrom sender\n indented"},
		{"signature ends paragraph", "; format=flowed; delsp=yes", "body \r\n-- \r\nName", "body\n-- \nName"},
		{"quoted signature", "; format=flowed", "> body \r\n> -- \r\n> Name", ">body \n>-- \n>Name"},
		{"LF source", "; format=flowed", "joined \nwords\n", "joined words"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := "Content-Type: text/plain; charset=utf-8" + test.parameters + "\r\n\r\n" + test.body
			document, err := parseMIMEDocument(strings.NewReader(raw), false, false, false)
			if err != nil || !document.Complete || document.Content != test.want {
				t.Fatalf("body=%q complete=%t error=%v, want %q", document.Content, document.Complete, err, test.want)
			}
			excerpt, complete, err := excerptText(context.Background(), []byte(raw))
			if err != nil || !complete || strings.TrimSpace(excerpt) != test.want {
				t.Fatalf("excerpt=%q complete=%t error=%v, want %q", excerpt, complete, err, test.want)
			}
		})
	}
}

func TestMIMEExcerptKeepsPartBoundaries(t *testing.T) {
	for _, kind := range []string{"text/plain", "text/html"} {
		t.Run(kind, func(t *testing.T) {
			raw := "Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
				"--b\r\nContent-Type: " + kind + "\r\n\r\nFirst\r\n" +
				"--b\r\nContent-Type: " + kind + "\r\n\r\nSecond\r\n--b--\r\n"
			text, complete, err := excerptText(context.Background(), []byte(raw))
			if err != nil || !complete || mail.BuildExcerpt(text, 100) != "First Second" {
				t.Fatalf("excerpt=%q complete=%t error=%v", text, complete, err)
			}
		})
	}
}

func TestInvalidTextPreservesRawExportAndIncompleteSearch(t *testing.T) {
	ctx := context.Background()
	store, inbox := newSearchFixture(t)
	closeTestResource(t, store, "invalid text fixture")
	page, err := store.ListMessages(ctx, mail.ListMessagesRequest{MailboxRef: inbox, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	ref := messageRefWithSubject(t, page.Messages, "Status Update")
	raw := []byte("From: Alice <alice@example.com>\r\nMessage-ID: <102@example.com>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nretained \xff evidence")
	writeFixtureEMLX(t, store, 102, "imap://"+testAccountID+"/INBOX", raw)
	client := &Client{store: store}
	query, err := mail.PrepareQuery(mail.Query{Text: "retained", MailboxRef: inbox, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found, err := client.SearchMessages(ctx, query)
	if err != nil || len(found.Messages) != 1 || found.Coverage.SourcesComplete || found.Coverage.Complete {
		t.Fatalf("lossy source claimed complete search: %+v error=%v", found, err)
	}
	service := mail.NewService(client)
	for _, normalized := range []bool{true, false} {
		path := filepath.Join(t.TempDir(), "export")
		args := []string{"messages", "raw", "--ref", ref, "--export", path, "--json"}
		if normalized {
			args = []string{"messages", "get", "--ref", ref, "--view", "plain", "--export", path, "--json"}
		}
		var stdout, stderr bytes.Buffer
		status := cli.Run(ctx, service, args, &stdout, &stderr)
		var response struct {
			OK    bool `json:"ok"`
			Error *struct {
				Code string `json:"code"`
			} `json:"error"`
			Data struct {
				Export mail.ContentExport `json:"content_export"`
			} `json:"data"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || stderr.Len() != 0 {
			t.Fatalf("decode=%v stdout=%s stderr=%s", err, &stdout, &stderr)
		}
		if normalized {
			if status != 1 || response.OK || response.Error == nil || response.Error.Code != "content_incomplete" {
				t.Fatalf("lossy normalized export accepted: status=%d output=%s", status, &stdout)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("incomplete export created a destination: %v", err)
			}
			continue
		}
		content, err := os.ReadFile(path)
		digest := sha256.Sum256(raw)
		if status != 0 || !response.OK || response.Error != nil || err != nil || !bytes.Equal(content, raw) ||
			response.Data.Export.Size != int64(len(raw)) || response.Data.Export.SHA256 != hex.EncodeToString(digest[:]) {
			t.Fatalf("raw export lost source bytes: status=%d data=%+v error=%v", status, response.Data.Export, err)
		}
	}
}
