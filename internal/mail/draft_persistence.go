package mail

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maximumDraftStateBytes = int64(20 * 1024 * 1024)

func writeDraftFile(root string, draft Draft) (resultErr error) {
	path, err := draftPath(root, draft.Ref)
	if err != nil {
		return err
	}
	draft.SendAttempt = nil
	draft.SaveAttempt = nil
	draft.HandoffAttempt = nil
	payload, err := json.MarshalIndent(draft, "", "  ")
	if err != nil {
		return fmt.Errorf("encode draft: %w", err)
	}
	payload = append(payload, '\n')
	if int64(len(payload)) > maximumDraftStateBytes {
		return validationError("draft state exceeds 20 MiB")
	}
	temporary, err := attachmentTemporaryPath(path)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, removeIfPresent(temporary))
	}()
	if err := writePrivateFile(temporary, payload); err != nil {
		return fmt.Errorf("write draft: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("publish draft: %w", err)
	}
	return syncDirectory(root)
}

func writePrivateFile(path string, payload []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create private file: %w", err)
	}
	if _, err := file.Write(payload); err != nil {
		return errors.Join(
			fmt.Errorf("write private file: %w", err), file.Close(), removeIfPresent(path),
		)
	}
	if err := file.Sync(); err != nil {
		return errors.Join(
			fmt.Errorf("sync private file: %w", err), file.Close(), removeIfPresent(path),
		)
	}
	if err := file.Close(); err != nil {
		return errors.Join(fmt.Errorf("close private file: %w", err), removeIfPresent(path))
	}
	return syncDirectory(filepath.Dir(path))
}

func readDraftFile(root string, ref string) (Draft, error) {
	return readDraftFileWithObserver(root, ref, nil)
}

func readDraftFileWithObserver(root string, ref string, observer draftContentObserver) (Draft, error) {
	draft, err := loadDraftDocument(root, ref)
	if err != nil {
		return Draft{}, wrapDraftStateError(root, ref, err)
	}
	if err := validateStoredDraftContentWithObserver(&draft, observer); err != nil {
		return Draft{}, wrapDraftStateError(root, ref, fmt.Errorf("validate draft content: %w", err))
	}
	if err := attachDraftAttempts(root, ref, &draft); err != nil {
		return Draft{}, wrapDraftStateError(root, ref, err)
	}
	return draft, nil
}

func wrapDraftStateError(root, ref string, err error) error {
	var operation *OperationError
	if errors.As(err, &operation) && operation.Code == "not_found" {
		return err
	}
	return &OperationError{
		Code: "draft_state_error",
		Message: fmt.Sprintf(
			"draft %s has invalid state in %s: %v; delete the corrupt state file or discard the draft",
			ref, filepath.Base(filepath.Join(root, ref+".json")), err,
		),
	}
}

// readDraftSummary loads the list view of a draft: same envelope
// discipline as readDraftFile but no canonical body validation and no
// Markdown/HTML re-render, so listing stays cheap and a draft with a
// corrupt body still appears (inspect keeps the full gate).
func readDraftSummary(root string, ref string, observer draftContentObserver) (DraftSummary, error) {
	draft, err := loadDraftDocument(root, ref)
	if err != nil {
		return DraftSummary{}, err
	}
	if err := attachDraftAttempts(root, ref, &draft); err != nil {
		return DraftSummary{}, err
	}
	return draftSummaryFrom(draft), nil
}

// loadDraftDocument decodes one draft file: bounded regular file, exactly
// one JSON object with strict fields, reference match. No content
// validation and no attempt sidecars; callers add what their path needs.
func loadDraftDocument(root string, ref string) (Draft, error) {
	path, err := draftPath(root, ref)
	if err != nil {
		return Draft{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Draft{}, &OperationError{Code: "not_found", Message: "draft not found"}
		}
		return Draft{}, fmt.Errorf("inspect draft: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maximumDraftStateBytes {
		return Draft{}, fmt.Errorf("draft is not a bounded regular file")
	}
	payload, err := readBoundedRegularFile(path, info, maximumDraftStateBytes)
	if err != nil {
		return Draft{}, fmt.Errorf("read draft: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var draft Draft
	if err := decoder.Decode(&draft); err != nil {
		return Draft{}, fmt.Errorf("decode draft: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Draft{}, fmt.Errorf("draft must contain exactly one JSON object")
	}
	if draft.Ref != ref {
		return Draft{}, fmt.Errorf("draft reference does not match its file")
	}
	if draft.BodyFormat == "" {
		draft.BodyFormat = DraftBodyPlain
	}
	return draft, nil
}

// attachDraftAttempts loads the send/save attempt sidecars onto a decoded
// draft, enforcing the same claim-conflict rule on every read path.
func attachDraftAttempts(root string, ref string, draft *Draft) error {
	draft.SendAttempt = nil
	attempt, err := readSendAttempt(root, ref)
	if err != nil {
		return err
	}
	draft.SendAttempt = attempt
	draft.SaveAttempt = nil
	saveAttempt, err := readDraftSaveAttempt(root, ref)
	if err != nil {
		return err
	}
	draft.SaveAttempt = saveAttempt
	draft.HandoffAttempt = nil
	handoffAttempt, err := readHandoffAttempt(root, ref)
	if err != nil {
		return err
	}
	draft.HandoffAttempt = handoffAttempt
	if draft.SendAttempt != nil && draft.SaveAttempt != nil {
		return fmt.Errorf("draft has conflicting send and save claims")
	}
	if draft.HandoffAttempt != nil && (draft.SendAttempt != nil || draft.SaveAttempt != nil) {
		return fmt.Errorf("draft has conflicting handoff and send/save claims")
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open state directory: %w", err)
	}
	if err := directory.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync state directory: %w", err), directory.Close())
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close state directory: %w", err)
	}
	return nil
}

func readBoundedRegularFile(path string, expected os.FileInfo, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) || opened.Size() > maximum {
		return nil, errors.Join(fmt.Errorf("file identity changed while opening"), file.Close())
	}
	payload, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if int64(len(payload)) > maximum {
		return nil, fmt.Errorf("file exceeds maximum size")
	}
	return payload, nil
}
