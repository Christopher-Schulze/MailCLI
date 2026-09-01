package mailapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mailcli/internal/mail"
)

//go:embed scripts/bridge.js
var bridgeScript string

const (
	osaCancellationGrace        = 15 * time.Second
	processGroupTerminationWait = time.Second
)

type scriptRunner interface {
	Run(ctx context.Context, script string, request string) ([]byte, bool, error)
}

type osaScriptRunner struct {
	started           func(int)
	cancellationGrace time.Duration
}

type Client struct {
	runner                 scriptRunner
	gate                   accessGate
	blockComposeAutomation bool
}

type OperationError struct {
	Code    string
	Message string
}

type bridgeRequest struct {
	Operation              string       `json:"operation"`
	MailPID                int          `json:"mail_pid,omitempty"`
	AccountID              string       `json:"account_id,omitempty"`
	MailboxPath            []string     `json:"mailbox_path,omitempty"`
	MessageID              string       `json:"message_id,omitempty"`
	ExpectedMessageID      string       `json:"expected_message_id,omitempty"`
	ExpectedSubject        string       `json:"expected_subject,omitempty"`
	Offset                 int          `json:"offset,omitempty"`
	ExpectedPreviousID     string       `json:"expected_previous_id,omitempty"`
	Limit                  int          `json:"limit,omitempty"`
	AttachmentID           string       `json:"attachment_id,omitempty"`
	OutputPath             string       `json:"output_path,omitempty"`
	Draft                  *bridgeDraft `json:"draft,omitempty"`
	Read                   *bool        `json:"read,omitempty"`
	Flagged                *bool        `json:"flagged,omitempty"`
	Junk                   *bool        `json:"junk,omitempty"`
	DestinationAccountID   string       `json:"destination_account_id,omitempty"`
	DestinationMailboxPath []string     `json:"destination_mailbox_path,omitempty"`
	Copy                   bool         `json:"copy,omitempty"`
	MaximumRawSourceBytes  int64        `json:"maximum_raw_source_bytes,omitempty"`
	EvidencePath           string       `json:"evidence_path,omitempty"`
}

type bridgeDraft struct {
	Kind                          mail.DraftKind         `json:"kind"`
	Source                        *messageReference      `json:"source,omitempty"`
	ReplyAll                      bool                   `json:"reply_all,omitempty"`
	From                          string                 `json:"from,omitempty"`
	To                            []mail.Recipient       `json:"to"`
	CC                            []mail.Recipient       `json:"cc"`
	BCC                           []mail.Recipient       `json:"bcc"`
	Subject                       string                 `json:"subject,omitempty"`
	Body                          string                 `json:"body"`
	Attachments                   []mail.DraftAttachment `json:"attachments"`
	ExpectedNativeAttachmentCount int                    `json:"expected_native_attachment_count,omitempty"`
}

func (c *Client) SaveAttachmentTo(
	ctx context.Context,
	messageRef string,
	attachmentID string,
	outputPath string,
) error {
	ref, err := decodeMessageReference(messageRef)
	if err != nil {
		return invalidReference("message ref", err)
	}
	_, err = c.invoke(ctx, bridgeRequest{
		Operation: "attachments.save", AccountID: ref.AccountID,
		MailboxPath: ref.MailboxPath, MessageID: ref.LibraryID,
		ExpectedMessageID: ref.ExpectedMessageID, ExpectedSubject: ref.ExpectedSubject,
		AttachmentID: attachmentID,
		OutputPath:   outputPath,
	})
	return err
}

func (c *Client) SendDraft(ctx context.Context, draft mail.Draft) (mail.SendEvidence, error) {
	if err := c.validateComposeDraft(draft); err != nil {
		return mail.SendEvidence{}, err
	}
	bridge, err := encodeBridgeDraft(draft)
	if err != nil {
		return mail.SendEvidence{}, err
	}
	bridge, cleanup, err := snapshotBridgeAttachments(bridge)
	if err != nil {
		return mail.SendEvidence{}, err
	}
	response, started, invokeErr := c.invokeWithState(ctx, bridgeRequest{Operation: "drafts.send", Draft: &bridge})
	cleanupErr := attachmentSnapshotCleanupError(cleanup())
	if invokeErr != nil {
		attempted := started
		if response.Error != nil {
			attempted = response.SendAttempted
		}
		return mail.SendEvidence{InvocationStarted: attempted}, errors.Join(invokeErr, cleanupErr)
	}
	evidence := mail.SendEvidence{
		InvocationStarted: true,
		AcceptedByMail:    response.Accepted,
		Materialized:      mapSendMaterialization(response.Materialized),
	}
	return evidence, cleanupErr
}

func (c *Client) SaveDraft(ctx context.Context, draft mail.Draft) (mail.MessageSummary, error) {
	summary, _, _, _, err := c.SaveDraftWithInvocationState(ctx, draft)
	return summary, err
}

func (c *Client) SaveDraftWithMaterialization(
	ctx context.Context,
	draft mail.Draft,
) (mail.MessageSummary, *mail.SendMaterialization, error) {
	summary, materialized, _, _, err := c.SaveDraftWithInvocationState(ctx, draft)
	return summary, materialized, err
}

func (c *Client) SaveDraftWithInvocationState(
	ctx context.Context,
	draft mail.Draft,
) (result mail.MessageSummary, materialized *mail.SendMaterialization, invocationStarted bool, accepted bool, resultErr error) {
	if err := c.validateComposeDraft(draft); err != nil {
		return mail.MessageSummary{}, nil, false, false, err
	}
	bridge, err := encodeBridgeDraft(draft)
	if err != nil {
		return mail.MessageSummary{}, nil, false, false, err
	}
	bridge, cleanup, err := snapshotBridgeAttachments(bridge)
	if err != nil {
		return mail.MessageSummary{}, nil, false, false, err
	}
	evidenceRoot, err := os.MkdirTemp("", "mailcli-save-evidence-*")
	if err != nil {
		return mail.MessageSummary{}, nil, false, false, errors.Join(err, attachmentSnapshotCleanupError(cleanup()))
	}
	defer func() {
		resultErr = errors.Join(
			resultErr,
			bridgeCleanupError("remove private draft-save evidence", os.RemoveAll(evidenceRoot)),
		)
	}()
	evidencePath := filepath.Join(evidenceRoot, "materialization.json")
	response, _, invokeErr := c.invokeWithState(ctx, bridgeRequest{
		Operation: "drafts.save", Draft: &bridge, EvidencePath: evidencePath,
	})
	cleanupErr := attachmentSnapshotCleanupError(cleanup())
	materialized, evidenceErr := readDraftSaveEvidence(evidencePath)
	invocationStarted = materialized != nil
	if response.Materialized != nil {
		materialized = mapSendMaterialization(response.Materialized)
		invocationStarted = true
	}
	if invokeErr != nil {
		return mail.MessageSummary{}, materialized, invocationStarted, response.Accepted, errors.Join(invokeErr, evidenceErr, cleanupErr)
	}
	if !response.Accepted {
		return mail.MessageSummary{}, materialized, invocationStarted, false, errors.Join(&OperationError{
			Code: "mutation_not_accepted", Message: "Mail.app did not accept the native draft save",
		}, evidenceErr, cleanupErr)
	}
	summary := mail.MessageSummary{Subject: draft.Subject}
	if materialized != nil {
		summary.Subject = materialized.Subject
		summary.Sender = materialized.From
		summary.AttachmentCount = materialized.AttachmentCount
	}
	return summary, materialized, invocationStarted, true, errors.Join(evidenceErr, cleanupErr)
}

func (c *Client) validateComposeDraft(draft mail.Draft) error {
	if err := c.ComposeWriteSupportError(); err != nil {
		return err
	}
	if len(draft.Attachments) == 0 {
		return nil
	}
	return &OperationError{
		Code:    "compose_attachments_unsupported",
		Message: "Mail 16 rejects scripted compose attachments; remove reviewed attachments or add them manually in Mail",
	}
}

func (c *Client) ComposeWriteSupportError() error {
	if !c.blockComposeAutomation {
		return nil
	}
	return &OperationError{
		Code:    "compose_automation_unsupported",
		Message: "Mail 16 compose scripting discards reviewed content or creates phantom drafts; use Mail's UI for send and native draft save",
	}
}

func readDraftSaveEvidence(path string) (*mail.SendMaterialization, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect draft-save evidence: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 20*1024*1024 {
		return nil, fmt.Errorf("draft-save evidence is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open draft-save evidence: %w", err)
	}
	payload, readErr := io.ReadAll(io.LimitReader(file, 20*1024*1024+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if len(payload) > 20*1024*1024 {
		return nil, fmt.Errorf("draft-save evidence exceeds 20 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var evidence bridgeMaterialized
	if err := decoder.Decode(&evidence); err != nil {
		return nil, fmt.Errorf("decode draft-save evidence: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("draft-save evidence must contain one JSON object")
	}
	return mapSendMaterialization(&evidence), nil
}

func snapshotBridgeAttachments(draft bridgeDraft) (bridgeDraft, func() error, error) {
	if len(draft.Attachments) == 0 {
		return draft, func() error { return nil }, nil
	}
	root, err := os.MkdirTemp("", "mailcli-send-attachments-*")
	if err != nil {
		return bridgeDraft{}, func() error { return nil }, fmt.Errorf("create private attachment snapshot: %w", err)
	}
	cleanup := func() error { return os.RemoveAll(root) }
	attachments := make([]mail.DraftAttachment, 0, len(draft.Attachments))
	for index, attachment := range draft.Attachments {
		directory := filepath.Join(root, fmt.Sprintf("%04d", index))
		if err := os.Mkdir(directory, 0o700); err != nil {
			return bridgeDraft{}, func() error { return nil }, errors.Join(
				fmt.Errorf("create attachment snapshot directory: %w", err), cleanup(),
			)
		}
		target := filepath.Join(directory, filepath.Base(attachment.Path))
		if err := copyVerifiedAttachment(attachment, target); err != nil {
			return bridgeDraft{}, func() error { return nil }, errors.Join(err, cleanup())
		}
		attachment.Path = target
		attachments = append(attachments, attachment)
	}
	draft.Attachments = attachments
	return draft, cleanup, nil
}

func attachmentSnapshotCleanupError(err error) error {
	if err == nil {
		return nil
	}
	return &OperationError{
		Code:    "attachment_snapshot_cleanup_failed",
		Message: fmt.Sprintf("remove private attachment snapshot: %v", err),
	}
}

func copyVerifiedAttachment(attachment mail.DraftAttachment, target string) (resultErr error) {
	info, err := os.Lstat(attachment.Path)
	if err != nil {
		return fmt.Errorf("inspect reviewed attachment: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return &OperationError{Code: "attachment_changed", Message: "reviewed attachment is not a regular file"}
	}
	source, err := os.Open(attachment.Path)
	if err != nil {
		return fmt.Errorf("open reviewed attachment: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, source.Close())
	}()
	opened, err := source.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return &OperationError{Code: "attachment_changed", Message: "reviewed attachment changed while opening"}
	}
	destination, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create attachment snapshot: %w", err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(destination, hash), source)
	closeErr := destination.Close()
	final, statErr := source.Stat()
	digest := hex.EncodeToString(hash.Sum(nil))
	if copyErr != nil || closeErr != nil || statErr != nil || !os.SameFile(opened, final) ||
		opened.Size() != final.Size() || !opened.ModTime().Equal(final.ModTime()) ||
		written != attachment.Size || !strings.EqualFold(digest, attachment.SHA256) {
		return errors.Join(
			&OperationError{Code: "attachment_changed", Message: "reviewed attachment bytes changed before Mail.app composition"},
			os.Remove(target),
		)
	}
	return nil
}

func encodeBridgeDraft(draft mail.Draft) (bridgeDraft, error) {
	bridge := bridgeDraft{
		Kind: draft.Kind, ReplyAll: draft.ReplyAll, From: draft.From,
		To: draft.To, CC: draft.CC, BCC: draft.BCC,
		Subject: draft.Subject, Body: draft.Body, Attachments: draft.Attachments,
		ExpectedNativeAttachmentCount: draft.ExpectedNativeAttachmentCount,
	}
	if draft.SourceRef == "" {
		return bridge, nil
	}
	source, err := decodeMessageReference(draft.SourceRef)
	if err != nil {
		return bridgeDraft{}, invalidReference("source message ref", err)
	}
	bridge.Source = &source
	return bridge, nil
}

func (c *Client) MarkMessage(ctx context.Context, request mail.MarkMessageRequest) (mail.MessageSummary, error) {
	ref, err := decodeMessageReference(request.Ref)
	if err != nil {
		return mail.MessageSummary{}, invalidReference("message ref", err)
	}
	response, err := c.invoke(ctx, bridgeRequest{
		Operation: "messages.mark", AccountID: ref.AccountID,
		MailboxPath: ref.MailboxPath, MessageID: ref.LibraryID,
		ExpectedMessageID: ref.ExpectedMessageID, ExpectedSubject: ref.ExpectedSubject,
		Read: request.Read, Flagged: request.Flagged, Junk: request.Junk,
	})
	if err != nil {
		return mail.MessageSummary{}, err
	}
	if !response.Accepted {
		return mail.MessageSummary{}, &OperationError{
			Code: "mutation_not_accepted", Message: "Mail.app did not accept the message state change",
		}
	}
	return requestedMessageState(request), nil
}

func (c *Client) TransferMessage(
	ctx context.Context,
	request mail.TransferMessageRequest,
) (mail.MessageSummary, error) {
	ref, err := decodeMessageReference(request.Ref)
	if err != nil {
		return mail.MessageSummary{}, invalidReference("message ref", err)
	}
	destination, err := decodeMailboxReference(request.DestinationMailbox)
	if err != nil {
		return mail.MessageSummary{}, invalidReference("destination mailbox ref", err)
	}
	response, err := c.invoke(ctx, bridgeRequest{
		Operation: "messages.transfer", AccountID: ref.AccountID,
		MailboxPath: ref.MailboxPath, MessageID: ref.LibraryID,
		ExpectedMessageID:      ref.ExpectedMessageID,
		ExpectedSubject:        ref.ExpectedSubject,
		DestinationAccountID:   destination.AccountID,
		DestinationMailboxPath: destination.Path, Copy: request.Copy,
	})
	if err != nil {
		return mail.MessageSummary{}, err
	}
	if !response.Accepted {
		return mail.MessageSummary{}, &OperationError{
			Code: "mutation_not_accepted", Message: "Mail.app did not accept the message transfer",
		}
	}
	return mail.MessageSummary{Ref: request.Ref, MailboxRef: request.DestinationMailbox}, nil
}

func (c *Client) DeleteMessage(ctx context.Context, request mail.DeleteMessageRequest) error {
	ref, err := decodeMessageReference(request.Ref)
	if err != nil {
		return invalidReference("message ref", err)
	}
	_, err = c.invoke(ctx, bridgeRequest{
		Operation: "messages.delete", AccountID: ref.AccountID,
		MailboxPath: ref.MailboxPath, MessageID: ref.LibraryID,
		ExpectedMessageID: ref.ExpectedMessageID, ExpectedSubject: ref.ExpectedSubject,
	})
	return err
}

func (c *Client) Sync(ctx context.Context, accountRef string) error {
	request := bridgeRequest{Operation: "mail.sync"}
	if accountRef != "" {
		account, err := decodeAccountReference(accountRef)
		if err != nil {
			return invalidReference("account ref", err)
		}
		request.AccountID = account.AccountID
	}
	_, err := c.invoke(ctx, request)
	return err
}

type bridgeResponse struct {
	OK               bool                `json:"ok"`
	Error            *bridgeError        `json:"error"`
	Accounts         []bridgeAccount     `json:"accounts"`
	Mailboxes        []bridgeMailbox     `json:"mailboxes"`
	Messages         []bridgeMessage     `json:"messages"`
	Message          *bridgeMessage      `json:"message"`
	RawSource        string              `json:"raw_source"`
	NextOffset       *int                `json:"next_offset"`
	NextPreviousID   *string             `json:"next_previous_id"`
	Accepted         bool                `json:"accepted"`
	SendAttempted    bool                `json:"send_attempted"`
	RecoveryRequired bool                `json:"recovery_required"`
	Materialized     *bridgeMaterialized `json:"materialized"`
}

type bridgeMaterialized struct {
	From            string           `json:"from"`
	To              []mail.Recipient `json:"to"`
	CC              []mail.Recipient `json:"cc"`
	BCC             []mail.Recipient `json:"bcc"`
	Subject         string           `json:"subject"`
	Body            *string          `json:"body"`
	AttachmentCount int              `json:"attachment_count"`
}

func mapSendMaterialization(value *bridgeMaterialized) *mail.SendMaterialization {
	if value == nil {
		return nil
	}
	return &mail.SendMaterialization{
		From: value.From, To: append([]mail.Recipient(nil), value.To...),
		CC: append([]mail.Recipient(nil), value.CC...), BCC: append([]mail.Recipient(nil), value.BCC...),
		Subject: value.Subject, Body: cloneStringPointer(value.Body), AttachmentCount: value.AttachmentCount,
	}
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

type bridgeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type bridgeAccount struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	EmailAddresses []string `json:"email_addresses"`
}

type bridgeMailbox struct {
	AccountID   string   `json:"account_id"`
	Name        string   `json:"name"`
	Path        []string `json:"path"`
	UnreadCount int      `json:"unread_count"`
}

type bridgeMessage struct {
	AccountID       string            `json:"account_id"`
	MailboxPath     []string          `json:"mailbox_path"`
	LibraryID       string            `json:"library_id"`
	MessageID       string            `json:"message_id"`
	Subject         string            `json:"subject"`
	Sender          string            `json:"sender"`
	DateReceived    string            `json:"date_received"`
	DateSent        string            `json:"date_sent"`
	Read            bool              `json:"read"`
	Flagged         bool              `json:"flagged"`
	Junk            bool              `json:"junk"`
	Deleted         bool              `json:"deleted"`
	Size            int64             `json:"size"`
	AttachmentCount int               `json:"attachment_count"`
	ReplyTo         string            `json:"reply_to"`
	To              []mail.Recipient  `json:"to"`
	CC              []mail.Recipient  `json:"cc"`
	BCC             []mail.Recipient  `json:"bcc"`
	Headers         string            `json:"headers"`
	Content         string            `json:"content"`
	Attachments     []mail.Attachment `json:"attachments"`
}

func NewClient() *Client {
	return &Client{
		runner: osaScriptRunner{}, gate: newFileAccessGate(),
		blockComposeAutomation: true,
	}
}

func (e *OperationError) Error() string {
	return e.Message
}

func (e *OperationError) ErrorCode() string {
	return e.Code
}

func (r osaScriptRunner) Run(ctx context.Context, script string, request string) ([]byte, bool, error) {
	return r.run(ctx, script, request)
}

func (r osaScriptRunner) run(
	ctx context.Context,
	script string,
	request string,
) (output []byte, started bool, resultErr error) {
	requestFile, err := os.CreateTemp("", "mailcli-bridge-request-*")
	if err != nil {
		return nil, false, fmt.Errorf("create Mail.app bridge request: %w", err)
	}
	requestPath := requestFile.Name()
	defer func() {
		resultErr = errors.Join(resultErr, bridgeCleanupError("remove private bridge request", os.Remove(requestPath)))
	}()
	if _, err := requestFile.WriteString(request); err != nil {
		return nil, false, errors.Join(
			fmt.Errorf("write Mail.app bridge request: %w", err), requestFile.Close(),
		)
	}
	if err := requestFile.Close(); err != nil {
		return nil, false, fmt.Errorf("close Mail.app bridge request: %w", err)
	}

	cancellationRoot, err := os.MkdirTemp("", "mailcli-bridge-cancellation-*")
	if err != nil {
		return nil, false, fmt.Errorf("create Mail.app bridge cancellation directory: %w", err)
	}
	defer func() {
		resultErr = errors.Join(
			resultErr,
			bridgeCleanupError("remove private bridge cancellation directory", os.RemoveAll(cancellationRoot)),
		)
	}()
	cancellationPath := filepath.Join(cancellationRoot, "requested")

	command := exec.CommandContext(
		ctx, osaScriptPath, "-l", "JavaScript", "-e", script, requestPath, cancellationPath,
	)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = r.cancellationGrace
	if command.WaitDelay <= 0 {
		command.WaitDelay = osaCancellationGrace
	}
	command.Cancel = func() error {
		marker, createErr := os.OpenFile(cancellationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(createErr, os.ErrExist) {
			return nil
		}
		if createErr != nil {
			return createErr
		}
		return marker.Close()
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return nil, false, formatOSAError(ctx, err, nil)
	}
	if r.started != nil {
		r.started(command.Process.Pid)
	}
	err = command.Wait()
	groupErr := terminateOwnedProcessGroup(command.Process.Pid)
	if err == nil || command.ProcessState != nil && command.ProcessState.Success() {
		if groupErr != nil {
			return stdout.Bytes(), true, bridgeCleanupError("terminate owned osascript process group", groupErr)
		}
		return stdout.Bytes(), true, nil
	}
	return nil, true, errors.Join(formatOSAError(ctx, err, stderr.Bytes()), groupErr)
}

func bridgeCleanupError(action string, err error) error {
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return &OperationError{Code: "bridge_cleanup_failed", Message: fmt.Sprintf("%s: %v", action, err)}
}

func terminateOwnedProcessGroup(processID int) error {
	groupID := -processID
	probeErr := syscall.Kill(groupID, syscall.Signal(0))
	if errors.Is(probeErr, syscall.ESRCH) {
		return nil
	}
	if probeErr != nil {
		return fmt.Errorf("inspect osascript process group %d: %w", processID, probeErr)
	}
	if err := syscall.Kill(groupID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("terminate osascript process group %d: %w", processID, err)
	}

	deadline := time.Now().Add(processGroupTerminationWait)
	for {
		err := syscall.Kill(groupID, syscall.Signal(0))
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("verify osascript process group %d termination: %w", processID, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("osascript process group %d remained after termination", processID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func formatOSAError(ctx context.Context, err error, stderr []byte) error {
	contextErr := ctx.Err()
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		detail := strings.TrimSpace(string(stderr))
		if detail == "" {
			detail = strings.TrimSpace(string(exitError.Stderr))
		}
		err = fmt.Errorf("run Mail.app bridge: %w: %s", err, detail)
	} else {
		err = fmt.Errorf("run Mail.app bridge: %w", err)
	}
	if contextErr != nil {
		return errors.Join(contextErr, err)
	}
	return err
}

func (c *Client) Probe(ctx context.Context, live bool) mail.DiagnosticReport {
	return Probe(ctx, live, c.gate)
}

func (c *Client) ListAccounts(ctx context.Context) ([]mail.Account, error) {
	response, err := c.invoke(ctx, bridgeRequest{Operation: "accounts.list"})
	if err != nil {
		return nil, err
	}

	accounts := make([]mail.Account, 0, len(response.Accounts))
	for _, item := range response.Accounts {
		ref, err := encodeAccountReference(item.ID)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, mail.Account{
			Ref: ref, Name: item.Name, EmailAddresses: item.EmailAddresses,
		})
	}
	return accounts, nil
}

func (c *Client) ListMailboxes(ctx context.Context, request mail.ListMailboxesRequest) ([]mail.Mailbox, error) {
	bridge := bridgeRequest{Operation: "mailboxes.list"}
	if request.AccountRef != "" {
		account, err := decodeAccountReference(request.AccountRef)
		if err != nil {
			return nil, invalidReference("account ref", err)
		}
		bridge.AccountID = account.AccountID
	}

	response, err := c.invoke(ctx, bridge)
	if err != nil {
		return nil, err
	}
	mailboxes := make([]mail.Mailbox, 0, len(response.Mailboxes))
	for _, item := range response.Mailboxes {
		accountRef, err := encodeAccountReference(item.AccountID)
		if err != nil {
			return nil, err
		}
		mailboxRef, err := encodeMailboxReference(item.AccountID, item.Path)
		if err != nil {
			return nil, err
		}
		mailboxes = append(mailboxes, mail.Mailbox{
			Ref: mailboxRef, AccountRef: accountRef, Name: item.Name,
			Path: item.Path, UnreadCount: item.UnreadCount,
		})
	}
	return mailboxes, nil
}

func (c *Client) ListMessages(ctx context.Context, request mail.ListMessagesRequest) (mail.MessagePage, error) {
	bridge, err := listMessagesBridgeRequest(request)
	if err != nil {
		return mail.MessagePage{}, err
	}
	response, err := c.invoke(ctx, bridge)
	if err != nil {
		return mail.MessagePage{}, err
	}
	page, err := c.messagePage(response.Messages)
	if err != nil {
		return mail.MessagePage{}, err
	}
	return addListCursor(page, response, request.MailboxRef)
}

func listMessagesBridgeRequest(request mail.ListMessagesRequest) (bridgeRequest, error) {
	mailbox, err := decodeMailboxReference(request.MailboxRef)
	if err != nil {
		return bridgeRequest{}, invalidReference("mailbox ref", err)
	}
	bridge := bridgeRequest{
		Operation: "messages.list", AccountID: mailbox.AccountID,
		MailboxPath: mailbox.Path, Limit: request.Limit,
	}
	if request.Cursor != "" {
		cursor, err := decodeListCursor(request.Cursor)
		if err != nil {
			return bridgeRequest{}, invalidCursor(err)
		}
		if cursor.MailboxRef != request.MailboxRef {
			return bridgeRequest{}, invalidCursor(fmt.Errorf("cursor belongs to a different mailbox"))
		}
		bridge.Offset = cursor.Offset
		bridge.ExpectedPreviousID = cursor.PreviousID
	}
	return bridge, nil
}

func addListCursor(page mail.MessagePage, response bridgeResponse, mailboxRef string) (mail.MessagePage, error) {
	if (response.NextOffset == nil) != (response.NextPreviousID == nil) {
		return mail.MessagePage{}, fmt.Errorf("mail bridge returned an incomplete cursor")
	}
	if response.NextOffset != nil && response.NextPreviousID != nil {
		cursor, err := encodeListCursor(listCursor{
			MailboxRef: mailboxRef,
			Offset:     *response.NextOffset, PreviousID: *response.NextPreviousID,
		})
		if err != nil {
			return mail.MessagePage{}, err
		}
		page.NextCursor = cursor
	}
	return page, nil
}

func (c *Client) GetMessage(ctx context.Context, ref string) (mail.Message, error) {
	return c.readMessage(ctx, ref, "messages.get")
}

func (c *Client) OpenDraft(ctx context.Context, ref string) (mail.Message, error) {
	return c.readMessage(ctx, ref, "drafts.open")
}

func (c *Client) readMessage(ctx context.Context, ref string, operation string) (mail.Message, error) {
	messageRef, err := decodeMessageReference(ref)
	if err != nil {
		return mail.Message{}, invalidReference("message ref", err)
	}
	response, err := c.invoke(ctx, bridgeRequest{
		Operation: operation, AccountID: messageRef.AccountID,
		MailboxPath: messageRef.MailboxPath, MessageID: messageRef.LibraryID,
		ExpectedMessageID: messageRef.ExpectedMessageID, ExpectedSubject: messageRef.ExpectedSubject,
	})
	if err != nil {
		return mail.Message{}, err
	}
	if response.Message == nil {
		return mail.Message{}, fmt.Errorf("mail bridge returned no message")
	}
	summary, err := mapMessageSummary(*response.Message)
	if err != nil {
		return mail.Message{}, err
	}
	attachments := append([]mail.Attachment(nil), response.Message.Attachments...)
	return mail.Message{
		Summary: summary, ReplyTo: response.Message.ReplyTo,
		To: response.Message.To, CC: response.Message.CC, BCC: response.Message.BCC,
		Headers: response.Message.Headers, Content: response.Message.Content,
		ContentSource: "mail_app", ContentComplete: false,
		MissingParts: []string{"raw RFC 5322 verification required"},
		Attachments:  attachments,
	}, nil
}

func (c *Client) GetRawSource(ctx context.Context, ref string) (string, error) {
	messageRef, err := decodeMessageReference(ref)
	if err != nil {
		return "", invalidReference("message ref", err)
	}
	response, err := c.invoke(ctx, bridgeRequest{
		Operation: "messages.raw", AccountID: messageRef.AccountID,
		MailboxPath: messageRef.MailboxPath, MessageID: messageRef.LibraryID,
		ExpectedMessageID: messageRef.ExpectedMessageID, ExpectedSubject: messageRef.ExpectedSubject,
		MaximumRawSourceBytes: mail.MaximumRawSourceBytes,
	})
	if err != nil {
		return "", err
	}
	if int64(len(response.RawSource)) > mail.MaximumRawSourceBytes {
		return "", &OperationError{Code: "raw_source_too_large", Message: "raw RFC message source exceeds 64 MiB"}
	}
	return response.RawSource, nil
}

func invalidReference(name string, err error) error {
	return &OperationError{
		Code: "invalid_reference", Message: fmt.Sprintf("invalid %s: %v", name, err),
	}
}

func invalidCursor(err error) error {
	return &OperationError{Code: "invalid_cursor", Message: "invalid cursor: " + err.Error()}
}

func (c *Client) invoke(ctx context.Context, request bridgeRequest) (bridgeResponse, error) {
	response, _, err := c.invokeWithState(ctx, request)
	return response, err
}

func (c *Client) invokeWithState(
	ctx context.Context,
	request bridgeRequest,
) (bridgeResponse, bool, error) {
	release, err := c.acquireAccess(ctx)
	if err != nil {
		return bridgeResponse{}, false, err
	}
	request.MailPID = release.TargetPID()
	mutation := operationCanLeaveUncertainMailState(request.Operation)
	if mutation {
		if armErr := release.ArmUncertainState(); armErr != nil {
			releaseErr := release.Release(false)
			return bridgeResponse{}, false, errors.Join(&OperationError{
				Code: "mail_access_gate_failed",
				Message: fmt.Sprintf(
					"MailCLI could not durably arm recovery state; the Mail.app operation did not start: %v",
					armErr,
				),
			}, releaseErr)
		}
	}
	response, started, completed, invokeErr := c.invokeUnlockedWithState(ctx, request)
	uncertain := response.RecoveryRequired ||
		started && !completed && mutation &&
			!isAutomationDenial(invokeErr)
	releaseErr := release.Release(uncertain)
	if releaseErr != nil {
		releaseErr = fmt.Errorf("release Mail.app access gate: %w", releaseErr)
		if invokeErr != nil {
			return bridgeResponse{}, started, errors.Join(invokeErr, releaseErr)
		}
		return bridgeResponse{}, started, releaseErr
	}
	return response, started, invokeErr
}

func operationCanLeaveUncertainMailState(operation string) bool {
	switch operation {
	case "drafts.send", "drafts.save", "messages.mark", "messages.transfer", "messages.delete":
		return true
	default:
		return false
	}
}

func isAutomationDenial(err error) bool {
	if err == nil {
		return false
	}
	detail := strings.ToLower(err.Error())
	return strings.Contains(detail, "-1743") ||
		strings.Contains(detail, "not authorized to send apple events") ||
		strings.Contains(detail, "not authorised to send apple events")
}

func (c *Client) acquireAccess(ctx context.Context) (accessLease, error) {
	if c.gate == nil {
		return noOpAccessLease{}, nil
	}
	release, err := c.gate.Acquire(ctx)
	if err == nil {
		return release, nil
	}
	return nil, mapAccessGateError(err)
}

func mapAccessGateError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &OperationError{
			Code:    "mail_busy",
			Message: "Mail.app access is busy with another MailCLI operation; this request did not start",
		}
	}
	var uncertainState *uncertainMailStateError
	if errors.As(err, &uncertainState) {
		return &OperationError{
			Code:    "mail_recovery_required",
			Message: "a previous Mail.app operation timed out and may still be running; quit and reopen Mail before retrying",
		}
	}
	var invalidState *invalidAccessGateStateError
	if errors.As(err, &invalidState) {
		return &OperationError{
			Code:    "mail_access_gate_corrupt",
			Message: "MailCLI recovery state is invalid; quit Mail, remove ~/Library/Application Support/MailCLI/mail-access.lock, then reopen Mail",
		}
	}
	var notRunning *mailNotRunningError
	if errors.As(err, &notRunning) {
		return &OperationError{
			Code:    "mail_not_running",
			Message: "Mail.app is not running; open it once before a write or live Automation probe",
		}
	}
	return fmt.Errorf("acquire Mail.app access gate: %w", err)
}

type noOpAccessLease struct{}

func (noOpAccessLease) TargetPID() int           { return 0 }
func (noOpAccessLease) ArmUncertainState() error { return nil }
func (noOpAccessLease) Release(bool) error       { return nil }

func (c *Client) invokeUnlockedWithState(
	ctx context.Context,
	request bridgeRequest,
) (bridgeResponse, bool, bool, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return bridgeResponse{}, false, false, fmt.Errorf("encode Mail.app bridge request: %w", err)
	}
	output, started, err := c.runner.Run(ctx, bridgeScript, string(payload))
	postflightErr := errorWithCode(err, "bridge_cleanup_failed")
	if err != nil && (postflightErr == nil || len(output) == 0) {
		return bridgeResponse{}, started, false, err
	}

	var response bridgeResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return bridgeResponse{}, true, false, fmt.Errorf("decode Mail.app bridge response: %w", err)
	}
	if !response.OK {
		if response.Error == nil {
			return response, true, true, errors.Join(
				fmt.Errorf("mail bridge failed without an error"), postflightErr,
			)
		}
		return response, true, true, errors.Join(&OperationError{
			Code: response.Error.Code, Message: response.Error.Message,
		}, postflightErr)
	}
	return response, true, true, postflightErr
}

func errorWithCode(err error, code string) error {
	if err == nil {
		return nil
	}
	if coded, ok := err.(interface {
		error
		ErrorCode() string
	}); ok && coded.ErrorCode() == code {
		return coded
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if matched := errorWithCode(child, code); matched != nil {
				return matched
			}
		}
		return nil
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return errorWithCode(wrapped.Unwrap(), code)
	}
	return nil
}

func (c *Client) messagePage(items []bridgeMessage) (mail.MessagePage, error) {
	messages := make([]mail.MessageSummary, 0, len(items))
	for _, item := range items {
		summary, err := mapMessageSummary(item)
		if err != nil {
			return mail.MessagePage{}, err
		}
		messages = append(messages, summary)
	}
	return mail.MessagePage{Messages: messages}, nil
}

func requestedMessageState(request mail.MarkMessageRequest) mail.MessageSummary {
	result := mail.MessageSummary{Ref: request.Ref}
	if request.Read != nil {
		result.Read = *request.Read
	}
	if request.Flagged != nil {
		result.Flagged = *request.Flagged
	}
	if request.Junk != nil {
		result.Junk = *request.Junk
	}
	return result
}

func mapMessageSummary(item bridgeMessage) (mail.MessageSummary, error) {
	mailboxRef, err := encodeMailboxReference(item.AccountID, item.MailboxPath)
	if err != nil {
		return mail.MessageSummary{}, err
	}
	messageRef, err := encodeMessageReference(messageReference{
		AccountID: item.AccountID, MailboxPath: item.MailboxPath,
		LibraryID: item.LibraryID, ExpectedMessageID: item.MessageID, ExpectedSubject: item.Subject,
	})
	if err != nil {
		return mail.MessageSummary{}, err
	}
	return mail.MessageSummary{
		Ref: messageRef, MailboxRef: mailboxRef, MessageID: item.MessageID,
		Subject: item.Subject, Sender: item.Sender, DateReceived: item.DateReceived,
		DateSent: item.DateSent, Read: item.Read, Flagged: item.Flagged,
		Junk: item.Junk, Deleted: item.Deleted,
		Size: item.Size, AttachmentCount: item.AttachmentCount,
	}, nil
}
