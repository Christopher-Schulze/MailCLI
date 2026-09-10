package mail

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maximumAcceptedMessageSpoolBytes = int64(1 << 30)

func acceptedMessageSpoolPath(root string, ref string) (string, error) {
	if _, err := draftPath(root, ref); err != nil {
		return "", err
	}
	return filepath.Join(root, ref+".send-spool"), nil
}

func validAcceptedMessageSpool(spool *AcceptedMessageSpool) bool {
	if spool == nil || spool.Size <= 0 || spool.Size > maximumAcceptedMessageSpoolBytes {
		return spool == nil
	}
	digest, err := hex.DecodeString(strings.TrimSpace(spool.SHA256))
	return err == nil && len(digest) == sha256.Size
}

func persistAcceptedMessageSpool(root string, ref string, message *ComposedMessage) (*AcceptedMessageSpool, error) {
	if message == nil || message.Size() <= 0 || message.Size() > maximumAcceptedMessageSpoolBytes {
		return nil, fmt.Errorf("accepted message spool exceeds its byte limit")
	}
	path, err := acceptedMessageSpoolPath(root, ref)
	if err != nil {
		return nil, err
	}
	temporary, err := attachmentTemporaryPath(path)
	if err != nil {
		return nil, err
	}
	source, _, err := openRegularAttachment(message.path)
	if err != nil {
		return nil, fmt.Errorf("open composed message for recovery: %w", err)
	}
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create recovery spool: %w", err), source.Close())
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(source, message.Size()+1))
	sourceCloseErr := source.Close()
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil || sourceCloseErr != nil || syncErr != nil || closeErr != nil {
		return nil, errors.Join(
			fmt.Errorf("write recovery spool: %w", errors.Join(copyErr, sourceCloseErr, syncErr, closeErr)),
			removeIfPresent(temporary),
		)
	}
	if written != message.Size() {
		return nil, errors.Join(
			fmt.Errorf("recovery spool size changed while copying"),
			removeIfPresent(temporary),
		)
	}
	if err := os.Link(temporary, path); err != nil {
		return nil, errors.Join(fmt.Errorf("publish recovery spool: %w", err), removeIfPresent(temporary))
	}
	if err := os.Remove(temporary); err != nil {
		return nil, errors.Join(fmt.Errorf("remove recovery spool temporary file: %w", err), removeIfPresent(temporary))
	}
	if err := syncDirectory(root); err != nil {
		return nil, fmt.Errorf("persist recovery spool directory: %w", err)
	}
	return &AcceptedMessageSpool{Size: written, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func openAcceptedMessageSpool(root string, ref string, attempt SendAttempt) (*ComposedMessage, error) {
	spool := attempt.RecoverySpool
	if !validAcceptedMessageSpool(spool) {
		return nil, &OperationError{Code: "send_recovery_spool_invalid", Message: "recovery spool metadata is invalid"}
	}
	if spool == nil {
		return nil, &OperationError{Code: "send_recovery_spool_unavailable", Message: "no recovery spool is retained; automatic mirror retry is unavailable"}
	}
	path, err := acceptedMessageSpoolPath(root, ref)
	if err != nil {
		return nil, err
	}
	file, info, err := openRegularAttachment(path)
	if os.IsNotExist(err) {
		return nil, &OperationError{Code: "send_recovery_spool_missing", Message: "the accepted-message recovery spool is missing; the draft is retained for explicit resolution"}
	}
	if err != nil {
		return nil, &OperationError{Code: "send_recovery_spool_changed", Message: "the retained recovery spool could not be opened"}
	}
	if info.Size() != spool.Size {
		if closeErr := file.Close(); closeErr != nil {
			return nil, errors.Join(&OperationError{Code: "send_recovery_spool_changed", Message: "the retained recovery spool size changed"}, closeErr)
		}
		return nil, &OperationError{Code: "send_recovery_spool_changed", Message: "the accepted-message recovery spool changed after it was retained"}
	}
	hash := sha256.New()
	written, copyErr := io.Copy(hash, io.LimitReader(file, spool.Size+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return nil, &OperationError{Code: "send_recovery_spool_changed", Message: "the retained recovery spool could not be read"}
	}
	if written != spool.Size || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), spool.SHA256) {
		return nil, &OperationError{Code: "send_recovery_spool_changed", Message: "the retained recovery spool bytes no longer match"}
	}
	return &ComposedMessage{path: path, size: spool.Size, messageID: attempt.MessageID}, nil
}

func removeAcceptedMessageSpool(root string, ref string, attempt *SendAttempt) error {
	if attempt == nil || attempt.RecoverySpool == nil {
		return nil
	}
	message, err := openAcceptedMessageSpool(root, ref, *attempt)
	if err != nil {
		var operation *OperationError
		if errors.As(err, &operation) && operation.Code == "send_recovery_spool_missing" {
			return nil
		}
		return err
	}
	if err := message.Remove(); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove recovery spool: %w", err)
	}
	return syncDirectory(root)
}
