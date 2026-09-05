package mail

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestLoadComposerAttachmentsEmpty(t *testing.T) {
	got, err := loadComposerAttachments(nil)
	if err != nil {
		t.Fatalf("loadComposerAttachments(nil) error = %v", err)
	}
	if got != nil {
		t.Errorf("loadComposerAttachments(nil) = %v, want nil", got)
	}
}

func TestLoadComposerAttachmentsSingleFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(path, []byte("test content"), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	got, err := loadComposerAttachments([]DraftAttachment{{Path: path}})
	if err != nil {
		t.Fatalf("loadComposerAttachments error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d attachments, want 1", len(got))
	}
	if got[0].filename != "test.txt" {
		t.Errorf("filename = %q, want test.txt", got[0].filename)
	}
	if string(got[0].data) != "test content" {
		t.Errorf("data = %q, want test content", string(got[0].data))
	}
	if got[0].contentType != "text/plain; charset=utf-8" {
		t.Errorf("contentType = %q, want text/plain; charset=utf-8", got[0].contentType)
	}
}

func TestLoadComposerAttachmentsMultipleFilesParallel(t *testing.T) {
	dir := t.TempDir()
	paths := make([]DraftAttachment, 3)
	for i := 0; i < 3; i++ {
		path := filepath.Join(dir, "file"+string(rune('A'+i))+".txt")
		content := "content-" + string(rune('A'+i))
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile error = %v", err)
		}
		paths[i] = DraftAttachment{Path: path}
	}
	got, err := loadComposerAttachments(paths)
	if err != nil {
		t.Fatalf("loadComposerAttachments error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d attachments, want 3", len(got))
	}
	// Verify all files loaded correctly (order preserved)
	for i, att := range got {
		expected := "content-" + string(rune('A'+i))
		if string(att.data) != expected {
			t.Errorf("attachment %d data = %q, want %q", i, string(att.data), expected)
		}
	}
}

func TestLoadComposerAttachmentsMissingFile(t *testing.T) {
	_, err := loadComposerAttachments([]DraftAttachment{{Path: "/nonexistent/file.txt"}})
	if err == nil {
		t.Fatal("loadComposerAttachments error = nil, want file not found")
	}
	composerErr, ok := err.(*ComposerError)
	if !ok {
		t.Fatalf("error type = %T, want *ComposerError", err)
	}
	if composerErr.Message != "read draft attachment file.txt" {
		t.Errorf("Message = %q, want read draft attachment file.txt", composerErr.Message)
	}
}

func TestLoadComposerAttachmentsUnknownExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.xyzunknown")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	got, err := loadComposerAttachments([]DraftAttachment{{Path: path}})
	if err != nil {
		t.Fatalf("loadComposerAttachments error = %v", err)
	}
	if got[0].contentType != "application/octet-stream" {
		t.Errorf("contentType = %q, want application/octet-stream", got[0].contentType)
	}
}

func TestBuildMessageWithPreloadedAttachmentsPreservesWireFormat(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "invoice.pdf")
	if err := os.WriteFile(path, []byte("invoice-bytes"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Attachment", Body: "Body",
		Attachments: []DraftAttachment{{Path: path}},
	}
	legacy, err := BuildMessage(draft, "<test@example.com>")
	if err != nil {
		t.Fatalf("BuildMessage() error = %v", err)
	}
	loaded, err := loadComposerAttachments(draft.Attachments)
	if err != nil {
		t.Fatalf("loadComposerAttachments() error = %v", err)
	}
	preloaded, err := buildMessageWithAttachments(draft, "<test@example.com>", loaded)
	if err != nil {
		t.Fatalf("buildMessageWithAttachments() error = %v", err)
	}
	if normalizeGeneratedMessage(legacy) != normalizeGeneratedMessage(preloaded) {
		t.Fatalf("preloaded composition changed the MIME wire format:\nlegacy:\n%s\npreloaded:\n%s", legacy, preloaded)
	}
}

func normalizeGeneratedMessage(message []byte) string {
	lines := strings.Split(string(message), "\r\n")
	var boundaries []string
	for _, line := range lines {
		marker := `boundary="`
		start := strings.Index(line, marker)
		if start < 0 {
			continue
		}
		value := line[start+len(marker):]
		if end := strings.IndexByte(value, '"'); end >= 0 {
			boundaries = append(boundaries, value[:end])
		}
	}
	for index, line := range lines {
		if strings.HasPrefix(line, "Date: ") {
			line = "Date: <generated>"
		}
		for _, boundary := range boundaries {
			line = strings.ReplaceAll(line, boundary, "<boundary>")
		}
		lines[index] = line
	}
	return strings.Join(lines, "\r\n")
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
