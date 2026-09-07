//go:build !darwin && !plan9 && !(js && wasm)

package mailstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRegularPathRejectsSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	rootDirectory, err := os.Open(root)
	if err != nil {
		t.Fatalf("Open() root error = %v", err)
	}
	t.Cleanup(func() {
		if err := rootDirectory.Close(); err != nil {
			t.Errorf("close test root: %v", err)
		}
	})
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	if err := os.WriteFile(target, []byte("private"), 0o600); err != nil {
		t.Fatalf("WriteFile() target error = %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	opened, _, err := openRegularPath(rootDirectory, root, target)
	if err != nil {
		t.Fatalf("openRegularPath() target error = %v", err)
	}
	t.Cleanup(func() {
		if err := opened.Close(); err != nil {
			t.Errorf("close opened target: %v", err)
		}
	})

	_, _, err = openRegularPath(rootDirectory, root, link)
	if secureOpenErrorCode(err) != "unsafe_message_source" {
		t.Fatalf("openRegularPath() error = %v, want unsafe_message_source", err)
	}
}

func TestOpenRegularPathRejectsReplacedParent(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	originalRoot := filepath.Join(parent, "root")
	replacementRoot := filepath.Join(parent, "replacement")
	if err := os.Mkdir(originalRoot, 0o700); err != nil {
		t.Fatalf("Mkdir() original root error = %v", err)
	}
	rootDirectory, err := os.Open(originalRoot)
	if err != nil {
		t.Fatalf("Open() root error = %v", err)
	}
	t.Cleanup(func() {
		if err := rootDirectory.Close(); err != nil {
			t.Errorf("close test root: %v", err)
		}
	})
	if err := os.WriteFile(filepath.Join(originalRoot, "message"), []byte("original"), 0o600); err != nil {
		t.Fatalf("WriteFile() original error = %v", err)
	}
	if err := os.Mkdir(replacementRoot, 0o700); err != nil {
		t.Fatalf("Mkdir() replacement root error = %v", err)
	}
	if err := os.Rename(originalRoot, filepath.Join(parent, "moved")); err != nil {
		t.Fatalf("Rename() original root error = %v", err)
	}
	if err := os.Symlink(replacementRoot, originalRoot); err != nil {
		t.Fatalf("Symlink() replacement root error = %v", err)
	}

	_, _, err = openRegularPath(rootDirectory, originalRoot, filepath.Join(originalRoot, "message"))
	if secureOpenErrorCode(err) != "store_changed" {
		t.Fatalf("openRegularPath() error = %v, want store_changed", err)
	}
	if _, err := os.Lstat(filepath.Join(replacementRoot, "message")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement root message = %v, want absent", err)
	}
}

func secureOpenErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		return coded.ErrorCode()
	}
	return ""
}
