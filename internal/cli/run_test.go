package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

type testGateway struct{}

type failingGateway struct {
	testGateway
}

type busyProbeGateway struct {
	testGateway
}

type unknownSendGateway struct {
	testGateway
}

type reconciledSendGateway struct {
	testGateway
	sends int
}

func (testGateway) Probe(context.Context, bool) mail.DiagnosticReport {
	return mail.DiagnosticReport{Checks: []mail.Check{
		{Name: "platform", Status: "pass", Detail: "darwin"},
		{Name: "architecture", Status: "pass", Detail: "arm64"},
		{Name: "osascript", Status: "pass", Detail: "/usr/bin/osascript"},
		{Name: "mail-app", Status: "pass", Detail: "Mail.app"},
		{Name: "mail-scripting-definition", Status: "pass", Detail: "Mail.sdef"},
		{Name: "mail-automation", Status: "not-run", Detail: "not requested"},
	}}
}

func (testGateway) ListAccounts(context.Context) ([]mail.Account, error) {
	return []mail.Account{{Ref: "acct_ref", Name: "Account", EmailAddresses: []string{"mail@example.com"}}}, nil
}

func (testGateway) ListMailboxes(context.Context, mail.ListMailboxesRequest) ([]mail.Mailbox, error) {
	return []mail.Mailbox{{Ref: "mbx_ref", AccountRef: "acct_ref", Name: "Inbox", Path: []string{"Inbox"}}}, nil
}

func (testGateway) ListMessages(context.Context, mail.ListMessagesRequest) (mail.MessagePage, error) {
	return mail.MessagePage{Messages: []mail.MessageSummary{{Ref: "msg_ref", Subject: "Subject"}}}, nil
}

func (testGateway) GetMessage(context.Context, string) (mail.Message, error) {
	return mail.Message{Summary: mail.MessageSummary{Ref: "msg_ref", Subject: "Subject"}}, nil
}

func (testGateway) OpenDraft(context.Context, string) (mail.Message, error) {
	return mail.Message{Summary: mail.MessageSummary{Ref: "msg_ref", Subject: "Draft"}}, nil
}

func (testGateway) GetRawSource(context.Context, string) (string, error) {
	return "Raw source\r\n", nil
}

func (testGateway) SaveAttachmentTo(_ context.Context, _ string, _ string, path string) error {
	return os.WriteFile(path, []byte("attachment bytes"), 0o600)
}

func (testGateway) SaveDraft(context.Context, mail.Draft) (mail.MessageSummary, error) {
	return mail.MessageSummary{Ref: "msg_saved", Subject: "Saved"}, nil
}

func (testGateway) SendDraft(context.Context, mail.Draft) (mail.SendEvidence, error) {
	return mail.SendEvidence{InvocationStarted: true, AcceptedByMail: true}, nil
}

func (testGateway) MarkMessage(_ context.Context, request mail.MarkMessageRequest) (mail.MessageSummary, error) {
	return mail.MessageSummary{Ref: request.Ref, Read: request.Read != nil && *request.Read}, nil
}

func (testGateway) TransferMessage(_ context.Context, request mail.TransferMessageRequest) (mail.MessageSummary, error) {
	return mail.MessageSummary{Ref: request.Ref, MailboxRef: request.DestinationMailbox}, nil
}

func (testGateway) DeleteMessage(context.Context, string) error {
	return nil
}

func (testGateway) Sync(context.Context, string) error {
	return nil
}

func (testGateway) SearchMessages(context.Context, mail.PreparedQuery) (mail.SearchPage, error) {
	return mail.SearchPage{
		Messages: []mail.SearchMessage{{
			Summary: mail.MessageSummary{Ref: "msg_ref", Subject: "Searchable"},
			Snippet: "Needle body",
		}},
		Coverage: mail.SearchCoverage{Backend: "test", Complete: true},
	}, nil
}

func newTestService() *mail.Service {
	return mail.NewService(testGateway{})
}

func (failingGateway) ListAccounts(context.Context) ([]mail.Account, error) {
	return nil, fmt.Errorf("account read failed")
}

func (busyProbeGateway) Probe(context.Context, bool) mail.DiagnosticReport {
	return mail.DiagnosticReport{Checks: []mail.Check{{
		Name: "mail-automation", Status: "fail", Code: "mail_busy", Detail: "did not start",
	}}}
}

func (unknownSendGateway) SendDraft(context.Context, mail.Draft) (mail.SendEvidence, error) {
	return mail.SendEvidence{InvocationStarted: true}, context.DeadlineExceeded
}

func (g *reconciledSendGateway) PrepareSend(context.Context, mail.Draft) (mail.SendObservationBaseline, error) {
	return mail.SendObservationBaseline{
		StoreUUID: "test-store", MaximumRowID: 1, CapturedUnix: 1, SentMailboxIDs: []int64{2},
	}, nil
}

func (g *reconciledSendGateway) SendDraft(context.Context, mail.Draft) (mail.SendEvidence, error) {
	g.sends++
	return mail.SendEvidence{InvocationStarted: true}, context.DeadlineExceeded
}

func (g *reconciledSendGateway) ReconcileSend(
	context.Context,
	mail.Draft,
	mail.SendAttempt,
) (mail.SendEvidence, error) {
	return mail.SendEvidence{
		InvocationStarted: true, AcceptedByMail: true, SentStoreObserved: true,
		ObservedMessageRef: "msg_reconciled",
	}, nil
}

func TestRunTable(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "help", args: []string{"help"}, wantCode: 0, wantStdout: "Usage:"},
		{name: "version", args: []string{"version"}, wantCode: 0, wantStdout: "mailcli 1.0.0"},
		{name: "unknown command", args: []string{"missing"}, wantCode: 2, wantStderr: `unknown command "missing"`},
		{name: "unknown flag", args: []string{"version", "--missing"}, wantCode: 2, wantStderr: `unknown flag "--missing"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			code := Run(context.Background(), newTestService(), test.args, &stdout, &stderr)

			if code != test.wantCode {
				t.Fatalf("Run() code = %d, want %d", code, test.wantCode)
			}
			if !strings.Contains(stdout.String(), test.wantStdout) {
				t.Errorf("stdout = %q, want substring %q", stdout.String(), test.wantStdout)
			}
			if !strings.Contains(stderr.String(), test.wantStderr) {
				t.Errorf("stderr = %q, want substring %q", stderr.String(), test.wantStderr)
			}
		})
	}
}

func TestVersionJSON(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := Run(context.Background(), newTestService(), []string{"version", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}

	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if !response.OK || response.SchemaVersion != schemaVersion {
		t.Fatalf("response = %+v", response)
	}
	if response.Data.Name != name || response.Data.Version != version {
		t.Fatalf("response data = %+v", response.Data)
	}
	if !strings.Contains(stdout.String(), `"error":null`) {
		t.Fatalf("stdout = %q, want stable null error field", stdout.String())
	}
}

func TestGlobalJSONFlagBeforeCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := Run(context.Background(), newTestService(), []string{"--json", "version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"command":"version"`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestJSONSyntaxFailuresUseEnvelopeTable(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantCommand string
		wantCode    string
	}{
		{name: "missing command", args: []string{"--json"}, wantCode: "invalid_argument"},
		{name: "unknown command", args: []string{"--json", "missing"}, wantCommand: "missing", wantCode: "unknown_command"},
		{
			name:        "leading global flag parse failure",
			args:        []string{"--json", "messages", "list", "--mailbox", "mbx_ref", "--limit", "nope"},
			wantCommand: "messages.list", wantCode: "invalid_argument",
		},
		{
			name:        "local flag parse failure",
			args:        []string{"messages", "list", "--mailbox", "mbx_ref", "--limit", "nope", "--json"},
			wantCommand: "messages.list", wantCode: "invalid_argument",
		},
		{
			name:        "doctor unknown flag",
			args:        []string{"doctor", "--json", "--missing"},
			wantCommand: "doctor", wantCode: "invalid_argument",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := Run(context.Background(), newTestService(), test.args, &stdout, &stderr)
			if code != 2 || stderr.Len() != 0 {
				t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
			}
			var response envelope
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatalf("json.Unmarshal() error = %v, stdout = %q", err, stdout.String())
			}
			if response.OK || response.Command != test.wantCommand || response.Error == nil ||
				response.Error.Code != test.wantCode {
				t.Fatalf("response = %+v", response)
			}
		})
	}
}

func TestDoctorJSONWithoutLiveProbe(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := Run(context.Background(), newTestService(), []string{"doctor", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}

	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if !response.OK {
		t.Fatalf("response = %+v", response)
	}
	if len(response.Data.Checks) != 6 {
		t.Fatalf("checks = %d, want 6", len(response.Data.Checks))
	}
	if response.Data.Checks[5].Status != "not-run" {
		t.Fatalf("mail automation status = %q, want not-run", response.Data.Checks[5].Status)
	}
}

func TestDoctorJSONPreservesTypedCapabilityFailure(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	service := mail.NewService(busyProbeGateway{})
	code := Run(context.Background(), service, []string{"doctor", "--live", "--json"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stdout.String(), `"code":"mail_busy"`) {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestReadCommandsJSONTable(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		command string
		dataKey string
	}{
		{name: "accounts", args: []string{"accounts", "list", "--json"}, command: "accounts.list", dataKey: `"accounts"`},
		{name: "mailboxes", args: []string{"mailboxes", "list", "--json"}, command: "mailboxes.list", dataKey: `"mailboxes"`},
		{name: "mailbox resolve", args: []string{"mailboxes", "resolve", "--account", "acct_ref", "--path", "Inbox", "--json"}, command: "mailboxes.resolve", dataKey: `"mailbox"`},
		{name: "message list", args: []string{"messages", "list", "--mailbox", "mbx_ref", "--json"}, command: "messages.list", dataKey: `"page":{"messages"`},
		{name: "message get", args: []string{"messages", "get", "--ref", "msg_ref", "--json"}, command: "messages.get", dataKey: `"message"`},
		{name: "message raw", args: []string{"messages", "raw", "--ref", "msg_ref", "--json"}, command: "messages.raw", dataKey: `"raw_source"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := Run(context.Background(), newTestService(), test.args, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), `"command":"`+test.command+`"`) || !strings.Contains(stdout.String(), test.dataKey) {
				t.Fatalf("stdout = %q", stdout.String())
			}
		})
	}
}

func TestDraftSaveAndOpenCommands(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(testGateway{}, filepath.Join(t.TempDir(), "drafts"))
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		From: "mail@example.com", To: []mail.Recipient{{Address: "recipient@example.com"}},
		Subject: "Saved", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runDraftSave(context.Background(), service, []string{"--ref", draft.Ref, "--json"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), `"command":"drafts.save"`) || !strings.Contains(stdout.String(), `"msg_saved"`) {
		t.Fatalf("save code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = runMailDraftOpen(context.Background(), service, []string{"--message", "msg_ref", "--json"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), `"command":"drafts.open"`) {
		t.Fatalf("open code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestDraftSendUnknownJSONPreservesOutcomeEvidence(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(unknownSendGateway{}, filepath.Join(t.TempDir(), "drafts"))
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		From:    "mail@example.com",
		To:      []mail.Recipient{{Address: "recipient@example.com"}},
		Subject: "Unknown", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runDraftSend(
		context.Background(), service,
		[]string{"--ref", draft.Ref, "--confirm", "--json"}, &stdout, &stderr,
	)
	if code != 1 || stderr.Len() != 0 ||
		!strings.Contains(stdout.String(), `"ok":false`) ||
		!strings.Contains(stdout.String(), `"code":"send_outcome_unknown"`) ||
		!strings.Contains(stdout.String(), `"outcome":"outcome_unknown"`) ||
		!strings.Contains(stdout.String(), `"draft_retained":true`) {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestDraftReconcileJSONNeverSendsAgain(t *testing.T) {
	gateway := &reconciledSendGateway{}
	service := mail.NewServiceWithDraftRoot(gateway, filepath.Join(t.TempDir(), "drafts"))
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		From: "mail@example.com", To: []mail.Recipient{{Address: "recipient@example.com"}},
		Subject: "Reconcile", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	if _, err := service.SendDraft(context.Background(), draft.Ref); err == nil {
		t.Fatal("SendDraft() error = nil, want unknown outcome")
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(
		context.Background(), service,
		[]string{"drafts", "reconcile", "--ref", draft.Ref, "--json"}, &stdout, &stderr,
	)
	if code != 0 || stderr.Len() != 0 || gateway.sends != 1 ||
		!strings.Contains(stdout.String(), `"command":"drafts.reconcile"`) ||
		!strings.Contains(stdout.String(), `"outcome":"sent_store_observed"`) ||
		!strings.Contains(stdout.String(), `"reconciled":true`) {
		t.Fatalf("code = %d, sends = %d, stdout = %q, stderr = %q", code, gateway.sends, stdout.String(), stderr.String())
	}
}

func TestRawHumanOutputIsUnchanged(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(
		context.Background(), newTestService(),
		[]string{"messages", "raw", "--ref", "msg_ref"}, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	if stdout.String() != "Raw source\r\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestJSONOperationFailure(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	service := mail.NewService(failingGateway{})
	code := Run(context.Background(), service, []string{"accounts", "list", "--json"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("Run() code = %d", code)
	}
	if !strings.Contains(stdout.String(), `"ok":false`) || !strings.Contains(stdout.String(), `"code":"operation_failed"`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestLimitValidationReturnsFailure(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(
		context.Background(), newTestService(),
		[]string{"messages", "list", "--mailbox", "mbx_ref", "--limit", "101", "--json"},
		&stdout, &stderr,
	)
	if code != 1 || !strings.Contains(stdout.String(), `"code":"invalid_argument"`) || !strings.Contains(stdout.String(), "limit must be between") {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}
