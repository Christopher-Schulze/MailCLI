//go:build darwin

package mailstore

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func openDarwinTestRoot(t *testing.T) (*os.File, string) {
	t.Helper()
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
	return rootDirectory, root
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

func TestOpenPathAtRejectsSymlinkLeaf(t *testing.T) {
	t.Parallel()
	rootDirectory, root := openDarwinTestRoot(t)
	target := filepath.Join(root, "target.emlx")
	link := filepath.Join(root, "link.emlx")
	if err := os.WriteFile(target, []byte("data"), 0o600); err != nil {
		t.Fatalf("WriteFile() target error = %v", err)
	}
	if err := os.Symlink("target.emlx", link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	opened, _, err := openRegularPath(rootDirectory, root, link)
	if opened != nil {
		_ = opened.Close()
	}
	if secureOpenErrorCode(err) != "unsafe_message_source" {
		t.Fatalf("openRegularPath() symlink error = %v, want unsafe_message_source", err)
	}
}

func TestOpenPathAtRejectsSymlinkParent(t *testing.T) {
	t.Parallel()
	rootDirectory, root := openDarwinTestRoot(t)
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatalf("Mkdir() real error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "file.emlx"), []byte("data"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	linkDir := filepath.Join(root, "link")
	if err := os.Symlink("real", linkDir); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	opened, _, err := openRegularPath(rootDirectory, root, filepath.Join(linkDir, "file.emlx"))
	if opened != nil {
		_ = opened.Close()
	}
	if secureOpenErrorCode(err) != "unsafe_message_source" {
		t.Fatalf("openRegularPath() symlink parent error = %v, want unsafe_message_source", err)
	}
}

func TestOpenPathAtRejectsRootEscape(t *testing.T) {
	t.Parallel()
	rootDirectory, root := openDarwinTestRoot(t)
	outside := filepath.Join(filepath.Dir(root), "outside.emlx")
	opened, _, err := openRegularPath(rootDirectory, root, outside)
	if opened != nil {
		_ = opened.Close()
	}
	if secureOpenErrorCode(err) != "unsafe_message_source" {
		t.Fatalf("openRegularPath() escape error = %v, want unsafe_message_source", err)
	}
}

func TestOpenPathAtRejectsDirectoryAsRegularFile(t *testing.T) {
	t.Parallel()
	rootDirectory, root := openDarwinTestRoot(t)
	subdir := filepath.Join(root, "subdir")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatalf("Mkdir() subdir error = %v", err)
	}
	opened, _, err := openRegularPath(rootDirectory, root, subdir)
	if opened != nil {
		_ = opened.Close()
	}
	if secureOpenErrorCode(err) != "unsafe_message_source" {
		t.Fatalf("openRegularPath() directory error = %v, want unsafe_message_source", err)
	}
}

func TestOpenDirectoryPathRejectsRegularFile(t *testing.T) {
	t.Parallel()
	rootDirectory, root := openDarwinTestRoot(t)
	regular := filepath.Join(root, "regular.txt")
	if err := os.WriteFile(regular, []byte("data"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	opened, _, err := openDirectoryPath(rootDirectory, root, regular)
	if opened != nil {
		_ = opened.Close()
	}
	if err == nil {
		t.Fatal("openDirectoryPath() regular file error = nil, want failure")
	}
	if secureOpenErrorCode(err) == "unsafe_message_source" {
		t.Fatalf("openDirectoryPath() regular file error = %v, want OS error not unsafe classification", err)
	}
}

func TestOpenPathAtServesPinnedRootAfterReplacement(t *testing.T) {
	t.Parallel()
	rootDirectory, root := openDarwinTestRoot(t)
	if err := os.WriteFile(filepath.Join(root, "file.emlx"), []byte("pinned"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	parent := filepath.Dir(root)
	replacement := filepath.Join(parent, "replacement")
	if err := os.Mkdir(replacement, 0o700); err != nil {
		t.Fatalf("Mkdir() replacement error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(replacement, "file.emlx"), []byte("swapped"), 0o600); err != nil {
		t.Fatalf("WriteFile() replacement error = %v", err)
	}
	if err := os.Rename(root, filepath.Join(parent, "renamed-original")); err != nil {
		t.Fatalf("Rename() root error = %v", err)
	}
	if err := os.Rename(replacement, root); err != nil {
		t.Fatalf("Rename() replacement error = %v", err)
	}
	opened, _, err := openRegularPath(rootDirectory, root, filepath.Join(root, "file.emlx"))
	if err != nil {
		t.Fatalf("openRegularPath() pinned root error = %v", err)
	}
	defer func() { _ = opened.Close() }()
	content, err := io.ReadAll(opened)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(content) != "pinned" {
		t.Fatalf("openRegularPath() pinned content = %q, want %q", content, "pinned")
	}
}
