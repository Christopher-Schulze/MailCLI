package mail

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func rejectClaimedDraft(draft Draft) error {
	if draft.SendAttempt != nil {
		return &OperationError{
			Code: "send_retry_blocked",
			Message: fmt.Sprintf(
				"draft has send attempt %s with outcome %s; inspect it and discard explicitly instead of retrying",
				draft.SendAttempt.ID, draft.SendAttempt.Outcome,
			),
		}
	}
	if draft.SaveAttempt != nil {
		return &OperationError{
			Code: "draft_save_retry_blocked",
			Message: fmt.Sprintf(
				"draft has legacy native save attempt %s; recover it with `mailcli drafts save --ref %s --json` (reconcile-only), or discard explicitly",
				draft.SaveAttempt.ID, draft.Ref,
			),
		}
	}
	return nil
}

func cloneSendObservationBaseline(value *SendObservationBaseline) *SendObservationBaseline {
	if value == nil {
		return nil
	}
	clone := *value
	clone.SentMailboxIDs = append([]int64(nil), value.SentMailboxIDs...)
	return &clone
}

func beginSendAttempt(root string, ref string, messageID, envelopeFingerprint string) (SendAttempt, error) {
	return beginSendAttemptWithBaseline(root, ref, nil, messageID, envelopeFingerprint)
}

func beginSendAttemptWithBaseline(
	root string,
	ref string,
	baseline *SendObservationBaseline,
	messageID string,
	envelopeFingerprint string,
) (SendAttempt, error) {
	id, err := newSendAttemptID()
	if err != nil {
		return SendAttempt{}, err
	}
	now := time.Now().UTC()
	attempt := SendAttempt{
		ID: id, StartedAt: now, UpdatedAt: now, Outcome: SendOutcomeUnknown,
		MessageID: messageID, EnvelopeFingerprint: envelopeFingerprint,
		ObservationBaseline: cloneSendObservationBaseline(baseline),
	}
	path, err := sendClaimPath(root, ref)
	if err != nil {
		return SendAttempt{}, err
	}
	payload, err := encodeSendAttempt(ref, attempt)
	if err != nil {
		return SendAttempt{}, err
	}
	if err := writePrivateFile(path, payload); err != nil {
		if errors.Is(err, os.ErrExist) {
			return SendAttempt{}, &OperationError{
				Code:    "send_retry_blocked",
				Message: "draft already has a send attempt; inspect it and discard explicitly instead of retrying",
			}
		}
		return SendAttempt{}, fmt.Errorf("create send claim: %w", err)
	}
	return attempt, nil
}

func newSendAttemptID() (string, error) {
	var value [18]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate send attempt id: %w", err)
	}
	return "send_" + base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func newMirrorAttemptID() (string, error) {
	id, err := newSendAttemptID()
	if err != nil {
		return "", fmt.Errorf("generate mirror attempt id: %w", err)
	}
	return "mirror_" + strings.TrimPrefix(id, "send_"), nil
}

type storedSendAttempt struct {
	Version  int         `json:"version"`
	DraftRef string      `json:"draft_ref"`
	Attempt  SendAttempt `json:"attempt"`
}

func encodeSendAttempt(ref string, attempt SendAttempt) ([]byte, error) {
	payload, err := json.MarshalIndent(storedSendAttempt{
		Version: 1, DraftRef: ref, Attempt: attempt,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode send claim: %w", err)
	}
	return append(payload, '\n'), nil
}

func readSendAttempt(root string, ref string) (*SendAttempt, error) {
	path, err := sendClaimPath(root, ref)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect send claim: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maximumDraftStateBytes {
		return nil, fmt.Errorf("send claim is not a bounded regular file")
	}
	payload, err := readBoundedRegularFile(path, info, maximumDraftStateBytes)
	if err != nil {
		return nil, fmt.Errorf("read send claim: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var stored storedSendAttempt
	if err := decoder.Decode(&stored); err != nil {
		return nil, fmt.Errorf("decode send claim: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("send claim must contain exactly one JSON object")
	}
	if !validSendAttempt(stored, ref) {
		return nil, fmt.Errorf("send claim is invalid")
	}
	return &stored.Attempt, nil
}

func validSendAttempt(stored storedSendAttempt, ref string) bool {
	attempt := stored.Attempt
	if stored.Version != 1 || stored.DraftRef != ref || attempt.ID == "" ||
		attempt.StartedAt.IsZero() || attempt.UpdatedAt.IsZero() {
		return false
	}
	if !validObservationBaseline(attempt.ObservationBaseline) {
		return false
	}
	if !validSendMaterialization(attempt.Materialized) {
		return false
	}
	switch attempt.Outcome {
	case SendOutcomeUnknown:
		return !attempt.SentStoreObserved && !attempt.AcceptedByMail
	case SendOutcomeAccepted:
		return attempt.InvocationStarted && attempt.AcceptedByMail && !attempt.SentStoreObserved
	case SendOutcomeObserved:
		return attempt.InvocationStarted && attempt.SentStoreObserved
	case SendOutcomeSent:
		return attempt.InvocationStarted && attempt.AcceptedByMail && attempt.SentStoreObserved &&
			validTransportEvidence(attempt.Transport)
	case SendOutcomeMirrorPending:
		return attempt.InvocationStarted && attempt.AcceptedByMail && !attempt.SentStoreObserved &&
			validTransportEvidence(attempt.Transport)
	default:
		return false
	}
}

func validTransportEvidence(value *TransportEvidence) bool {
	return value != nil && strings.TrimSpace(value.MessageID) != ""
}

func validSendMaterialization(value *SendMaterialization) bool {
	if value == nil {
		return true
	}
	if value.AttachmentCount < 0 || strings.TrimSpace(value.From) == "" ||
		len(value.To)+len(value.CC)+len(value.BCC) == 0 {
		return false
	}
	if value.Body != nil && int64(len(*value.Body)) > maximumDraftStateBytes {
		return false
	}
	return validateDraftAddresses(DraftInput{
		From: value.From, To: value.To, CC: value.CC, BCC: value.BCC,
	}) == nil
}

func validObservationBaseline(value *SendObservationBaseline) bool {
	if value == nil {
		return true
	}
	if value.StoreUUID == "" || value.MaximumRowID < 0 || value.CapturedUnix < 1 || len(value.SentMailboxIDs) == 0 {
		return false
	}
	seen := make(map[int64]struct{}, len(value.SentMailboxIDs))
	for _, identifier := range value.SentMailboxIDs {
		if identifier < 1 {
			return false
		}
		if _, exists := seen[identifier]; exists {
			return false
		}
		seen[identifier] = struct{}{}
	}
	return true
}

func replaceSendAttempt(root string, ref string, attempt SendAttempt) (resultErr error) {
	path, err := sendClaimPath(root, ref)
	if err != nil {
		return err
	}
	payload, err := encodeSendAttempt(ref, attempt)
	if err != nil {
		return err
	}
	temporary, err := attachmentTemporaryPath(path)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, removeIfPresent(temporary))
	}()
	if err := writePrivateFile(temporary, payload); err != nil {
		return fmt.Errorf("write send claim update: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("publish send claim update: %w", err)
	}
	return syncDirectory(root)
}

func removeSendAttempt(root string, ref string) error {
	path, err := sendClaimPath(root, ref)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove send claim: %w", err)
	}
	return syncDirectory(root)
}

type storedDraftSaveAttempt struct {
	Version  int              `json:"version"`
	DraftRef string           `json:"draft_ref"`
	Attempt  DraftSaveAttempt `json:"attempt"`
}

func encodeDraftSaveAttempt(ref string, attempt DraftSaveAttempt) ([]byte, error) {
	payload, err := json.MarshalIndent(storedDraftSaveAttempt{
		Version: 1, DraftRef: ref, Attempt: attempt,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode draft-save claim: %w", err)
	}
	payload = append(payload, '\n')
	if int64(len(payload)) > maximumDraftStateBytes {
		return nil, validationError("draft-save claim exceeds 20 MiB")
	}
	return payload, nil
}

func readDraftSaveAttempt(root string, ref string) (*DraftSaveAttempt, error) {
	path, err := saveClaimPath(root, ref)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect draft-save claim: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maximumDraftStateBytes {
		return nil, fmt.Errorf("draft-save claim is not a bounded regular file")
	}
	payload, err := readBoundedRegularFile(path, info, maximumDraftStateBytes)
	if err != nil {
		return nil, fmt.Errorf("read draft-save claim: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var stored storedDraftSaveAttempt
	if err := decoder.Decode(&stored); err != nil {
		return nil, fmt.Errorf("decode draft-save claim: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("draft-save claim must contain exactly one JSON object")
	}
	if !validDraftSaveAttempt(stored, ref) {
		return nil, fmt.Errorf("draft-save claim is invalid")
	}
	return &stored.Attempt, nil
}

func validDraftSaveAttempt(stored storedDraftSaveAttempt, ref string) bool {
	attempt := stored.Attempt
	if stored.Version != 1 || stored.DraftRef != ref || attempt.ID == "" ||
		attempt.StartedAt.IsZero() || attempt.UpdatedAt.IsZero() ||
		attempt.ObservationBaseline == nil || !validObservationBaseline(attempt.ObservationBaseline) ||
		!validSendMaterialization(attempt.Materialized) {
		return false
	}
	if attempt.AcceptedByMail && !attempt.InvocationStarted {
		return false
	}
	return attempt.ObservedMessageRef == "" || attempt.InvocationStarted && attempt.AcceptedByMail
}

func validateDraftSaveAttempt(ref string, attempt DraftSaveAttempt) error {
	if attempt.ObservationBaseline == nil {
		return validationError("draft-save claim requires an observation baseline")
	}
	if !validDraftSaveAttempt(storedDraftSaveAttempt{
		Version: 1, DraftRef: ref, Attempt: attempt,
	}, ref) {
		return validationError("draft-save claim is invalid")
	}
	return nil
}

func replaceDraftSaveAttempt(root string, ref string, attempt DraftSaveAttempt) (resultErr error) {
	path, err := saveClaimPath(root, ref)
	if err != nil {
		return err
	}
	if err := validateDraftSaveAttempt(ref, attempt); err != nil {
		return err
	}
	payload, err := encodeDraftSaveAttempt(ref, attempt)
	if err != nil {
		return err
	}
	temporary, err := attachmentTemporaryPath(path)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, removeIfPresent(temporary)) }()
	if err := writePrivateFile(temporary, payload); err != nil {
		return fmt.Errorf("write draft-save claim update: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("publish draft-save claim update: %w", err)
	}
	return syncDirectory(root)
}

func removeDraftSaveAttempt(root string, ref string) error {
	path, err := saveClaimPath(root, ref)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove draft-save claim: %w", err)
	}
	return syncDirectory(root)
}
