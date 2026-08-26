package mail

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdmail "net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"mailcli/internal/mailref"
)

const (
	draftLockPoll          = 25 * time.Millisecond
	draftMutationLockWait  = 2 * time.Second
	maximumDraftStateBytes = int64(20 * 1024 * 1024)
)

func (s *Service) CreateDraft(request CreateDraftRequest) (Draft, error) {
	draft, err := prepareDraft(request)
	if err != nil {
		return Draft{}, err
	}
	root, err := s.resolveDraftRoot()
	if err != nil {
		return Draft{}, err
	}
	if err := writeDraftFile(root, draft, true); err != nil {
		return Draft{}, err
	}
	return draft, nil
}

func (s *Service) GetDraft(ref string) (Draft, error) {
	root, err := s.resolveDraftRoot()
	if err != nil {
		return Draft{}, err
	}
	return readDraftFile(root, ref)
}

func (s *Service) ListDrafts() ([]Draft, error) {
	root, err := s.resolveDraftRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("list drafts: %w", err)
	}
	drafts := make([]Draft, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "draft_") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		draft, err := readDraftFile(root, strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		drafts = append(drafts, draft)
	}
	sort.Slice(drafts, func(left int, right int) bool {
		return drafts[left].UpdatedAt.After(drafts[right].UpdatedAt)
	})
	return drafts, nil
}

func (s *Service) UpdateDraft(request UpdateDraftRequest) (result Draft, resultErr error) {
	root, err := s.resolveDraftRoot()
	if err != nil {
		return Draft{}, err
	}
	lockContext, cancel := context.WithTimeout(context.Background(), draftMutationLockWait)
	defer cancel()
	lease, err := acquireDraftLease(lockContext, root, request.Ref)
	if err != nil {
		return Draft{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, lease.release()) }()
	current, err := readDraftFile(root, request.Ref)
	if err != nil {
		return Draft{}, err
	}
	if err := rejectClaimedDraft(current); err != nil {
		return Draft{}, err
	}
	replacement, err := prepareDraft(CreateDraftRequest{
		Kind: current.Kind, SourceRef: current.SourceRef,
		ReplyAll: current.ReplyAll, Input: request.Input,
	})
	if err != nil {
		return Draft{}, err
	}
	replacement.Ref = current.Ref
	replacement.CreatedAt = current.CreatedAt
	if err := writeDraftFile(root, replacement, false); err != nil {
		return Draft{}, err
	}
	return replacement, nil
}

func (s *Service) DiscardDraft(ref string) (resultErr error) {
	root, err := s.resolveDraftRoot()
	if err != nil {
		return err
	}
	lockContext, cancel := context.WithTimeout(context.Background(), draftMutationLockWait)
	defer cancel()
	lease, err := acquireDraftLease(lockContext, root, ref)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lease.release()) }()
	return discardDraftFiles(root, ref)
}

func (s *Service) SendDraft(ctx context.Context, ref string) (result SendResult, resultErr error) {
	root, err := s.resolveDraftRoot()
	if err != nil {
		return SendResult{}, err
	}
	lease, err := acquireDraftLease(ctx, root, ref)
	if err != nil {
		return SendResult{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, lease.release()) }()
	draft, err := readDraftFile(root, ref)
	if err != nil {
		return SendResult{}, err
	}
	if draft.SendAttempt != nil {
		return replaySendAttempt(root, ref, *draft.SendAttempt)
	}
	if draft.Kind == DraftKindNew && strings.TrimSpace(draft.From) == "" {
		return SendResult{}, validationError("sending a new draft requires an explicit configured from address")
	}
	if draft.Kind == DraftKindForward && len(draft.To)+len(draft.CC)+len(draft.BCC) == 0 {
		return SendResult{}, validationError("sending a forward draft requires at least one explicit recipient")
	}
	if err := s.validateDraftSender(ctx, draft.From); err != nil {
		return SendResult{}, err
	}
	if err := verifyDraftAttachments(draft.Attachments); err != nil {
		return SendResult{}, err
	}
	var preparedBaseline *SendObservationBaseline
	if preparer, ok := s.gateway.(SendPreparer); ok {
		baseline, prepareErr := preparer.PrepareSend(ctx, draft)
		if prepareErr != nil {
			return SendResult{}, prepareErr
		}
		preparedBaseline = cloneSendObservationBaseline(&baseline)
		if !validObservationBaseline(preparedBaseline) {
			return SendResult{}, &OperationError{
				Code: "send_prepare_failed", Message: "Mail backend returned an invalid send observation baseline",
			}
		}
		draft.PreparedSendBaseline = cloneSendObservationBaseline(preparedBaseline)
	}
	attempt, err := beginSendAttemptWithBaseline(root, ref, preparedBaseline)
	if err != nil {
		return SendResult{}, err
	}
	evidence, sendErr := s.gateway.SendDraft(ctx, draft)
	if !evidence.InvocationStarted {
		if cleanupErr := removeSendAttempt(root, ref); cleanupErr != nil {
			result := resultForAttempt(ref, attempt, true)
			return result, &OperationError{
				Code:    "send_state_cleanup_failed",
				Message: fmt.Sprintf("send did not start, but its local claim could not be cleared: %v", cleanupErr),
			}
		}
		if sendErr == nil {
			sendErr = &OperationError{
				Code:    "send_not_started",
				Message: "Mail.app send did not start",
			}
		}
		return SendResult{}, sendErr
	}
	attempt.InvocationStarted = true
	attempt.AcceptedByMail = evidence.AcceptedByMail
	attempt.SentStoreObserved = evidence.SentStoreObserved
	attempt.ObservedMessageRef = evidence.ObservedMessageRef
	attempt.Materialized = cloneSendMaterialization(evidence.Materialized)
	if attempt.ObservationBaseline == nil {
		attempt.ObservationBaseline = cloneSendObservationBaseline(evidence.ObservationBaseline)
	}
	attempt.UpdatedAt = time.Now().UTC()
	if evidence.SentStoreObserved {
		attempt.Outcome = SendOutcomeObserved
	} else if evidence.AcceptedByMail {
		attempt.Outcome = SendOutcomeAccepted
	} else {
		attempt.Outcome = SendOutcomeUnknown
	}
	result = resultForAttempt(ref, attempt, true)
	if err := replaceSendAttempt(root, ref, attempt); err != nil {
		return result, &OperationError{
			Code:    "send_outcome_unknown",
			Message: fmt.Sprintf("send started, but its outcome state could not be recorded safely: %v", err),
		}
	}
	if evidence.SentStoreObserved {
		if err := discardDraftFiles(root, ref); err != nil {
			return result, &OperationError{
				Code:    "send_cleanup_failed",
				Message: fmt.Sprintf("sent message was observed, but local draft cleanup failed: %v", err),
			}
		}
		result.DraftRetained = false
		if sendErr != nil {
			return result, &OperationError{
				Code:    "send_postflight_failed",
				Message: fmt.Sprintf("sent message was observed, but private postflight cleanup failed: %v", sendErr),
			}
		}
		return result, nil
	}
	if !evidence.AcceptedByMail {
		message := "Mail.app send outcome is unknown; the draft is retained and retries are blocked"
		if sendErr != nil {
			message += ": " + sendErr.Error()
		}
		return result, &OperationError{Code: "send_outcome_unknown", Message: message}
	}
	return result, &OperationError{
		Code:    "send_not_observed",
		Message: "Mail.app accepted the send, but Sent does not yet prove the exact body, recipients, subject, and attachments; the draft is retained and retries are blocked",
	}
}

type SendPreparer interface {
	PrepareSend(ctx context.Context, draft Draft) (SendObservationBaseline, error)
}

type SendReconciler interface {
	ReconcileSend(ctx context.Context, draft Draft, attempt SendAttempt) (SendEvidence, error)
}

func (s *Service) ReconcileDraft(ctx context.Context, ref string) (result SendResult, resultErr error) {
	root, err := s.resolveDraftRoot()
	if err != nil {
		return SendResult{}, err
	}
	lease, err := acquireDraftLease(ctx, root, ref)
	if err != nil {
		return SendResult{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, lease.release()) }()
	draft, err := readDraftFile(root, ref)
	if err != nil {
		return SendResult{}, err
	}
	if draft.SendAttempt == nil {
		return SendResult{}, &OperationError{Code: "send_reconcile_unavailable", Message: "draft has no send attempt to reconcile"}
	}
	attempt := *draft.SendAttempt
	if attempt.Outcome == SendOutcomeObserved {
		result, err := replaySendAttempt(root, ref, attempt)
		result.Reconciled = true
		return result, err
	}
	if attempt.ObservationBaseline == nil {
		return resultForReconcile(ref, attempt), &OperationError{
			Code:    "send_reconcile_unavailable",
			Message: "send attempt predates durable store observation; it remains blocked and must not be retried",
		}
	}
	reconciler, ok := s.gateway.(SendReconciler)
	if !ok {
		return resultForReconcile(ref, attempt), &OperationError{
			Code: "send_reconcile_unavailable", Message: "the selected Mail backend cannot reconcile send outcomes safely",
		}
	}
	evidence, err := reconciler.ReconcileSend(ctx, draft, attempt)
	if err != nil {
		return resultForReconcile(ref, attempt), err
	}
	if evidence.SentStoreObserved && evidence.ObservedMessageRef != "" {
		return persistReconciledSend(root, ref, attempt, evidence)
	}
	result = resultForReconcile(ref, attempt)
	if attempt.Outcome == SendOutcomeAccepted {
		return result, &OperationError{
			Code:    "send_not_observed",
			Message: "Mail.app accepted the send, but Sent still does not prove the exact message; the draft is retained and retries remain blocked",
		}
	}
	return result, &OperationError{
		Code:    "send_outcome_unknown",
		Message: "the sent store still does not prove this send; the draft is retained and retries remain blocked",
	}
}

func persistReconciledSend(
	root string,
	ref string,
	attempt SendAttempt,
	evidence SendEvidence,
) (SendResult, error) {
	attempt.InvocationStarted = true
	attempt.AcceptedByMail = true
	attempt.SentStoreObserved = true
	attempt.ObservedMessageRef = evidence.ObservedMessageRef
	attempt.Outcome = SendOutcomeObserved
	attempt.UpdatedAt = time.Now().UTC()
	result := resultForReconcile(ref, attempt)
	if err := replaceSendAttempt(root, ref, attempt); err != nil {
		return result, &OperationError{
			Code:    "send_reconcile_state_failed",
			Message: fmt.Sprintf("sent message was observed, but the reconciled state could not be recorded: %v", err),
		}
	}
	if err := discardDraftFiles(root, ref); err != nil {
		return result, &OperationError{
			Code:    "send_cleanup_failed",
			Message: fmt.Sprintf("sent message was observed, but local draft cleanup failed: %v", err),
		}
	}
	result.DraftRetained = false
	return result, nil
}

func resultForReconcile(ref string, attempt SendAttempt) SendResult {
	result := resultForAttempt(ref, attempt, true)
	result.Reconciled = true
	return result
}

func cloneSendObservationBaseline(value *SendObservationBaseline) *SendObservationBaseline {
	if value == nil {
		return nil
	}
	clone := *value
	clone.SentMailboxIDs = append([]int64(nil), value.SentMailboxIDs...)
	return &clone
}

func cloneSendMaterialization(value *SendMaterialization) *SendMaterialization {
	if value == nil {
		return nil
	}
	clone := *value
	clone.To = append([]Recipient(nil), value.To...)
	clone.CC = append([]Recipient(nil), value.CC...)
	clone.BCC = append([]Recipient(nil), value.BCC...)
	return &clone
}

func (s *Service) SaveDraft(ctx context.Context, ref string) (result SavedDraft, resultErr error) {
	root, err := s.resolveDraftRoot()
	if err != nil {
		return SavedDraft{}, err
	}
	lease, err := acquireDraftLease(ctx, root, ref)
	if err != nil {
		return SavedDraft{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, lease.release()) }()
	draft, err := readDraftFile(root, ref)
	if err != nil {
		return SavedDraft{}, err
	}
	if err := rejectClaimedDraft(draft); err != nil {
		return SavedDraft{}, err
	}
	if draft.From == "" {
		return SavedDraft{}, validationError("saving a Mail.app draft requires an explicit configured from address")
	}
	if err := s.validateDraftSender(ctx, draft.From); err != nil {
		return SavedDraft{}, err
	}
	if err := verifyDraftAttachments(draft.Attachments); err != nil {
		return SavedDraft{}, err
	}
	message, saveErr := s.gateway.SaveDraft(ctx, draft)
	if message.Ref == "" {
		if saveErr != nil {
			return SavedDraft{}, saveErr
		}
		return SavedDraft{}, &OperationError{
			Code: "draft_outcome_unknown", Message: "Mail backend returned no observed native draft",
		}
	}
	result = SavedDraft{LocalDraftRef: ref, Message: message}
	if err := discardDraftFiles(root, ref); err != nil {
		return result, fmt.Errorf("native Mail.app draft saved but local draft cleanup failed: %w", err)
	}
	if saveErr != nil {
		return result, &OperationError{
			Code:    "draft_postflight_failed",
			Message: fmt.Sprintf("native Mail.app draft was observed, but private postflight cleanup failed: %v", saveErr),
		}
	}
	return result, nil
}

func (s *Service) validateDraftSender(ctx context.Context, sender string) error {
	if sender == "" {
		return nil
	}
	parsed, err := stdmail.ParseAddress(sender)
	if err != nil {
		return validationError("invalid from address")
	}
	accounts, err := s.gateway.ListAccounts(ctx)
	if err != nil {
		return fmt.Errorf("validate draft sender: %w", err)
	}
	for _, account := range accounts {
		for _, address := range account.EmailAddresses {
			if strings.EqualFold(parsed.Address, address) {
				return nil
			}
		}
	}
	return validationError("from address is not configured in an enabled Mail.app account")
}

func prepareDraft(request CreateDraftRequest) (Draft, error) {
	if request.Kind == "" {
		request.Kind = DraftKindNew
	}
	if request.Kind != DraftKindNew && request.Kind != DraftKindReply && request.Kind != DraftKindForward {
		return Draft{}, validationError("draft kind must be new, reply, or forward")
	}
	if request.Kind != DraftKindNew && request.SourceRef == "" {
		return Draft{}, validationError("reply and forward drafts require a source message ref")
	}
	if request.Kind != DraftKindNew {
		ref, err := mailref.DecodeMessage(request.SourceRef)
		if err != nil || !ref.IsStoreBound() {
			return Draft{}, validationError("reply and forward drafts require a store-bound source message ref")
		}
	}
	if request.Kind == DraftKindNew && len(request.Input.To)+len(request.Input.CC)+len(request.Input.BCC) == 0 {
		return Draft{}, validationError("new drafts require at least one recipient")
	}
	if request.Kind == DraftKindForward && len(request.Input.To)+len(request.Input.CC)+len(request.Input.BCC) == 0 {
		return Draft{}, validationError("forward drafts require at least one explicit recipient")
	}
	if err := validateDraftAddresses(request.Input); err != nil {
		return Draft{}, err
	}
	attachments, err := fingerprintAttachments(request.Input.Attachments)
	if err != nil {
		return Draft{}, err
	}
	ref, err := newDraftReference()
	if err != nil {
		return Draft{}, err
	}
	now := time.Now().UTC()
	return Draft{
		Ref: ref, Kind: request.Kind, SourceRef: request.SourceRef,
		ReplyAll: request.ReplyAll, From: request.Input.From,
		To: nonNilRecipients(request.Input.To), CC: nonNilRecipients(request.Input.CC),
		BCC: nonNilRecipients(request.Input.BCC), Subject: request.Input.Subject,
		Body: request.Input.Body, Attachments: attachments,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

func validateDraftAddresses(input DraftInput) error {
	if input.From != "" {
		if _, err := stdmail.ParseAddress(input.From); err != nil {
			return validationError("invalid from address")
		}
	}
	seen := make(map[string]struct{})
	for _, group := range [][]Recipient{input.To, input.CC, input.BCC} {
		for _, recipient := range group {
			address := recipient.Address
			if recipient.Name != "" {
				address = (&stdmail.Address{Name: recipient.Name, Address: recipient.Address}).String()
			}
			parsed, err := stdmail.ParseAddress(address)
			if err != nil {
				return validationError("invalid recipient address")
			}
			normalized := strings.ToLower(parsed.Address)
			if _, duplicate := seen[normalized]; duplicate {
				return validationError("duplicate recipient address")
			}
			seen[normalized] = struct{}{}
		}
	}
	return nil
}

func fingerprintAttachments(paths []string) ([]DraftAttachment, error) {
	attachments := make([]DraftAttachment, 0, len(paths))
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return nil, validationError("draft attachment paths must be absolute")
		}
		attachment, err := fingerprintAttachment(path)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
	}
	return attachments, nil
}

func fingerprintAttachment(path string) (DraftAttachment, error) {
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
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return DraftAttachment{}, errors.Join(fmt.Errorf("hash draft attachment: %w", err), file.Close())
	}
	if err := file.Close(); err != nil {
		return DraftAttachment{}, fmt.Errorf("close draft attachment: %w", err)
	}
	return DraftAttachment{Path: path, Size: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func verifyDraftAttachments(attachments []DraftAttachment) error {
	for _, expected := range attachments {
		actual, err := fingerprintAttachment(expected.Path)
		if err != nil {
			return err
		}
		if actual.Size != expected.Size || actual.SHA256 != expected.SHA256 {
			return validationError("draft attachment changed after review; update the draft before sending")
		}
	}
	return nil
}

func (s *Service) resolveDraftRoot() (string, error) {
	root := s.draftRoot
	if root == "" {
		configRoot, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("resolve Application Support directory: %w", err)
		}
		root = filepath.Join(configRoot, "MailCLI", "drafts")
	}
	if !filepath.IsAbs(root) {
		return "", validationError("draft root must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create draft directory: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", fmt.Errorf("restrict draft directory: %w", err)
	}
	return root, nil
}

func newDraftReference() (string, error) {
	var bytes [18]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate draft ref: %w", err)
	}
	return "draft_" + base64.RawURLEncoding.EncodeToString(bytes[:]), nil
}

func draftPath(root string, ref string) (string, error) {
	if !strings.HasPrefix(ref, "draft_") || len(ref) != 30 || strings.ContainsAny(ref, "/\\") {
		return "", validationError("invalid draft ref")
	}
	return filepath.Join(root, ref+".json"), nil
}

func draftLockPath(root string, ref string) (string, error) {
	if _, err := draftPath(root, ref); err != nil {
		return "", err
	}
	return filepath.Join(root, ref+".lock"), nil
}

func sendClaimPath(root string, ref string) (string, error) {
	if _, err := draftPath(root, ref); err != nil {
		return "", err
	}
	return filepath.Join(root, ref+".send-claim"), nil
}

type draftLease struct {
	file *os.File
}

func acquireDraftLease(ctx context.Context, root string, ref string) (*draftLease, error) {
	path, err := draftLockPath(root, ref)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open draft lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, errors.Join(fmt.Errorf("restrict draft lock: %w", err), file.Close())
	}
	ticker := time.NewTicker(draftLockPoll)
	defer ticker.Stop()
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &draftLease{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			closeErr := file.Close()
			return nil, errors.Join(fmt.Errorf("lock draft: %w", err), closeErr)
		}
		select {
		case <-ctx.Done():
			closeErr := file.Close()
			return nil, errors.Join(
				&OperationError{Code: "draft_busy", Message: "draft is busy with another operation"},
				closeErr,
			)
		case <-ticker.C:
		}
	}
}

func (l *draftLease) release() error {
	return errors.Join(
		syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN),
		l.file.Close(),
	)
}

func rejectClaimedDraft(draft Draft) error {
	if draft.SendAttempt == nil {
		return nil
	}
	return &OperationError{
		Code: "send_retry_blocked",
		Message: fmt.Sprintf(
			"draft has send attempt %s with outcome %s; inspect it and discard explicitly instead of retrying",
			draft.SendAttempt.ID, draft.SendAttempt.Outcome,
		),
	}
}

func beginSendAttempt(root string, ref string) (SendAttempt, error) {
	return beginSendAttemptWithBaseline(root, ref, nil)
}

func beginSendAttemptWithBaseline(
	root string,
	ref string,
	baseline *SendObservationBaseline,
) (SendAttempt, error) {
	id, err := newSendAttemptID()
	if err != nil {
		return SendAttempt{}, err
	}
	now := time.Now().UTC()
	attempt := SendAttempt{
		ID: id, StartedAt: now, UpdatedAt: now, Outcome: SendOutcomeUnknown,
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
	if !info.Mode().IsRegular() || info.Size() > 64*1024 {
		return nil, fmt.Errorf("send claim is not a bounded regular file")
	}
	payload, err := readBoundedRegularFile(path, info, 64*1024)
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
	default:
		return false
	}
}

func validSendMaterialization(value *SendMaterialization) bool {
	if value == nil {
		return true
	}
	if value.AttachmentCount < 0 || strings.TrimSpace(value.From) == "" ||
		len(value.To)+len(value.CC)+len(value.BCC) == 0 {
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

func resultForAttempt(ref string, attempt SendAttempt, draftRetained bool) SendResult {
	return SendResult{
		DraftRef: ref, AttemptID: attempt.ID, Outcome: attempt.Outcome,
		Accepted:          attempt.AcceptedByMail,
		InvocationStarted: attempt.InvocationStarted, AcceptedByMail: attempt.AcceptedByMail,
		SentStoreObserved: attempt.SentStoreObserved, ObservedMessageRef: attempt.ObservedMessageRef,
		DraftRetained: draftRetained,
	}
}

func replaySendAttempt(root string, ref string, attempt SendAttempt) (SendResult, error) {
	result := resultForAttempt(ref, attempt, true)
	result.Replayed = true
	switch attempt.Outcome {
	case SendOutcomeObserved:
		if err := discardDraftFiles(root, ref); err != nil {
			return result, &OperationError{
				Code: "send_cleanup_failed",
				Message: fmt.Sprintf(
					"sent message was already observed, but local draft cleanup failed: %v", err,
				),
			}
		}
		result.DraftRetained = false
		return result, nil
	case SendOutcomeAccepted:
		return result, &OperationError{
			Code:    "send_not_observed",
			Message: "Mail.app accepted the send, but Sent does not prove the exact message; the draft is retained and retries are blocked",
		}
	default:
		return result, &OperationError{
			Code:    "send_outcome_unknown",
			Message: "Mail.app send outcome is unknown; the draft is retained and retries are blocked",
		}
	}
}

func discardDraftFiles(root string, ref string) error {
	path, err := draftPath(root, ref)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return &OperationError{Code: "not_found", Message: "draft not found"}
		}
		return fmt.Errorf("discard draft: %w", err)
	}
	if err := syncDirectory(root); err != nil {
		return fmt.Errorf("persist draft removal: %w", err)
	}
	return removeSendAttempt(root, ref)
}

func writeDraftFile(root string, draft Draft, exclusive bool) (resultErr error) {
	path, err := draftPath(root, draft.Ref)
	if err != nil {
		return err
	}
	draft.SendAttempt = nil
	payload, err := json.MarshalIndent(draft, "", "  ")
	if err != nil {
		return fmt.Errorf("encode draft: %w", err)
	}
	payload = append(payload, '\n')
	if int64(len(payload)) > maximumDraftStateBytes {
		return validationError("draft state exceeds 20 MiB")
	}
	if exclusive {
		return writePrivateFile(path, payload)
	}
	temporary, err := attachmentTemporaryPath(path)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, removeIfPresent(temporary))
	}()
	if err := writePrivateFile(temporary, payload); err != nil {
		return fmt.Errorf("write draft update: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("publish draft update: %w", err)
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
	draft.SendAttempt = nil
	attempt, err := readSendAttempt(root, ref)
	if err != nil {
		return Draft{}, err
	}
	draft.SendAttempt = attempt
	return draft, nil
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

func nonNilRecipients(recipients []Recipient) []Recipient {
	if recipients == nil {
		return []Recipient{}
	}
	return recipients
}
