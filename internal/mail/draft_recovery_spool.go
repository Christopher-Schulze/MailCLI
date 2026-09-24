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

func persistAcceptedMessageSpool(root string, ref string, message *ComposedMessage, state *draftStorage) (*AcceptedMessageSpool, error) {
	if message == nil || message.Size() <= 0 || message.Size() > maximumAcceptedMessageSpoolBytes {
		return nil, errors.New("accepted message spool exceeds its byte limit")
	}
	name := ref + ".send-spool"
	temporaryPath, err := attachmentTemporaryPath(state.absolute(name))
	if err != nil {
		return nil, err
	}
	temporary := filepath.Base(temporaryPath)
	source, err := message.Open()
	if err != nil {
		return nil, fmt.Errorf("open composed message for recovery: %w", err)
	}
	file, temporaryIdentity, err := state.openFile(temporary, nil, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create recovery spool: %w", err), source.Close())
	}
	cleanupTemporary := func() error { return removeDraftStorageFile(state, temporary, temporaryIdentity, "") }
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(source, message.Size()+1))
	sourceCloseErr := source.Close()
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil || sourceCloseErr != nil || syncErr != nil || closeErr != nil {
		return nil, errors.Join(
			fmt.Errorf("write recovery spool: %w", errors.Join(copyErr, sourceCloseErr, syncErr, closeErr)),
			cleanupTemporary(),
		)
	}
	if written != message.Size() {
		return nil, errors.Join(
			errors.New("recovery spool size changed while copying"),
			cleanupTemporary(),
		)
	}
	current, err := state.lstat(temporary)
	if err != nil || !current.Mode().IsRegular() ||
		!os.SameFile(temporaryIdentity, current) || current.Size() != written {
		return nil, errors.Join(errors.New("recovery spool changed before publication"), cleanupTemporary())
	}
	if err := state.apply(draftStorageLink, temporary, name, 0); err != nil {
		return nil, errors.Join(fmt.Errorf("publish recovery spool: %w", err), cleanupTemporary())
	}
	if err := state.apply(draftStorageRemove, temporary, "", 0); err != nil {
		return nil, errors.Join(fmt.Errorf("remove recovery spool temporary file: %w", err), cleanupTemporary())
	}
	if err := state.apply(draftStorageSync, "", "", 0); err != nil {
		return nil, fmt.Errorf("persist recovery spool directory: %w", err)
	}
	return &AcceptedMessageSpool{Size: written, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

// recoverUnclaimedAcceptedMessageSpool removes only the canonical private
// spool for a ref when no send claim exists and the pinned file stays
// unchanged while it is verified. A claimed or ambiguous spool is retained.
func recoverUnclaimedAcceptedMessageSpool(root string, ref string, state *draftStorage) error {
	if _, err := acceptedMessageSpoolPath(root, ref); err != nil {
		return err
	}
	name := ref + ".send-spool"
	pathInfo, err := state.lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return unclaimedAcceptedMessageSpoolChanged(fmt.Errorf("inspect spool: %w", err))
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Mode().Perm() != 0o600 ||
		pathInfo.Size() <= 0 || pathInfo.Size() > maximumAcceptedMessageSpoolBytes {
		return unclaimedAcceptedMessageSpoolChanged(errors.New("spool is not a bounded regular mode-0600 file"))
	}
	if attempt, err := readSendAttempt(root, ref, state); err != nil {
		return unclaimedAcceptedMessageSpoolChanged(fmt.Errorf("inspect send claim: %w", err))
	} else if attempt != nil {
		return &OperationError{
			Code:    "send_retry_blocked",
			Message: "a retained send attempt owns the recovery spool; the claim and spool were preserved",
		}
	}

	file, identity, err := state.openFile(name, pathInfo, os.O_RDONLY, 0)
	if err != nil {
		return unclaimedAcceptedMessageSpoolChanged(fmt.Errorf("open spool: %w", err))
	}
	if !identity.Mode().IsRegular() || identity.Mode().Perm() != 0o600 ||
		identity.Size() != pathInfo.Size() || !os.SameFile(pathInfo, identity) {
		return errors.Join(
			unclaimedAcceptedMessageSpoolChanged(errors.New("spool identity changed while opening")),
			file.Close(),
		)
	}
	openedInfo, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil {
		return unclaimedAcceptedMessageSpoolChanged(errors.Join(statErr, closeErr))
	}
	if !os.SameFile(identity, openedInfo) || openedInfo.Size() != identity.Size() ||
		openedInfo.Mode().Perm() != 0o600 || !openedInfo.ModTime().Equal(identity.ModTime()) {
		return unclaimedAcceptedMessageSpoolChanged(errors.New("spool identity changed while verifying"))
	}
	currentInfo, err := state.lstat(name)
	if err != nil || !currentInfo.Mode().IsRegular() || !os.SameFile(identity, currentInfo) ||
		currentInfo.Size() != identity.Size() || currentInfo.Mode().Perm() != 0o600 ||
		!currentInfo.ModTime().Equal(identity.ModTime()) {
		return unclaimedAcceptedMessageSpoolChanged(errors.Join(errors.New("spool path identity changed"), err))
	}
	if attempt, err := readSendAttempt(root, ref, state); err != nil {
		return unclaimedAcceptedMessageSpoolChanged(fmt.Errorf("recheck send claim: %w", err))
	} else if attempt != nil {
		return &OperationError{
			Code:    "send_retry_blocked",
			Message: "a retained send attempt now owns the recovery spool; the claim and spool were preserved",
		}
	}
	if err := removeDraftStorageFile(state, name, identity, "recovery spool"); err != nil {
		return fmt.Errorf("remove verified unclaimed recovery spool: %w", err)
	}
	return nil
}

func unclaimedAcceptedMessageSpoolChanged(cause error) error {
	message := "the unclaimed recovery spool could not be verified and was preserved; SMTP was not contacted"
	if cause != nil {
		message += ": " + cause.Error()
	}
	return &OperationError{Code: "send_recovery_spool_changed", Message: message}
}

func openAcceptedMessageSpool(ref string, attempt SendAttempt, state *draftStorage) (*ComposedMessage, error) {
	spool := attempt.RecoverySpool
	if !validAcceptedMessageSpool(spool) {
		return nil, &OperationError{Code: "send_recovery_spool_invalid", Message: "recovery spool metadata is invalid"}
	}
	if spool == nil {
		return nil, &OperationError{Code: "send_recovery_spool_unavailable", Message: "no recovery spool is retained; automatic mirror retry is unavailable"}
	}
	name := ref + ".send-spool"
	var err error
	pathInfo, err := state.root.Lstat(name)
	if os.IsNotExist(err) {
		return nil, &OperationError{Code: "send_recovery_spool_missing", Message: "the accepted-message recovery spool is missing; the draft is retained for explicit resolution"}
	}
	if err != nil || !pathInfo.Mode().IsRegular() {
		return nil, &OperationError{Code: "send_recovery_spool_changed", Message: "the retained recovery spool is not a regular file"}
	}
	file, info, err := state.openFile(name, pathInfo, os.O_RDONLY, 0)
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
	return &ComposedMessage{
		size: spool.Size, messageID: attempt.MessageID,
		storage: state, storageName: name, storageIdentity: info,
	}, nil
}

func removeAcceptedMessageSpool(ref string, attempt *SendAttempt, state *draftStorage) error {
	if attempt == nil || attempt.RecoverySpool == nil {
		return nil
	}
	message, err := openAcceptedMessageSpool(ref, *attempt, state)
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
	return state.apply(draftStorageSync, "", "", 0)
}
