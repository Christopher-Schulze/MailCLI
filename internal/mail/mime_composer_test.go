package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const composerGoldenMessageID = "<build-0001@mailcli.local>"

var (
	composerGoldenDatePattern      = regexp.MustCompile(`(?m)^Date: .*$`)
	composerGoldenMessageIDPattern = regexp.MustCompile(`(?m)^Message-ID: .*$`)
	composerGoldenBoundaryPattern  = regexp.MustCompile(`=_[0-9a-f]{32}`)
)

type scriptedAttachmentReader struct {
	chunks [][]byte
	index  int
	bytes  int
}

func (r *scriptedAttachmentReader) Read(buffer []byte) (int, error) {
	if r.index == len(r.chunks) {
		return 0, io.EOF
	}
	chunk := r.chunks[r.index]
	read := copy(buffer, chunk)
	r.bytes += read
	if read == len(chunk) {
		r.index++
	} else {
		r.chunks[r.index] = chunk[read:]
	}
	return read, nil
}

func (r *scriptedAttachmentReader) Close() error { return nil }

func TestBuildMessageGolden(t *testing.T) {
	directory := t.TempDir()
	notes := composerGoldenAttachment(t, directory, "notes.txt", "meeting notes\nline two\n")
	resume := composerGoldenAttachment(t, directory, "résumé final.txt", "curriculum vitae with ünïcode\n")

	tests := []struct {
		name  string
		draft Draft
	}{
		{
			name: "plain-only",
			draft: Draft{
				Kind: DraftKindNew, From: "sender@example.com",
				To:   []Recipient{{Address: "alice@example.com"}},
				Body: "Hello Alice,\n\nHere is the update.\n\nBest,\nSender\n",
			},
		},
		{
			name: "plain-html",
			draft: Draft{
				Kind: DraftKindNew, From: "sender@example.com",
				To: []Recipient{{Address: "alice@example.com"}}, Subject: "Styled update",
				Body: "Hello Alice,\n\nHere is the update.\n", BodyFormat: DraftBodyHTML,
				BodyHTML: "<p>Hello Alice,</p><p>Here is the <strong>update</strong>.</p>",
			},
		},
		{
			name: "attachments",
			draft: Draft{
				Kind: DraftKindNew, From: "sender@example.com",
				To:      []Recipient{{Name: "Alice", Address: "alice@example.com"}},
				Subject: "Documents attached", Body: "Both documents are attached.\n",
				Attachments: []DraftAttachment{notes, resume},
			},
		},
		{
			name: "reply-threading",
			draft: Draft{
				Kind: DraftKindReply, SourceMessageID: "<original-123@example.com>",
				SourceReferences: "<first-1@example.com> <second-2@example.com>",
				From:             "sender@example.com", To: []Recipient{{Address: "original@example.com"}},
				Subject: "Re: Original subject", Body: "Thanks, understood.\n",
			},
		},
		{
			name: "bcc-excluded",
			draft: Draft{
				Kind: DraftKindNew, From: "sender@example.com",
				To:      []Recipient{{Name: "Alice", Address: "alice@example.com"}},
				CC:      []Recipient{{Name: "Copy", Address: "copy@example.com"}},
				BCC:     []Recipient{{Name: "Secret", Address: "secret@example.com"}},
				Subject: "Visible to To and CC only", Body: "The BCC recipient must never appear.\n",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message, err := BuildMessage(test.draft, composerGoldenMessageID)
			if err != nil {
				t.Fatalf("BuildMessage() error = %v", err)
			}
			messageText := string(message)
			if !strings.HasSuffix(messageText, composerCRLF) ||
				strings.Count(messageText, "\n") != strings.Count(messageText, composerCRLF) {
				t.Fatal("composed message contains a bare LF or lacks its final CRLF")
			}
			got := normalizeComposerGolden(message)
			goldenPath := filepath.Join("testdata", "golden", test.name+".golden")
			golden, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("ReadFile(%q) error = %v", goldenPath, err)
			}
			want := strings.ReplaceAll(string(golden), composerCRLF, "\n")
			if got != want {
				t.Fatalf("message mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

func composerGoldenAttachment(t *testing.T, directory, name, content string) DraftAttachment {
	t.Helper()
	path := filepath.Join(directory, name)
	payload := []byte(content)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
	digest := sha256.Sum256(payload)
	return DraftAttachment{Path: path, Size: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])}
}

func normalizeComposerGolden(message []byte) string {
	normalized := composerGoldenDatePattern.ReplaceAllString(string(message), "Date: <DATE>")
	normalized = composerGoldenMessageIDPattern.ReplaceAllString(normalized, "Message-ID: <MESSAGE-ID>")
	normalized = composerGoldenBoundaryPattern.ReplaceAllString(normalized, "<BOUNDARY>")
	return strings.ReplaceAll(normalized, composerCRLF, "\n")
}

func TestComposeMessageSpoolContextStopsCanceledWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ComposeMessageSpoolContext(ctx, Draft{}, "<canceled@example.com>")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ComposeMessageSpoolContext() error = %v, want context.Canceled", err)
	}
}

func TestComposeMessageSpoolReadsAttachmentOnceAndChecksFinalHash(t *testing.T) {
	payload := []byte("final attachment bytes")
	sum := sha256.Sum256(payload)
	reader := &scriptedAttachmentReader{chunks: [][]byte{payload}}
	opens := 0
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Attachment", Body: "Body", Attachments: []DraftAttachment{{
			Path: "brief.txt", Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:]),
		}},
	}
	message, err := composeMessageSpoolContext(context.Background(), draft, "<once@example.com>", func(string) (io.ReadCloser, error) {
		opens++
		return reader, nil
	})
	if err != nil {
		t.Fatalf("composeMessageSpoolContext() error = %v", err)
	}
	defer func() { _ = message.Remove() }()
	if opens != 1 || reader.bytes != len(payload) {
		t.Fatalf("attachment reads = opens %d, bytes %d; want one open and %d bytes", opens, reader.bytes, len(payload))
	}
}

func TestComposeMessageSpoolRejectsMutationDuringFinalRead(t *testing.T) {
	first := []byte("original-")
	originalTail := []byte("content")
	changedTail := []byte("CHANGED")
	original := append(append([]byte(nil), first...), originalTail...)
	sum := sha256.Sum256(original)
	reader := &scriptedAttachmentReader{chunks: [][]byte{first, changedTail}}
	opens := 0
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Attachment", Body: "Body", Attachments: []DraftAttachment{{
			Path: "brief.txt", Size: int64(len(original)), SHA256: hex.EncodeToString(sum[:]),
		}},
	}
	_, err := composeMessageSpoolContext(context.Background(), draft, "<changed@example.com>", func(string) (io.ReadCloser, error) {
		opens++
		return reader, nil
	})
	if err == nil || !strings.Contains(err.Error(), "changed after review") {
		t.Fatalf("composeMessageSpoolContext() error = %v, want mutation rejection", err)
	}
	if opens != 1 || reader.bytes != len(original) {
		t.Fatalf("mutation read = opens %d, bytes %d; want one open and %d bytes", opens, reader.bytes, len(original))
	}
}

func TestComposerErrorErrorWithInner(t *testing.T) {
	inner := errors.New("disk full")
	err := &ComposerError{Message: "write attachment", Err: inner}
	got := err.Error()
	want := "write attachment: disk full"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestComposerErrorErrorWithoutInner(t *testing.T) {
	err := &ComposerError{Message: "message id is required"}
	got := err.Error()
	if got != "message id is required" {
		t.Errorf("Error() = %q, want message id is required", got)
	}
}

func TestComposerErrorUnwrap(t *testing.T) {
	inner := errors.New("inner error")
	err := &ComposerError{Message: "outer", Err: inner}
	if unwrapped := err.Unwrap(); unwrapped != inner {
		t.Errorf("Unwrap() = %v, want %v", unwrapped, inner)
	}
}

func TestComposerErrorUnwrapNil(t *testing.T) {
	err := &ComposerError{Message: "no inner"}
	if unwrapped := err.Unwrap(); unwrapped != nil {
		t.Errorf("Unwrap() = %v, want nil", unwrapped)
	}
}

func TestThreadReferencesEmpty(t *testing.T) {
	got := threadReferences("", "<msg-123@example.com>")
	if got != "<msg-123@example.com>" {
		t.Errorf("threadReferences(empty) = %q, want source message ID only", got)
	}
}

func TestThreadReferencesWithPrior(t *testing.T) {
	prior := "<msg-1@example.com> <msg-2@example.com>"
	got := threadReferences(prior, "<msg-3@example.com>")
	want := "<msg-1@example.com> <msg-2@example.com> <msg-3@example.com>"
	if got != want {
		t.Errorf("threadReferences(with prior) = %q, want %q", got, want)
	}
}

func TestThreadReferencesWithWhitespace(t *testing.T) {
	got := threadReferences("  <msg-1@example.com>  ", "<msg-2@example.com>")
	want := "<msg-1@example.com> <msg-2@example.com>"
	if got != want {
		t.Errorf("threadReferences(whitespace) = %q, want %q", got, want)
	}
}

func TestBuildMessageUsesVerifiedSpoolForAttachments(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "invoice.pdf")
	payload := []byte("invoice-bytes")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	sum := sha256.Sum256(payload)
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Attachment", Body: "Body",
		Attachments: []DraftAttachment{{Path: path, Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:])}},
	}
	message, err := BuildMessage(draft, "<test@example.com>")
	if err != nil {
		t.Fatalf("BuildMessage() error = %v", err)
	}
	messageText := string(message)
	if !contains(messageText, "Content-Disposition: attachment; filename=invoice.pdf") {
		t.Fatal("verified composition missing attachment disposition")
	}
	if !contains(messageText, "aW52b2ljZS1ieXRlcw==") {
		t.Fatal("verified composition missing attachment payload")
	}
}

func TestBuildMessageRejectsSameSizeAttachmentReplacement(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "reviewed.txt")
	original := []byte("original payload")
	replacement := []byte("changed payload!")
	if len(original) != len(replacement) {
		t.Fatalf("test payload sizes differ: %d != %d", len(original), len(replacement))
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("write original attachment: %v", err)
	}
	sum := sha256.Sum256(original)
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Attachment", Body: "Body",
		Attachments: []DraftAttachment{{Path: path, Size: int64(len(original)), SHA256: hex.EncodeToString(sum[:])}},
	}
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatalf("replace attachment: %v", err)
	}
	_, err := BuildMessage(draft, "<replacement@example.com>")
	if err == nil || !strings.Contains(err.Error(), "changed after review") {
		t.Fatalf("BuildMessage() error = %v, want same-size replacement rejection", err)
	}
}

func TestBuildMessageWithHTMLBody(t *testing.T) {
	draft := Draft{
		From:       "sender@example.com",
		To:         []Recipient{{Address: "recipient@example.com"}},
		Subject:    "HTML Test",
		Body:       "Plain text",
		BodyHTML:   "<p>HTML text</p>",
		BodyFormat: DraftBodyHTML,
	}
	msg, err := BuildMessage(draft, "<test@example.com>")
	if err != nil {
		t.Fatalf("BuildMessage error = %v", err)
	}
	msgStr := string(msg)
	if !contains(msgStr, "text/html") {
		t.Error("message missing text/html content type")
	}
	if !contains(msgStr, "text/plain") {
		t.Error("message missing text/plain content type")
	}
}

func TestBuildMessageReplyWithThreading(t *testing.T) {
	draft := Draft{
		From:             "sender@example.com",
		To:               []Recipient{{Address: "recipient@example.com"}},
		Subject:          "Re: Original",
		Body:             "Reply body",
		Kind:             DraftKindReply,
		SourceMessageID:  "<original@example.com>",
		SourceReferences: "<msg-1@example.com>",
	}
	msg, err := BuildMessage(draft, "<reply@example.com>")
	if err != nil {
		t.Fatalf("BuildMessage error = %v", err)
	}
	msgStr := string(msg)
	if !contains(msgStr, "In-Reply-To: <original@example.com>") {
		t.Error("message missing In-Reply-To header")
	}
	if !contains(msgStr, "References: <msg-1@example.com> <original@example.com>") {
		t.Error("message missing References header")
	}
}

func TestComposeMessageSpoolCanBeRemovedAfterReplay(t *testing.T) {
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Spool", Body: "Body",
	}
	message, err := ComposeMessageSpool(draft, "<spool@example.com>")
	if err != nil {
		t.Fatalf("ComposeMessageSpool() error = %v", err)
	}
	reader, err := message.Open()
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if message.Size() <= 0 || message.MessageID() != "<spool@example.com>" {
		t.Fatalf("message metadata = size %d, id %q", message.Size(), message.MessageID())
	}
	if err := message.Remove(); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := message.Open(); err == nil {
		t.Fatal("Open() after Remove() succeeded")
	}
}

func TestBuildMessageForwardWithThreading(t *testing.T) {
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Fwd: Original", Body: "Forward body", Kind: DraftKindForward,
		SourceMessageID: "<original@example.com>", SourceReferences: "<msg-1@example.com>",
	}
	msg, err := BuildMessage(draft, "<forward@example.com>")
	if err != nil {
		t.Fatalf("BuildMessage error = %v", err)
	}
	msgStr := string(msg)
	if !contains(msgStr, "In-Reply-To: <original@example.com>") ||
		!contains(msgStr, "References: <msg-1@example.com> <original@example.com>") {
		t.Fatalf("forward message missing threading headers: %s", msgStr)
	}
}

func TestBuildMessageEmptyMessageID(t *testing.T) {
	draft := Draft{From: "sender@example.com", To: []Recipient{{Address: "r@example.com"}}}
	_, err := BuildMessage(draft, "")
	if err == nil {
		t.Fatal("BuildMessage error = nil, want message id required")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
