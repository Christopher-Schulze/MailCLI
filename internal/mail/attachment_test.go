package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type attachmentGateway struct {
	gatewayStub
	content       []byte
	afterEvidence func(string) error
}

type legacyAttachmentGateway struct {
	gatewayStub
	content []byte
}

func (g *attachmentGateway) SaveAttachmentTo(_ context.Context, _ string, _ string, path string) error {
	return os.WriteFile(path, g.content, 0o644)
}

func (g *legacyAttachmentGateway) SaveAttachmentTo(_ context.Context, _ string, _ string, path string) error {
	return os.WriteFile(path, g.content, 0o644)
}

func (g *attachmentGateway) SaveAttachmentToWithEvidence(
	_ context.Context,
	_ string,
	_ string,
	path string,
) (AttachmentEvidence, error) {
	if err := os.WriteFile(path, g.content, 0o644); err != nil {
		return AttachmentEvidence{}, err
	}
	digest := sha256.Sum256(g.content)
	identity, err := os.Lstat(path)
	if err != nil {
		return AttachmentEvidence{}, err
	}
	if g.afterEvidence != nil {
		if err := g.afterEvidence(path); err != nil {
			return AttachmentEvidence{}, err
		}
	}
	return AttachmentEvidence{
		Path: path, Size: int64(len(g.content)), SHA256: hex.EncodeToString(digest[:]),
		Identity: identity,
	}, nil
}

func TestSaveAttachmentPublishesExactPrivateFile(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "report.pdf")
	service := NewService(&attachmentGateway{content: []byte("exact attachment")})

	saved, err := service.SaveAttachment(context.Background(), SaveAttachmentRequest{
		MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
	})
	if err != nil {
		t.Fatalf("SaveAttachment() error = %v", err)
	}
	if saved.Size != 16 || saved.SHA256 != "c766525e1fc9ed7b91e950f7a23d94e932c1de4886d933096a51c3a48c66fda0" {
		t.Fatalf("saved = %+v", saved)
	}
	content, err := os.ReadFile(output)
	if err != nil || string(content) != "exact attachment" {
		t.Fatalf("saved content = %q, error = %v", content, err)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved mode = %v, error = %v", info.Mode().Perm(), err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(output) {
		t.Fatalf("directory entries after save = %v, want only %s", entries, filepath.Base(output))
	}
}

func TestSaveAttachmentNeverOverwrites(t *testing.T) {
	output := filepath.Join(t.TempDir(), "existing.txt")
	if err := os.WriteFile(output, []byte("keep"), 0o600); err != nil {
		t.Fatalf("seed output: %v", err)
	}
	service := NewService(&attachmentGateway{content: []byte("replace")})
	_, err := service.SaveAttachment(context.Background(), SaveAttachmentRequest{
		MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
	})
	if err == nil {
		t.Fatal("SaveAttachment() error = nil")
	}
	content, readErr := os.ReadFile(output)
	if readErr != nil || string(content) != "keep" {
		t.Fatalf("existing content = %q, error = %v", content, readErr)
	}
}

func TestSaveAttachmentPreservesOutputReplacementBeforeInspection(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "report.pdf")
	moved := output + ".original"
	replacement := []byte("replacement")
	setAttachmentPublicationHook(t, func(stage string, path string) error {
		if stage != "before-inspect" {
			return nil
		}
		if err := os.Rename(path, moved); err != nil {
			return err
		}
		return os.WriteFile(path, replacement, 0o644)
	})

	_, err := NewService(&attachmentGateway{content: []byte("original")}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if errorCode(err) != "attachment_changed" {
		t.Fatalf("SaveAttachment() error = %v, want attachment_changed", err)
	}
	assertAttachmentFile(t, output, replacement, 0o644)
}

func TestSaveAttachmentPreservesOutputReplacementBeforeChmod(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "report.pdf")
	moved := output + ".original"
	replacement := []byte("replacement")
	setAttachmentPublicationHook(t, func(stage string, path string) error {
		if stage != "before-chmod" {
			return nil
		}
		if err := os.Rename(path, moved); err != nil {
			return err
		}
		return os.WriteFile(path, replacement, 0o644)
	})

	_, err := NewService(&attachmentGateway{content: []byte("original")}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if errorCode(err) != "attachment_changed" {
		t.Fatalf("SaveAttachment() error = %v, want attachment_changed", err)
	}
	assertAttachmentFile(t, output, replacement, 0o644)
}

func TestSaveAttachmentPreservesReplacementDuringCleanup(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "report.pdf")
	replacement := []byte("replacement")
	inspectErr := errors.New("inspect failed")
	setAttachmentPublicationHook(t, func(stage string, path string) error {
		switch stage {
		case "before-inspect":
			return inspectErr
		case "before-cleanup":
			if path != output {
				return nil
			}
			moved := output + ".original"
			if err := os.Rename(path, moved); err != nil {
				return err
			}
			return os.WriteFile(path, replacement, 0o644)
		default:
			return nil
		}
	})

	_, err := NewService(&attachmentGateway{content: []byte("original")}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if !errors.Is(err, inspectErr) || errorCode(err) != "attachment_changed" {
		t.Fatalf("SaveAttachment() error = %v, want inspect and attachment_changed errors", err)
	}
	assertAttachmentFile(t, output, replacement, 0o644)
}

func TestSaveAttachmentRechecksRetainedOutputDuringCleanup(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "report.pdf")
	replacement := []byte("replacement")
	setAttachmentPublicationHook(t, func(stage string, path string) error {
		if stage != "before-cleanup" || path != output {
			return nil
		}
		if err := os.Rename(path, path+".original"); err != nil {
			return err
		}
		return os.WriteFile(path, replacement, 0o644)
	})

	_, err := NewService(&attachmentGateway{content: []byte("original")}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if errorCode(err) != "attachment_changed" {
		t.Fatalf("SaveAttachment() error = %v, want attachment_changed", err)
	}
	assertAttachmentFile(t, output, replacement, 0o644)
}

func TestSaveAttachmentUsesVerifiedCopyWhenLinkCrossesFilesystem(t *testing.T) {
	previous := attachmentLink
	attachmentLink = func(*os.Root, string, string) error { return syscall.EXDEV }
	t.Cleanup(func() { attachmentLink = previous })

	output := filepath.Join(t.TempDir(), "report.pdf")
	content := []byte("copied attachment")
	saved, err := NewService(&attachmentGateway{content: content}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if err != nil {
		t.Fatalf("SaveAttachment() error = %v", err)
	}
	if saved.Size != int64(len(content)) {
		t.Fatalf("saved.Size = %d, want %d", saved.Size, len(content))
	}
	assertAttachmentFile(t, output, content, 0o600)
}

func TestSaveAttachmentReusesGatewayEvidenceWithoutInspectionRead(t *testing.T) {
	var readBytes int64
	previous := attachmentPublicationReadHook
	attachmentPublicationReadHook = func(bytes int64) { readBytes += bytes }
	t.Cleanup(func() { attachmentPublicationReadHook = previous })

	content := []byte("verified without a second publication read")
	output := filepath.Join(t.TempDir(), "report.pdf")
	if _, err := NewService(&attachmentGateway{content: content}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	); err != nil {
		t.Fatalf("SaveAttachment() error = %v", err)
	}
	if readBytes != 0 {
		t.Fatalf("publication inspection read %d bytes, want zero", readBytes)
	}
}

func TestSaveAttachmentKeepsLegacyGatewayInspectionFallback(t *testing.T) {
	var readBytes int64
	previous := attachmentPublicationReadHook
	attachmentPublicationReadHook = func(bytes int64) { readBytes += bytes }
	t.Cleanup(func() { attachmentPublicationReadHook = previous })

	content := []byte("legacy gateway attachment")
	output := filepath.Join(t.TempDir(), "report.pdf")
	saved, err := NewService(&legacyAttachmentGateway{content: content}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if err != nil {
		t.Fatalf("SaveAttachment() error = %v", err)
	}
	if readBytes != int64(len(content)) {
		t.Fatalf("publication inspection read %d bytes, want %d", readBytes, len(content))
	}
	digest := sha256.Sum256(content)
	if saved.Size != int64(len(content)) || saved.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("saved = %+v, want size %d and sha256 %s", saved, len(content), hex.EncodeToString(digest[:]))
	}
}

func TestSaveAttachmentRejectsEvidenceAfterTemporaryReplacement(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "report.pdf")
	replacement := []byte("changed!")
	gateway := &attachmentGateway{content: []byte("original")}
	gateway.afterEvidence = func(path string) error {
		if err := os.Rename(path, path+".original"); err != nil {
			return err
		}
		return os.WriteFile(path, replacement, 0o644)
	}

	_, err := NewService(gateway).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if errorCode(err) != "attachment_changed" {
		t.Fatalf("SaveAttachment() error = %v, want attachment_changed", err)
	}
	entries, readErr := os.ReadDir(directory)
	if readErr != nil {
		t.Fatalf("ReadDir() error = %v", readErr)
	}
	found := false
	for _, entry := range entries {
		candidate := filepath.Join(directory, entry.Name())
		content, candidateErr := os.ReadFile(candidate)
		if candidateErr == nil && string(content) == string(replacement) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("temporary replacement was removed")
	}
}

func TestSaveAttachmentRejectsUnprivatePublishedFile(t *testing.T) {
	previous := attachmentChmod
	attachmentChmod = func(*os.File, os.FileMode) error { return nil }
	t.Cleanup(func() { attachmentChmod = previous })

	output := filepath.Join(t.TempDir(), "report.pdf")
	_, err := NewService(&attachmentGateway{content: []byte("original")}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if err == nil {
		t.Fatal("SaveAttachment() error = nil, want permission verification failure")
	}
	if _, statErr := os.Lstat(output); !os.IsNotExist(statErr) {
		t.Fatalf("unprivate output survived cleanup: %v", statErr)
	}
}

func TestSaveAttachmentReportsCleanupFailure(t *testing.T) {
	cleanupErr := errors.New("cleanup failed")
	previous := attachmentRemove
	attachmentRemove = func(*os.Root, string) error { return cleanupErr }
	t.Cleanup(func() { attachmentRemove = previous })
	inspectErr := errors.New("inspect failed")
	setAttachmentPublicationHook(t, func(stage string, _ string) error {
		if stage == "before-inspect" {
			return inspectErr
		}
		return nil
	})

	output := filepath.Join(t.TempDir(), "report.pdf")
	_, err := NewService(&attachmentGateway{content: []byte("original")}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if !errors.Is(err, inspectErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("SaveAttachment() error = %v, want inspect and cleanup errors", err)
	}
	assertAttachmentFile(t, output, []byte("original"), 0o600)
}

func TestSaveAttachmentReportsCloseFailureAfterVerifiedSave(t *testing.T) {
	closeErr := errors.New("close failed")
	previous := attachmentClose
	attachmentClose = func(file *os.File) error {
		return errors.Join(file.Close(), closeErr)
	}
	t.Cleanup(func() { attachmentClose = previous })

	output := filepath.Join(t.TempDir(), "report.pdf")
	_, err := NewService(&attachmentGateway{content: []byte("original")}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if !errors.Is(err, closeErr) {
		t.Fatalf("SaveAttachment() error = %v, want close failure", err)
	}
	assertAttachmentFile(t, output, []byte("original"), 0o600)
}

func TestSaveAttachmentPreservesTemporaryReplacement(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "report.pdf")
	replacement := []byte("replacement")
	setAttachmentPublicationHook(t, func(stage string, path string) error {
		if stage != "after-link-temporary" {
			return nil
		}
		if err := os.Rename(path, path+".original"); err != nil {
			return err
		}
		return os.WriteFile(path, replacement, 0o644)
	})

	_, err := NewService(&attachmentGateway{content: []byte("original")}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if errorCode(err) != "attachment_changed" {
		t.Fatalf("SaveAttachment() error = %v, want attachment_changed", err)
	}
	// The hook path is randomized; locate the replacement by its content.
	entries, readErr := os.ReadDir(directory)
	if readErr != nil {
		t.Fatalf("ReadDir() error = %v", readErr)
	}
	found := false
	for _, entry := range entries {
		if entry.Name() == filepath.Base(output) || entry.Name() == filepath.Base(output)+".original" {
			continue
		}
		candidate := filepath.Join(directory, entry.Name())
		candidateContent, candidateErr := os.ReadFile(candidate)
		if candidateErr == nil && string(candidateContent) == string(replacement) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("temporary replacement was removed")
	}
}

func setAttachmentPublicationHook(t *testing.T, hook func(string, string) error) {
	t.Helper()
	previous := attachmentPublicationHook
	attachmentPublicationHook = hook
	t.Cleanup(func() { attachmentPublicationHook = previous })
}

func assertAttachmentFile(t *testing.T, path string, want []byte, mode os.FileMode) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if string(content) != string(want) {
		t.Fatalf("content at %q = %q, want %q", path, content, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if info.Mode().Perm() != mode.Perm() {
		t.Fatalf("mode at %q = %o, want %o", path, info.Mode().Perm(), mode.Perm())
	}
}
