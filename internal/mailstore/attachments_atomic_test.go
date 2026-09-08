package mailstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestExternalAttachmentOperationsRejectSelectedFileReplacement(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Store, externalAttachment, string) error
	}{
		{
			name: "hash",
			run: func(store *Store, selected externalAttachment, _ string) error {
				_, err := store.hashStoreFile(selected)
				return err
			},
		},
		{
			name: "copy",
			run: func(store *Store, selected externalAttachment, output string) error {
				return store.copyExternalAttachment(selected, output)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			rootDirectory, err := openVersionDirectory(root)
			if err != nil {
				t.Fatalf("openVersionDirectory() error = %v", err)
			}
			closeTestResource(t, rootDirectory, "test root")
			store := &Store{versionRoot: root, versionDirectory: rootDirectory}
			path := filepath.Join(root, "attachment.bin")
			if err := os.WriteFile(path, []byte("reviewed bytes"), 0o600); err != nil {
				t.Fatalf("write selected attachment: %v", err)
			}
			identity, err := os.Lstat(path)
			if err != nil {
				t.Fatalf("inspect selected attachment: %v", err)
			}
			selected := externalAttachment{Path: path, Size: identity.Size(), identity: identity}
			if err := os.Rename(path, filepath.Join(root, "original.bin")); err != nil {
				t.Fatalf("move selected attachment: %v", err)
			}
			if err := os.WriteFile(path, []byte("replaced bytes"), 0o600); err != nil {
				t.Fatalf("write replacement attachment: %v", err)
			}
			output := filepath.Join(root, "output.bin")
			if err := test.run(store, selected, output); errorCodeForTest(err) != "store_changed" {
				t.Fatalf("operation error = %v, want store_changed", err)
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("output exists after rejected replacement: %v", err)
			}
		})
	}
}

func TestWriteVerifiedExclusiveFileCopiesAndVerifiesBytes(t *testing.T) {
	t.Parallel()
	source := []byte("verified attachment bytes")
	output := filepath.Join(t.TempDir(), "attachment.bin")
	if err := writeVerifiedExclusiveFile(output, bytes.NewReader(source), int64(len(source)), sha256.Sum256(source)); err != nil {
		t.Fatalf("writeVerifiedExclusiveFile() error = %v", err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Equal(got, source) {
		t.Fatalf("output bytes = %q, want %q", got, source)
	}
}

func TestCopyExternalAttachmentCopiesAndVerifiesBytes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	rootDirectory, err := openVersionDirectory(root)
	if err != nil {
		t.Fatalf("openVersionDirectory() error = %v", err)
	}
	closeTestResource(t, rootDirectory, "test root")
	store := &Store{versionRoot: root, versionDirectory: rootDirectory}
	source := []byte("external attachment bytes")
	path := filepath.Join(root, "attachment.bin")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatalf("WriteFile() source error = %v", err)
	}
	identity, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat() source error = %v", err)
	}
	output := filepath.Join(root, "output.bin")
	selected := externalAttachment{Path: path, Size: identity.Size(), identity: identity}
	if err := store.copyExternalAttachment(selected, output); err != nil {
		t.Fatalf("copyExternalAttachment() error = %v", err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("ReadFile() output error = %v", err)
	}
	if !bytes.Equal(got, source) {
		t.Fatalf("output bytes = %q, want %q", got, source)
	}
}

func TestCopyExternalAttachmentReturnsVerifiedEvidence(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	rootDirectory, err := openVersionDirectory(root)
	if err != nil {
		t.Fatalf("openVersionDirectory() error = %v", err)
	}
	closeTestResource(t, rootDirectory, "test root")
	store := &Store{versionRoot: root, versionDirectory: rootDirectory}
	source := []byte("external attachment evidence")
	path := filepath.Join(root, "attachment.bin")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatalf("WriteFile() source error = %v", err)
	}
	identity, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat() source error = %v", err)
	}
	output := filepath.Join(root, "output.bin")
	evidence, err := store.copyExternalAttachmentWithEvidence(
		externalAttachment{Path: path, Size: identity.Size(), identity: identity}, output,
	)
	if err != nil {
		t.Fatalf("copyExternalAttachmentWithEvidence() error = %v", err)
	}
	digest := sha256.Sum256(source)
	outputInfo, err := os.Lstat(output)
	if err != nil {
		t.Fatalf("Lstat() output error = %v", err)
	}
	if evidence.Path != output || evidence.Size != int64(len(source)) ||
		evidence.SHA256 != hex.EncodeToString(digest[:]) ||
		evidence.Identity == nil || evidence.Identity.Size() != int64(len(source)) ||
		!os.SameFile(outputInfo, evidence.Identity) {
		t.Fatalf("evidence = %+v, want path %q, size %d, sha256 %s", evidence, output, len(source), hex.EncodeToString(digest[:]))
	}
}

func TestWriteVerifiedExclusiveFileRejectsSameSizeSourceMutation(t *testing.T) {
	t.Parallel()
	expected := []byte("reviewed bytes")
	mutated := []byte("replaced bytes")
	output := filepath.Join(t.TempDir(), "attachment.bin")
	err := writeVerifiedExclusiveFile(output, bytes.NewReader(mutated), int64(len(expected)), sha256.Sum256(expected))
	if errorCodeForTest(err) != "store_changed" {
		t.Fatalf("writeVerifiedExclusiveFile() error = %v, want store_changed", err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("output exists after rejected source mutation: %v", err)
	}
}

func TestWriteVerifiedExclusiveFileRejectsShortCopy(t *testing.T) {
	t.Parallel()
	expected := []byte("complete attachment")
	output := filepath.Join(t.TempDir(), "attachment.bin")
	err := writeVerifiedExclusiveFile(output, bytes.NewReader([]byte("short")), int64(len(expected)), sha256.Sum256(expected))
	if errorCodeForTest(err) != "store_changed" {
		t.Fatalf("writeVerifiedExclusiveFile() error = %v, want store_changed", err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("output exists after rejected short copy: %v", err)
	}
}

func TestWriteVerifiedExclusiveFileDoesNotRemoveRegularReplacement(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	output := filepath.Join(root, "attachment.bin")
	ownedPath := filepath.Join(root, "owned.bin")
	replacement := []byte("replacement bytes")
	reader := &replacingAttachmentReader{
		reader: bytes.NewReader([]byte("verified bytes")),
		replace: func() error {
			if err := os.Rename(output, ownedPath); err != nil {
				return err
			}
			return os.WriteFile(output, replacement, 0o600)
		},
	}
	err := writeVerifiedExclusiveFile(output, reader, int64(len("verified bytes")), sha256.Sum256([]byte("verified bytes")))
	if errorCodeForTest(err) != "store_changed" {
		t.Fatalf("writeVerifiedExclusiveFile() error = %v, want store_changed", err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("ReadFile() replacement error = %v", err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("replacement bytes = %q, want %q", got, replacement)
	}
}

func TestWriteVerifiedExclusiveFileDoesNotRemoveSymlinkReplacement(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	output := filepath.Join(root, "attachment.bin")
	ownedPath := filepath.Join(root, "owned.bin")
	target := filepath.Join(root, "target.bin")
	if err := os.WriteFile(target, []byte("keep target"), 0o600); err != nil {
		t.Fatalf("WriteFile() target error = %v", err)
	}
	reader := &replacingAttachmentReader{
		reader: bytes.NewReader([]byte("verified bytes")),
		replace: func() error {
			if err := os.Rename(output, ownedPath); err != nil {
				return err
			}
			return os.Symlink(target, output)
		},
	}
	err := writeVerifiedExclusiveFile(output, reader, int64(len("verified bytes")), sha256.Sum256([]byte("verified bytes")))
	if errorCodeForTest(err) != "store_changed" {
		t.Fatalf("writeVerifiedExclusiveFile() error = %v, want store_changed", err)
	}
	info, err := os.Lstat(output)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("replacement output = %v, want symlink", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile() target error = %v", err)
	}
	if string(got) != "keep target" {
		t.Fatalf("target bytes = %q, want %q", got, "keep target")
	}
}

type replacingAttachmentReader struct {
	reader  *bytes.Reader
	replace func() error
	done    bool
}

func (r *replacingAttachmentReader) Read(buffer []byte) (int, error) {
	if !r.done {
		if err := r.replace(); err != nil {
			return 0, err
		}
		r.done = true
	}
	return r.reader.Read(buffer)
}
