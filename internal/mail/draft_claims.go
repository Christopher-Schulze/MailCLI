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
	if draft.HandoffAttempt != nil {
		return handoffRetryBlockedError(draft.HandoffAttempt.ID)
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

func beginSendAttempt(root string, ref string, messageID, envelopeFingerprint string, storage ...*draftStorage) (SendAttempt, error) {
	return beginSendAttemptWithMIMEFingerprint(root, ref, nil, messageID, envelopeFingerprint, "", storage...)
}

func beginSendAttemptWithBaseline(
	root string,
	ref string,
	baseline *SendObservationBaseline,
	messageID string,
	envelopeFingerprint string,
	storage ...*draftStorage,
) (SendAttempt, error) {
	return beginSendAttemptWithMIMEFingerprint(root, ref, baseline, messageID, envelopeFingerprint, "", storage...)
}

func beginSendAttemptWithMIMEFingerprint(
	root string,
	ref string,
	baseline *SendObservationBaseline,
	messageID string,
	envelopeFingerprint string,
	mimeFingerprint string,
	storage ...*draftStorage,
) (SendAttempt, error) {
	return beginSendAttemptWithMIMEFingerprintAndRecoverySpool(
		root, ref, baseline, messageID, envelopeFingerprint, mimeFingerprint, nil, storage...,
	)
}

func beginSendAttemptWithMIMEFingerprintAndRecoverySpool(
	root string,
	ref string,
	baseline *SendObservationBaseline,
	messageID string,
	envelopeFingerprint string,
	mimeFingerprint string,
	recoverySpool *AcceptedMessageSpool,
	storage ...*draftStorage,
) (SendAttempt, error) {
	id, err := newSendAttemptID()
	if err != nil {
		return SendAttempt{}, err
	}
	now := time.Now().UTC()
	attempt := SendAttempt{
		ID: id, StartedAt: now, UpdatedAt: now, Outcome: SendOutcomeUnknown,
		MessageID: messageID, EnvelopeFingerprint: envelopeFingerprint,
		MIMEFingerprint:     mimeFingerprint,
		RecoverySpool:       cloneAcceptedMessageSpool(recoverySpool),
		ObservationBaseline: cloneSendObservationBaseline(baseline),
	}
	state := draftStorageFor(root, storage...)
	name := ref + ".send-claim"
	payload, err := encodeSendAttempt(ref, attempt)
	if err != nil {
		return SendAttempt{}, err
	}
	if _, err := writePrivateDraftFile(state, name, payload); err != nil {
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

func cloneAcceptedMessageSpool(value *AcceptedMessageSpool) *AcceptedMessageSpool {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
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

func readSendAttempt(root string, ref string, storage ...*draftStorage) (*SendAttempt, error) {
	state := draftStorageFor(root, storage...)
	name := ref + ".send-claim"
	info, err := state.lstat(name)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect send claim: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maximumDraftStateBytes {
		return nil, errors.New("send claim is not a bounded regular file")
	}
	payload, err := readBoundedRegularFile(name, info, maximumDraftStateBytes, state)
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
		return nil, errors.New("send claim must contain exactly one JSON object")
	}
	if !validSendAttempt(stored, ref) {
		return nil, errors.New("send claim is invalid")
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
	if !validAcceptedMessageSpool(attempt.RecoverySpool) {
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

func replaceSendAttempt(root string, ref string, attempt SendAttempt, storage ...*draftStorage) error {
	state := draftStorageFor(root, storage...)
	name := ref + ".send-claim"
	payload, err := encodeSendAttempt(ref, attempt)
	if err != nil {
		return err
	}
	return replacePrivateDraftFile(state, name, payload, "write send claim update", "publish send claim update")
}

func removeSendAttempt(root string, ref string, state *draftStorage) error {
	attempt, err := readSendAttempt(root, ref, state)
	if err != nil {
		return err
	}
	if attempt != nil {
		if err := removeAcceptedMessageSpool(ref, attempt, state); err != nil {
			return err
		}
	}
	name := ref + ".send-claim"
	return removeDraftStorageFile(state, name, nil, "send claim")
}

const maximumSendReceiptBytes = 64 * 1024

type storedSendReceipt struct {
	Version  int         `json:"version"`
	DraftRef string      `json:"draft_ref"`
	Receipt  SendReceipt `json:"receipt"`
}

func receiptFromAttempt(ref string, attempt SendAttempt) SendReceipt {
	receipt := SendReceipt{
		DraftRef:           ref,
		AttemptID:          attempt.ID,
		StartedAt:          attempt.StartedAt,
		CompletedAt:        attempt.UpdatedAt,
		Outcome:            attempt.Outcome,
		Accepted:           attempt.AcceptedByMail || attempt.SentStoreObserved,
		SubmissionAccepted: submissionAcceptedForAttempt(attempt),
		SentCopyObserved:   attempt.SentStoreObserved,
		ObservedMessageRef: attempt.ObservedMessageRef,
	}
	receipt.ExpiresAt = receipt.CompletedAt.Add(SendReceiptRetention)
	if attempt.Transport != nil {
		receipt.MessageID = attempt.Transport.MessageID
		receipt.ServerResponse = attempt.Transport.ServerResponse
		receipt.SentMailbox = attempt.Transport.MirrorMailbox
		receipt.UIDValidity = attempt.Transport.MirrorUIDValidity
		receipt.UID = attempt.Transport.MirrorUID
		receipt.SentAppended = attempt.Transport.MirrorAppended
	}
	if receipt.MessageID == "" {
		receipt.MessageID = attempt.MessageID
	}
	return receipt
}

func normalizeSendReceipt(receipt SendReceipt) SendReceipt {
	if !receipt.SubmissionAccepted && strings.TrimSpace(receipt.ServerResponse) != "" {
		receipt.SubmissionAccepted = true
	}
	if !receipt.SentCopyObserved && (receipt.Outcome == SendOutcomeObserved || receipt.Outcome == SendOutcomeSent) {
		receipt.SentCopyObserved = true
	}
	return receipt
}

func resultForReceipt(receipt SendReceipt) SendResult {
	receipt = normalizeSendReceipt(receipt)
	return SendResult{
		DraftRef: receipt.DraftRef, AttemptID: receipt.AttemptID, Outcome: receipt.Outcome,
		Accepted: receipt.Accepted, SubmissionAccepted: receipt.SubmissionAccepted,
		InvocationStarted: true, AcceptedByMail: receipt.Accepted,
		SentStoreObserved: receipt.SentCopyObserved, SentCopyObserved: receipt.SentCopyObserved,
		DraftRetained: false, Replayed: true,
		Receipt: &receipt,
	}
}

func encodeSendReceipt(receipt SendReceipt) ([]byte, error) {
	payload, err := json.MarshalIndent(storedSendReceipt{
		Version: 1, DraftRef: receipt.DraftRef, Receipt: receipt,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode send receipt: %w", err)
	}
	payload = append(payload, '\n')
	if len(payload) > maximumSendReceiptBytes {
		return nil, validationError("send receipt exceeds 64 KiB")
	}
	return payload, nil
}

func readSendReceipt(root string, ref string, storage ...*draftStorage) (*SendReceipt, error) {
	if _, err := draftPath(root, ref); err != nil {
		return nil, err
	}
	state := draftStorageFor(root, storage...)
	name := ref + ".send-receipt"
	info, err := state.lstat(name)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect send receipt: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maximumSendReceiptBytes {
		return nil, &OperationError{Code: "send_receipt_invalid", Message: "send receipt is not a bounded regular file"}
	}
	payload, err := readBoundedRegularFile(name, info, maximumSendReceiptBytes, state)
	if err != nil {
		return nil, fmt.Errorf("read send receipt: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var stored storedSendReceipt
	if err := decoder.Decode(&stored); err != nil {
		return nil, &OperationError{Code: "send_receipt_invalid", Message: fmt.Sprintf("decode send receipt: %v", err)}
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, &OperationError{Code: "send_receipt_invalid", Message: "send receipt must contain exactly one JSON object"}
	}
	stored.Receipt = normalizeSendReceipt(stored.Receipt)
	if !validSendReceipt(stored, ref) {
		return nil, &OperationError{Code: "send_receipt_invalid", Message: "send receipt is invalid"}
	}
	receipt := stored.Receipt
	return &receipt, nil
}

func readActiveSendReceipt(root string, ref string, storage ...*draftStorage) (*SendReceipt, error) {
	receipt, err := readSendReceipt(root, ref, storage...)
	if err != nil || receipt == nil {
		return receipt, err
	}
	if !time.Now().UTC().Before(receipt.ExpiresAt) {
		return nil, &OperationError{Code: "send_receipt_expired", Message: "the terminal send receipt has expired"}
	}
	return receipt, nil
}

func validSendReceipt(stored storedSendReceipt, ref string) bool {
	receipt := stored.Receipt
	if stored.Version != 1 || stored.DraftRef != ref || receipt.DraftRef != ref ||
		receipt.AttemptID == "" || receipt.StartedAt.IsZero() || receipt.CompletedAt.IsZero() ||
		receipt.ExpiresAt.IsZero() || receipt.CompletedAt.Before(receipt.StartedAt) ||
		receipt.ExpiresAt.Before(receipt.CompletedAt) || !receipt.Accepted {
		return false
	}
	if receipt.ExpiresAt.Sub(receipt.CompletedAt) != SendReceiptRetention {
		return false
	}
	switch receipt.Outcome {
	case SendOutcomeObserved, SendOutcomeSent:
		return true
	default:
		return false
	}
}

func sendReceiptsEqual(left SendReceipt, right SendReceipt) bool {
	return left.DraftRef == right.DraftRef && left.AttemptID == right.AttemptID &&
		left.StartedAt.Equal(right.StartedAt) && left.CompletedAt.Equal(right.CompletedAt) &&
		left.ExpiresAt.Equal(right.ExpiresAt) && left.Outcome == right.Outcome &&
		left.Accepted == right.Accepted && left.SubmissionAccepted == right.SubmissionAccepted &&
		left.SentCopyObserved == right.SentCopyObserved && left.ObservedMessageRef == right.ObservedMessageRef &&
		left.MessageID == right.MessageID &&
		left.ServerResponse == right.ServerResponse && left.SentMailbox == right.SentMailbox &&
		left.UIDValidity == right.UIDValidity && left.UID == right.UID &&
		left.SentAppended == right.SentAppended
}

func persistSendReceipt(root string, ref string, receipt SendReceipt, storage ...*draftStorage) error {
	state := draftStorageFor(root, storage...)
	receipt = normalizeSendReceipt(receipt)
	if !validSendReceipt(storedSendReceipt{Version: 1, DraftRef: ref, Receipt: receipt}, ref) {
		return &OperationError{Code: "send_receipt_invalid", Message: "terminal send receipt is invalid"}
	}
	existing, err := readSendReceipt(root, ref, state)
	if err != nil {
		return err
	}
	if existing != nil {
		if !sendReceiptsEqual(*existing, receipt) {
			return &OperationError{Code: "send_receipt_conflict", Message: "an immutable send receipt already exists with different evidence"}
		}
		return nil
	}
	payload, err := encodeSendReceipt(receipt)
	if err != nil {
		return err
	}
	name := ref + ".send-receipt"
	if _, err := writePrivateDraftFile(state, name, payload); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("persist send receipt: %w", err)
		}
		existing, readErr := readSendReceipt(root, ref, state)
		if readErr != nil {
			return readErr
		}
		if existing == nil || !sendReceiptsEqual(*existing, receipt) {
			return &OperationError{Code: "send_receipt_conflict", Message: "an immutable send receipt already exists with different evidence"}
		}
	}
	return nil
}

func ensureSendReceipt(root string, ref string, attempt SendAttempt, storage ...*draftStorage) (*SendReceipt, error) {
	state := draftStorageFor(root, storage...)
	derived := receiptFromAttempt(ref, attempt)
	if !time.Now().UTC().Before(derived.ExpiresAt) {
		return nil, &OperationError{Code: "send_receipt_expired", Message: "the terminal send evidence is older than the receipt retention window"}
	}
	receipt, err := readSendReceipt(root, ref, state)
	if err != nil {
		return nil, err
	}
	if receipt != nil {
		if !time.Now().UTC().Before(receipt.ExpiresAt) {
			return nil, &OperationError{Code: "send_receipt_expired", Message: "the terminal send receipt has expired; the retained claim remains available for explicit recovery"}
		}
		if receipt.DraftRef != derived.DraftRef || receipt.AttemptID != derived.AttemptID ||
			receipt.Outcome != derived.Outcome || receipt.Accepted != derived.Accepted ||
			receipt.SubmissionAccepted != derived.SubmissionAccepted ||
			receipt.SentCopyObserved != derived.SentCopyObserved {
			return nil, &OperationError{Code: "send_receipt_conflict", Message: "the terminal send receipt does not match the retained send attempt"}
		}
		return receipt, nil
	}
	if err := persistSendReceipt(root, ref, derived, state); err != nil {
		return nil, err
	}
	return &derived, nil
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

func readDraftSaveAttempt(root string, ref string, storage ...*draftStorage) (*DraftSaveAttempt, error) {
	state := draftStorageFor(root, storage...)
	name := ref + ".save-claim"
	info, err := state.lstat(name)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect draft-save claim: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maximumDraftStateBytes {
		return nil, errors.New("draft-save claim is not a bounded regular file")
	}
	payload, err := readBoundedRegularFile(name, info, maximumDraftStateBytes, state)
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
		return nil, errors.New("draft-save claim must contain exactly one JSON object")
	}
	if !validDraftSaveAttempt(stored, ref) {
		return nil, errors.New("draft-save claim is invalid")
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

func replaceDraftSaveAttempt(root string, ref string, attempt DraftSaveAttempt, storage ...*draftStorage) error {
	state := draftStorageFor(root, storage...)
	name := ref + ".save-claim"
	if err := validateDraftSaveAttempt(ref, attempt); err != nil {
		return err
	}
	payload, err := encodeDraftSaveAttempt(ref, attempt)
	if err != nil {
		return err
	}
	return replacePrivateDraftFile(state, name, payload, "write draft-save claim update", "publish draft-save claim update")
}
