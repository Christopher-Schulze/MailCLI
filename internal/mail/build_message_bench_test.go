package mail

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// benchmarkAttachmentFile materializes a deterministic attachment on disk so
// BuildMessage exercises the real file-loading and base64 pipeline.
func benchmarkAttachmentFile(b *testing.B, size int) (DraftAttachment, func()) {
	b.Helper()
	directory := b.TempDir()
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		b.Fatalf("generate attachment payload: %v", err)
	}
	path := filepath.Join(directory, "benchmark-attachment.bin")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		b.Fatalf("write attachment payload: %v", err)
	}
	sum := sha256.Sum256(payload)
	return DraftAttachment{
		Path:   path,
		Size:   int64(size),
		SHA256: hex.EncodeToString(sum[:]),
	}, func() {}
}

func benchmarkBody(size int) string {
	sentence := []byte("The quick brown fox jumps over the lazy dog while performance auditing runs. ")
	var buffer bytes.Buffer
	for buffer.Len() < size {
		buffer.Write(sentence)
	}
	return buffer.String()[:size]
}

func benchmarkDraft(body string, attachments ...DraftAttachment) Draft {
	return Draft{
		Kind:        DraftKindNew,
		From:        "ch.schulze90@gmail.com",
		To:          []Recipient{{Address: "recipient@example.com"}},
		Subject:     "Performance audit benchmark message",
		Body:        body,
		Attachments: attachments,
	}
}

// BenchmarkBuildMessagePlain4KiB measures pure CPU and allocation cost of RFC
// 5322 composition for a realistic 4 KiB plain-text body without attachments.
func BenchmarkBuildMessagePlain4KiB(b *testing.B) {
	draft := benchmarkDraft(benchmarkBody(4 * 1024))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := BuildMessage(draft, "<benchmark@mailcli>"); err != nil {
			b.Fatalf("build message: %v", err)
		}
	}
}

// BenchmarkBuildMessageAttachment64KiB measures composition including a
// realistic 64 KiB binary attachment (disk read + base64 + MIME framing).
func BenchmarkBuildMessageAttachment64KiB(b *testing.B) {
	attachment, cleanup := benchmarkAttachmentFile(b, 64*1024)
	defer cleanup()
	draft := benchmarkDraft(benchmarkBody(4*1024), attachment)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := BuildMessage(draft, "<benchmark@mailcli>"); err != nil {
			b.Fatalf("build message: %v", err)
		}
	}
}

// BenchmarkBuildMessageAttachment1MiB stresses the base64 body encoding path
// with a larger attachment.
func BenchmarkBuildMessageAttachment1MiB(b *testing.B) {
	attachment, cleanup := benchmarkAttachmentFile(b, 1024*1024)
	defer cleanup()
	draft := benchmarkDraft(benchmarkBody(4*1024), attachment)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := BuildMessage(draft, "<benchmark@mailcli>"); err != nil {
			b.Fatalf("build message: %v", err)
		}
	}
}

// BenchmarkBuildMessageAttachment64MiB measures the current in-memory
// composition peak at the maximum raw attachment size used by the send path.
func BenchmarkBuildMessageAttachment64MiB(b *testing.B) {
	attachment, cleanup := benchmarkAttachmentFile(b, 64*1024*1024)
	defer cleanup()
	draft := benchmarkDraft(benchmarkBody(4*1024), attachment)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := BuildMessage(draft, "<benchmark@mailcli>"); err != nil {
			b.Fatalf("build message: %v", err)
		}
	}
}

// BenchmarkSendAttachmentDoubleRead64MiB models the pre-059 send path:
// fingerprinting reads the file once and BuildMessage reads it again.
func BenchmarkSendAttachmentDoubleRead64MiB(b *testing.B) {
	attachment, cleanup := benchmarkAttachmentFile(b, 64*1024*1024)
	defer cleanup()
	draft := benchmarkDraft(benchmarkBody(4*1024), attachment)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := verifyDraftAttachments(draft.Attachments); err != nil {
			b.Fatalf("verify attachments: %v", err)
		}
		if _, err := BuildMessage(draft, "<benchmark@mailcli>"); err != nil {
			b.Fatalf("build message: %v", err)
		}
	}
}

// BenchmarkSendAttachmentSingleRead64MiB measures the TASK 059 send path:
// verification and composition share the one in-memory attachment snapshot.
func BenchmarkSendAttachmentSingleRead64MiB(b *testing.B) {
	attachment, cleanup := benchmarkAttachmentFile(b, 64*1024*1024)
	defer cleanup()
	draft := benchmarkDraft(benchmarkBody(4*1024), attachment)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		loaded, err := verifyAndLoadAttachments(draft)
		if err != nil {
			b.Fatalf("verify and load attachments: %v", err)
		}
		if _, err := buildMessageWithAttachments(draft, "<benchmark@mailcli>", loaded); err != nil {
			b.Fatalf("build message: %v", err)
		}
	}
}
