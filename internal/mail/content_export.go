package mail

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ContentExport is proof for one complete, exclusively-created content file.
// The digest is recomputed from the published file before the proof is
// returned; callers can therefore use the path without embedding its bytes in
// a structured response.
type ContentExport struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// WriteExclusiveContent writes one content stream to an absolute path that
// must not exist. The callback receives the owned file descriptor, and the
// path is removed on failure only while it still names the file created here.
func WriteExclusiveContent(path string, write func(io.Writer) error) (result ContentExport, resultErr error) {
	if path == "" || !filepath.IsAbs(path) {
		return ContentExport{}, validationError("content export path must be absolute")
	}
	if write == nil {
		return ContentExport{}, validationError("content export writer is required")
	}
	parentPath := filepath.Dir(path)
	parentIdentity, err := os.Lstat(parentPath)
	if err != nil {
		return ContentExport{}, fmt.Errorf("inspect content export directory: %w", err)
	}
	if !parentIdentity.IsDir() || parentIdentity.Mode()&os.ModeSymlink != 0 {
		return ContentExport{}, validationError("content export parent is not a directory")
	}
	root, err := os.OpenRoot(parentPath)
	if err != nil {
		return ContentExport{}, fmt.Errorf("open content export directory: %w", err)
	}
	name := filepath.Base(path)
	owned := false
	retained := false
	var createdIdentity os.FileInfo
	var file *os.File
	defer func() {
		if file != nil {
			resultErr = errors.Join(resultErr, file.Close())
		}
		if owned && !retained {
			resultErr = errors.Join(resultErr, removeOwnedExport(root, parentPath, parentIdentity, name, createdIdentity))
		}
		resultErr = errors.Join(resultErr, root.Close())
	}()
	if err := verifyExportParent(root, parentPath, parentIdentity); err != nil {
		return ContentExport{}, err
	}
	if _, err := root.Lstat(name); err == nil {
		return ContentExport{}, validationError("content export path already exists")
	} else if !os.IsNotExist(err) {
		return ContentExport{}, fmt.Errorf("inspect content export path: %w", err)
	}
	file, err = root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ContentExport{}, fmt.Errorf("create content export: %w", err)
	}
	owned = true
	identity, err := file.Stat()
	if err != nil {
		return ContentExport{}, fmt.Errorf("inspect content export: %w", err)
	}
	createdIdentity = identity
	if !identity.Mode().IsRegular() || identity.Mode().Perm() != 0o600 {
		return ContentExport{}, exportChangedError("content export is not a private regular file")
	}
	hash := sha256.New()
	count := &exportCountingWriter{writer: io.MultiWriter(file, hash)}
	writeErr := write(count)
	if count.limitErr != nil {
		return ContentExport{}, count.limitErr
	}
	if writeErr != nil {
		return ContentExport{}, fmt.Errorf("write content export: %w", writeErr)
	}
	if err := file.Sync(); err != nil {
		return ContentExport{}, fmt.Errorf("sync content export: %w", err)
	}
	if err := verifyExportFile(root, name, file, identity, count.written); err != nil {
		return ContentExport{}, err
	}
	writtenDigest := hex.EncodeToString(hash.Sum(nil))
	if err := file.Close(); err != nil {
		file = nil
		return ContentExport{}, fmt.Errorf("close content export: %w", err)
	}
	file = nil
	verifiedSize, verifiedDigest, err := verifyExportBytes(root, name, identity)
	if err != nil {
		return ContentExport{}, err
	}
	if verifiedSize != count.written || verifiedDigest != writtenDigest {
		return ContentExport{}, exportChangedError("content export bytes changed after writing")
	}
	if err := verifyExportParent(root, parentPath, parentIdentity); err != nil {
		return ContentExport{}, err
	}
	result = ContentExport{Path: path, Size: verifiedSize, SHA256: verifiedDigest}
	retained = true
	return result, nil
}

type exportCountingWriter struct {
	writer   io.Writer
	written  int64
	limitErr error
}

func (w *exportCountingWriter) Write(payload []byte) (int, error) {
	if w.limitErr != nil {
		return 0, w.limitErr
	}
	remaining := MaximumRawSourceBytes - w.written
	if remaining < int64(len(payload)) {
		if remaining > 0 {
			written, err := w.writer.Write(payload[:remaining])
			w.written += int64(written)
			if err != nil {
				return written, err
			}
			if written != int(remaining) {
				return written, io.ErrShortWrite
			}
		}
		w.limitErr = &OperationError{
			Code:    "content_export_too_large",
			Message: fmt.Sprintf("content export exceeds the %d-byte limit", MaximumRawSourceBytes),
		}
		return int(remaining), w.limitErr
	}
	written, err := w.writer.Write(payload)
	w.written += int64(written)
	if err == nil && written != len(payload) {
		return written, io.ErrShortWrite
	}
	return written, err
}

func verifyExportParent(root *os.Root, path string, expected os.FileInfo) error {
	current, err := os.Lstat(path)
	if err != nil {
		return exportChangedError(fmt.Sprintf("content export directory changed: %v", err))
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(expected, current) {
		return exportChangedError("content export directory changed")
	}
	opened, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("inspect pinned content export directory: %w", err)
	}
	if !opened.IsDir() || !os.SameFile(expected, opened) {
		return exportChangedError("pinned content export directory changed")
	}
	return nil
}

func verifyExportFile(root *os.Root, name string, file *os.File, expected os.FileInfo, size int64) error {
	pathInfo, err := root.Lstat(name)
	if err != nil {
		return fmt.Errorf("inspect content export path: %w", err)
	}
	identity, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect content export descriptor: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() ||
		!identity.Mode().IsRegular() || identity.Mode().Perm() != 0o600 || !os.SameFile(expected, pathInfo) ||
		!os.SameFile(expected, identity) || identity.Size() != size {
		return exportChangedError("content export changed while writing")
	}
	return nil
}

func verifyExportBytes(root *os.Root, name string, expected os.FileInfo) (size int64, digest string, resultErr error) {
	file, err := root.Open(name)
	if err != nil {
		return 0, "", fmt.Errorf("open content export for verification: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	identity, err := file.Stat()
	if err != nil {
		return 0, "", fmt.Errorf("inspect content export verification: %w", err)
	}
	if !identity.Mode().IsRegular() || identity.Mode().Perm() != 0o600 || !os.SameFile(expected, identity) {
		return 0, "", exportChangedError("content export changed before verification")
	}
	if identity.Size() < 0 || identity.Size() > MaximumRawSourceBytes {
		return 0, "", exportChangedError("content export exceeds its byte limit")
	}
	hash := sha256.New()
	writtenSize, err := io.Copy(hash, io.LimitReader(file, MaximumRawSourceBytes+1))
	if err != nil {
		return 0, "", fmt.Errorf("hash content export: %w", err)
	}
	if writtenSize > MaximumRawSourceBytes {
		return 0, "", exportChangedError("content export exceeds its byte limit")
	}
	current, err := root.Lstat(name)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() ||
		!os.SameFile(expected, current) {
		return 0, "", exportChangedError("content export changed during verification")
	}
	if writtenSize != identity.Size() {
		return 0, "", exportChangedError("content export size changed during verification")
	}
	return writtenSize, hex.EncodeToString(hash.Sum(nil)), nil
}

func removeOwnedExport(root *os.Root, parentPath string, parent os.FileInfo, name string, expected os.FileInfo) error {
	if err := verifyExportParent(root, parentPath, parent); err != nil {
		return err
	}
	identity, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect content export for cleanup: %w", err)
	}
	if expected == nil || identity.Mode()&os.ModeSymlink != 0 || !identity.Mode().IsRegular() ||
		!os.SameFile(expected, identity) {
		return exportChangedError("content export changed; refusing cleanup")
	}
	if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove content export: %w", err)
	}
	return nil
}

func exportChangedError(message string) error {
	return &OperationError{Code: "content_export_changed", Message: message}
}
