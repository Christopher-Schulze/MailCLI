package mail

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maximumDraftStateBytes = int64(20 * 1024 * 1024)

func writeDraftFile(root string, draft Draft, storage ...*draftStorage) error {
	state := draftStorageFor(root, storage...)
	if _, err := draftPath(root, draft.Ref); err != nil {
		return err
	}
	name := draft.Ref + ".json"
	var err error
	draft.SendAttempt = nil
	draft.SaveAttempt = nil
	draft.HandoffAttempt = nil
	payload, err := json.Marshal(draft)
	if err != nil {
		return fmt.Errorf("encode draft: %w", err)
	}
	payload = append(payload, '\n')
	if int64(len(payload)) > maximumDraftStateBytes {
		return validationError("draft state exceeds 20 MiB")
	}
	return replacePrivateDraftFile(state, name, payload, "write draft", "publish draft")
}

func writePrivateFile(path string, payload []byte) error {
	storage := &draftStorage{rootName: filepath.Dir(path)}
	_, err := writePrivateDraftFile(storage, filepath.Base(path), payload)
	return err
}

func readDraftFile(root string, ref string, storage ...*draftStorage) (Draft, error) {
	return readDraftFileWithObserver(root, ref, nil, storage...)
}

func readDraftFileWithObserver(root string, ref string, observer draftContentObserver, storage ...*draftStorage) (Draft, error) {
	return readDraftFileChecked(root, ref, observer, true, storage...)
}

// readDraftFileForInspection loads a draft for display without the canonical
// re-render; mutation gates still verify the canonical transformation.
func readDraftFileForInspection(root string, ref string, observer draftContentObserver, storage ...*draftStorage) (Draft, error) {
	return readDraftFileChecked(root, ref, observer, false, storage...)
}

func readDraftFileChecked(root string, ref string, observer draftContentObserver, canonical bool, storage ...*draftStorage) (Draft, error) {
	draft, err := loadDraftDocument(root, ref, storage...)
	if err != nil {
		return Draft{}, wrapDraftStateError(root, ref, err)
	}
	if canonical {
		err = validateStoredDraftContentWithObserver(&draft, observer)
	} else {
		err = validateStoredDraftContentStructuralWithObserver(&draft, observer)
	}
	if err != nil {
		return Draft{}, wrapDraftStateError(root, ref, fmt.Errorf("validate draft content: %w", err))
	}
	if err := refreshDraftRevision(&draft); err != nil {
		return Draft{}, wrapDraftStateError(root, ref, err)
	}
	if err := attachDraftAttempts(root, ref, &draft, storage...); err != nil {
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

// loadDraftDocument decodes one draft file: bounded regular file, exactly
// one JSON object with strict fields, reference match. No content
// validation and no attempt sidecars; callers add what their path needs.
func loadDraftDocument(root string, ref string, storage ...*draftStorage) (Draft, error) {
	state := draftStorageFor(root, storage...)
	if _, err := draftPath(root, ref); err != nil {
		return Draft{}, err
	}
	name := ref + ".json"
	info, err := state.lstat(name)
	if err != nil {
		if os.IsNotExist(err) {
			return Draft{}, &OperationError{Code: "not_found", Message: "draft not found"}
		}
		return Draft{}, fmt.Errorf("inspect draft: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maximumDraftStateBytes {
		return Draft{}, errors.New("draft is not a bounded regular file")
	}
	payload, err := readBoundedRegularFile(name, info, maximumDraftStateBytes, state)
	if err != nil {
		return Draft{}, fmt.Errorf("read draft: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var draft Draft
	if err := decoder.Decode(&draft); err != nil {
		return Draft{}, fmt.Errorf("decode draft: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Draft{}, errors.New("draft must contain exactly one JSON object")
	}
	if draft.Ref != ref {
		return Draft{}, errors.New("draft reference does not match its file")
	}
	if draft.BodyFormat == "" {
		draft.BodyFormat = DraftBodyPlain
	}
	return draft, nil
}

// attachDraftAttempts loads the send/save attempt sidecars onto a decoded
// draft, enforcing the same claim-conflict rule on every read path.
func attachDraftAttempts(root string, ref string, draft *Draft, storage ...*draftStorage) error {
	state := draftStorageFor(root, storage...)
	draft.SendAttempt = nil
	attempt, err := readSendAttempt(root, ref, state)
	if err != nil {
		return err
	}
	draft.SendAttempt = attempt
	draft.SaveAttempt = nil
	saveAttempt, err := readDraftSaveAttempt(root, ref, state)
	if err != nil {
		return err
	}
	draft.SaveAttempt = saveAttempt
	draft.HandoffAttempt = nil
	handoffAttempt, err := readHandoffAttempt(ref, state)
	if err != nil {
		return err
	}
	draft.HandoffAttempt = handoffAttempt
	if draft.SendAttempt != nil && draft.SaveAttempt != nil {
		return errors.New("draft has conflicting send and save claims")
	}
	if draft.HandoffAttempt != nil && (draft.SendAttempt != nil || draft.SaveAttempt != nil) {
		return errors.New("draft has conflicting handoff and send/save claims")
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

func readBoundedRegularFile(name string, expected os.FileInfo, maximum int64, storage ...*draftStorage) ([]byte, error) {
	state := draftStorageFor("", storage...)
	file, _, err := state.openFile(name, expected, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	payload, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if int64(len(payload)) > maximum {
		return nil, errors.New("file exceeds maximum size")
	}
	return payload, nil
}
