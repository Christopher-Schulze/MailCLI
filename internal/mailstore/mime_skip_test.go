package mailstore

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func skipModeFixture() []byte {
	payload := bytes.Repeat([]byte{0xAB, 0xCD, 0xEF, 0x01}, 256)
	encoded := base64.StdEncoding.EncodeToString(payload)
	var source bytes.Buffer
	source.WriteString("Content-Type: multipart/mixed; boundary=skipb\r\n\r\n" +
		"--skipb\r\nContent-Type: text/plain\r\n\r\nSearchable body text\r\n" +
		"--skipb\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=data.bin\r\n\r\n" +
		encoded + "\r\n--skipb--\r\n")
	return source.Bytes()
}

func TestSkipModeKeepsTextSkipsAttachmentBodies(t *testing.T) {
	t.Parallel()

	document, err := parseMIMEDocument(bytes.NewReader(skipModeFixture()), false, false, true)
	if err != nil {
		t.Fatalf("parseMIMEDocument() error = %v", err)
	}
	if document.Content != "Searchable body text" {
		t.Fatalf("Content = %q, want searchable text", document.Content)
	}
	if !document.Complete {
		t.Fatal("Complete = false, want true for a full source")
	}
	part, exists := document.Parts["2"]
	if !exists {
		t.Fatalf("Parts = %#v, want attachment at 2", document.Parts)
	}
	if part.Name != "data.bin" {
		t.Errorf("Name = %q, want data.bin", part.Name)
	}
	if part.Size != -1 {
		t.Errorf("Size = %d, want -1 (unverified in skip mode)", part.Size)
	}
}

func TestSkipModeMatchesFullParseOnTextAndNames(t *testing.T) {
	t.Parallel()

	fixture := skipModeFixture()
	skipped, err := parseMIMEDocument(bytes.NewReader(fixture), false, false, true)
	if err != nil {
		t.Fatalf("skip parse error = %v", err)
	}
	full, err := parseMIMEDocument(bytes.NewReader(fixture), false, false, false)
	if err != nil {
		t.Fatalf("full parse error = %v", err)
	}
	if skipped.Content != full.Content {
		t.Errorf("skip Content = %q, full Content = %q", skipped.Content, full.Content)
	}
	if len(skipped.Parts) != len(full.Parts) {
		t.Fatalf("skip Parts = %d, full Parts = %d", len(skipped.Parts), len(full.Parts))
	}
	for id, want := range full.Parts {
		got, exists := skipped.Parts[id]
		if !exists {
			t.Fatalf("skip Parts missing %q", id)
		}
		if got.Name != want.Name || got.MIMEType != want.MIMEType {
			t.Errorf("skip Part %q = %#v, full = %#v", id, got, want)
		}
	}
	if skipped.Complete != full.Complete {
		t.Errorf("skip Complete = %v, full Complete = %v", skipped.Complete, full.Complete)
	}
}

func TestSkipModeMarksPartialSourceIncomplete(t *testing.T) {
	t.Parallel()

	document, err := parseMIMEDocument(bytes.NewReader(skipModeFixture()), true, false, true)
	if err != nil {
		t.Fatalf("parseMIMEDocument() error = %v", err)
	}
	if document.Content != "Searchable body text" {
		t.Fatalf("Content = %q, want searchable text", document.Content)
	}
	if document.Complete {
		t.Error("Complete = true, want false for unverified attachment on a partial source")
	}
}

func skipBenchFixture(b *testing.B, attachmentBytes int) []byte {
	b.Helper()
	payload := bytes.Repeat([]byte{0xAB, 0xCD, 0xEF, 0x01}, attachmentBytes/4)
	encoded := base64.StdEncoding.EncodeToString(payload)
	var source bytes.Buffer
	source.WriteString("Content-Type: multipart/mixed; boundary=benchb\r\n\r\n" +
		"--benchb\r\nContent-Type: text/plain\r\n\r\nBench body text\r\n" +
		"--benchb\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=big.bin\r\n\r\n" +
		encoded + "\r\n--benchb--\r\n")
	return source.Bytes()
}

func BenchmarkSkipVsFullAttachment1MiB(b *testing.B) {
	fixture := skipBenchFixture(b, 1024*1024)
	b.Run("skip", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(fixture)))
		for b.Loop() {
			if _, err := parseMIMEDocument(bytes.NewReader(fixture), false, false, true); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("full", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(fixture)))
		for b.Loop() {
			if _, err := parseMIMEDocument(bytes.NewReader(fixture), false, false, false); err != nil {
				b.Fatal(err)
			}
		}
	})
}
