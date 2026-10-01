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

func TestPersistentHandoffPreservesApprovedBytesAndCleansUp(t *testing.T) {
	source := filepath.Join(t.TempDir(), "report.pdf")
	content := []byte("approved attachment bytes")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	service := NewServiceWithDraftRoot(nil, t.TempDir())
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		To: []Recipient{{Address: "recipient@example.com"}}, Body: "Body", Attachments: []string{source},
	}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.BeginDraftHandoff(draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	paths := session.Preparation().AttachmentPaths
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
	if info, err := os.Stat(paths[0]); err != nil || info.Mode().Perm() != 0o400 {
		t.Fatalf("snapshot mode = %v, %v", info, err)
	}
	if err := session.CancelBeforeDispatch(); err != nil {
		t.Fatalf("CancelBeforeDispatch() error = %v", err)
	}
	if _, err := os.Stat(paths[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged path still exists: %v", err)
	}
}

func TestPersistentHandoffRejectsSameSizeReplacement(t *testing.T) {
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

	staged, err := stagePersistentAttachmentFixture(t, context.Background(), expected)
	if errorCode(err) != "handoff_attachment_changed" {
		t.Fatalf("stageDraftAttachmentAt() error = %v, want handoff_attachment_changed", err)
	}
	if staged != "" {
		t.Fatalf("staged path = %q, want none", staged)
	}
}

func TestPersistentHandoffRejectsReplacementDuringSingleCopy(t *testing.T) {
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

	staged, err := stagePersistentAttachmentFixture(t, context.Background(), expected)
	if errorCode(err) != "handoff_attachment_changed" {
		t.Fatalf("stageDraftAttachmentAt() error = %v, want handoff_attachment_changed", err)
	}
	if staged != "" {
		t.Fatalf("staged path = %q, want none", staged)
	}
}

func TestPersistentHandoffUsesOneAuthoritativeReadAfterPreflight(t *testing.T) {
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
	session, err := service.BeginDraftHandoffContext(context.Background(), draft.Ref)
	if err != nil {
		t.Fatalf("BeginDraftHandoffContext() error = %v", err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	paths := session.Preparation().AttachmentPaths
	if len(paths) != 1 || readBytes != len(content) {
		t.Fatalf("staged paths = %v, read bytes = %d, want one path and %d bytes", paths, readBytes, len(content))
	}
}

func TestPersistentHandoffRejectsSymlinkReplacement(t *testing.T) {
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

	staged, err := stagePersistentAttachmentFixture(t, context.Background(), expected)
	if errorCode(err) != "handoff_attachment_unreadable" {
		t.Fatalf("stageDraftAttachmentAt() error = %v, want handoff_attachment_unreadable", err)
	}
	if staged != "" {
		t.Fatalf("staged path = %q, want none", staged)
	}
}

func TestPersistentHandoffRejectsDeletion(t *testing.T) {
	source := filepath.Join(t.TempDir(), "report.pdf")
	content := []byte("attachment bytes")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	expected := draftAttachmentFingerprint(source, content)
	if err := os.Remove(source); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	staged, err := stagePersistentAttachmentFixture(t, context.Background(), expected)
	if errorCode(err) != "handoff_attachment_missing" {
		t.Fatalf("stageDraftAttachmentAt() error = %v, want handoff_attachment_missing", err)
	}
	if staged != "" {
		t.Fatalf("staged path = %q, want none", staged)
	}
}

func TestPersistentHandoffReportsCleanupFailure(t *testing.T) {
	source := filepath.Join(t.TempDir(), "report.pdf")
	content := []byte("attachment bytes")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	service := NewServiceWithDraftRoot(nil, t.TempDir())
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		To: []Recipient{{Address: "recipient@example.com"}}, Body: "Body", Attachments: []string{source},
	}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.BeginDraftHandoff(draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	path := session.Preparation().AttachmentPaths[0]
	foreign := filepath.Join(filepath.Dir(path), "unrelated")
	if err := os.WriteFile(foreign, []byte("foreign bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := session.CancelBeforeDispatch(); err == nil {
		t.Fatal("cleanup silently removed an unrelated file")
	}
	for retainedPath, want := range map[string]string{path: string(content), foreign: "foreign bytes"} {
		if got, err := os.ReadFile(retainedPath); err != nil || string(got) != want {
			t.Fatalf("retained file = %q, %v", got, err)
		}
	}
	retained, err := service.GetDraft(draft.Ref)
	if err != nil || retained.HandoffAttempt == nil || retained.HandoffAttempt.ID != session.AttemptID() {
		t.Fatalf("cleanup lost owning claim: %+v, %v", retained.HandoffAttempt, err)
	}
}

func stagePersistentAttachmentFixture(t *testing.T, ctx context.Context, expected DraftAttachment) (string, error) {
	t.Helper()
	const ref = "draft_000000000000000000000000"
	lease, err := acquireDraftLease(context.Background(), t.TempDir(), ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.release(); err != nil {
			t.Error(err)
		}
	})
	name := filepath.Join(ref+handoffSnapshotSuffix, "handoff_123456789012345678901234")
	for _, directory := range []string{filepath.Dir(name), name} {
		if err := ensurePrivateDirectoryAt(lease.storage, directory); err != nil {
			t.Fatal(err)
		}
	}
	return stageDraftAttachmentAt(ctx, lease.storage, name, 0, expected)
}

func TestPersistentHandoffPreservesStagingCancellation(t *testing.T) {
	for _, duringRead := range []bool{false, true} {
		t.Run(map[bool]string{false: "before copy", true: "during copy"}[duringRead], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "attachment")
			content := []byte("verified attachment bytes")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			previous := handoffAttachmentReadHook
			handoffAttachmentReadHook = func(int) { cancel() }
			t.Cleanup(func() { handoffAttachmentReadHook = previous })
			if !duringRead {
				cancel()
			}
			staged, err := stagePersistentAttachmentFixture(t, ctx, draftAttachmentFingerprint(path, content))
			if staged != "" || !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled staging = %q, %v", staged, err)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != string(content) {
				t.Fatalf("source changed = %q, %v", got, err)
			}
		})
	}
}

func draftAttachmentFingerprint(path string, content []byte) DraftAttachment {
	hash := sha256.Sum256(content)
	return DraftAttachment{
		Path: path, Size: int64(len(content)), SHA256: hex.EncodeToString(hash[:]),
	}
}
