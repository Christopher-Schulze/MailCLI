package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var removeHandoffSnapshotRoot = os.RemoveAll

// StageDraftAttachments copies verified draft attachments into private paths
// that remain stable while a native compose handoff consumes them.
func StageDraftAttachments(
	ctx context.Context,
	attachments []DraftAttachment,
) (paths []string, cleanup func() error, resultErr error) {
	if len(attachments) > MaximumDraftAttachments {
		return nil, nil, validationError("draft exceeds 100 attachments")
	}
	remaining := MaximumDraftAttachmentBytes
	for _, attachment := range attachments {
		if attachment.Size < 0 || attachment.Size > remaining {
			return nil, nil, validationError("draft attachments exceed 512 MiB total")
		}
		remaining -= attachment.Size
	}
	if len(attachments) == 0 {
		return nil, nil, nil
	}

	root, err := os.MkdirTemp("", "mailcli-handoff-attachments-")
	if err != nil {
		return nil, nil, fmt.Errorf("create private handoff attachment directory: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, nil, errors.Join(
			fmt.Errorf("protect private handoff attachment directory: %w", err),
			os.RemoveAll(root),
		)
	}
	cleanup = func() error {
		return removeHandoffSnapshotRoot(root)
	}
	paths = make([]string, 0, len(attachments))
	remaining = MaximumDraftAttachmentBytes
	for index, attachment := range attachments {
		if err := ctx.Err(); err != nil {
			return paths, cleanup, err
		}
		if attachment.Size < 0 || attachment.Size > remaining {
			return paths, cleanup, validationError("draft attachments exceed 512 MiB total")
		}
		stagedPath, err := stageDraftAttachment(ctx, root, index, attachment)
		if err != nil {
			return paths, cleanup, err
		}
		paths = append(paths, stagedPath)
		remaining -= attachment.Size
	}
	return paths, cleanup, nil
}

func stageDraftAttachment(
	ctx context.Context,
	root string,
	index int,
	expected DraftAttachment,
) (stagedPath string, resultErr error) {
	source, sourceInfo, err := openHandoffAttachment(expected.Path)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := source.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close handoff attachment source: %w", err))
		}
	}()

	directory := filepath.Join(root, strconv.Itoa(index))
	if err := os.Mkdir(directory, 0o700); err != nil {
		return "", fmt.Errorf("create private handoff attachment directory: %w", err)
	}
	stagedPath = filepath.Join(directory, filepath.Base(expected.Path))
	destination, err := os.OpenFile(stagedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create private handoff attachment snapshot: %w", err)
	}
	hash := sha256.New()
	limited := io.LimitReader(source, expected.Size+1)
	written, copyErr := io.Copy(destination, io.TeeReader(contextReader{ctx: ctx, reader: limited}, hash))
	syncErr := destination.Sync()
	closeErr := destination.Close()
	if copyErr != nil {
		var copyFailure error
		if errors.Is(copyErr, context.Canceled) || errors.Is(copyErr, context.DeadlineExceeded) {
			copyFailure = copyErr
		} else {
			copyFailure = handoffAttachmentChanged(expected.Path, fmt.Errorf("copy attachment: %w", copyErr))
		}
		if closeErr != nil {
			copyFailure = errors.Join(copyFailure, fmt.Errorf("close private handoff attachment snapshot: %w", closeErr))
		}
		return "", copyFailure
	}
	if syncErr != nil {
		syncFailure := fmt.Errorf("sync private handoff attachment snapshot: %w", syncErr)
		if closeErr != nil {
			syncFailure = errors.Join(syncFailure, fmt.Errorf("close private handoff attachment snapshot: %w", closeErr))
		}
		return "", syncFailure
	}
	if closeErr != nil {
		return "", fmt.Errorf("close private handoff attachment snapshot: %w", closeErr)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	currentInfo, err := os.Lstat(expected.Path)
	if err != nil {
		return "", handoffAttachmentChanged(expected.Path, fmt.Errorf("recheck attachment: %w", err))
	}
	if !currentInfo.Mode().IsRegular() || !os.SameFile(sourceInfo, currentInfo) {
		return "", handoffAttachmentChanged(expected.Path, errors.New("attachment path changed while it was staged"))
	}
	actualHash := hex.EncodeToString(hash.Sum(nil))
	if written != expected.Size || !strings.EqualFold(actualHash, expected.SHA256) {
		return "", handoffAttachmentChanged(expected.Path, errors.New("attachment size or SHA-256 fingerprint changed"))
	}
	if err := os.Chmod(stagedPath, 0o400); err != nil {
		return "", fmt.Errorf("make private handoff attachment snapshot read-only: %w", err)
	}
	return stagedPath, nil
}

func openHandoffAttachment(path string) (*os.File, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, handoffAttachmentMissing(path, err)
		}
		return nil, nil, handoffAttachmentUnreadable(path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, handoffAttachmentUnreadable(path, errors.New("attachment is not a regular file"))
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, handoffAttachmentMissing(path, err)
		}
		return nil, nil, handoffAttachmentUnreadable(path, err)
	}
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, nil, errors.Join(
			handoffAttachmentUnreadable(path, err),
			closeHandoffAttachment(file),
		)
	}
	currentInfo, err := os.Lstat(path)
	if err != nil {
		return nil, nil, errors.Join(
			handoffAttachmentChanged(path, err),
			closeHandoffAttachment(file),
		)
	}
	if !currentInfo.Mode().IsRegular() || !os.SameFile(openedInfo, currentInfo) {
		return nil, nil, errors.Join(
			handoffAttachmentChanged(path, errors.New("attachment path changed while it was opened")),
			closeHandoffAttachment(file),
		)
	}
	return file, openedInfo, nil
}

func closeHandoffAttachment(file *os.File) error {
	if err := file.Close(); err != nil {
		return fmt.Errorf("close handoff attachment source: %w", err)
	}
	return nil
}

func handoffAttachmentMissing(path string, err error) error {
	return &OperationError{
		Code:    "handoff_attachment_missing",
		Message: fmt.Sprintf("attachment %s is missing: %v", path, err),
	}
}

func handoffAttachmentUnreadable(path string, err error) error {
	return &OperationError{
		Code:    "handoff_attachment_unreadable",
		Message: fmt.Sprintf("attachment %s is not safely readable: %v", path, err),
	}
}

func handoffAttachmentChanged(path string, err error) error {
	return &OperationError{
		Code:    "handoff_attachment_changed",
		Message: fmt.Sprintf("attachment %s changed while it was staged: %v", path, err),
	}
}
