//go:build darwin

package mail

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireDraftLeaseRejectsSymlinkLockPath(t *testing.T) {
	root := t.TempDir()
	ref := "draft_abcdefghijklmnopqrstuvwx"
	outside := filepath.Join(t.TempDir(), "outside.lock")
	if err := os.WriteFile(outside, []byte("must remain"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := draftLockPath(root, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}

	_, err = acquireDraftLease(context.Background(), root, ref)
	if errorCode(err) != "draft_lock_unsafe" {
		t.Fatalf("acquireDraftLease() error = %v, want draft_lock_unsafe", err)
	}
	content, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "must remain" {
		t.Fatalf("outside lock content = %q, want unchanged sentinel", content)
	}
}

func TestAcquireDraftLeaseRejectsSymlinkParent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	target := t.TempDir()
	if err := os.Symlink(target, root); err != nil {
		t.Fatal(err)
	}

	_, err := acquireDraftLease(context.Background(), root, "draft_abcdefghijklmnopqrstuvwx")
	if errorCode(err) != "draft_lock_unsafe" {
		t.Fatalf("acquireDraftLease() error = %v, want draft_lock_unsafe", err)
	}
}

func TestDraftLockCleanupRejectsReplacedPath(t *testing.T) {
	root := t.TempDir()
	ref := "draft_abcdefghijklmnopqrstuvwx"
	lease, err := acquireDraftLease(context.Background(), root, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.release() }()

	path, err := draftLockPath(root, ref)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.lock")
	if err := os.WriteFile(outside, []byte("must remain"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}

	if err := lease.removeLock(); errorCode(err) != "draft_lock_unsafe" {
		t.Fatalf("removeLock() error = %v, want draft_lock_unsafe", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("replaced lock path error = %v, want symlink to remain", err)
	}
	content, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "must remain" {
		t.Fatalf("outside lock content = %q, want unchanged sentinel", content)
	}
}

func TestDraftLockCleanupUsesPinnedParentAfterReplacement(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "drafts")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ref := "draft_abcdefghijklmnopqrstuvwx"
	lease, err := acquireDraftLease(context.Background(), root, ref)
	if err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	outsideLock := filepath.Join(outside, ref+".lock")
	if err := os.WriteFile(outsideLock, []byte("must remain"), 0o600); err != nil {
		t.Fatal(err)
	}
	movedRoot := filepath.Join(base, "moved-drafts")
	if err := os.Rename(root, movedRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}

	if err := lease.removeLock(); err != nil {
		t.Fatalf("removeLock() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(movedRoot, ref+".lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pinned lock path error = %v, want not-exist", err)
	}
	if _, err := os.Stat(outsideLock); err != nil {
		t.Fatalf("outside lock error = %v, want preserved", err)
	}
	if err := lease.release(); err != nil {
		t.Fatal(err)
	}
}
