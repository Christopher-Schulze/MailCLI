package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

type cliSubmitter struct{ calls int }

func (s *cliSubmitter) Submit(
	context.Context,
	transport.SubmitConfig,
	string,
	[]string,
	[]byte,
) (transport.SubmitEvidence, error) {
	s.calls++
	return transport.SubmitEvidence{ServerResponse: "250 2.0.0 OK", MessageID: "<abc123@icloud.com>"}, nil
}

type cliMirror struct {
	calls int
	err   error
}

func (m *cliMirror) AppendToSent(
	context.Context,
	transport.ImapConfig,
	[]byte,
	string,
) (transport.AppendEvidence, error) {
	m.calls++
	if m.err != nil {
		return transport.AppendEvidence{}, m.err
	}
	return transport.AppendEvidence{Mailbox: "Sent", Appended: true}, nil
}

type cliCredentials struct{}

func (cliCredentials) Load(string) (string, error) { return "secret", nil }
func (cliCredentials) Store(string, string) error  { return nil }
func (cliCredentials) Delete(string) error         { return nil }

func mirrorFailure() *cliMirror {
	return &cliMirror{err: &transport.TransportError{
		Code: transport.CodeIMAPAppendFailed, Message: "NO mailbox",
	}}
}

func newTransportTestService(root string, mirror *cliMirror) *mail.Service {
	return mail.NewServiceWithTransport(nil, root, mail.SendTransport{
		Submitter: &cliSubmitter{}, Mirror: mirror, Credentials: cliCredentials{},
	})
}

type testGateway struct{}

type failingGateway struct {
	testGateway
}

type busyProbeGateway struct {
	testGateway
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

type shortWriter struct{}

func (shortWriter) Write(payload []byte) (int, error) {
	return max(0, len(payload)-1), nil
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

func (testGateway) MarkMessage(_ context.Context, request mail.MarkMessageRequest) (mail.MessageSummary, error) {
	return mail.MessageSummary{Ref: request.Ref, Read: request.Read != nil && *request.Read,
		ServerTruth: &mail.ServerMutationEvidence{Command: "STORE", ServerResponse: "OK STORE completed", UID: 1}}, nil
}

func (testGateway) TransferMessage(_ context.Context, request mail.TransferMessageRequest) (mail.MessageSummary, error) {
	return mail.MessageSummary{Ref: request.Ref, MailboxRef: request.DestinationMailbox,
		ServerTruth: &mail.ServerMutationEvidence{Command: "MOVE", ServerResponse: "OK MOVE completed", UID: 1}}, nil
}

func (testGateway) DeleteMessage(context.Context, mail.DeleteMessageRequest) (mail.DeleteResult, error) {
	return mail.DeleteResult{Deleted: true,
		ServerTruth: &mail.ServerMutationEvidence{Command: "DELETE", ServerResponse: "OK DELETE completed", UID: 1}}, nil
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

func TestRunTable(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "help", args: []string{"help"}, wantCode: 0, wantStdout: "Usage:"},
		{name: "version", args: []string{"version"}, wantCode: 0, wantStdout: "mailcli " + version},
		{name: "update help", args: []string{"update", "--help"}, wantCode: 0, wantStdout: "mailcli update [options]"},
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

func TestDoctorDiagnosticsExposeTimingsWithoutPrivateData(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(
		context.Background(), newTestService(), []string{"doctor", "--diagnostics", "--json"},
		&stdout, &stderr,
	)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), `"timings":[{"phase":"probe"`) {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	for _, forbidden := range []string{"@example.com", "/Users/", "Subject", "Body"} {
		if strings.Contains(stdout.String(), forbidden) {
			t.Fatalf("diagnostics leaked %q: %s", forbidden, stdout.String())
		}
	}
}

func TestUnknownCommandDoesNotDumpHelp(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), newTestService(), []string{"missing"}, &stdout, &stderr)
	if code != 2 || stdout.Len() != 0 || strings.Contains(stderr.String(), "Commands:") {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestRunFailsWhenCommandOutputCannotBeWritten(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		stdout io.Writer
		stderr io.Writer
	}{
		{name: "human stdout", args: []string{"version"}, stdout: failingWriter{}, stderr: io.Discard},
		{name: "JSON stdout", args: []string{"version", "--json"}, stdout: failingWriter{}, stderr: io.Discard},
		{name: "short stdout", args: []string{"version"}, stdout: shortWriter{}, stderr: io.Discard},
		{name: "stderr", args: []string{"missing"}, stdout: io.Discard, stderr: failingWriter{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if code := Run(
				context.Background(), newTestService(), test.args, test.stdout, test.stderr,
			); code != 1 {
				t.Fatalf("Run() code = %d, want 1", code)
			}
		})
	}
}

func TestWriteMessageFailsOnWriteError(t *testing.T) {
	// writeMessage must return an error when stdout fails, causing exit 1.
	// Use messages get which calls writeMessage in human mode.
	if code := Run(
		context.Background(), newTestService(),
		[]string{"messages", "get", "--ref", "msg_1"},
		failingWriter{}, io.Discard,
	); code != 1 {
		t.Fatalf("Run() code = %d, want 1 for messages get with failing stdout", code)
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

func TestDraftSendWithoutTransportIsJSONFailure(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(nil, filepath.Join(t.TempDir(), "drafts"))
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		From: "sender@icloud.com", To: []mail.Recipient{{Address: "recipient@example.com"}},
		Subject: "Send", Body: "Body",
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
		!strings.Contains(stdout.String(), `"code":"send_transport_unavailable"`) {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestDraftSendMirrorPendingJSONPreservesOutcomeEvidence(t *testing.T) {
	service := newTransportTestService(filepath.Join(t.TempDir(), "drafts"), mirrorFailure())
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		From: "sender@icloud.com", To: []mail.Recipient{{Address: "recipient@example.com"}},
		Subject: "Mirror pending", Body: "Body",
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
		!strings.Contains(stdout.String(), `"code":"imap_append_failed"`) ||
		!strings.Contains(stdout.String(), `"outcome":"sent_mirror_pending"`) ||
		!strings.Contains(stdout.String(), `"draft_retained":true`) ||
		!strings.Contains(stdout.String(), `"attempt_id":"`) {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestDraftReconcileJSONReplaysWithoutSendingAgain(t *testing.T) {
	service := newTransportTestService(filepath.Join(t.TempDir(), "drafts"), mirrorFailure())
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		From: "sender@icloud.com", To: []mail.Recipient{{Address: "recipient@example.com"}},
		Subject: "Reconcile", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	if _, err := service.SendDraft(context.Background(), draft.Ref); err == nil {
		t.Fatal("SendDraft() error = nil, want mirror pending")
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(
		context.Background(), service,
		[]string{"drafts", "reconcile", "--ref", draft.Ref, "--json"}, &stdout, &stderr,
	)
	if code != 1 ||
		!strings.Contains(stdout.String(), `"command":"drafts.reconcile"`) ||
		!strings.Contains(stdout.String(), `"outcome":"sent_mirror_pending"`) ||
		!strings.Contains(stdout.String(), `"reconciled":true`) {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
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
	if code != 2 || !strings.Contains(stdout.String(), `"code":"invalid_argument"`) || !strings.Contains(stdout.String(), "limit must be between") {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

type syncCheckGateway struct {
	testGateway
	result mail.SyncCheckResult
	err    error
}

func (g syncCheckGateway) SyncCheck(context.Context, string) (mail.SyncCheckResult, error) {
	return g.result, g.err
}

func TestSyncCheckJSON(t *testing.T) {
	gw := syncCheckGateway{
		result: mail.SyncCheckResult{
			Mailboxes: []mail.MailboxDelta{
				{
					MailboxRef:     "mbx_1",
					AccountRef:     "acct_1",
					Name:           "INBOX",
					Path:           []string{"INBOX"},
					LocalMessages:  10,
					ServerMessages: 15,
					Delta:          5,
					Unseen:         2,
				},
			},
		},
	}
	service := mail.NewService(gw)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), service, []string{"sync", "--check", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run() code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"sync_check"`) || !strings.Contains(stdout.String(), `"delta":5`) {
		t.Fatalf("unexpected stdout: %s", stdout.String())
	}
}

func TestRequiredFlagValidation(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{name: "messages get missing ref", args: []string{"messages", "get", "--json"}, wantCode: 2, wantErr: "missing required --ref"},
		{name: "messages raw missing ref", args: []string{"messages", "raw", "--json"}, wantCode: 2, wantErr: "missing required --ref"},
		{name: "mailboxes resolve missing account", args: []string{"mailboxes", "resolve", "--path", "INBOX", "--json"}, wantCode: 2, wantErr: "missing required --account"},
		{name: "mailboxes resolve missing path", args: []string{"mailboxes", "resolve", "--account", "acct_1", "--json"}, wantCode: 2, wantErr: "missing required --path"},
		{name: "drafts inspect missing ref", args: []string{"drafts", "inspect", "--json"}, wantCode: 2, wantErr: "missing required --ref"},
		{name: "drafts update missing ref", args: []string{"drafts", "update", "--body", "x", "--json"}, wantCode: 2, wantErr: "missing required --ref"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := Run(context.Background(), newTestService(), test.args, &stdout, &stderr)
			if code != test.wantCode {
				t.Fatalf("Run() code = %d, want %d, stdout = %s, stderr = %s",
					code, test.wantCode, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), test.wantErr) {
				t.Fatalf("stdout = %s; want error containing %q", stdout.String(), test.wantErr)
			}
		})
	}
}

func TestEmptyStdinDetection(t *testing.T) {
	root := t.TempDir()
	service := newTransportTestService(root, &cliMirror{})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	// Pipe empty stdin by setting os.Stdin to /dev/null-like.
	oldStdin := os.Stdin
	defer func() { os.Stdin = oldStdin }()
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer func() { _ = devNull.Close() }()
	os.Stdin = devNull
	code := Run(context.Background(), service, []string{"drafts", "create", "--input", "-", "--json"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("Run() code = %d, want 2, stdout = %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "no input received on stdin") {
		t.Fatalf("stdout = %s; want error about empty stdin", stdout.String())
	}
}

func TestInvalidBodyFormatValidation(t *testing.T) {
	root := t.TempDir()
	service := newTransportTestService(root, &cliMirror{})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), service,
		[]string{"drafts", "create", "--body", "x", "--format", "bogus", "--json"},
		&stdout, &stderr)
	if code != 2 {
		t.Fatalf("Run() code = %d, want 2, stdout = %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "invalid body format") {
		t.Fatalf("stdout = %s; want error about invalid format", stdout.String())
	}
}

func TestRuntimeErrorClassifiedAsOperationFailed(t *testing.T) {
	service := mail.NewService(failingGateway{})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), service, []string{"accounts", "list", "--json"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("Run() code = %d, want 1, stdout = %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"code":"operation_failed"`) {
		t.Fatalf("stdout = %s; want operation_failed error code", stdout.String())
	}
}
