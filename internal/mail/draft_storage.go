package mail

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// draftStorage is the filesystem boundary for one draft operation. An empty
// root uses ordinary path operations for unlocked reads; a non-empty root is
// descriptor-backed so a renamed or replaced pathname cannot redirect a
// locked operation.
type draftStorage struct {
	rootName  string
	root      *os.Root
	directory *os.File
}

func (s *draftStorage) absolute(name string) string { return filepath.Join(s.rootName, name) }

func (s *draftStorage) lstat(name string) (os.FileInfo, error) {
	if s.root != nil {
		return s.root.Lstat(name)
	}
	return os.Lstat(s.absolute(name))
}

func (s *draftStorage) openFile(name string, expected os.FileInfo, flag int, perm os.FileMode) (*os.File, os.FileInfo, error) {
	var (
		file *os.File
		err  error
	)
	if s.root != nil {
		file, err = s.root.OpenFile(name, flag, perm)
	} else {
		file, err = os.OpenFile(s.absolute(name), flag, perm)
	}
	if err != nil {
		return nil, nil, err
	}
	identity, err := file.Stat()
	if err != nil {
		return nil, nil, errors.Join(err, file.Close())
	}
	if expected != nil && !os.SameFile(expected, identity) {
		return nil, nil, errors.Join(errors.New("file identity changed while opening"), file.Close())
	}
	return file, identity, nil
}

const (
	draftStorageRename = iota
	draftStorageRemove
	draftStorageLink
	draftStorageMkdir
	draftStorageSync
)

func removeDraftStorageFile(storage *draftStorage, name string, expected os.FileInfo, object string) error {
	if expected != nil {
		current, err := storage.lstat(name)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect draft file for cleanup: %w", err)
		}
		if !current.Mode().IsRegular() || !os.SameFile(expected, current) {
			return draftLockChangedError("draft file changed before cleanup")
		}
	}
	var removeErr error
	if storage.root == nil {
		removeErr = os.Remove(storage.absolute(name))
	} else {
		removeErr = storage.apply(draftStorageRemove, name, "", 0)
	}
	if removeErr != nil && !os.IsNotExist(removeErr) {
		if object != "" {
			return fmt.Errorf("remove %s: %w", object, removeErr)
		}
		return fmt.Errorf("remove draft file: %w", removeErr)
	}
	if object != "" {
		return storage.apply(draftStorageSync, "", "", 0)
	}
	return nil
}

func draftStorageFor(root string, storage ...*draftStorage) *draftStorage {
	if len(storage) == 1 {
		return storage[0]
	}
	return &draftStorage{rootName: root}
}

func replacePrivateDraftFile(
	storage *draftStorage,
	name string,
	payload []byte,
	writeMessage string,
	publishMessage string,
) error {
	temporaryPath, err := attachmentTemporaryPath(storage.absolute(name))
	if err != nil {
		return err
	}
	temporary := filepath.Base(temporaryPath)
	temporaryIdentity, err := writePrivateDraftFile(storage, temporary, payload)
	if err != nil {
		return fmt.Errorf("%s: %w", writeMessage, err)
	}
	var publishErr error
	if storage.root == nil {
		publishErr = os.Rename(storage.absolute(temporary), storage.absolute(name))
	} else {
		publishErr = storage.apply(draftStorageRename, temporary, name, 0)
	}
	if publishErr != nil {
		return fmt.Errorf("%s: %w", publishMessage, errors.Join(publishErr, removeDraftStorageFile(storage, temporary, temporaryIdentity, "")))
	}
	if storage.root == nil {
		return syncDirectory(storage.rootName)
	}
	return storage.apply(draftStorageSync, "", "", 0)
}

func writePrivateDraftFile(storage *draftStorage, name string, payload []byte) (os.FileInfo, error) {
	file, identity, err := storage.openFile(name, nil, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create private draft file: %w", err)
	}
	var operationErr error
	if !identity.Mode().IsRegular() || identity.Mode().Perm() != 0o600 {
		operationErr = draftLockUnsafeError("private draft file is not a regular mode-0600 file")
	} else if written, writeErr := file.Write(payload); writeErr != nil {
		operationErr = fmt.Errorf("write private draft file: %w", writeErr)
	} else if written != len(payload) {
		operationErr = fmt.Errorf("write private draft file: %w", io.ErrShortWrite)
	} else if syncErr := file.Sync(); syncErr != nil {
		operationErr = fmt.Errorf("sync private draft file: %w", syncErr)
	}
	if operationErr == nil {
		if err := file.Close(); err != nil {
			operationErr = fmt.Errorf("close private draft file: %w", err)
		}
	} else {
		operationErr = errors.Join(operationErr, file.Close())
	}
	if operationErr == nil {
		if storage.root == nil {
			operationErr = syncDirectory(storage.rootName)
		} else {
			operationErr = storage.apply(draftStorageSync, "", "", 0)
		}
	}
	if operationErr != nil {
		return nil, errors.Join(operationErr, removeDraftStorageFile(storage, name, identity, ""))
	}
	return identity, nil
}
