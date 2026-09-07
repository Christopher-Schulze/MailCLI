//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openPrivateKeyFile(path string) (openedFile *os.File, openedInfo os.FileInfo, resultErr error) {
	if !filepath.IsAbs(path) {
		return nil, nil, fmt.Errorf("private key path must be absolute")
	}
	cleanPath := filepath.Clean(path)
	requestedParentPath := filepath.Dir(cleanPath)
	name := filepath.Base(cleanPath)
	requestedParentInfo, err := os.Lstat(requestedParentPath)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect private key directory: %w", err)
	}
	if !requestedParentInfo.IsDir() || requestedParentInfo.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("private key directory must be a real directory")
	}
	resolvedParentPath, err := filepath.EvalSymlinks(requestedParentPath)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve private key directory: %w", err)
	}
	parentPath := resolvedParentPath
	parentInfo, err := os.Lstat(parentPath)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect private key directory: %w", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(requestedParentInfo, parentInfo) {
		return nil, nil, fmt.Errorf("private key directory must be a real directory")
	}

	parentDescriptor, err := unix.Open(
		parentPath,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW_ANY,
		0,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("open private key directory: %w", err)
	}
	parent := os.NewFile(uintptr(parentDescriptor), parentPath)
	if parent == nil {
		_ = unix.Close(parentDescriptor)
		return nil, nil, fmt.Errorf("open private key directory: invalid file descriptor")
	}
	defer func() {
		closeErr := parent.Close()
		if resultErr != nil {
			resultErr = errors.Join(resultErr, closeErr)
			if openedFile != nil {
				resultErr = errors.Join(resultErr, openedFile.Close())
				openedFile = nil
				openedInfo = nil
			}
			return
		}
		if closeErr != nil {
			resultErr = closeErr
			if openedFile != nil {
				resultErr = errors.Join(resultErr, openedFile.Close())
				openedFile = nil
				openedInfo = nil
			}
		}
	}()

	openedParentInfo, err := parent.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("inspect opened private key directory: %w", err)
	}
	if !openedParentInfo.IsDir() || !os.SameFile(parentInfo, openedParentInfo) {
		return nil, nil, fmt.Errorf("private key directory changed while opening")
	}

	expectedInfo, err := os.Lstat(filepath.Join(parentPath, name))
	if err != nil {
		return nil, nil, fmt.Errorf("inspect private key path: %w", err)
	}
	if expectedInfo.Mode()&os.ModeSymlink != 0 || !expectedInfo.Mode().IsRegular() ||
		expectedInfo.Mode().Perm()&0o077 != 0 {
		return nil, nil, fmt.Errorf("private key must be a regular file accessible only by its owner")
	}
	fileDescriptor, err := unix.Openat(
		int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW_ANY, 0,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("open private key: %w", err)
	}
	openedFile = os.NewFile(uintptr(fileDescriptor), cleanPath)
	if openedFile == nil {
		_ = unix.Close(fileDescriptor)
		return nil, nil, fmt.Errorf("open private key: invalid file descriptor")
	}
	openedInfo, err = openedFile.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("inspect opened private key: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || openedInfo.Mode().Perm()&0o077 != 0 ||
		!os.SameFile(expectedInfo, openedInfo) {
		return nil, nil, fmt.Errorf("private key must be a regular file accessible only by its owner")
	}

	currentParentInfo, err := os.Lstat(requestedParentPath)
	if err != nil || currentParentInfo.Mode()&os.ModeSymlink != 0 ||
		!currentParentInfo.IsDir() || !os.SameFile(requestedParentInfo, currentParentInfo) {
		return nil, nil, fmt.Errorf("private key directory changed while opening")
	}
	currentInfo, err := os.Lstat(filepath.Join(parentPath, name))
	if err != nil {
		return nil, nil, fmt.Errorf("inspect private key path: %w", err)
	}
	if currentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, currentInfo) {
		return nil, nil, fmt.Errorf("private key changed while opening")
	}
	return openedFile, openedInfo, nil
}
