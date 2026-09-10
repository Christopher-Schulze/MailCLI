//go:build !darwin

package mail

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func openPinnedDraftStorage(root string, lock *draftLockResource, ref string) (*draftStorage, error) {
	if lock == nil || lock.directory == nil {
		return nil, draftLockUnsafeError("draft lock parent descriptor is unavailable")
	}
	directoryInfo, err := lock.directory.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect leased draft directory: %w", err)
	}
	pinnedRoot, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open pinned draft directory: %w", err)
	}
	openedInfo, err := pinnedRoot.Stat(".")
	if err != nil {
		return nil, errors.Join(fmt.Errorf("inspect pinned draft directory: %w", err), pinnedRoot.Close())
	}
	if !openedInfo.IsDir() || !os.SameFile(directoryInfo, openedInfo) {
		return nil, errors.Join(
			draftLockChangedError("draft directory changed while pinning operation storage"),
			pinnedRoot.Close(),
		)
	}
	storage := &draftStorage{root: pinnedRoot, rootName: root, directory: lock.directory}
	name := ref + ".lock"
	current, err := storage.lstat(name)
	if err != nil {
		return nil, errors.Join(draftLockChangedError("draft lock disappeared while pinning operation storage"), pinnedRoot.Close())
	}
	if !current.Mode().IsRegular() || !os.SameFile(lock.identity, current) {
		return nil, errors.Join(draftLockChangedError("draft lock does not belong to the leased directory"), pinnedRoot.Close())
	}
	return storage, nil
}

func (s *draftStorage) apply(action uint8, name string, other string, perm uint32) error {
	switch action {
	case draftStorageRename:
		return s.root.Rename(name, other)
	case draftStorageRemove:
		return s.root.Remove(name)
	case draftStorageLink:
		return s.root.Link(name, other)
	case draftStorageMkdir:
		return s.root.Mkdir(name, os.FileMode(perm))
	case draftStorageSync:
		if err := s.directory.Sync(); err != nil {
			return fmt.Errorf("sync pinned state directory: %w", err)
		}
	}
	return nil
}

func draftStorageLstat(storage *draftStorage, name string) (os.FileInfo, error) {
	if storage.root != nil {
		return storage.root.Lstat(name)
	}
	return os.Lstat(storage.absolute(name))
}

func draftStorageOpen(storage *draftStorage, name string) (*os.File, error) {
	if storage.root != nil {
		return storage.root.Open(name)
	}
	return os.Open(storage.absolute(name))
}

func draftStorageOpenFile(storage *draftStorage, name string, flag int, perm os.FileMode) (*os.File, error) {
	if storage.root != nil {
		return storage.root.OpenFile(name, flag, perm)
	}
	return os.OpenFile(storage.absolute(name), flag, perm)
}

func removeDraftStorageLock(storage *draftStorage, name string, expected os.FileInfo) error {
	current, err := storage.lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect draft lock for cleanup: %w", err)
	}
	if !current.Mode().IsRegular() {
		return draftLockUnsafeError("refusing to remove a non-regular draft lock")
	}
	if !os.SameFile(expected, current) {
		return draftLockChangedError("draft lock changed before cleanup")
	}
	if err := storage.apply(draftStorageRemove, name, "", 0); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove draft lock: %w", err)
	}
	return nil
}

// The non-Darwin fallback uses exclusive creation and Lstat/File.Stat identity
// checks for the lock itself. Once acquired, the lease opens a descriptor-backed
// os.Root and all draft state operations use that pinned root; Darwin additionally
// uses openat/unlinkat with O_NOFOLLOW_ANY for the lock path.
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

func sweepOrphanDraftLock(root string, ref string, lock *draftLockResource) (bool, error) {
	storage, err := openPinnedDraftStorage(root, lock, ref)
	if err != nil {
		return false, errors.Join(err, lock.close())
	}
	if err := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := errors.Join(storage.root.Close(), lock.close())
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, closeErr
		}
		return false, errors.Join(err, closeErr)
	}
	removeErr := removeDraftStorageLock(storage, lock.name, lock.identity)
	unlockErr := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := errors.Join(lock.close(), storage.root.Close())
	return removeErr == nil && unlockErr == nil && closeErr == nil, errors.Join(removeErr, unlockErr, closeErr)
}
