package mailapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

type runnerStub struct {
	response   string
	err        error
	notStarted bool
	request    string
	script     string
}

type blockedAccessGate struct{}

func (blockedAccessGate) Acquire(context.Context) (accessLease, error) {
	return nil, context.DeadlineExceeded
}

type accessLeaseRecorder struct {
	armErr       error
	armCalls     int
	releaseCalls int
	uncertain    bool
	targetPID    int
}

func (lease *accessLeaseRecorder) TargetPID() int {
	return lease.targetPID
}

func (lease *accessLeaseRecorder) ArmUncertainState() error {
	lease.armCalls++
	return lease.armErr
}

func (lease *accessLeaseRecorder) Release(uncertain bool) error {
	lease.releaseCalls++
	lease.uncertain = uncertain
	return nil
}

type accessGateStub struct {
	lease accessLease
}

func (gate accessGateStub) Acquire(context.Context) (accessLease, error) {
	return gate.lease, nil
}

type cancelingSuccessRunner struct {
	cancel context.CancelFunc
}

func (runner cancelingSuccessRunner) Run(context.Context, string, string) ([]byte, bool, error) {
	runner.cancel()
	return []byte(`{"ok":true,"error":null,"accounts":[]}`), true, nil
}

type cancelingBridgeErrorRunner struct {
	cancel context.CancelFunc
}

type saveEvidenceFailureRunner struct{}

func (saveEvidenceFailureRunner) Run(
	_ context.Context,
	_ string,
	request string,
) ([]byte, bool, error) {
	var decoded bridgeRequest
	if err := json.Unmarshal([]byte(request), &decoded); err != nil {
		return nil, false, err
	}
	payload := []byte(`{"from":"sender@example.com","to":[{"address":"to@example.com"}],"cc":[],"bcc":[],"subject":"Subject","body":"Body\n\n--\nSignature","attachment_count":0}`)
	if err := os.WriteFile(decoded.EvidencePath, payload, 0o600); err != nil {
		return nil, false, err
	}
	return nil, true, context.DeadlineExceeded
}

func (runner cancelingBridgeErrorRunner) Run(context.Context, string, string) ([]byte, bool, error) {
	runner.cancel()
	return []byte(`{"ok":false,"error":{"code":"compose_busy","message":"busy"}}`), true, nil
}

func (runner *runnerStub) Run(_ context.Context, script string, request string) ([]byte, bool, error) {
	runner.script = script
	runner.request = request
	return []byte(runner.response), !runner.notStarted, runner.err
}

func TestClientBindsBridgeRequestToGateProcess(t *testing.T) {
	runner := &runnerStub{response: `{"ok":true,"error":null,"accounts":[]}`}
	client := &Client{
		runner: runner,
		gate:   accessGateStub{lease: &accessLeaseRecorder{targetPID: 42}},
	}
	if _, err := client.ListAccounts(context.Background()); err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	var request bridgeRequest
	if err := json.Unmarshal([]byte(runner.request), &request); err != nil {
		t.Fatalf("decode bridge request: %v", err)
	}
	if request.MailPID != 42 {
		t.Fatalf("bridge mail PID = %d, want 42", request.MailPID)
	}
}

func TestSaveDraftRecoversNativeEvidenceAfterRunnerTimeout(t *testing.T) {
	client := &Client{runner: saveEvidenceFailureRunner{}}
	_, materialized, started, accepted, err := client.SaveDraftWithInvocationState(
		context.Background(),
		mail.Draft{
			Kind: mail.DraftKindNew, From: "sender@example.com",
			To: []mail.Recipient{{Address: "to@example.com"}}, Subject: "Subject", Body: "Body",
		},
	)
	if !errors.Is(err, context.DeadlineExceeded) || !started || accepted || materialized == nil ||
		materialized.Body == nil || !strings.Contains(*materialized.Body, "Signature") {
		t.Fatalf(
			"SaveDraftWithInvocationState() materialized = %+v, started = %t, accepted = %t, error = %v",
			materialized, started, accepted, err,
		)
	}
}

func TestClientMapsAccountsAndMailboxes(t *testing.T) {
	runner := &runnerStub{response: `{"ok":true,"error":null,"accounts":[{"id":"account-id","name":"Primary","email_addresses":["mail@example.com"]}]}`}
	client := &Client{runner: runner}

	accounts, err := client.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if len(accounts) != 1 || !strings.HasPrefix(accounts[0].Ref, "acct_") {
		t.Fatalf("accounts = %+v", accounts)
	}

	runner.response = `{"ok":true,"error":null,"mailboxes":[{"account_id":"account-id","name":"Inbox","path":["Inbox"],"unread_count":3}]}`
	mailboxes, err := client.ListMailboxes(context.Background(), mail.ListMailboxesRequest{AccountRef: accounts[0].Ref})
	if err != nil {
		t.Fatalf("ListMailboxes() error = %v", err)
	}
	if len(mailboxes) != 1 || !strings.HasPrefix(mailboxes[0].Ref, "mbx_") || mailboxes[0].UnreadCount != 3 {
		t.Fatalf("mailboxes = %+v", mailboxes)
	}
	if !strings.Contains(runner.request, `"account_id":"account-id"`) {
		t.Fatalf("bridge request = %q", runner.request)
	}
}

func TestSnapshotBridgeAttachmentsPreservesReviewedBytesAndName(t *testing.T) {
	content := []byte("reviewed attachment")
	digest := sha256.Sum256(content)
	path := filepath.Join(t.TempDir(), "evidence.txt")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	draft, cleanup, err := snapshotBridgeAttachments(bridgeDraft{Attachments: []mail.DraftAttachment{{
		Path: path, Size: int64(len(content)), SHA256: hex.EncodeToString(digest[:]),
	}}})
	if err != nil {
		t.Fatalf("snapshotBridgeAttachments() error = %v", err)
	}
	snapshotPath := draft.Attachments[0].Path
	if filepath.Base(snapshotPath) != "evidence.txt" || snapshotPath == path {
		t.Fatalf("snapshot path = %q", snapshotPath)
	}
	if err := os.WriteFile(path, []byte("changed after snapshot"), 0o600); err != nil {
		t.Fatalf("change source: %v", err)
	}
	snapshot, err := os.ReadFile(snapshotPath)
	if err != nil || string(snapshot) != string(content) {
		t.Fatalf("snapshot bytes = %q, error = %v", snapshot, err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup attachment snapshot: %v", err)
	}
	if _, err := os.Stat(snapshotPath); !os.IsNotExist(err) {
		t.Fatalf("snapshot cleanup error = %v", err)
	}
}

func TestClientRejectsScriptedComposeAttachmentsBeforeMailApp(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Client) error
	}{
		{
			name: "send",
			run: func(client *Client) error {
				_, err := client.SendDraft(context.Background(), mail.Draft{
					Attachments: []mail.DraftAttachment{{Path: "/reviewed/file.txt"}},
				})
				return err
			},
		},
		{
			name: "save",
			run: func(client *Client) error {
				_, err := client.SaveDraft(context.Background(), mail.Draft{
					Attachments: []mail.DraftAttachment{{Path: "/reviewed/file.txt"}},
				})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &runnerStub{}
			err := test.run(&Client{runner: runner})
			var operationError *OperationError
			if !errors.As(err, &operationError) || operationError.Code != "compose_attachments_unsupported" {
				t.Fatalf("error = %v", err)
			}
			if runner.request != "" {
				t.Fatalf("Mail.app bridge unexpectedly invoked with %q", runner.request)
			}
		})
	}
}

func TestProductionClientBlocksUnsafeMail16ComposeBeforeMailApp(t *testing.T) {
	runner := &runnerStub{}
	client := &Client{runner: runner, blockComposeAutomation: true}
	_, err := client.SendDraft(context.Background(), mail.Draft{Kind: mail.DraftKindNew})
	var operationError *OperationError
	if !errors.As(err, &operationError) || operationError.Code != "compose_automation_unsupported" {
		t.Fatalf("SendDraft() error = %v", err)
	}
	if runner.request != "" {
		t.Fatalf("Mail.app bridge unexpectedly invoked with %q", runner.request)
	}
}

func TestClientMapsMessagePageAndCursor(t *testing.T) {
	mailboxRef, err := encodeMailboxReference("account-id", []string{"Inbox"})
	if err != nil {
		t.Fatalf("encodeMailboxReference() error = %v", err)
	}
	nextOffset := 1
	nextPreviousID := "42"
	response := bridgeResponse{
		OK: true,
		Messages: []bridgeMessage{{
			AccountID: "account-id", MailboxPath: []string{"Inbox"},
			LibraryID: "42", MessageID: "message-id", Subject: "Subject",
		}},
		NextOffset: &nextOffset, NextPreviousID: &nextPreviousID,
	}
	payload := mustBridgeJSON(t, response)
	runner := &runnerStub{response: payload}
	client := &Client{runner: runner}

	page, err := client.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: mailboxRef, Limit: 1,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if len(page.Messages) != 1 || !strings.HasPrefix(page.Messages[0].Ref, "msg_") {
		t.Fatalf("page = %+v", page)
	}
	if !strings.HasPrefix(page.NextCursor, "cur_") {
		t.Fatalf("next cursor = %q", page.NextCursor)
	}
	assertListCursorRequest(t, client, runner, mailboxRef, page.NextCursor)
}

func assertListCursorRequest(t *testing.T, client *Client, runner *runnerStub, mailboxRef string, cursor string) {
	t.Helper()
	runner.response = `{"ok":true,"error":null,"messages":[]}`
	_, err := client.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: mailboxRef, Cursor: cursor, Limit: 1,
	})
	if err != nil {
		t.Fatalf("ListMessages() with cursor error = %v", err)
	}
	if !strings.Contains(runner.request, `"offset":1`) || !strings.Contains(runner.request, `"expected_previous_id":"42"`) {
		t.Fatalf("bridge request = %q", runner.request)
	}
}

func TestClientRejectsIncompleteBridgeCursor(t *testing.T) {
	mailboxRef, err := encodeMailboxReference("account-id", []string{"Inbox"})
	if err != nil {
		t.Fatalf("encodeMailboxReference() error = %v", err)
	}
	nextOffset := 1
	client := &Client{runner: &runnerStub{response: mustBridgeJSON(t, bridgeResponse{
		OK: true, NextOffset: &nextOffset,
	})}}

	_, err = client.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: mailboxRef, Limit: 1,
	})
	if err == nil {
		t.Fatal("ListMessages() error = nil, want incomplete cursor error")
	}
}

func TestClientReturnsTypedBridgeError(t *testing.T) {
	client := &Client{runner: &runnerStub{response: `{"ok":false,"error":{"code":"not_found","message":"message not found"}}`}}
	_, err := client.ListAccounts(context.Background())
	if err == nil {
		t.Fatal("ListAccounts() error = nil")
	}
	var operationError *OperationError
	if !errors.As(err, &operationError) {
		t.Fatalf("error type = %T, want *OperationError", err)
	}
	if operationError.ErrorCode() != "not_found" {
		t.Fatalf("error code = %q", operationError.ErrorCode())
	}
}

func TestClientNeverClaimsConvenienceMessageReadComplete(t *testing.T) {
	messageRef, err := encodeMessageReference(messageReference{
		AccountID: "account-id", MailboxPath: []string{"Inbox"}, LibraryID: "42",
	})
	if err != nil {
		t.Fatalf("encodeMessageReference() error = %v", err)
	}
	runner := &runnerStub{response: `{"ok":true,"message":{"account_id":"account-id","mailbox_path":["Inbox"],"library_id":"42","content":""}}`}
	client := &Client{runner: runner}
	message, err := client.GetMessage(context.Background(), messageRef)
	if err != nil || message.ContentComplete || len(message.MissingParts) != 1 {
		t.Fatalf("GetMessage() = %+v, error = %v", message, err)
	}
}

func TestClientDoesNotInvokeBridgeWhenAccessGateIsBusy(t *testing.T) {
	runner := &runnerStub{response: `{"ok":true,"error":null}`}
	client := &Client{runner: runner, gate: blockedAccessGate{}}
	_, err := client.ListAccounts(context.Background())
	var operationError *OperationError
	if !errors.As(err, &operationError) || operationError.ErrorCode() != "mail_busy" {
		t.Fatalf("ListAccounts() error = %v, want mail_busy", err)
	}
	if runner.request != "" {
		t.Fatalf("bridge unexpectedly invoked with %q", runner.request)
	}
}

func TestClientDoesNotRunScriptWhenMailIsStopped(t *testing.T) {
	runner := &runnerStub{response: `{"ok":true,"accounts":[]}`}
	client := &Client{
		runner: runner,
		gate: &fileAccessGate{
			path: filepath.Join(t.TempDir(), "mail.lock"),
			mailPID: func(context.Context) (int, error) {
				return 0, nil
			},
		},
	}
	_, err := client.ListAccounts(context.Background())
	var operationError *OperationError
	if !errors.As(err, &operationError) || operationError.Code != "mail_not_running" {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if runner.request != "" {
		t.Fatalf("bridge unexpectedly invoked with %q", runner.request)
	}
}

func TestClientDoesNotMarkSuccessfulCallUncertainAfterConcurrentCancel(t *testing.T) {
	lease := &accessLeaseRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	client := &Client{
		runner: cancelingSuccessRunner{cancel: cancel},
		gate:   accessGateStub{lease: lease},
	}

	accounts, err := client.ListAccounts(ctx)
	if err != nil || accounts == nil || lease.uncertain {
		t.Fatalf("ListAccounts() = %+v, error = %v, lease uncertain = %t", accounts, err, lease.uncertain)
	}
}

func TestClientDoesNotMarkCompletedBridgeErrorUncertainAfterConcurrentCancel(t *testing.T) {
	lease := &accessLeaseRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	client := &Client{
		runner: cancelingBridgeErrorRunner{cancel: cancel},
		gate:   accessGateStub{lease: lease},
	}

	_, err := client.ListAccounts(ctx)
	var operationError *OperationError
	if !errors.As(err, &operationError) || operationError.Code != "compose_busy" || lease.uncertain {
		t.Fatalf("ListAccounts() error = %v, lease uncertain = %t", err, lease.uncertain)
	}
}

func TestClientLatchesBridgeRequestedRecovery(t *testing.T) {
	lease := &accessLeaseRecorder{}
	client := &Client{
		runner: &runnerStub{response: `{"ok":false,"recovery_required":true,"error":{"code":"compose_cleanup_failed","message":"retained"}}`},
		gate:   accessGateStub{lease: lease},
	}
	_, err := client.ListAccounts(context.Background())
	var operationError *OperationError
	if !errors.As(err, &operationError) || operationError.Code != "compose_cleanup_failed" || !lease.uncertain {
		t.Fatalf("ListAccounts() error = %v, lease uncertain = %t", err, lease.uncertain)
	}
}

func TestClientTreatsPrivateBridgeCleanupFailureAsCompleted(t *testing.T) {
	lease := &accessLeaseRecorder{}
	client := &Client{
		runner: &runnerStub{
			response: `{"ok":true,"error":null,"accounts":[]}`,
			err:      &OperationError{Code: "bridge_cleanup_failed", Message: "private request remained"},
		},
		gate: accessGateStub{lease: lease},
	}

	_, err := client.ListAccounts(context.Background())
	if errorWithCode(err, "bridge_cleanup_failed") == nil || lease.uncertain {
		t.Fatalf("ListAccounts() error = %v, lease uncertain = %t", err, lease.uncertain)
	}
}

func TestClientDoesNotLatchIncompleteReadCompletion(t *testing.T) {
	tests := []struct {
		name     string
		response string
		err      error
	}{
		{name: "runner transport failed", err: errors.New("transport failed")},
		{name: "bridge response was truncated", response: `{"ok":true`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lease := &accessLeaseRecorder{}
			client := &Client{
				runner: &runnerStub{response: test.response, err: test.err},
				gate:   accessGateStub{lease: lease},
			}
			if _, err := client.ListAccounts(context.Background()); err == nil {
				t.Fatal("ListAccounts() error = nil")
			}
			if lease.uncertain {
				t.Fatal("incomplete read-only bridge completion latched Mail recovery")
			}
		})
	}
}

func TestClientLatchesOnlyIncompleteMutations(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		err       error
		want      bool
	}{
		{name: "compose mutation", operation: "drafts.save", err: errors.New("transport failed"), want: true},
		{name: "message mutation", operation: "messages.delete", err: errors.New("transport failed"), want: true},
		{name: "sync", operation: "mail.sync", err: context.DeadlineExceeded},
		{name: "read", operation: "messages.raw", err: context.DeadlineExceeded},
		{name: "automation denial", operation: "drafts.send", err: errors.New("not authorized to send Apple events (-1743)")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lease := &accessLeaseRecorder{}
			client := &Client{
				runner: &runnerStub{err: test.err}, gate: accessGateStub{lease: lease},
			}
			_, _, _ = client.invokeWithState(context.Background(), bridgeRequest{Operation: test.operation})
			wantArmCalls := 0
			if operationCanLeaveUncertainMailState(test.operation) {
				wantArmCalls = 1
			}
			if lease.armCalls != wantArmCalls {
				t.Fatalf("lease arm calls = %d, want %d", lease.armCalls, wantArmCalls)
			}
			if lease.uncertain != test.want {
				t.Fatalf("lease uncertain = %t, want %t", lease.uncertain, test.want)
			}
		})
	}
}

func TestClientDoesNotStartMutationWhenRecoveryStateCannotBeArmed(t *testing.T) {
	lease := &accessLeaseRecorder{armErr: errors.New("disk write failed")}
	runner := &runnerStub{response: `{"ok":true,"accepted":true}`}
	client := &Client{runner: runner, gate: accessGateStub{lease: lease}}

	_, _, err := client.invokeWithState(
		context.Background(), bridgeRequest{Operation: "messages.delete"},
	)
	var operationError *OperationError
	if !errors.As(err, &operationError) || operationError.Code != "mail_access_gate_failed" {
		t.Fatalf("invokeWithState() error = %v", err)
	}
	if runner.request != "" {
		t.Fatalf("Mail.app bridge unexpectedly invoked with %q", runner.request)
	}
	if lease.armCalls != 1 || lease.releaseCalls != 1 || lease.uncertain {
		t.Fatalf("lease = %+v", lease)
	}
}

func TestClientDoesNotMarkPreStartRunnerFailureUncertain(t *testing.T) {
	lease := &accessLeaseRecorder{}
	client := &Client{
		runner: &runnerStub{err: errors.New("process did not start"), notStarted: true},
		gate:   accessGateStub{lease: lease},
	}
	if _, err := client.ListAccounts(context.Background()); err == nil {
		t.Fatal("ListAccounts() error = nil")
	}
	if lease.uncertain {
		t.Fatal("pre-start runner failure marked Mail state uncertain")
	}
}

func TestProbeReportsTypedBusyGate(t *testing.T) {
	report := Probe(context.Background(), true, blockedAccessGate{})
	check := report.Checks[len(report.Checks)-1]
	if check.Status != "fail" || check.Code != "mail_busy" {
		t.Fatalf("mail automation check = %+v", check)
	}
}

func TestProbeTargetsTheGatedMailProcess(t *testing.T) {
	runner := &runnerStub{response: "16.0"}
	check := probeAutomationWithRunner(
		context.Background(),
		accessGateStub{lease: &accessLeaseRecorder{targetPID: 77}},
		runner,
	)
	if check.Status != "pass" || !strings.Contains(runner.script, "const processID = 77") ||
		!strings.Contains(runner.script, "Application(processID)") ||
		!strings.Contains(runner.script, `bundleIdentifier !== "com.apple.mail"`) ||
		strings.Contains(runner.script, `Application("Mail")`) {
		t.Fatalf("probe check = %+v, script = %q", check, runner.script)
	}
}

func TestProbeMapsAutomationFailureTable(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		response   string
		wantCode   string
		wantDetail string
	}{
		{
			name: "tcc denied", err: errors.New("osascript exited"),
			response: "Not authorized to send Apple events to Mail. (-1743)",
			wantCode: "mail_automation_denied", wantDetail: "System Settings > Privacy & Security > Automation",
		},
		{name: "timeout", err: context.DeadlineExceeded, wantCode: "mail_automation_timeout", wantDetail: "retry after Mail.app becomes responsive"},
		{name: "other", err: errors.New("transport"), wantCode: "mail_automation_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lease := &accessLeaseRecorder{}
			check := probeAutomationWithRunner(
				context.Background(), accessGateStub{lease: lease},
				&runnerStub{err: test.err, response: test.response},
			)
			if check.Status != "fail" || check.Code != test.wantCode ||
				!strings.Contains(check.Detail, test.wantDetail) {
				t.Fatalf("check = %+v", check)
			}
			if lease.uncertain {
				t.Fatalf("lease uncertain = %t", lease.uncertain)
			}
		})
	}
}

func TestClientRejectsCursorForDifferentMailbox(t *testing.T) {
	firstMailbox, err := encodeMailboxReference("account-id", []string{"Inbox"})
	if err != nil {
		t.Fatalf("encodeMailboxReference() error = %v", err)
	}
	secondMailbox, err := encodeMailboxReference("account-id", []string{"Archive"})
	if err != nil {
		t.Fatalf("encodeMailboxReference() error = %v", err)
	}
	cursor, err := encodeListCursor(listCursor{MailboxRef: firstMailbox, Offset: 1, PreviousID: "42"})
	if err != nil {
		t.Fatalf("encodeListCursor() error = %v", err)
	}
	client := &Client{runner: &runnerStub{response: `{"ok":true,"error":null,"messages":[]}`}}

	_, err = client.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: secondMailbox, Cursor: cursor, Limit: 1,
	})
	if err == nil {
		t.Fatal("ListMessages() error = nil, want cursor scope error")
	}
}

func TestClientReturnsTypedInvalidReference(t *testing.T) {
	client := &Client{runner: &runnerStub{}}
	_, err := client.GetMessage(context.Background(), "not-a-message-ref")
	if err == nil {
		t.Fatal("GetMessage() error = nil")
	}
	var operationError *OperationError
	if !errors.As(err, &operationError) {
		t.Fatalf("error type = %T, want *OperationError", err)
	}
	if operationError.ErrorCode() != "invalid_reference" {
		t.Fatalf("error code = %q", operationError.ErrorCode())
	}
}

func TestClientEncodesReplyDraftForConfirmedSend(t *testing.T) {
	messageRef, err := encodeMessageReference(messageReference{
		AccountID: "account-id", MailboxPath: []string{"Inbox"},
		LibraryID: "42", ExpectedMessageID: "message-id",
	})
	if err != nil {
		t.Fatalf("encodeMessageReference() error = %v", err)
	}
	runner := &runnerStub{response: `{"ok":true,"error":null,"accepted":true}`}
	client := &Client{runner: runner}
	evidence, err := client.SendDraft(context.Background(), mail.Draft{
		Kind: mail.DraftKindReply, SourceRef: messageRef, ReplyAll: true,
		Body: "Reply body", CC: []mail.Recipient{{Address: "copy@example.com"}},
	})
	if err != nil || !evidence.AcceptedByMail || !evidence.InvocationStarted {
		t.Fatalf("SendDraft() evidence = %+v, error = %v", evidence, err)
	}
	for _, expected := range []string{
		`"operation":"drafts.send"`, `"kind":"reply"`,
		`"account_id":"account-id"`, `"library_id":"42"`, `"reply_all":true`,
	} {
		if !strings.Contains(runner.request, expected) {
			t.Fatalf("bridge request = %q, missing %q", runner.request, expected)
		}
	}
}

func TestClientClassifiesSendStartBoundary(t *testing.T) {
	tests := []struct {
		name         string
		response     string
		gate         accessGate
		wantStarted  bool
		wantAccepted bool
		wantError    bool
	}{
		{
			name: "access gate rejected before runner", response: `{"ok":true,"accepted":true}`,
			gate: blockedAccessGate{}, wantError: true,
		},
		{
			name: "bridge failed before send", response: `{"ok":false,"send_attempted":false,"error":{"code":"invalid_request","message":"invalid"}}`,
			wantError: true,
		},
		{
			name: "bridge failed after send call began", response: `{"ok":false,"send_attempted":true,"error":{"code":"mail_error","message":"failed"}}`,
			wantStarted: true, wantError: true,
		},
		{
			name: "bridge accepted", response: `{"ok":true,"send_attempted":true,"accepted":true,"error":null}`,
			wantStarted: true, wantAccepted: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &Client{runner: &runnerStub{response: test.response}, gate: test.gate}
			evidence, err := client.SendDraft(context.Background(), mail.Draft{Kind: mail.DraftKindNew})
			if evidence.InvocationStarted != test.wantStarted || evidence.AcceptedByMail != test.wantAccepted ||
				(err != nil) != test.wantError {
				t.Fatalf("SendDraft() = %+v, error = %v", evidence, err)
			}
		})
	}
}

func TestClientTreatsUndecodableSendResponseAsUncertain(t *testing.T) {
	client := &Client{runner: &runnerStub{response: `{not-json`}}
	evidence, err := client.SendDraft(context.Background(), mail.Draft{Kind: mail.DraftKindNew})
	if err == nil || !evidence.InvocationStarted || evidence.AcceptedByMail {
		t.Fatalf("SendDraft() = %+v, error = %v", evidence, err)
	}
}

func TestClientEncodesAndMapsSavedDraft(t *testing.T) {
	runner := &runnerStub{response: `{"ok":true,"error":null,"accepted":true}`}
	client := &Client{runner: runner}
	message, err := client.SaveDraft(context.Background(), mail.Draft{
		Kind: mail.DraftKindNew, From: "mail@example.com",
		To: []mail.Recipient{{Address: "recipient@example.com"}}, Subject: "Saved", Body: "Body",
	})
	if err != nil || message.Ref != "" || message.Subject != "Saved" {
		t.Fatalf("SaveDraft() = %+v, error = %v", message, err)
	}
	for _, expected := range []string{`"operation":"drafts.save"`, `"from":"mail@example.com"`} {
		if !strings.Contains(runner.request, expected) {
			t.Fatalf("bridge request = %q, missing %q", runner.request, expected)
		}
	}
}

func TestClientDoesNotRestartOrReopenSavedDraft(t *testing.T) {
	runner := &runnerStub{response: `{"ok":true,"error":null,"accepted":true}`}
	client := &Client{runner: runner}
	message, err := client.SaveDraft(context.Background(), mail.Draft{
		Kind: mail.DraftKindNew, From: "mail@example.com",
		To: []mail.Recipient{{Address: "recipient@example.com"}}, Subject: "Saved", Body: "Body",
	})
	if err != nil || message.Subject != "Saved" {
		t.Fatalf("SaveDraft() = %+v, error = %v", message, err)
	}
	if !strings.Contains(runner.request, `"operation":"drafts.save"`) {
		t.Fatalf("bridge request = %q", runner.request)
	}
}

func TestClientUsesDraftOpenOperation(t *testing.T) {
	messageRef, err := encodeMessageReference(messageReference{
		AccountID: "account-id", MailboxPath: []string{"Drafts"},
		LibraryID: "43", ExpectedMessageID: "draft-id",
	})
	if err != nil {
		t.Fatalf("encodeMessageReference() error = %v", err)
	}
	runner := &runnerStub{response: `{"ok":true,"error":null,"message":{"account_id":"account-id","mailbox_path":["Drafts"],"library_id":"44","message_id":"draft-id","subject":"Draft"}}`}
	client := &Client{runner: runner}
	message, err := client.OpenDraft(context.Background(), messageRef)
	if err != nil || message.Summary.Ref == messageRef {
		t.Fatalf("OpenDraft() = %+v, error = %v", message, err)
	}
	if !strings.Contains(runner.request, `"operation":"drafts.open"`) {
		t.Fatalf("bridge request = %q", runner.request)
	}
}

func TestClientEncodesAndMapsMessageMark(t *testing.T) {
	messageRef, err := encodeMessageReference(messageReference{
		AccountID: "account-id", MailboxPath: []string{"Inbox"},
		LibraryID: "42", ExpectedMessageID: "message-id",
	})
	if err != nil {
		t.Fatalf("encodeMessageReference() error = %v", err)
	}
	runner := &runnerStub{response: `{"ok":true,"error":null,"accepted":true}`}
	client := &Client{runner: runner}
	read := true
	junk := true
	message, err := client.MarkMessage(context.Background(), mail.MarkMessageRequest{
		Ref: messageRef, Read: &read, Junk: &junk,
	})
	if err != nil || !message.Read || !message.Junk || message.Ref == "" {
		t.Fatalf("MarkMessage() = %+v, error = %v", message, err)
	}
	for _, expected := range []string{`"operation":"messages.mark"`, `"read":true`, `"junk":true`} {
		if !strings.Contains(runner.request, expected) {
			t.Fatalf("bridge request = %q, missing %q", runner.request, expected)
		}
	}
}
