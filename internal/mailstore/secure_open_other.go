//go:build !darwin

package mailstore

import (
	"fmt"
	"os"
)

func openRegularPath(_ *os.File, root string, path string) (*os.File, os.FileInfo, error) {
	return openPathWithoutSymlinks(root, path, false)
}

func openDirectoryPath(_ *os.File, root string, path string) (*os.File, os.FileInfo, error) {
	return openPathWithoutSymlinks(root, path, true)
}

func openPathWithoutSymlinks(root string, path string, directory bool) (*os.File, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	validType := info.Mode().IsRegular()
	if directory {
		validType = info.IsDir()
	}
	if !validType || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, operationError("unsafe_message_source", "Mail store path has an unexpected file type")
	}
	if err := validatePathWithoutSymlinks(root, path); err != nil {
		return nil, nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		resultErr := error(operationError("store_changed", "Mail store path changed while opening"))
		if err != nil {
			resultErr = fmt.Errorf("inspect opened Mail store path: %w", err)
		}
		joinCloseError(&resultErr, file, "Mail store path")
		return nil, nil, resultErr
	}
	return file, opened, nil
}
