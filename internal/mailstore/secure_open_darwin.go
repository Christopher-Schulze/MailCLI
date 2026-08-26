//go:build darwin

package mailstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func openRegularPath(rootDirectory *os.File, root string, path string) (*os.File, os.FileInfo, error) {
	return openPathAt(rootDirectory, root, path, false)
}

func openDirectoryPath(rootDirectory *os.File, root string, path string) (*os.File, os.FileInfo, error) {
	return openPathAt(rootDirectory, root, path, true)
}

func openPathAt(
	rootDirectory *os.File,
	root string,
	path string,
	directory bool,
) (*os.File, os.FileInfo, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, nil, operationError("unsafe_message_source", "Mail store path escapes its root")
	}
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW_ANY
	if directory {
		flags |= unix.O_DIRECTORY
	}
	fileDescriptor, err := unix.Openat(
		int(rootDirectory.Fd()), relative,
		flags, 0,
	)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, nil, operationError("unsafe_message_source", "Mail store path contains a symlink")
		}
		return nil, nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fileDescriptor), path)
	if file == nil {
		closeErr := unix.Close(fileDescriptor)
		return nil, nil, errors.Join(fmt.Errorf("adopt Mail store file descriptor"), closeErr)
	}
	info, err := file.Stat()
	if err != nil {
		resultErr := fmt.Errorf("inspect opened Mail store path: %w", err)
		joinCloseError(&resultErr, file, "Mail store path")
		return nil, nil, resultErr
	}
	validType := info.Mode().IsRegular()
	if directory {
		validType = info.IsDir()
	}
	if !validType {
		resultErr := error(operationError("unsafe_message_source", "Mail store path has an unexpected file type"))
		joinCloseError(&resultErr, file, "Mail store path")
		return nil, nil, resultErr
	}
	return file, info, nil
}
