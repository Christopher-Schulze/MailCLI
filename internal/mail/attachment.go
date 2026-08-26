package mail

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func (s *Service) SaveAttachment(
	ctx context.Context,
	request SaveAttachmentRequest,
) (saved SavedAttachment, resultErr error) {
	if err := validateAttachmentRequest(request); err != nil {
		return SavedAttachment{}, err
	}
	temporaryPath, err := attachmentTemporaryPath(request.OutputPath)
	if err != nil {
		return SavedAttachment{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, removeIfPresent(temporaryPath))
	}()
	if err := s.gateway.SaveAttachmentTo(ctx, request.MessageRef, request.AttachmentID, temporaryPath); err != nil {
		return SavedAttachment{}, err
	}
	if err := publishAttachment(temporaryPath, request.OutputPath); err != nil {
		return SavedAttachment{}, err
	}
	saved, err = inspectSavedAttachment(request.AttachmentID, request.OutputPath)
	if err != nil {
		return SavedAttachment{}, errors.Join(err, removeIfPresent(request.OutputPath))
	}
	return saved, nil
}

func validateAttachmentRequest(request SaveAttachmentRequest) error {
	if request.MessageRef == "" || request.AttachmentID == "" || request.OutputPath == "" {
		return validationError("message ref, attachment id, and output path are required")
	}
	if !filepath.IsAbs(request.OutputPath) {
		return validationError("attachment output path must be absolute")
	}
	if _, err := os.Lstat(request.OutputPath); err == nil {
		return validationError("attachment output path already exists")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect attachment output path: %w", err)
	}
	info, err := os.Stat(filepath.Dir(request.OutputPath))
	if err != nil {
		return fmt.Errorf("inspect attachment output directory: %w", err)
	}
	if !info.IsDir() {
		return validationError("attachment output parent is not a directory")
	}
	return nil
}

func attachmentTemporaryPath(outputPath string) (string, error) {
	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate attachment temporary path: %w", err)
	}
	return filepath.Join(
		filepath.Dir(outputPath),
		"."+filepath.Base(outputPath)+".mailcli-"+hex.EncodeToString(suffix[:]),
	), nil
}

func publishAttachment(temporaryPath string, outputPath string) error {
	info, err := os.Lstat(temporaryPath)
	if err != nil {
		return fmt.Errorf("inspect saved attachment: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("mail.app did not save a regular attachment file")
	}
	if err := os.Link(temporaryPath, outputPath); err != nil {
		return fmt.Errorf("publish attachment without overwrite: %w", err)
	}
	if err := os.Chmod(outputPath, 0o600); err != nil {
		return errors.Join(fmt.Errorf("restrict saved attachment: %w", err), removeIfPresent(outputPath))
	}
	return nil
}

func inspectSavedAttachment(attachmentID string, path string) (saved SavedAttachment, resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		return SavedAttachment{}, fmt.Errorf("open saved attachment: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
	}()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return SavedAttachment{}, fmt.Errorf("hash saved attachment: %w", err)
	}
	return SavedAttachment{
		AttachmentID: attachmentID, Path: path, Size: size,
		SHA256: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

func removeIfPresent(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
