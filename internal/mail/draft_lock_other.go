//go:build !darwin

package mail

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The non-Darwin fallback uses exclusive creation and Lstat/File.Stat identity
// checks, but the standard library has no portable descriptor-relative unlink
// or no-follow open primitive. A same-privilege replacement can still race the
// validation on these platforms; Darwin uses openat/unlinkat with O_NOFOLLOW_ANY.
func openDraftLockResource(root string, ref string) (*draftLockResource, error) {
	return openDraftLockResourceOther(root, ref, true)
}

func openExistingDraftLockResource(root string, ref string) (*draftLockResource, error) {
	return openDraftLockResourceOther(root, ref, false)
}

func openDraftLockResourceOther(root string, ref string, create bool) (*draftLockResource, error) {
	path, err := draftLockPath(root, ref)
	if err != nil {
		return nil, err
	}
	directory, err := openDraftLockDirectoryOther(root)
	if err != nil {
		return nil, err
	}
	var file *os.File
	if create {
		file, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errors.Is(err, os.ErrExist) {
			file, err = openExistingDraftLockFileOther(path)
		}
	} else {
		file, err = openExistingDraftLockFileOther(path)
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open draft lock: %w", err), directory.Close())
	}
	return draftLockResourceFromFile(directory, file, filepath.Base(path), path)
}

func openDraftLockDirectoryOther(root string) (*os.File, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect draft directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, draftLockUnsafeError("draft lock parent is not a real directory")
	}
	directory, err := os.Open(root)
	if err != nil {
		return nil, fmt.Errorf("open draft lock parent: %w", err)
	}
	openedInfo, err := directory.Stat()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("inspect opened draft lock parent: %w", err), directory.Close())
	}
	if !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
		return nil, errors.Join(draftLockChangedError("draft lock parent changed while opening"), directory.Close())
	}
	return directory, nil
}

func openExistingDraftLockFileOther(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, draftLockUnsafeError("draft lock path is not a regular file")
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, errors.Join(draftLockChangedError("draft lock changed while opening"), file.Close())
	}
	return file, nil
}

func (r *draftLockResource) remove() error {
	info, err := os.Lstat(r.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect draft lock for cleanup: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return draftLockUnsafeError("refusing to remove a non-regular draft lock")
	}
	if !os.SameFile(r.identity, info) {
		return draftLockChangedError("draft lock changed before cleanup")
	}
	if err := os.Remove(r.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove draft lock: %w", err)
	}
	return nil
}
