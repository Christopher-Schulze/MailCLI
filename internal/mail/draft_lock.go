package mail

import (
	"errors"
	"fmt"
	"os"
)

type draftLockResource struct {
	directory *os.File
	file      *os.File
	identity  os.FileInfo
	name      string
	path      string
}

func draftLockResourceFromFile(
	directory *os.File,
	file *os.File,
	name string,
	path string,
) (*draftLockResource, error) {
	identity, err := file.Stat()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("inspect draft lock: %w", err), file.Close(), directory.Close())
	}
	if !identity.Mode().IsRegular() {
		return nil, errors.Join(
			draftLockUnsafeError("draft lock is not a regular file"),
			file.Close(),
			directory.Close(),
		)
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, errors.Join(fmt.Errorf("restrict draft lock: %w", err), file.Close(), directory.Close())
	}
	return &draftLockResource{
		directory: directory,
		file:      file,
		identity:  identity,
		name:      name,
		path:      path,
	}, nil
}

func (r *draftLockResource) close() error {
	if r == nil {
		return nil
	}
	var fileErr error
	if r.file != nil {
		fileErr = r.file.Close()
	}
	var directoryErr error
	if r.directory != nil {
		directoryErr = r.directory.Close()
	}
	return errors.Join(fileErr, directoryErr)
}

func draftLockUnsafeError(message string) error {
	return &OperationError{Code: "draft_lock_unsafe", Message: message}
}

func draftLockChangedError(message string) error {
	return &OperationError{Code: "draft_lock_changed", Message: message}
}
