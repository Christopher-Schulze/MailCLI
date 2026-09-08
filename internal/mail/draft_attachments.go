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
)

func fingerprintAttachmentsWithPreviousContext(
	ctx context.Context,
	paths []string,
	previous []DraftAttachment,
) ([]DraftAttachment, error) {
	_ = previous
	attachments := make([]DraftAttachment, 0, len(paths))
	remaining := MaximumDraftAttachmentBytes
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !filepath.IsAbs(path) {
			return nil, validationError("draft attachment paths must be absolute")
		}
		// A stored size and modification time are useful metadata, but they do
		// not prove that the bytes are unchanged. Re-fingerprint every path so
		// an update can never carry stale evidence into a later send.
		attachment, err := fingerprintAttachmentContext(ctx, path, remaining)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
		remaining -= attachment.Size
	}
	return attachments, nil
}

func fingerprintAttachment(path string, maximumSize int64) (DraftAttachment, error) {
	return fingerprintAttachmentContext(context.Background(), path, maximumSize)
}

func fingerprintAttachmentContext(
	ctx context.Context,
	path string,
	maximumSize int64,
) (result DraftAttachment, resultErr error) {
	return fingerprintAttachmentContextWithObserver(ctx, path, maximumSize, nil)
}

func fingerprintAttachmentContextWithObserver(
	ctx context.Context,
	path string,
	maximumSize int64,
	afterRead func(int),
) (result DraftAttachment, resultErr error) {
	if err := ctx.Err(); err != nil {
		return DraftAttachment{}, err
	}
	if maximumSize < 0 {
		return DraftAttachment{}, validationError("draft attachments exceed 512 MiB total")
	}
	file, info, err := openRegularAttachment(path)
	if err != nil {
		if errors.Is(err, errAttachmentNotRegular) {
			return DraftAttachment{}, validationError("draft attachment must be a regular file")
		}
		return DraftAttachment{}, fmt.Errorf("open draft attachment: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close draft attachment: %w", err))
		}
	}()
	if info.Size() < 0 || info.Size() > maximumSize {
		return DraftAttachment{}, validationError("draft attachments exceed 512 MiB total")
	}
	hash := sha256.New()
	limit := maximumSize
	if maximumSize < 1<<63-1 {
		limit++
	}
	reader := attachmentFingerprintReader{ctx: ctx, reader: file, afterRead: afterRead}
	written, err := io.Copy(hash, io.LimitReader(reader, limit))
	if err != nil {
		return DraftAttachment{}, fmt.Errorf("hash draft attachment: %w", err)
	}
	if written > maximumSize {
		return DraftAttachment{}, validationError("draft attachments exceed 512 MiB total")
	}
	if err := ctx.Err(); err != nil {
		return DraftAttachment{}, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		return DraftAttachment{}, fmt.Errorf("stat draft attachment after hashing: %w", err)
	}
	currentInfo, err := os.Lstat(path)
	if err != nil {
		return DraftAttachment{}, fmt.Errorf("recheck draft attachment after hashing: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !currentInfo.Mode().IsRegular() ||
		!os.SameFile(info, openedInfo) || !os.SameFile(openedInfo, currentInfo) ||
		written != info.Size() || openedInfo.Size() != info.Size() || currentInfo.Size() != info.Size() ||
		openedInfo.ModTime().UnixNano() != info.ModTime().UnixNano() ||
		currentInfo.ModTime().UnixNano() != info.ModTime().UnixNano() {
		return DraftAttachment{}, validationError("draft attachment changed while it was fingerprinted")
	}
	return DraftAttachment{
		Path: path, Size: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil)),
		ModTimeNanos: info.ModTime().UnixNano(),
	}, nil
}

type attachmentFingerprintReader struct {
	ctx       context.Context
	reader    io.Reader
	afterRead func(int)
}

func (r attachmentFingerprintReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	read, err := r.reader.Read(buffer)
	if read > 0 && r.afterRead != nil {
		r.afterRead(read)
	}
	return read, err
}

func verifyDraftAttachments(attachments []DraftAttachment) error {
	return verifyDraftAttachmentsContext(context.Background(), attachments)
}

func verifyDraftAttachmentsContext(ctx context.Context, attachments []DraftAttachment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(attachments) > MaximumDraftAttachments {
		return validationError("draft exceeds 100 attachments")
	}
	remaining := MaximumDraftAttachmentBytes
	for _, expected := range attachments {
		if err := ctx.Err(); err != nil {
			return err
		}
		if expected.Size < 0 || expected.Size > remaining {
			return validationError("draft attachments exceed 512 MiB total")
		}
		actual, err := fingerprintAttachmentContext(ctx, expected.Path, remaining)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if actual.Size != expected.Size || actual.SHA256 != expected.SHA256 {
			return validationError("draft attachment " + filepath.Base(expected.Path) + " changed after review; update the draft before sending")
		}
		remaining -= actual.Size
	}
	return nil
}

func preflightDraftAttachmentsContext(ctx context.Context, attachments []DraftAttachment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(attachments) > MaximumDraftAttachments {
		return validationError("draft exceeds 100 attachments")
	}
	remaining := MaximumDraftAttachmentBytes
	for _, expected := range attachments {
		if err := ctx.Err(); err != nil {
			return err
		}
		if expected.Size < 0 || expected.Size > remaining {
			return validationError("draft attachments exceed 512 MiB total")
		}
		file, info, err := openRegularAttachment(expected.Path)
		if err != nil {
			if errors.Is(err, errAttachmentNotRegular) {
				return validationError("draft attachment must be a regular file")
			}
			return fmt.Errorf("stat draft attachment: %w", err)
		}
		closeErr := file.Close()
		if closeErr != nil {
			return fmt.Errorf("close draft attachment preflight: %w", closeErr)
		}
		if info.Size() != expected.Size {
			return validationError("draft attachment " + filepath.Base(expected.Path) + " changed after review; update the draft before sending")
		}
		remaining -= info.Size()
	}
	return nil
}
