//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type exclusiveOutputDarwin struct {
	file     *os.File
	parent   *os.File
	name     string
	identity unix.Stat_t
}

func openExclusiveOutput(path string, mode os.FileMode) (exclusiveOutput, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("release output path must be absolute")
	}
	cleanPath := filepath.Clean(path)
	requestedParentPath := filepath.Dir(cleanPath)
	name := filepath.Base(cleanPath)
	requestedParentInfo, err := os.Lstat(requestedParentPath)
	if err != nil {
		return nil, fmt.Errorf("inspect release output directory: %w", err)
	}
	if !requestedParentInfo.IsDir() || requestedParentInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("release output directory must be a real directory")
	}
	resolvedParentPath, err := filepath.EvalSymlinks(requestedParentPath)
	if err != nil {
		return nil, fmt.Errorf("resolve release output directory: %w", err)
	}
	parentInfo, err := os.Lstat(resolvedParentPath)
	if err != nil {
		return nil, fmt.Errorf("inspect release output directory: %w", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(requestedParentInfo, parentInfo) {
		return nil, fmt.Errorf("release output directory changed while opening")
	}
	parentDescriptor, err := unix.Open(
		resolvedParentPath,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW_ANY,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open release output directory: %w", err)
	}
	parent := os.NewFile(uintptr(parentDescriptor), resolvedParentPath)
	if parent == nil {
		_ = unix.Close(parentDescriptor)
		return nil, fmt.Errorf("open release output directory: invalid file descriptor")
	}
	opened := &exclusiveOutputDarwin{
		parent: parent, name: name,
	}
	fileDescriptor, err := unix.Openat(
		int(parent.Fd()), name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW_ANY,
		uint32(mode.Perm()),
	)
	if err != nil {
		_ = opened.CloseParent()
		return nil, fmt.Errorf("create release output: %w", err)
	}
	opened.file = os.NewFile(uintptr(fileDescriptor), cleanPath)
	if opened.file == nil {
		_ = unix.Close(fileDescriptor)
		_ = opened.CloseParent()
		return nil, fmt.Errorf("create release output: invalid file descriptor")
	}
	if err := unix.Fstat(int(opened.file.Fd()), &opened.identity); err != nil {
		_ = opened.CloseFile()
		_ = opened.CloseParent()
		return nil, fmt.Errorf("inspect created release output: %w", err)
	}
	if opened.identity.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = opened.CloseFile()
		_ = opened.Cleanup()
		_ = opened.CloseParent()
		return nil, fmt.Errorf("release output is not a regular file")
	}
	return opened, nil
}

func (output *exclusiveOutputDarwin) Write(payload []byte) (int, error) {
	return output.file.Write(payload)
}

func (output *exclusiveOutputDarwin) Sync() error {
	return output.file.Sync()
}

func (output *exclusiveOutputDarwin) Validate(expectedSize int64) error {
	var opened unix.Stat_t
	if err := unix.Fstat(int(output.file.Fd()), &opened); err != nil {
		return fmt.Errorf("inspect release output descriptor: %w", err)
	}
	if !sameDarwinFileIdentity(output.identity, opened) ||
		opened.Mode&unix.S_IFMT != unix.S_IFREG || opened.Size != expectedSize {
		return fmt.Errorf("release output changed before close")
	}
	var current unix.Stat_t
	if err := unix.Fstatat(int(output.parent.Fd()), output.name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("inspect release output path: %w", err)
	}
	if !sameDarwinFileIdentity(output.identity, current) ||
		current.Mode&unix.S_IFMT != unix.S_IFREG || current.Size != expectedSize {
		return fmt.Errorf("release output path changed before close")
	}
	return nil
}

func (output *exclusiveOutputDarwin) CloseFile() error {
	if output.file == nil {
		return nil
	}
	file := output.file
	output.file = nil
	return file.Close()
}

func (output *exclusiveOutputDarwin) Cleanup() error {
	if output.parent == nil {
		return fmt.Errorf("release output parent is closed")
	}
	var current unix.Stat_t
	if err := unix.Fstatat(int(output.parent.Fd()), output.name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("inspect failed release output: %w", err)
	}
	if !sameDarwinFileIdentity(output.identity, current) || current.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("release output changed; refusing cleanup")
	}
	if err := unix.Unlinkat(int(output.parent.Fd()), output.name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove failed release output: %w", err)
	}
	return nil
}

func (output *exclusiveOutputDarwin) CloseParent() error {
	if output.parent == nil {
		return nil
	}
	parent := output.parent
	output.parent = nil
	return parent.Close()
}

func sameDarwinFileIdentity(first unix.Stat_t, second unix.Stat_t) bool {
	return first.Dev == second.Dev && first.Ino == second.Ino
}
