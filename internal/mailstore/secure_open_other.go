//go:build !darwin && !plan9 && !(js && wasm)

package mailstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func openRegularPath(rootDirectory *os.File, root string, path string) (*os.File, os.FileInfo, error) {
	return openPathWithoutSymlinks(rootDirectory, root, path, false)
}

func openDirectoryPath(rootDirectory *os.File, root string, path string) (*os.File, os.FileInfo, error) {
	return openPathWithoutSymlinks(rootDirectory, root, path, true)
}

func ensureSecureOpenSupported() error {
	return nil
}

func openPathWithoutSymlinks(
	rootDirectory *os.File,
	root string,
	path string,
	directory bool,
) (openedFile *os.File, openedInfo os.FileInfo, resultErr error) {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return nil, nil, operationError("unsafe_message_source", "Mail store path cannot be made relative to its root")
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, nil, operationError("unsafe_message_source", "Mail store path escapes its root")
	}
	if rootDirectory == nil {
		return nil, nil, operationError("unsafe_message_source", "Mail store root descriptor is unavailable")
	}
	pinnedRoot, err := os.OpenRoot(root)
	if err != nil {
		return nil, nil, fmt.Errorf("open Mail store root: %w", err)
	}
	var file *os.File
	defer func() {
		closeErr := pinnedRoot.Close()
		if resultErr != nil {
			if file != nil {
				joinCloseError(&resultErr, file, "Mail store path")
			}
			if closeErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("close Mail store root: %w", closeErr))
			}
			openedFile = nil
			openedInfo = nil
			return
		}
		if closeErr != nil {
			resultErr = fmt.Errorf("close Mail store root: %w", closeErr)
			if file != nil {
				joinCloseError(&resultErr, file, "Mail store path")
			}
			openedFile = nil
			openedInfo = nil
		}
	}()
	rootInfo, err := rootDirectory.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("inspect Mail store root descriptor: %w", err)
	}
	pinnedInfo, err := pinnedRoot.Stat(".")
	if err != nil {
		return nil, nil, fmt.Errorf("inspect pinned Mail store root: %w", err)
	}
	if !rootInfo.IsDir() || !pinnedInfo.IsDir() || !os.SameFile(rootInfo, pinnedInfo) {
		return nil, nil, operationError("store_changed", "Mail store root changed while opening")
	}
	if err := validatePinnedPathWithoutSymlinks(pinnedRoot, relative); err != nil {
		return nil, nil, err
	}
	info, err := pinnedRoot.Lstat(relative)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect Mail store path: %w", err)
	}
	validType := info.Mode().IsRegular()
	if directory {
		validType = info.IsDir()
	}
	if !validType || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, operationError("unsafe_message_source", "Mail store path has an unexpected file type")
	}
	file, err = pinnedRoot.OpenFile(relative, os.O_RDONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, err
		}
		return nil, nil, operationErrorWithCause(
			"unsafe_message_source", "Mail store path cannot be opened safely", err,
		)
	}
	openedInfo, err = file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("inspect opened Mail store path: %w", err)
	}
	if !os.SameFile(info, openedInfo) {
		return nil, nil, operationError("store_changed", "Mail store path changed while opening")
	}
	if err := validatePinnedPathWithoutSymlinks(pinnedRoot, relative); err != nil {
		return nil, nil, err
	}
	openedFile = file
	return openedFile, openedInfo, nil
}

func validatePinnedPathWithoutSymlinks(root *os.Root, relative string) error {
	clean := filepath.Clean(relative)
	if clean == "." {
		return nil
	}
	current := ""
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := root.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect Mail store path component: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return operationError("unsafe_message_source", "Mail store path contains a symlink")
		}
	}
	return nil
}
