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
	Operation          string   `json:"operation"`
	MailPID            int      `json:"mail_pid,omitempty"`
	AccountID          string   `json:"account_id,omitempty"`
	MailboxPath        []string `json:"mailbox_path,omitempty"`
	Offset             int      `json:"offset,omitempty"`
	ExpectedPreviousID string   `json:"expected_previous_id,omitempty"`
	Limit              int      `json:"limit,omitempty"`
}

func (c *Client) ComposeWriteSupportError() error {
	if !c.blockComposeAutomation {
		return nil
	}
	return &OperationError{
		Code:    "compose_automation_unsupported",
		Message: "Mail 16 compose scripting discards reviewed content or creates phantom drafts; use 'drafts send --confirm' for sending and Mail's UI for native draft save",
	}
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
	OK               bool            `json:"ok"`
	Error            *bridgeError    `json:"error"`
	Accounts         []bridgeAccount `json:"accounts"`
	Messages         []bridgeMessage `json:"messages"`
	NextOffset       *int            `json:"next_offset"`
	NextPreviousID   *string         `json:"next_previous_id"`
	RecoveryRequired bool            `json:"recovery_required"`
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

type bridgeMessage struct {
	AccountID       string   `json:"account_id"`
	MailboxPath     []string `json:"mailbox_path"`
	LibraryID       string   `json:"library_id"`
	MessageID       string   `json:"message_id"`
	Subject         string   `json:"subject"`
	Sender          string   `json:"sender"`
	DateReceived    string   `json:"date_received"`
	DateSent        string   `json:"date_sent"`
	Read            bool     `json:"read"`
	Flagged         bool     `json:"flagged"`
	Junk            bool     `json:"junk"`
	Deleted         bool     `json:"deleted"`
	Size            int64    `json:"size"`
	AttachmentCount int      `json:"attachment_count"`
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

func operationCanLeaveUncertainMailState(string) bool {
	return false
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
