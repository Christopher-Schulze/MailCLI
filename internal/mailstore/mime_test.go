package mailstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

func TestParseMIMEDocument(t *testing.T) {
	t.Parallel()
	source := []byte("From: Sender <sender@example.com>\r\n" +
		"To: Receiver <receiver@example.com>\r\n" +
		"Cc: Copy <copy@example.com>\r\n" +
		"Bcc: Hidden <hidden@example.com>\r\n" +
		"Reply-To: Reply <reply@example.com>\r\n" +
		"Message-ID: <message@example.com>\r\n" +
		"Content-Type: multipart/mixed; boundary=test-boundary\r\n\r\n" +
		"--test-boundary\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nHello, world.\r\n" +
		"--test-boundary\r\nContent-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=report.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\ncGRmLWJ5dGVz\r\n" +
		"--test-boundary--\r\n")
	document, err := parseMIMEDocument(bytes.NewReader(source), false, true, false)
	if err != nil {
		t.Fatalf("parseMIMEDocument() error = %v", err)
	}
	if !document.Complete || document.MessageID != "message@example.com" || document.Content != "Hello, world." {
		t.Fatalf("parseMIMEDocument() = %#v", document)
	}
	if !reflect.DeepEqual(document.To, []mail.Recipient{{Name: "Receiver", Address: "receiver@example.com"}}) ||
		!reflect.DeepEqual(document.CC, []mail.Recipient{{Name: "Copy", Address: "copy@example.com"}}) ||
		!reflect.DeepEqual(document.BCC, []mail.Recipient{{Name: "Hidden", Address: "hidden@example.com"}}) {
		t.Fatalf("recipients = to %#v, cc %#v, bcc %#v", document.To, document.CC, document.BCC)
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
	document, err := parseMIMEDocument(bytes.NewReader(source), false, false, false)
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
	document, err := parseMIMEDocument(bytes.NewReader(source), true, true, false)
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
		false,
	)
	if err != nil {
		t.Fatalf("parseMIMEDocument() error = %v", err)
	}
	if document.Complete || document.Content != "locally available body" {
		t.Fatalf("parseMIMEDocument() = %#v", document)
	}
}

func TestParseMIMEDocumentSelectsPlainAlternativeOnce(t *testing.T) {
	t.Parallel()
	source := strings.NewReader("Content-Type: multipart/alternative; boundary=alt\r\n\r\n" +
		"--alt\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nPlain body\r\n" +
		"--alt\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>HTML body</p>\r\n--alt--\r\n")
	document, err := parseMIMEDocument(source, false, false, false)
	if err != nil || !document.Complete || document.Content != "Plain body" {
		t.Fatalf("parseMIMEDocument() = %#v, error = %v", document, err)
	}
}

func TestParseMIMEDocumentPreservesMixedPartOrderAroundAlternative(t *testing.T) {
	t.Parallel()
	source := strings.NewReader("Content-Type: multipart/mixed; boundary=mix\r\n\r\n" +
		"--mix\r\nContent-Type: text/plain\r\n\r\nBefore\r\n" +
		"--mix\r\nContent-Type: multipart/alternative; boundary=alt\r\n\r\n" +
		"--alt\r\nContent-Type: text/plain\r\n\r\nSelected\r\n" +
		"--alt\r\nContent-Type: text/html\r\n\r\n<p>Ignored</p>\r\n--alt--\r\n" +
		"--mix\r\nContent-Type: text/plain\r\n\r\nAfter\r\n--mix--\r\n")
	document, err := parseMIMEDocument(source, false, false, false)
	if err != nil || document.Content != "Before\n\nSelected\n\nAfter" {
		t.Fatalf("parseMIMEDocument() = %#v, error = %v", document, err)
	}
}

func TestParseMIMEDocumentSelectsOneNestedAlternative(t *testing.T) {
	t.Parallel()
	source := strings.NewReader("Content-Type: multipart/alternative; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: multipart/related; boundary=related\r\n\r\n" +
		"--related\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n" +
		"--inner\r\nContent-Type: text/plain\r\n\r\nNested plain\r\n" +
		"--inner\r\nContent-Type: text/html\r\n\r\n<p>Nested HTML</p>\r\n--inner--\r\n" +
		"--related--\r\n" +
		"--outer\r\nContent-Type: text/html\r\n\r\n<p>Outer HTML</p>\r\n--outer--\r\n")
	document, err := parseMIMEDocument(source, false, false, false)
	if err != nil || document.Content != "Nested plain" {
		t.Fatalf("parseMIMEDocument() = %#v, error = %v", document, err)
	}
}

func TestParseMIMEDocumentMarksMalformedRecipientHeaderIncomplete(t *testing.T) {
	t.Parallel()
	document, err := parseMIMEDocument(strings.NewReader(
		"To: broken <\r\nContent-Type: text/plain\r\n\r\nBody\r\n",
	), false, false, false)
	if err != nil {
		t.Fatalf("parseMIMEDocument() error = %v", err)
	}
	if document.Complete || !reflect.DeepEqual(document.MissingParts, []string{"header:to"}) {
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

func TestHTMLToTextPreservesSemanticContent(t *testing.T) {
	source := `<p>Read <a href="https://example.com/report">report</a></p>` +
		`<pre>  first` + "\n" + `    second</pre>` +
		`<ul><li>One<ul><li>Nested</li></ul></li></ul>` +
		`<table><tr><th>Item</th><th>Qty</th></tr><tr><td>12</td><td>34</td></tr></table>`
	want := "Read report (https://example.com/report)\n  first\n    second\n- One\n  - Nested\n| Item | Qty |\n| 12 | 34 |"
	if got := htmlToText([]byte(source)); got != want {
		t.Fatalf("htmlToText() = %q, want %q", got, want)
	}
	document, err := parseMIMEDocument(strings.NewReader(
		"Content-Type: text/html; charset=utf-8\r\n\r\n"+source,
	), false, false, false)
	if err != nil {
		t.Fatalf("parseMIMEDocument() error = %v", err)
	}
	if document.Content != want {
		t.Fatalf("parseMIMEDocument().Content = %q, want %q", document.Content, want)
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
	document, err := parseMIMEDocument(bytes.NewReader(source), false, true, false)
	if err != nil {
		t.Fatalf("parseMIMEDocument() error = %v", err)
	}
	part, exists := document.Parts["2"]
	if !exists || part.Complete || document.Complete || !reflect.DeepEqual(document.MissingParts, []string{"2"}) {
		t.Fatalf("externalized attachment document = %#v, part = %#v, exists = %t", document, part, exists)
	}
}

func TestParseMIMEDocumentPropagatesMalformedAttachment(t *testing.T) {
	t.Parallel()
	source := []byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--b\r\nContent-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=broken.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nnot-base64!\r\n--b--\r\n")
	document, err := parseMIMEDocument(bytes.NewReader(source), false, true, false)
	if err != nil {
		t.Fatalf("parseMIMEDocument() error = %v", err)
	}
	part, exists := document.Parts["2"]
	if !exists || part.Complete || document.Complete || !reflect.DeepEqual(document.MissingParts, []string{"2"}) {
		t.Fatalf("malformed attachment document = %#v, part = %#v, exists = %t", document, part, exists)
	}
	if document.Content != "body" {
		t.Fatalf("Content = %q, want body retained", document.Content)
	}
}

func TestParseMIMEDocumentPropagatesNestedAttachmentFailure(t *testing.T) {
	t.Parallel()
	source := []byte("Content-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--outer\r\nContent-Type: multipart/mixed; boundary=inner\r\n\r\n" +
		"--inner\r\nContent-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=healthy.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\naGVhbHRoeQ==\r\n" +
		"--inner\r\nContent-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=truncated.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nYnl0ZXM\r\n" +
		"--inner--\r\n--outer--\r\n")
	document, err := parseMIMEDocument(bytes.NewReader(source), false, true, false)
	if err != nil {
		t.Fatalf("parseMIMEDocument() error = %v", err)
	}
	healthy, healthyExists := document.Parts["2.1"]
	failed, failedExists := document.Parts["2.2"]
	digest := sha256.Sum256([]byte("healthy"))
	if !healthyExists || !healthy.Complete || healthy.Size != int64(len("healthy")) ||
		healthy.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("healthy nested attachment = %#v, exists = %t", healthy, healthyExists)
	}
	if !failedExists || failed.Complete || document.Complete ||
		!reflect.DeepEqual(document.MissingParts, []string{"2.2"}) {
		t.Fatalf("nested attachment document = %#v, failed = %#v, exists = %t", document, failed, failedExists)
	}
	if document.Content != "body" {
		t.Fatalf("Content = %q, want body retained", document.Content)
	}
}

func TestParseMIMEDocumentDeduplicatesUnsupportedAttachmentFailure(t *testing.T) {
	t.Parallel()
	source := []byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=unknown.bin\r\n" +
		"Content-Transfer-Encoding: unsupported\r\n\r\nbytes\r\n--b--\r\n")
	document, err := parseMIMEDocument(bytes.NewReader(source), false, true, false)
	if err != nil {
		t.Fatalf("parseMIMEDocument() error = %v", err)
	}
	part, exists := document.Parts["1"]
	if !exists || part.Complete || document.Complete || !reflect.DeepEqual(document.MissingParts, []string{"1"}) {
		t.Fatalf("unsupported attachment document = %#v, part = %#v, exists = %t", document, part, exists)
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

func TestReadRawHeadersAcceptsExactMaximum(t *testing.T) {
	t.Parallel()
	source := strings.Repeat("X", maximumHeaderBytes-4) + "\r\n\r\n"
	parsed, err := readRawHeaders(strings.NewReader(source))
	if err != nil || len(parsed) != maximumHeaderBytes {
		t.Fatalf("readRawHeaders() length = %d, error = %v; want exact maximum %d", len(parsed), err, maximumHeaderBytes)
	}
}

func TestReadRawHeadersRejectsOneByteBeyondMaximumWithBoundary(t *testing.T) {
	t.Parallel()
	source := strings.Repeat("X", maximumHeaderBytes-3) + "\r\n\r\n"
	if _, err := readRawHeaders(strings.NewReader(source)); errorCodeForTest(err) != "invalid_message_source" {
		t.Fatalf("readRawHeaders() error = %v, want invalid_message_source", err)
	}
}

func TestReadRawHeadersStopsAtHeaderBoundary(t *testing.T) {
	t.Parallel()
	header := "Subject: hello\r\n\r\n"
	reader := &rawHeaderCountingReader{reader: strings.NewReader(header + strings.Repeat("attachment", 1024*1024))}
	parsed, err := readRawHeaders(reader)
	if err != nil || parsed != header {
		t.Fatalf("readRawHeaders() = %q, error = %v; want header only", parsed, err)
	}
	if reader.bytesRead > int64(len(header)+mimeHeaderReaderBuffer) {
		t.Fatalf("readRawHeaders() consumed %d bytes, want at most one bounded buffer past the header", reader.bytesRead)
	}
}

func BenchmarkReadRawHeaders(b *testing.B) {
	benchmarks := []struct {
		name   string
		source string
	}{
		{name: "small", source: "Subject: hello\r\n\r\n"},
		{name: "medium", source: strings.Repeat("X-Trace: value\r\n", 256) + "\r\n"},
		{name: "maximum", source: strings.Repeat("X", maximumHeaderBytes-4) + "\r\n\r\n"},
	}
	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(benchmark.source)))
			for b.Loop() {
				parsed, err := readRawHeaders(strings.NewReader(benchmark.source))
				if err != nil || len(parsed) != len(benchmark.source) {
					b.Fatalf("readRawHeaders() length = %d, error = %v", len(parsed), err)
				}
			}
		})
	}
}

type rawHeaderCountingReader struct {
	reader    io.Reader
	bytesRead int64
}

func (r *rawHeaderCountingReader) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	r.bytesRead += int64(read)
	return read, err
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

func TestHasVisibleText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input []byte
		want  bool
	}{
		{name: "empty", input: []byte{}, want: false},
		{name: "only spaces", input: []byte("   \t\n"), want: false},
		{name: "only unicode whitespace", input: []byte("\u00a0\u2000\u2001"), want: false},
		{name: "ascii text", input: []byte("hello"), want: true},
		{name: "text after spaces", input: []byte("  hi  "), want: true},
		{name: "unicode text", input: []byte("café"), want: true},
		{name: "mixed whitespace and text", input: []byte(" \t hello \n "), want: true},
		{name: "single non-space ascii", input: []byte("x"), want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hasVisibleText(test.input); got != test.want {
				t.Fatalf("hasVisibleText(%q) = %t, want %t", test.input, got, test.want)
			}
		})
	}
}

func TestMimePartID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path []int
		want string
	}{
		{name: "empty path", path: nil, want: "1"},
		{name: "single", path: []int{0}, want: "1"},
		{name: "nested", path: []int{0, 1, 2}, want: "1.2.3"},
		{name: "deep", path: []int{2, 0, 4}, want: "3.1.5"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := mimePartID(test.path); got != test.want {
				t.Fatalf("mimePartID(%v) = %q, want %q", test.path, got, test.want)
			}
		})
	}
}

func TestCombineMIMETextAlternative(t *testing.T) {
	t.Parallel()
	children := []mimeTextRepresentation{
		{Text: "<p>html</p>", Rank: mimeTextHTML},
		{Text: "plain text", Rank: mimeTextPlain},
	}
	result := combineMIMEText("multipart/alternative", children)
	// multipart/alternative selects the highest-rank non-empty child.
	if result.Text != "plain text" {
		t.Fatalf("combineMIMEText alternative = %q, want %q (highest rank)", result.Text, "plain text")
	}
	if result.Rank != mimeTextPlain {
		t.Fatalf("combineMIMEText alternative rank = %d, want %d", result.Rank, mimeTextPlain)
	}
}

func TestCombineMIMETextMixed(t *testing.T) {
	t.Parallel()
	children := []mimeTextRepresentation{
		{Text: "first", Rank: mimeTextPlain},
		{Text: "second", Rank: mimeTextPlain},
	}
	result := combineMIMEText("multipart/mixed", children)
	want := "first\n\nsecond"
	if result.Text != want {
		t.Fatalf("combineMIMEText mixed = %q, want %q", result.Text, want)
	}
}

func TestCombineMIMETextSkipsEmptyChildren(t *testing.T) {
	t.Parallel()
	children := []mimeTextRepresentation{
		{Text: "", Rank: mimeTextNone},
		{Text: "only", Rank: mimeTextPlain},
		{Text: "", Rank: mimeTextHTML},
	}
	result := combineMIMEText("multipart/mixed", children)
	if result.Text != "only" {
		t.Fatalf("combineMIMEText mixed with empties = %q, want %q", result.Text, "only")
	}
}

func TestMessageIDFromSourceReadsHeadersOnly(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("x", 1<<20)
	raw := "From: Alice <alice@example.com>\r\n" +
		"Subject: Header only\r\n" +
		"Message-ID: <identity@example.com>\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Disposition: attachment; filename=big.bin\r\n\r\n" +
		body + "\r\n--b--\r\n"
	reader := &countingSourceReader{reader: strings.NewReader(raw)}
	messageID, err := messageIDFromSource(reader)
	if err != nil {
		t.Fatalf("messageIDFromSource() error = %v", err)
	}
	document, err := parseMIMEDocument(strings.NewReader(raw), false, false, false)
	if err != nil {
		t.Fatalf("parseMIMEDocument() error = %v", err)
	}
	if messageID != document.MessageID || messageID == "" {
		t.Fatalf("messageIDFromSource() = %q, full parse = %q; want identical non-empty", messageID, document.MessageID)
	}
	// Header-only reads must stop at the blank line, never reach the 1 MiB body.
	if reader.count > int64(len(raw))/2 {
		t.Fatalf("read %d bytes, want header block only (< %d)", reader.count, len(raw)/2)
	}
}

func TestMessageIDFromSourceMissingHeader(t *testing.T) {
	t.Parallel()
	raw := "From: Alice <alice@example.com>\r\nSubject: No ID\r\n\r\nBody\r\n"
	messageID, err := messageIDFromSource(strings.NewReader(raw))
	if err != nil || messageID != "" {
		t.Fatalf("messageIDFromSource() = %q, error = %v; want empty, nil", messageID, err)
	}
	document, err := parseMIMEDocument(strings.NewReader(raw), false, false, false)
	if err != nil || document.MessageID != "" {
		t.Fatalf("parseMIMEDocument() = %q, error = %v; want empty, nil", document.MessageID, err)
	}
}

func TestSourceHeadersPreservesReplyToListAndStopsAtHeaders(t *testing.T) {
	t.Parallel()
	raw := "From: Alice <alice@example.com>\r\n" +
		"Reply-To: =?UTF-8?Q?B=C3=B6b?= <bob@example.com>, \"Carol Example\" <carol@example.com>\r\n" +
		"Message-ID: <reply@example.com>\r\n\r\n" + strings.Repeat("x", 1<<20)
	reader := &countingSourceReader{reader: strings.NewReader(raw)}
	headers, err := sourceHeadersFromReader(reader)
	if err != nil {
		t.Fatalf("sourceHeadersFromReader() error = %v", err)
	}
	if headers.ReplyToError != nil || len(headers.ReplyTo) != 2 {
		t.Fatalf("reply-to headers = %+v", headers)
	}
	if headers.ReplyTo[0].Name != "Böb" || headers.ReplyTo[0].Address != "bob@example.com" ||
		headers.ReplyTo[1].Name != "Carol Example" || headers.ReplyTo[1].Address != "carol@example.com" {
		t.Fatalf("reply-to recipients = %+v", headers.ReplyTo)
	}
	if reader.count > int64(len(raw))/2 {
		t.Fatalf("read %d bytes, want header block only (< %d)", reader.count, len(raw)/2)
	}
}

func TestSourceHeadersPreservesMissingAndMalformedReplyToState(t *testing.T) {
	t.Parallel()
	missing, err := sourceHeadersFromReader(strings.NewReader(
		"From: Alice <alice@example.com>\r\nMessage-ID: <missing@example.com>\r\n\r\nBody\r\n"))
	if err != nil || missing.ReplyToError != nil || len(missing.ReplyTo) != 0 {
		t.Fatalf("missing Reply-To = %+v, error = %v", missing, err)
	}
	malformed, err := sourceHeadersFromReader(strings.NewReader(
		"From: Alice <alice@example.com>\r\nReply-To: Bob <bob@example.com>, <broken\r\n" +
			"Message-ID: <malformed@example.com>\r\n\r\nBody\r\n"))
	if err != nil {
		t.Fatalf("malformed sourceHeadersFromReader() error = %v", err)
	}
	if malformed.ReplyToError == nil {
		t.Fatalf("malformed Reply-To state = %+v, want parser error", malformed)
	}
}

type countingSourceReader struct {
	reader io.Reader
	count  int64
}

func (r *countingSourceReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.count += int64(n)
	return n, err
}
