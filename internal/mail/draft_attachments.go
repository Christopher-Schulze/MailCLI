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

func fingerprintAttachmentsWithPrevious(paths []string, previous []DraftAttachment) ([]DraftAttachment, error) {
	attachments := make([]DraftAttachment, 0, len(paths))
	remaining := MaximumDraftAttachmentBytes
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return nil, validationError("draft attachment paths must be absolute")
		}
		if reused, ok := reuseAttachmentFingerprint(path, previous); ok && reused.Size <= remaining {
			attachments = append(attachments, reused)
			remaining -= reused.Size
			continue
		}
		attachment, err := fingerprintAttachment(path, remaining)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
		remaining -= attachment.Size
	}
	return attachments, nil
}

func reuseAttachmentFingerprint(path string, previous []DraftAttachment) (DraftAttachment, bool) {
	for _, attachment := range previous {
		if attachment.Path != path || attachment.ModTimeNanos == 0 || attachment.SHA256 == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != attachment.Size || info.ModTime().UnixNano() != attachment.ModTimeNanos {
			return DraftAttachment{}, false
		}
		return attachment, true
	}
	return DraftAttachment{}, false
}

func fingerprintAttachment(path string, maximumSize int64) (DraftAttachment, error) {
	return fingerprintAttachmentContext(context.Background(), path, maximumSize)
}

func fingerprintAttachmentContext(
	ctx context.Context,
	path string,
	maximumSize int64,
) (DraftAttachment, error) {
	file, err := os.Open(path)
	if err != nil {
		return DraftAttachment{}, fmt.Errorf("open draft attachment: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		return DraftAttachment{}, errors.Join(fmt.Errorf("stat draft attachment: %w", err), file.Close())
	}
	if !info.Mode().IsRegular() {
		return DraftAttachment{}, errors.Join(
			validationError("draft attachment must be a regular file"), file.Close(),
		)
	}
	if info.Size() < 0 || info.Size() > maximumSize {
		return DraftAttachment{}, errors.Join(
			validationError("draft attachments exceed 512 MiB total"), file.Close(),
		)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{ctx: ctx, reader: file}); err != nil {
		return DraftAttachment{}, errors.Join(fmt.Errorf("hash draft attachment: %w", err), file.Close())
	}
	if err := ctx.Err(); err != nil {
		return DraftAttachment{}, errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return DraftAttachment{}, fmt.Errorf("close draft attachment: %w", err)
	}
	return DraftAttachment{
		Path: path, Size: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil)),
		ModTimeNanos: info.ModTime().UnixNano(),
	}, nil
}

func verifyDraftAttachments(attachments []DraftAttachment) error {
	return verifyDraftAttachmentsContext(context.Background(), attachments)
}

func verifyDraftAttachmentsContext(ctx context.Context, attachments []DraftAttachment) error {
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
		info, err := os.Stat(expected.Path)
		if err != nil {
			return fmt.Errorf("stat draft attachment: %w", err)
		}
		if !info.Mode().IsRegular() {
			return validationError("draft attachment must be a regular file")
		}
		if info.Size() != expected.Size {
			return validationError("draft attachment " + filepath.Base(expected.Path) + " changed after review; update the draft before sending")
		}
		remaining -= info.Size()
	}
	return nil
}
