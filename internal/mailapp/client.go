package mailapp

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"mailcli/internal/mail"
)

//go:embed scripts/bridge.js
var bridgeScript string

type scriptRunner interface {
	Run(ctx context.Context, script string, request string) ([]byte, error)
}

type osaScriptRunner struct {
	started func(int)
}

type Client struct {
	runner scriptRunner
	gate   accessGate
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
}

type bridgeDraft struct {
	Kind        mail.DraftKind         `json:"kind"`
	Source      *messageReference      `json:"source,omitempty"`
	ReplyAll    bool                   `json:"reply_all,omitempty"`
	From        string                 `json:"from,omitempty"`
	To          []mail.Recipient       `json:"to"`
	CC          []mail.Recipient       `json:"cc"`
	BCC         []mail.Recipient       `json:"bcc"`
	Subject     string                 `json:"subject,omitempty"`
	Body        string                 `json:"body"`
	Attachments []mail.DraftAttachment `json:"attachments"`
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
	bridge, err := encodeBridgeDraft(draft)
	if err != nil {
		return mail.SendEvidence{}, err
	}
	response, started, err := c.invokeWithState(ctx, bridgeRequest{Operation: "drafts.send", Draft: &bridge})
	if err != nil {
		attempted := started
		if response.Error != nil {
			attempted = response.SendAttempted
		}
		return mail.SendEvidence{InvocationStarted: attempted}, err
	}
	return mail.SendEvidence{
		InvocationStarted: true,
		AcceptedByMail:    response.Accepted,
	}, nil
}

func (c *Client) SaveDraft(ctx context.Context, draft mail.Draft) (mail.MessageSummary, error) {
	bridge, err := encodeBridgeDraft(draft)
	if err != nil {
		return mail.MessageSummary{}, err
	}
	response, err := c.invoke(ctx, bridgeRequest{Operation: "drafts.save", Draft: &bridge})
	if err != nil {
		return mail.MessageSummary{}, err
	}
	if !response.Accepted {
		return mail.MessageSummary{}, &OperationError{
			Code: "mutation_not_accepted", Message: "Mail.app did not accept the native draft save",
		}
	}
	return mail.MessageSummary{Subject: draft.Subject}, nil
}

func encodeBridgeDraft(draft mail.Draft) (bridgeDraft, error) {
	bridge := bridgeDraft{
		Kind: draft.Kind, ReplyAll: draft.ReplyAll, From: draft.From,
		To: draft.To, CC: draft.CC, BCC: draft.BCC,
		Subject: draft.Subject, Body: draft.Body, Attachments: draft.Attachments,
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

func (c *Client) DeleteMessage(ctx context.Context, value string) error {
	ref, err := decodeMessageReference(value)
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
	OK             bool            `json:"ok"`
	Error          *bridgeError    `json:"error"`
	Accounts       []bridgeAccount `json:"accounts"`
	Mailboxes      []bridgeMailbox `json:"mailboxes"`
	Messages       []bridgeMessage `json:"messages"`
	Message        *bridgeMessage  `json:"message"`
	RawSource      string          `json:"raw_source"`
	NextOffset     *int            `json:"next_offset"`
	NextPreviousID *string         `json:"next_previous_id"`
	Accepted       bool            `json:"accepted"`
	SendAttempted  bool            `json:"send_attempted"`
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
	return &Client{runner: osaScriptRunner{}, gate: newFileAccessGate()}
}

func (e *OperationError) Error() string {
	return e.Message
}

func (e *OperationError) ErrorCode() string {
	return e.Code
}

func (r osaScriptRunner) Run(ctx context.Context, script string, request string) ([]byte, error) {
	return r.run(ctx, script, request)
}

func (r osaScriptRunner) run(ctx context.Context, script string, request string) ([]byte, error) {
	command := exec.CommandContext(ctx, osaScriptPath, "-l", "JavaScript", "-e", script, request)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = time.Second
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return nil, formatOSAError(ctx, err, nil)
	}
	if r.started != nil {
		r.started(command.Process.Pid)
	}
	err := command.Wait()
	if err == nil {
		return stdout.Bytes(), nil
	}
	return nil, formatOSAError(ctx, err, stderr.Bytes())
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
	for index := range attachments {
		attachments[index].SizeKnown = true
	}
	return mail.Message{
		Summary: summary, ReplyTo: response.Message.ReplyTo,
		To: response.Message.To, CC: response.Message.CC, BCC: response.Message.BCC,
		Headers: response.Message.Headers, Content: response.Message.Content,
		ContentSource: "mail_app", ContentComplete: true, MissingParts: []string{},
		Attachments: attachments,
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
	})
	if err != nil {
		return "", err
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
	response, started, completed, invokeErr := c.invokeUnlockedWithState(ctx, request)
	uncertain := started && !completed
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

func (noOpAccessLease) TargetPID() int     { return 0 }
func (noOpAccessLease) Release(bool) error { return nil }

func (c *Client) invokeUnlockedWithState(
	ctx context.Context,
	request bridgeRequest,
) (bridgeResponse, bool, bool, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return bridgeResponse{}, false, false, fmt.Errorf("encode Mail.app bridge request: %w", err)
	}
	output, err := c.runner.Run(ctx, bridgeScript, string(payload))
	if err != nil {
		return bridgeResponse{}, true, false, err
	}

	var response bridgeResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return bridgeResponse{}, true, false, fmt.Errorf("decode Mail.app bridge response: %w", err)
	}
	if !response.OK {
		if response.Error == nil {
			return response, true, true, fmt.Errorf("mail bridge failed without an error")
		}
		return response, true, true, &OperationError{
			Code: response.Error.Code, Message: response.Error.Message,
		}
	}
	return response, true, true, nil
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
