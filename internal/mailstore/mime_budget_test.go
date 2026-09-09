package mailstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseMIMEEnforcesAggregateTextBudget(t *testing.T) {
	fixture := multipartTextFixture(32, "small")
	limits := defaultMIMEParseBudgetLimits()
	limits.textBytes = 16
	document, err := parseMIMEDocumentWithLimits(
		context.Background(), strings.NewReader(fixture), false, false, false, limits,
	)
	if err != nil {
		t.Fatalf("parseMIMEDocumentWithLimits() error = %v", err)
	}
	if document.Complete || !containsMissingPart(document, "mime:budget:text_bytes") {
		t.Fatalf("document = %#v, want incomplete text budget diagnostic", document)
	}
	if document.Content == "" || len(document.Content) >= len(strings.Repeat("small\n\n", 32)) {
		t.Fatalf("Content length = %d, want bounded partial text", len(document.Content))
	}
	if document.budget.textBytes > limits.textBytes {
		t.Fatalf("decoded text bytes = %d, want <= %d", document.budget.textBytes, limits.textBytes)
	}
}

func TestParseMIMEEnforcesVisitedPartBudget(t *testing.T) {
	fixture := multipartTextFixture(32, "x")
	limits := defaultMIMEParseBudgetLimits()
	limits.parts = 4
	limits.textBytes = 1 << 20
	document, err := parseMIMEDocumentWithLimits(
		context.Background(), strings.NewReader(fixture), false, false, false, limits,
	)
	if err != nil {
		t.Fatalf("parseMIMEDocumentWithLimits() error = %v", err)
	}
	if document.Complete || !containsMissingPart(document, "mime:budget:parts") {
		t.Fatalf("document = %#v, want incomplete part budget diagnostic", document)
	}
	if document.budget.parts > limits.parts {
		t.Fatalf("visited parts = %d, want <= %d", document.budget.parts, limits.parts)
	}
}

func TestParseMIMEEnforcesNestingDepthBeforePathAllocation(t *testing.T) {
	fixture := nestedMultipartFixture(8)
	limits := defaultMIMEParseBudgetLimits()
	limits.depth = 3
	limits.textBytes = 1 << 20
	document, err := parseMIMEDocumentWithLimits(
		context.Background(), strings.NewReader(fixture), false, false, false, limits,
	)
	if err != nil {
		t.Fatalf("parseMIMEDocumentWithLimits() error = %v", err)
	}
	if document.Complete || !containsMissingPart(document, "mime:budget:depth") {
		t.Fatalf("document = %#v, want incomplete depth budget diagnostic", document)
	}
}

func TestParseMIMEEnforcesRetainedMetadataBudget(t *testing.T) {
	fixture := multipartAttachmentFixture(8)
	limits := defaultMIMEParseBudgetLimits()
	limits.metadata = mimePartMetadataBytes("1", "attachment-0.bin", "application/octet-stream", false) + 1
	limits.textBytes = 1 << 20
	document, err := parseMIMEDocumentWithLimits(
		context.Background(), strings.NewReader(fixture), false, false, false, limits,
	)
	if err != nil {
		t.Fatalf("parseMIMEDocumentWithLimits() error = %v", err)
	}
	if document.Complete || !containsMissingPart(document, "mime:budget:metadata_bytes") {
		t.Fatalf("document = %#v, want incomplete metadata budget diagnostic", document)
	}
	if len(document.Parts) != 1 || document.budget.metadata > limits.metadata {
		t.Fatalf("parts = %#v, metadata = %d, want one bounded entry", document.Parts, document.budget.metadata)
	}
}

func TestParseMIMEEnforcesRawByteBudget(t *testing.T) {
	fixture := "Content-Type: text/plain\r\n\r\n" + strings.Repeat("x", 128)
	limits := defaultMIMEParseBudgetLimits()
	limits.rawBytes = int64(len("Content-Type: text/plain\r\n\r\n")) + 16
	limits.textBytes = 1 << 20
	document, err := parseMIMEDocumentWithLimits(
		context.Background(), strings.NewReader(fixture), false, false, false, limits,
	)
	if err != nil {
		t.Fatalf("parseMIMEDocumentWithLimits() error = %v", err)
	}
	if document.Complete || !containsMissingPart(document, "mime:budget:raw_bytes") {
		t.Fatalf("document = %#v, want incomplete raw-byte budget diagnostic", document)
	}
	if document.budget.rawBytes > limits.rawBytes {
		t.Fatalf("raw bytes = %d, want <= %d", document.budget.rawBytes, limits.rawBytes)
	}
}

func TestParseMIMECancellationClosesReaderAndClearsHashProof(t *testing.T) {
	reader := newBlockingMIMEReader(
		"Content-Type: application/octet-stream\r\n"+
			"Content-Disposition: attachment; filename=blocked.bin\r\n"+
			"Content-Transfer-Encoding: base64\r\n\r\n",
		"YmxvY2tlZA==\r\n",
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		document mimeDocument
		err      error
	}
	done := make(chan result, 1)
	go func() {
		document, err := parseMIMEDocumentWithContext(ctx, reader, false, true, false)
		done <- result{document: document, err: err}
	}()
	select {
	case <-reader.bodyStarted:
	case <-time.After(time.Second):
		t.Fatal("MIME parser did not enter attachment drain")
	}
	cancel()
	select {
	case outcome := <-done:
		if !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("parse error = %v, want context.Canceled", outcome.err)
		}
		if outcome.document.Complete || !containsMissingPart(outcome.document, "mime:canceled") {
			t.Fatalf("document = %#v, want canceled incomplete result", outcome.document)
		}
		part := outcome.document.Parts["1"]
		if part.Complete || part.SHA256 != "" {
			t.Fatalf("attachment part = %#v, want no complete hash proof", part)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled MIME parser did not return promptly")
	}
	select {
	case <-reader.closed:
	default:
		t.Fatal("cancellation did not close the MIME reader")
	}
}

func BenchmarkParseMIMEAggregateBudget(b *testing.B) {
	fixture := multipartTextFixture(256, "benchmark body")
	b.ReportAllocs()
	b.SetBytes(int64(len(fixture)))
	for b.Loop() {
		document, err := parseMIMEDocumentWithContext(
			context.Background(), strings.NewReader(fixture), false, false, false,
		)
		if err != nil || !document.Complete {
			b.Fatalf("parseMIMEDocumentWithContext() = %#v, error = %v", document, err)
		}
	}
}

func containsMissingPart(document mimeDocument, identifier string) bool {
	for _, existing := range document.MissingParts {
		if existing == identifier {
			return true
		}
	}
	return false
}

func multipartTextFixture(parts int, text string) string {
	var source strings.Builder
	fmt.Fprintf(&source, "Content-Type: multipart/mixed; boundary=text-boundary\r\n\r\n")
	for index := 0; index < parts; index++ {
		fmt.Fprintf(&source, "--text-boundary\r\nContent-Type: text/plain\r\n\r\n%s\r\n", text)
	}
	source.WriteString("--text-boundary--\r\n")
	return source.String()
}

func multipartAttachmentFixture(parts int) string {
	var source strings.Builder
	source.WriteString("Content-Type: multipart/mixed; boundary=attachment-boundary\r\n\r\n")
	for index := 0; index < parts; index++ {
		fmt.Fprintf(&source,
			"--attachment-boundary\r\nContent-Type: application/octet-stream\r\n"+
				"Content-Disposition: attachment; filename=attachment-%d.bin\r\n\r\nbytes-%d\r\n",
			index, index,
		)
	}
	source.WriteString("--attachment-boundary--\r\n")
	return source.String()
}

func nestedMultipartFixture(depth int) string {
	var source strings.Builder
	fmt.Fprintf(&source, "Content-Type: multipart/mixed; boundary=nested-0\r\n\r\n")
	for index := 0; index < depth; index++ {
		fmt.Fprintf(&source,
			"--nested-%d\r\nContent-Type: multipart/mixed; boundary=nested-%d\r\n\r\n",
			index, index+1,
		)
	}
	source.WriteString("--nested-" + fmt.Sprint(depth) + "\r\nContent-Type: text/plain\r\n\r\nleaf\r\n")
	for index := depth; index >= 0; index-- {
		fmt.Fprintf(&source, "--nested-%d--\r\n", index)
	}
	return source.String()
}

type blockingMIMEReader struct {
	header      []byte
	body        []byte
	offset      int
	bodyStarted chan struct{}
	release     chan struct{}
	closed      chan struct{}
	startOnce   sync.Once
	closeOnce   sync.Once
}

func newBlockingMIMEReader(header, body string) *blockingMIMEReader {
	return &blockingMIMEReader{
		header:      []byte(header),
		body:        []byte(body),
		bodyStarted: make(chan struct{}),
		release:     make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

func (r *blockingMIMEReader) Read(buffer []byte) (int, error) {
	if r.offset < len(r.header) {
		read := copy(buffer, r.header[r.offset:])
		r.offset += read
		return read, nil
	}
	r.startOnce.Do(func() { close(r.bodyStarted) })
	<-r.release
	if r.offset-len(r.header) >= len(r.body) {
		return 0, io.EOF
	}
	read := copy(buffer, r.body[r.offset-len(r.header):])
	r.offset += read
	return read, nil
}

func (r *blockingMIMEReader) Close() error {
	r.closeOnce.Do(func() {
		close(r.closed)
		close(r.release)
	})
	return nil
}
