package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStageDraftAttachmentsPreservesApprovedBytesAndCleansUp(t *testing.T) {
	source := filepath.Join(t.TempDir(), "report.pdf")
	content := []byte("approved attachment bytes")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	paths, cleanup, err := StageDraftAttachments(context.Background(), []DraftAttachment{
		draftAttachmentFingerprint(source, content),
	})
	if err != nil {
		t.Fatalf("StageDraftAttachments() error = %v", err)
	}
	if len(paths) != 1 || paths[0] == source {
		t.Fatalf("staged paths = %v, want one private path distinct from source", paths)
	}
	staged, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("ReadFile(staged) error = %v", err)
	}
	if string(staged) != string(content) {
		t.Fatalf("staged bytes = %q, want %q", staged, content)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup() error = %v", err)
	}
	if _, err := os.Stat(paths[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged path still exists: %v", err)
	}
}

func TestStageDraftAttachmentsRejectsSameSizeReplacement(t *testing.T) {
	source := filepath.Join(t.TempDir(), "report.pdf")
	original := []byte("original bytes")
	replacement := []byte("replaced bytes")
	if err := os.WriteFile(source, original, 0o600); err != nil {
		t.Fatalf("WriteFile(original) error = %v", err)
	}
	expected := draftAttachmentFingerprint(source, original)
	if err := os.WriteFile(source, replacement, 0o600); err != nil {
		t.Fatalf("WriteFile(replacement) error = %v", err)
	}

	paths, cleanup, err := StageDraftAttachments(context.Background(), []DraftAttachment{expected})
	if cleanup != nil {
		t.Cleanup(func() { _ = cleanup() })
	}
	if errorCode(err) != "handoff_attachment_changed" {
		t.Fatalf("StageDraftAttachments() error = %v, want handoff_attachment_changed", err)
	}
	if len(paths) != 0 {
		t.Fatalf("staged paths = %v, want none", paths)
	}
}

func TestStageDraftAttachmentsRejectsReplacementDuringSingleCopy(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "report.pdf")
	original := []byte("original bytes that span the staged read")
	replacement := []byte("replacement bytes that span the staged read")
	if err := os.WriteFile(source, original, 0o600); err != nil {
		t.Fatalf("WriteFile(original) error = %v", err)
	}
	expected := draftAttachmentFingerprint(source, original)
	previous := handoffAttachmentReadHook
	readCalls := 0
	handoffAttachmentReadHook = func(int) {
		readCalls++
		if readCalls != 1 {
			return
		}
		moved := source + ".original"
		if err := os.Rename(source, moved); err != nil {
			t.Errorf("Rename() error = %v", err)
			return
		}
		if err := os.WriteFile(source, replacement, 0o600); err != nil {
			t.Errorf("WriteFile(replacement) error = %v", err)
		}
	}
	t.Cleanup(func() { handoffAttachmentReadHook = previous })

	paths, cleanup, err := StageDraftAttachments(context.Background(), []DraftAttachment{expected})
	if cleanup != nil {
		t.Cleanup(func() { _ = cleanup() })
	}
	if errorCode(err) != "handoff_attachment_changed" {
		t.Fatalf("StageDraftAttachments() error = %v, want handoff_attachment_changed", err)
	}
	if len(paths) != 0 {
		t.Fatalf("staged paths = %v, want none", paths)
	}
}

func TestStageDraftAttachmentsUsesOneAuthoritativeReadAfterPreflight(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	source := filepath.Join(t.TempDir(), "report.pdf")
	content := []byte("one authoritative handoff read")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	service := NewServiceWithDraftRoot(&gatewayStub{}, root)
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		To: []Recipient{{Address: "recipient@example.com"}}, Body: "Body", Attachments: []string{source},
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}

	var readBytes int
	previous := handoffAttachmentReadHook
	handoffAttachmentReadHook = func(read int) { readBytes += read }
	t.Cleanup(func() { handoffAttachmentReadHook = previous })
	prepared, err := service.PrepareDraftHandoffContext(context.Background(), draft.Ref)
	if err != nil {
		t.Fatalf("PrepareDraftHandoffContext() error = %v", err)
	}
	paths, cleanup, err := StageDraftAttachments(context.Background(), prepared.Attachments)
	if cleanup != nil {
		t.Cleanup(func() { _ = cleanup() })
	}
	if err != nil {
		t.Fatalf("StageDraftAttachments() error = %v", err)
	}
	if len(paths) != 1 || readBytes != len(content) {
		t.Fatalf("staged paths = %v, read bytes = %d, want one path and %d bytes", paths, readBytes, len(content))
	}
}

func TestStageDraftAttachmentsRejectsSymlinkReplacement(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "report.pdf")
	target := filepath.Join(directory, "replacement.pdf")
	original := []byte("original bytes")
	if err := os.WriteFile(source, original, 0o600); err != nil {
		t.Fatalf("WriteFile(source) error = %v", err)
	}
	expected := draftAttachmentFingerprint(source, original)
	if err := os.Remove(source); err != nil {
		t.Fatalf("Remove(source) error = %v", err)
	}
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}
	if err := os.Symlink(target, source); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	paths, cleanup, err := StageDraftAttachments(context.Background(), []DraftAttachment{expected})
	if cleanup != nil {
		t.Cleanup(func() { _ = cleanup() })
	}
	if errorCode(err) != "handoff_attachment_unreadable" {
		t.Fatalf("StageDraftAttachments() error = %v, want handoff_attachment_unreadable", err)
	}
	if len(paths) != 0 {
		t.Fatalf("staged paths = %v, want none", paths)
	}
}

func TestStageDraftAttachmentsRejectsDeletion(t *testing.T) {
	source := filepath.Join(t.TempDir(), "report.pdf")
	content := []byte("attachment bytes")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	expected := draftAttachmentFingerprint(source, content)
	if err := os.Remove(source); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	paths, cleanup, err := StageDraftAttachments(context.Background(), []DraftAttachment{expected})
	if cleanup != nil {
		t.Cleanup(func() { _ = cleanup() })
	}
	if errorCode(err) != "handoff_attachment_missing" {
		t.Fatalf("StageDraftAttachments() error = %v, want handoff_attachment_missing", err)
	}
	if len(paths) != 0 {
		t.Fatalf("staged paths = %v, want none", paths)
	}
}

func TestStageDraftAttachmentsReportsCleanupFailure(t *testing.T) {
	source := filepath.Join(t.TempDir(), "report.pdf")
	content := []byte("attachment bytes")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	originalRemove := removeHandoffSnapshotRoot
	t.Cleanup(func() { removeHandoffSnapshotRoot = originalRemove })
	var root string
	cleanupFailure := errors.New("injected cleanup failure")
	removeHandoffSnapshotRoot = func(path string) error {
		root = path
		return cleanupFailure
	}

	paths, cleanup, err := StageDraftAttachments(context.Background(), []DraftAttachment{
		draftAttachmentFingerprint(source, content),
	})
	if err != nil {
		t.Fatalf("StageDraftAttachments() error = %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("staged paths = %v, want one path", paths)
	}
	if err := cleanup(); err == nil || !errors.Is(err, cleanupFailure) {
		t.Fatalf("cleanup() error = %v, want injected failure", err)
	}
	if root == "" {
		t.Fatal("cleanup root was not captured")
	}
	removeHandoffSnapshotRoot = os.RemoveAll
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("remove injected cleanup root: %v", err)
	}
}

func draftAttachmentFingerprint(path string, content []byte) DraftAttachment {
	hash := sha256.Sum256(content)
	return DraftAttachment{
		Path: path, Size: int64(len(content)), SHA256: hex.EncodeToString(hash[:]),
	}
}
