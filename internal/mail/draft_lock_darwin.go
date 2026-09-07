//go:build darwin

package mail

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openDraftLockResource(root string, ref string) (*draftLockResource, error) {
	return openDraftLockResourceDarwin(root, ref, true)
}

func openExistingDraftLockResource(root string, ref string) (*draftLockResource, error) {
	return openDraftLockResourceDarwin(root, ref, false)
}

func openDraftLockResourceDarwin(root string, ref string, create bool) (*draftLockResource, error) {
	path, err := draftLockPath(root, ref)
	if err != nil {
		return nil, err
	}
	directory, err := openDraftLockDirectoryDarwin(root)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(path)
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW_ANY
	var fileDescriptor int
	if create {
		fileDescriptor, err = unix.Openat(
			int(directory.Fd()), name,
			flags|unix.O_CREAT|unix.O_EXCL,
			0o600,
		)
		if errors.Is(err, unix.EEXIST) {
			fileDescriptor, err = unix.Openat(int(directory.Fd()), name, flags, 0)
		}
	} else {
		fileDescriptor, err = unix.Openat(int(directory.Fd()), name, flags, 0)
	}
	if err != nil {
		closeErr := directory.Close()
		if errors.Is(err, unix.ELOOP) {
			return nil, errors.Join(draftLockUnsafeError("draft lock path contains a symbolic link"), closeErr)
		}
		return nil, errors.Join(fmt.Errorf("open draft lock: %w", err), closeErr)
	}
	file := os.NewFile(uintptr(fileDescriptor), path)
	if file == nil {
		closeErr := errors.Join(unix.Close(fileDescriptor), directory.Close())
		return nil, fmt.Errorf("adopt draft lock file descriptor: %w", closeErr)
	}
	return draftLockResourceFromFile(directory, file, name, path)
}

func openDraftLockDirectoryDarwin(root string) (*os.File, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect draft directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, draftLockUnsafeError("draft lock parent is not a real directory")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve draft directory: %w", err)
	}
	fileDescriptor, err := unix.Open(
		resolvedRoot,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW_ANY,
		0,
	)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, draftLockUnsafeError("draft lock parent contains a symbolic link")
		}
		return nil, fmt.Errorf("open draft lock parent: %w", err)
	}
	directory := os.NewFile(uintptr(fileDescriptor), resolvedRoot)
	if directory == nil {
		return nil, fmt.Errorf("adopt draft lock parent descriptor: %w", unix.Close(fileDescriptor))
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

func (r *draftLockResource) remove() error {
	fileDescriptor, err := unix.Openat(
		int(r.directory.Fd()), r.name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW_ANY,
		0,
	)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		if errors.Is(err, unix.ELOOP) {
			return draftLockUnsafeError("refusing to remove a symbolic-link draft lock")
		}
		return fmt.Errorf("reopen draft lock for cleanup: %w", err)
	}
	current := os.NewFile(uintptr(fileDescriptor), r.path)
	if current == nil {
		return fmt.Errorf("adopt draft lock cleanup descriptor: %w", unix.Close(fileDescriptor))
	}
	currentInfo, statErr := current.Stat()
	closeErr := current.Close()
	if statErr != nil {
		return errors.Join(fmt.Errorf("inspect draft lock for cleanup: %w", statErr), closeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close draft lock cleanup descriptor: %w", closeErr)
	}
	if !currentInfo.Mode().IsRegular() || !os.SameFile(r.identity, currentInfo) {
		return draftLockChangedError("draft lock changed before cleanup")
	}
	if err := unix.Unlinkat(int(r.directory.Fd()), r.name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove draft lock: %w", err)
	}
	return nil
}
