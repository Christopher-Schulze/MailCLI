package mail

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"mime/quotedprintable"
	"os"
	"strings"
	"testing"
)

func TestStreamingAlternativePreservesEncodedBytes(t *testing.T) {
	for _, body := range []string{"", "ordinary body", "trailing \t\nnext\r\nlast ", strings.Repeat("x", 32767) + "\r\nend", strings.Repeat("x", 32767) + "é", strings.Repeat("é", 2<<20)} {
		for _, htmlBody := range []string{"", "<p>HTML &amp; text</p>\n"} {
			var got, want bytes.Buffer
			draft := Draft{Body: body, BodyHTML: htmlBody}
			if err := writeAlternativeMultipart(context.Background(), &got, "boundary", draft); err != nil {
				t.Fatal(err)
			}
			for _, part := range []struct{ mediaType, body string }{{"text/plain", body}, {"text/html", htmlBody}} {
				if part.mediaType == "text/html" && part.body == "" {
					continue
				}
				want.WriteString("--boundary\r\nContent-Type: " + part.mediaType + "; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
				encoder := quotedprintable.NewWriter(&want)
				if _, err := encoder.Write([]byte(part.body)); err != nil {
					t.Fatal(err)
				}
				if err := encoder.Close(); err != nil {
					t.Fatal(err)
				}
				want.WriteString("\r\n")
			}
			want.WriteString("--boundary--\r\n")
			if !bytes.Equal(got.Bytes(), want.Bytes()) {
				t.Fatalf("encoding changed: body=%d html=%d got=%d want=%d", len(body), len(htmlBody), got.Len(), want.Len())
			}
		}
	}
}

type limitedComposerWriter struct {
	remaining int
	cause     error
	written   int
}

func (writer *limitedComposerWriter) Write(value []byte) (int, error) {
	count := min(len(value), writer.remaining)
	writer.remaining -= count
	writer.written += count
	if count < len(value) {
		return count, writer.cause
	}
	return count, nil
}

func TestStreamingAlternativePropagatesWriteAndCloseFailures(t *testing.T) {
	cause := errors.New("injected spool failure")
	headerBytes := len("--boundary\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	for _, test := range []struct {
		name, body string
		remaining  int
		cause      error
	}{
		{"header", "body", 1, cause},
		{"encoder close", "body", headerBytes + 1, cause},
		{"encoder write", strings.Repeat("x", 40000), headerBytes + 100, cause},
		{"body separator", "body", headerBytes + 5, cause},
		{"section terminator", "body", headerBytes + 9, cause},
		{"short header without error", "body", 1, nil},
		{"short encoder close without error", "body", headerBytes + 1, nil},
		{"short encoder write without error", strings.Repeat("x", 40000), headerBytes + 100, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			writer := &limitedComposerWriter{remaining: test.remaining, cause: test.cause}
			err := writeAlternativeMultipart(context.Background(), writer, "boundary", Draft{Body: test.body})
			want := test.cause
			if want == nil {
				want = io.ErrShortWrite
			}
			if !errors.Is(err, want) || writer.written != test.remaining {
				t.Fatalf("written=%d error=%v, want %v", writer.written, err, want)
			}
		})
	}
}

func TestBodySpoolingCancellationRemovesPrivateFile(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("TMPDIR", directory)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &draftListBoundaryContext{Context: base, trigger: 8, action: cancel}
	message, err := ComposeMessageSpoolContext(ctx, benchmarkDraft(strings.Repeat("é", 2<<20)), "<cancel@example.com>")
	if message != nil || !errors.Is(err, context.Canceled) || ctx.checks < 8 {
		t.Fatalf("message=%v error=%v checks=%d", message, err, ctx.checks)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("canceled composition retained %d spool files", len(entries))
	}
}

func BenchmarkLargeBodySpool(b *testing.B) {
	for _, fixture := range []struct{ name, body string }{
		{"4MiB_ascii", strings.Repeat("x", 4<<20)},
		{"4MiB_unicode", strings.Repeat("é", 2<<20)},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			draft := benchmarkDraft(fixture.body)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				message, err := composeDraftSpool(context.Background(), draft, "<perf@example.com>")
				if err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(message.Size()), "spool_bytes")
				if err := message.Remove(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkBodySpoolBuffer(b *testing.B) {
	draft := benchmarkDraft(strings.Repeat("é", 2<<20))
	for _, size := range []struct {
		name  string
		bytes int
	}{
		{"32KiB", 32 * 1024}, {"128KiB", 128 * 1024}, {"256KiB", 256 * 1024}, {"512KiB", 512 * 1024}, {"1MiB", 1024 * 1024},
	} {
		b.Run(size.name, func(b *testing.B) {
			file, err := os.CreateTemp(b.TempDir(), "body-")
			if err != nil {
				b.Fatal(err)
			}
			defer func() {
				if err := file.Close(); err != nil {
					b.Error(err)
				}
			}()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := file.Truncate(0); err != nil {
					b.Fatal(err)
				}
				if _, err := file.Seek(0, io.SeekStart); err != nil {
					b.Fatal(err)
				}
				writer := bufio.NewWriterSize(file, size.bytes)
				if err := writeAlternativeMultipart(context.Background(), writer, "boundary", draft); err != nil {
					b.Fatal(err)
				}
				if err := writer.Flush(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
