package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type attachmentGateway struct {
	gatewayStub
	content       []byte
	saveErr       error
	afterEvidence func(string) error
}

type legacyAttachmentGateway struct {
	gatewayStub
	content []byte
}

func (g *attachmentGateway) SaveAttachmentTo(_ context.Context, _ string, _ string, path string) error {
	_, err := writeAttachmentPermissionFixture(path, g.content)
	return err
}

func (g *legacyAttachmentGateway) SaveAttachmentTo(_ context.Context, _ string, _ string, path string) error {
	_, err := writeAttachmentPermissionFixture(path, g.content)
	return err
}

func (g *attachmentGateway) SaveAttachmentToWithEvidence(
	_ context.Context,
	_ string,
	_ string,
	path string,
) (AttachmentEvidence, error) {
	if g.saveErr != nil {
		return AttachmentEvidence{}, g.saveErr
	}
	identity, err := writeAttachmentPermissionFixture(path, g.content)
	if err != nil {
		return AttachmentEvidence{}, err
	}
	digest := sha256.Sum256(g.content)
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

func TestSaveAttachmentReportsNoEffectBeforePublish(t *testing.T) {
	output := filepath.Join(t.TempDir(), "report.pdf")
	saveErr := context.DeadlineExceeded
	saved, err := NewService(&attachmentGateway{saveErr: saveErr}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if !errors.Is(err, saveErr) || saved != (SavedAttachment{}) {
		t.Fatalf("SaveAttachment() = (%+v, %v), want no evidence and the pre-publish error", saved, err)
	}
	var outcome *AttachmentSaveOutcomeError
	if !errors.As(err, &outcome) || outcome.EffectCertainty != EffectNone {
		t.Fatalf("outcome = %+v, want effect none", outcome)
	}
	guidance := GuidanceForError("attachments.save", err)
	if guidance.EffectCertainty != EffectNone || !guidance.ReplayAllowed || guidance.Retryability != RetrySafe {
		t.Fatalf("guidance = %+v, want safe replay with no effect", guidance)
	}
	if _, statErr := os.Lstat(output); !os.IsNotExist(statErr) {
		t.Fatalf("pre-publish output exists: %v", statErr)
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
	var replacementIdentity os.FileInfo
	setAttachmentPublicationHook(t, func(stage string, path string) error {
		if stage != "before-inspect" {
			return nil
		}
		if err := os.Rename(path, moved); err != nil {
			return err
		}
		var err error
		replacementIdentity, err = writeAttachmentPermissionFixture(path, replacement)
		return err
	})

	saved, err := NewService(&attachmentGateway{content: []byte("original")}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if errorCode(err) != "attachment_changed" {
		t.Fatalf("SaveAttachment() error = %v, want attachment_changed", err)
	}
	assertUnknownAttachmentSaveOutcome(t, saved, err)
	assertAttachmentFile(t, output, replacement, 0o644, replacementIdentity)
}

func TestSaveAttachmentPreservesOutputReplacementBeforeChmod(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "report.pdf")
	moved := output + ".original"
	replacement := []byte("replacement")
	var replacementIdentity os.FileInfo
	setAttachmentPublicationHook(t, func(stage string, path string) error {
		if stage != "before-chmod" {
			return nil
		}
		if err := os.Rename(path, moved); err != nil {
			return err
		}
		var err error
		replacementIdentity, err = writeAttachmentPermissionFixture(path, replacement)
		return err
	})

	_, err := NewService(&attachmentGateway{content: []byte("original")}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if errorCode(err) != "attachment_changed" {
		t.Fatalf("SaveAttachment() error = %v, want attachment_changed", err)
	}
	assertAttachmentFile(t, output, replacement, 0o644, replacementIdentity)
}

func TestSaveAttachmentPreservesReplacementDuringCleanup(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "report.pdf")
	replacement := []byte("replacement")
	var replacementIdentity os.FileInfo
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
			var err error
			replacementIdentity, err = writeAttachmentPermissionFixture(path, replacement)
			return err
		default:
			return nil
		}
	})

	saved, err := NewService(&attachmentGateway{content: []byte("original")}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if !errors.Is(err, inspectErr) || errorCode(err) != "attachment_changed" {
		t.Fatalf("SaveAttachment() error = %v, want inspect and attachment_changed errors", err)
	}
	assertUnknownAttachmentSaveOutcome(t, saved, err)
	assertAttachmentFile(t, output, replacement, 0o644, replacementIdentity)
}

func TestSaveAttachmentRechecksRetainedOutputDuringCleanup(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "report.pdf")
	replacement := []byte("replacement")
	var replacementIdentity os.FileInfo
	setAttachmentPublicationHook(t, func(stage string, path string) error {
		if stage != "before-cleanup" || path != output {
			return nil
		}
		if err := os.Rename(path, path+".original"); err != nil {
			return err
		}
		var err error
		replacementIdentity, err = writeAttachmentPermissionFixture(path, replacement)
		return err
	})

	saved, err := NewService(&attachmentGateway{content: []byte("original")}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if errorCode(err) != "attachment_changed" {
		t.Fatalf("SaveAttachment() error = %v, want attachment_changed", err)
	}
	assertUnknownAttachmentSaveOutcome(t, saved, err)
	assertAttachmentFile(t, output, replacement, 0o644, replacementIdentity)
}

func assertUnknownAttachmentSaveOutcome(t *testing.T, saved SavedAttachment, err error) {
	t.Helper()
	if saved != (SavedAttachment{}) {
		t.Fatalf("SaveAttachment() evidence = %+v, want no proof for the replacement", saved)
	}
	var outcome *AttachmentSaveOutcomeError
	if !errors.As(err, &outcome) || outcome.EffectCertainty != EffectUnknown {
		t.Fatalf("outcome = %+v, want unknown effect", outcome)
	}
	guidance := GuidanceForError("attachments.save", err)
	if guidance.EffectCertainty != EffectUnknown || guidance.ReplayAllowed || guidance.Retryability != RetryObserveRequired {
		t.Fatalf("guidance = %+v, want observation before replay", guidance)
	}
}

func TestSaveAttachmentUsesVerifiedCopyWhenLinkCrossesFilesystem(t *testing.T) {
	previous := attachmentLink
	attachmentLink = func(*os.Root, string, string) error { return syscall.EXDEV }
	t.Cleanup(func() { attachmentLink = previous })
	var hashedBytes int64
	previousHash := attachmentPublicationHashHook
	attachmentPublicationHashHook = func(bytes int64) { hashedBytes += bytes }
	t.Cleanup(func() { attachmentPublicationHashHook = previousHash })
	var verificationCount int
	setAttachmentPublicationHook(t, func(stage string, _ string) error {
		if stage == "verify-published" {
			verificationCount++
		}
		return nil
	})

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
	if hashedBytes != int64(len(content)) {
		t.Fatalf("publication hash bytes = %d, want one copy-pass hash of %d bytes", hashedBytes, len(content))
	}
	if verificationCount != 7 {
		t.Fatalf("verifyPublished calls = %d, want 7 across copy, chmod, inspection, and cleanup boundaries", verificationCount)
	}
	assertAttachmentFile(t, output, content, 0o600)
}

func TestSaveAttachmentReusesGatewayEvidenceWithoutRehash(t *testing.T) {
	var hashedBytes int64
	previousHash := attachmentPublicationHashHook
	attachmentPublicationHashHook = func(bytes int64) { hashedBytes += bytes }
	t.Cleanup(func() { attachmentPublicationHashHook = previousHash })
	var verificationCount int
	setAttachmentPublicationHook(t, func(stage string, _ string) error {
		if stage == "verify-published" {
			verificationCount++
		}
		return nil
	})

	content := []byte("verified without a second publication read")
	output := filepath.Join(t.TempDir(), "report.pdf")
	if _, err := NewService(&attachmentGateway{content: content}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	); err != nil {
		t.Fatalf("SaveAttachment() error = %v", err)
	}
	if hashedBytes != 0 {
		t.Fatalf("publication rehashed %d bytes, want the gateway evidence reused without a content read", hashedBytes)
	}
	if verificationCount != 6 {
		t.Fatalf("verifyPublished calls = %d, want 6 across unchanged identity boundaries", verificationCount)
	}
}

func TestSaveAttachmentKeepsLegacyGatewayInspectionFallback(t *testing.T) {
	var hashedBytes int64
	previousHash := attachmentPublicationHashHook
	attachmentPublicationHashHook = func(bytes int64) { hashedBytes += bytes }
	t.Cleanup(func() { attachmentPublicationHashHook = previousHash })
	var verificationCount int
	setAttachmentPublicationHook(t, func(stage string, _ string) error {
		if stage == "verify-published" {
			verificationCount++
		}
		return nil
	})

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
	if hashedBytes != int64(len(content)) {
		t.Fatalf("legacy publication hash bytes = %d, want one hash of %d bytes", hashedBytes, len(content))
	}
	if verificationCount != 7 {
		t.Fatalf("verifyPublished calls = %d, want 7 including the legacy content hash boundary", verificationCount)
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
	var replacementIdentity os.FileInfo
	gateway := &attachmentGateway{content: []byte("original")}
	gateway.afterEvidence = func(path string) error {
		if err := os.Rename(path, path+".original"); err != nil {
			return err
		}
		var err error
		replacementIdentity, err = writeAttachmentPermissionFixture(path, replacement)
		return err
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
			assertAttachmentFile(t, candidate, replacement, 0o644, replacementIdentity)
			found = true
			break
		}
	}
	if !found {
		t.Fatal("temporary replacement was removed")
	}
}

func TestSaveAttachmentRejectsInPlaceTemporaryMutationAfterEvidence(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "report.pdf")
	replacement := []byte("changed!")
	gateway := &attachmentGateway{content: []byte("original")}
	gateway.afterEvidence = func(path string) error {
		before, err := os.Stat(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, replacement, 0o644); err != nil {
			return err
		}
		changedAt := before.ModTime().Add(time.Second)
		if err := os.Chtimes(path, changedAt, changedAt); err != nil {
			return err
		}
		after, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime().Equal(after.ModTime()) {
			return fmt.Errorf("fixture mutation did not preserve identity and size while changing modification time")
		}
		return nil
	}

	saved, err := NewService(gateway).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if errorCode(err) != "attachment_changed" || saved != (SavedAttachment{}) {
		t.Fatalf("SaveAttachment() = (%+v, %v), want same-inode tampering refusal", saved, err)
	}
	entries, readErr := os.ReadDir(directory)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("directory after unpublished tampering = %v, %v; want empty", entries, readErr)
	}
}

func TestSaveAttachmentRejectsInPlacePublishedMutationBeforeInspection(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "report.pdf")
	replacement := []byte("tampered")
	var outputIdentity os.FileInfo
	setAttachmentPublicationHook(t, func(stage string, path string) error {
		if stage != "before-inspect" || path != output {
			return nil
		}
		before, err := os.Stat(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, replacement, 0o600); err != nil {
			return err
		}
		changedAt := before.ModTime().Add(time.Second)
		if err := os.Chtimes(path, changedAt, changedAt); err != nil {
			return err
		}
		after, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime().Equal(after.ModTime()) {
			return fmt.Errorf("fixture mutation did not preserve identity and size while changing modification time")
		}
		outputIdentity = before
		return nil
	})

	saved, err := NewService(&attachmentGateway{content: []byte("original")}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if errorCode(err) != "attachment_changed" {
		t.Fatalf("SaveAttachment() error = %v, want attachment_changed", err)
	}
	assertUnknownAttachmentSaveOutcome(t, saved, err)
	assertAttachmentFile(t, output, replacement, 0o600, outputIdentity)
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
	if err == nil || !strings.Contains(err.Error(), "saved attachment permissions are not private") {
		t.Fatalf("SaveAttachment() error = %v, want permission verification failure", err)
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
	saved, err := NewService(&attachmentGateway{content: []byte("original")}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if !errors.Is(err, inspectErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("SaveAttachment() error = %v, want inspect and cleanup errors", err)
	}
	assertSavedAttachmentEvidence(t, saved, output, []byte("original"))
	guidance := GuidanceForError("attachments.save", err)
	if guidance.EffectCertainty != EffectComplete || guidance.ReplayAllowed || guidance.Retryability != RetryObserveRequired {
		t.Fatalf("guidance = %+v, want complete effect requiring observation", guidance)
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
	saved, err := NewService(&attachmentGateway{content: []byte("original")}).SaveAttachment(
		context.Background(), SaveAttachmentRequest{
			MessageRef: "msg_ref", AttachmentID: "attachment-id", OutputPath: output,
		},
	)
	if !errors.Is(err, closeErr) {
		t.Fatalf("SaveAttachment() error = %v, want close failure", err)
	}
	assertSavedAttachmentEvidence(t, saved, output, []byte("original"))
	var outcome *AttachmentSaveOutcomeError
	if !errors.As(err, &outcome) || outcome.EffectCertainty != EffectComplete {
		t.Fatalf("outcome = %+v, want complete effect", outcome)
	}
	guidance := GuidanceForError("attachments.save", err)
	if guidance.EffectCertainty != EffectComplete || guidance.ReplayAllowed || guidance.Recovery.Action != RecoveryInspect {
		t.Fatalf("guidance = %+v, want inspect-only recovery", guidance)
	}
	assertAttachmentFile(t, output, []byte("original"), 0o600)
}

func TestSaveAttachmentPreservesTemporaryReplacement(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "report.pdf")
	replacement := []byte("replacement")
	var replacementIdentity os.FileInfo
	setAttachmentPublicationHook(t, func(stage string, path string) error {
		if stage != "after-link-temporary" {
			return nil
		}
		if err := os.Rename(path, path+".original"); err != nil {
			return err
		}
		var err error
		replacementIdentity, err = writeAttachmentPermissionFixture(path, replacement)
		return err
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
			assertAttachmentFile(t, candidate, replacement, 0o644, replacementIdentity)
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

func writeAttachmentPermissionFixture(path string, content []byte) (os.FileInfo, error) {
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o644); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o644 {
		return nil, fmt.Errorf("attachment fixture %q must be a regular file with mode 0644, got %v", path, info.Mode())
	}
	return info, nil
}

func assertAttachmentFile(t *testing.T, path string, want []byte, mode os.FileMode, identity ...os.FileInfo) {
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
	if len(identity) > 0 && (identity[0] == nil || !os.SameFile(identity[0], info)) {
		t.Fatalf("identity at %q changed", path)
	}
}

func assertSavedAttachmentEvidence(t *testing.T, saved SavedAttachment, path string, content []byte) {
	t.Helper()
	digest := sha256.Sum256(content)
	if saved.Path != path || saved.Size != int64(len(content)) || saved.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("saved attachment = %+v, want path %q, size %d and matching SHA-256", saved, path, len(content))
	}
}
