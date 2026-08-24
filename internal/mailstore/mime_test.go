package mailstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseMIMEDocument(t *testing.T) {
	t.Parallel()
	source := []byte("From: Sender <sender@example.com>\r\n" +
		"To: Receiver <receiver@example.com>\r\n" +
		"Cc: Copy <copy@example.com>\r\n" +
		"Reply-To: Reply <reply@example.com>\r\n" +
		"Message-ID: <message@example.com>\r\n" +
		"Content-Type: multipart/mixed; boundary=test-boundary\r\n\r\n" +
		"--test-boundary\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nHello, world.\r\n" +
		"--test-boundary\r\nContent-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=report.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\ncGRmLWJ5dGVz\r\n" +
		"--test-boundary--\r\n")
	document, err := parseMIMEDocument(bytes.NewReader(source), false, true)
	if err != nil {
		t.Fatalf("parseMIMEDocument() error = %v", err)
	}
	if !document.Complete || document.MessageID != "message@example.com" || document.Content != "Hello, world." {
		t.Fatalf("parseMIMEDocument() = %#v", document)
	}
	if len(document.To) != 1 || document.To[0].Address != "receiver@example.com" || len(document.CC) != 1 {
		t.Fatalf("recipients = %#v, %#v", document.To, document.CC)
	}
	part, exists := document.Parts["2"]
	digest := sha256.Sum256([]byte("pdf-bytes"))
	if !exists || !part.Complete || part.Name != "report.pdf" ||
		part.Size != int64(len("pdf-bytes")) || part.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("attachment part = %#v, %t", part, exists)
	}
}

func TestParseMIMEDocumentCanSkipAttachmentHashing(t *testing.T) {
	t.Parallel()
	source := []byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Disposition: attachment; filename=a.bin\r\n\r\nbytes\r\n--b--\r\n")
	document, err := parseMIMEDocument(bytes.NewReader(source), false, false)
	if err != nil {
		t.Fatalf("parseMIMEDocument() error = %v", err)
	}
	part, exists := document.Parts["1"]
	if !exists || part.Size != int64(len("bytes")) || part.SHA256 != "" {
		t.Fatalf("attachment part = %#v, exists = %t", part, exists)
	}
}

func TestParseMIMEDocumentMarksMissingPartialText(t *testing.T) {
	t.Parallel()
	source := []byte("Content-Type: text/plain\r\nX-Apple-Content-Length: 20\r\n\r\n")
	document, err := parseMIMEDocument(bytes.NewReader(source), true, true)
	if err != nil {
		t.Fatalf("parseMIMEDocument() error = %v", err)
	}
	if document.Complete || !reflect.DeepEqual(document.MissingParts, []string{"1"}) {
		t.Fatalf("parseMIMEDocument() = %#v", document)
	}
}

func TestParseMIMEDocumentNeverClaimsPartialSourceComplete(t *testing.T) {
	t.Parallel()
	document, err := parseMIMEDocument(
		strings.NewReader("Content-Type: text/plain\r\n\r\nlocally available body\r\n"),
		true,
		false,
	)
	if err != nil {
		t.Fatalf("parseMIMEDocument() error = %v", err)
	}
	if document.Complete || document.Content != "locally available body" {
		t.Fatalf("parseMIMEDocument() = %#v", document)
	}
}

func TestHTMLToTextPreservesReadableLayout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{
			name:   "blocks and entities",
			source: "<DIV>Hello&nbsp;   world</DIV> \n <p>Second<br/>line</p>",
			want:   "Hello world\nSecond\nline",
		},
		{
			name:   "script and style",
			source: `<style>hidden</style><p>Visible</p><SCRIPT>ignored</SCRIPT>tail`,
			want:   "Visible\ntail",
		},
		{
			name:   "nested skipped content",
			source: `<script><span>hidden</span></script><div>kept</div>`,
			want:   "kept",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := htmlToText([]byte(test.source)); got != test.want {
				t.Fatalf("htmlToText() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCollapseSearchText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "ascii", input: "  one\t two\nthree  ", want: "one two three"},
		{name: "unicode", input: "one\u00a0\u2003two", want: "one two"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := collapseSearchText(test.input); got != test.want {
				t.Fatalf("collapseSearchText() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNormalizeTextLayout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "spaces", input: "  one\t two  ", want: "one two"},
		{name: "paragraphs", input: "one\r\n\r\n\r\n two\nthree\n", want: "one\n\ntwo\nthree"},
		{name: "unicode whitespace", input: "one\u00a0\u2003two", want: "one two"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeTextLayout(test.input); got != test.want {
				t.Fatalf("normalizeTextLayout() = %q, want %q", got, test.want)
			}
		})
	}
}

func BenchmarkHTMLToText(b *testing.B) {
	source := []byte(strings.Repeat(
		`<div><strong>Status</strong>&nbsp; update<br/>Second line</div><style>.hidden{display:none}</style>`,
		128,
	))
	b.ReportAllocs()
	b.SetBytes(int64(len(source)))
	for b.Loop() {
		_ = htmlToText(source)
	}
}

func TestParseMIMEDocumentMarksExternalizedAttachmentIncomplete(t *testing.T) {
	t.Parallel()
	source := []byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--b\r\nContent-Disposition: attachment; filename=a.bin\r\n" +
		"X-Apple-Content-Length: 20\r\n\r\n--b--\r\n")
	document, err := parseMIMEDocument(bytes.NewReader(source), false, true)
	if err != nil {
		t.Fatalf("parseMIMEDocument() error = %v", err)
	}
	part, exists := document.Parts["2"]
	if !exists || part.Complete {
		t.Fatalf("externalized attachment part = %#v, exists = %t", part, exists)
	}
}

func TestExtractMIMEAttachment(t *testing.T) {
	t.Parallel()
	source := []byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--b\r\nContent-Disposition: attachment; filename=a.bin\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nYnl0ZXM=\r\n--b--\r\n")
	output := filepath.Join(t.TempDir(), "attachment.bin")
	if err := extractMIMEAttachment(bytes.NewReader(source), "2", output); err != nil {
		t.Fatalf("extractMIMEAttachment() error = %v", err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "bytes" {
		t.Fatalf("attachment bytes = %q", got)
	}
}

func TestExtractMIMEAttachmentNeverRemovesExistingOutput(t *testing.T) {
	t.Parallel()
	source := []byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Disposition: attachment; filename=a.bin\r\n\r\nnew\r\n--b--\r\n")
	output := filepath.Join(t.TempDir(), "existing.bin")
	if err := os.WriteFile(output, []byte("keep"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := extractMIMEAttachment(bytes.NewReader(source), "1", output); err == nil {
		t.Fatal("extractMIMEAttachment() error = nil")
	}
	preserved, err := os.ReadFile(output)
	if err != nil || string(preserved) != "keep" {
		t.Fatalf("existing output = %q, error = %v", preserved, err)
	}
}

func TestReadRawHeadersEnforcesBoundBeforeUnterminatedLine(t *testing.T) {
	t.Parallel()
	source := strings.Repeat("X", maximumHeaderBytes+100) + "\n\n"
	if _, err := readRawHeaders(strings.NewReader(source)); errorCodeForTest(err) != "invalid_message_source" {
		t.Fatalf("readRawHeaders() error = %v, want invalid_message_source", err)
	}
}

func TestValidAttachmentID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value string
		want  bool
	}{
		{value: "1", want: true}, {value: "1.2.3", want: true},
		{value: ""}, {value: "0"}, {value: "01"}, {value: "1/2"}, {value: "1..2"},
	}
	for _, test := range tests {
		if got := validAttachmentID(test.value); got != test.want {
			t.Errorf("validAttachmentID(%q) = %t, want %t", test.value, got, test.want)
		}
	}
}
