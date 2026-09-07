package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

const (
	syncProcessHelperEnv     = "MAILCLI_TEST_SYNC_PROCESS_HELPER"
	syncProcessHelperCaseEnv = "MAILCLI_TEST_SYNC_PROCESS_CASE"
	syncProcessHelperEnabled = "1"
)

func TestSyncHelpDocumentsRequireComplete(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), newTestService(), []string{"sync", "--help"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "--require-complete") ||
		!strings.Contains(stdout.String(), "exit 3") {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestSyncCheckProcessExitContract(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	tests := []struct {
		name              string
		scenario          string
		wantExit          int
		wantJSON          bool
		wantOK            bool
		wantComplete      bool
		wantSyncCheck     bool
		wantErrorCode     string
		wantOutputStrings []string
	}{
		{
			name: "complete required JSON", scenario: "complete_required_json", wantExit: 0,
			wantJSON: true, wantOK: true, wantComplete: true, wantSyncCheck: true,
		},
		{
			name: "partial default JSON", scenario: "partial_default_json", wantExit: 0,
			wantJSON: true, wantOK: true, wantSyncCheck: true,
		},
		{
			name: "partial required JSON", scenario: "partial_required_json", wantExit: syncCheckIncompleteExitCode,
			wantJSON: true, wantOK: true, wantSyncCheck: true,
		},
		{
			name: "all failed required human", scenario: "all_failed_required_human", wantExit: syncCheckIncompleteExitCode,
			wantOutputStrings: []string{"complete\tfalse", "failures", "imap_auth_failed"},
		},
		{
			name: "runtime error JSON", scenario: "runtime_error_json", wantExit: 1,
			wantJSON: true, wantErrorCode: "operation_failed",
		},
		{
			name: "require complete without check", scenario: "invalid_required_json", wantExit: 2,
			wantJSON: true, wantErrorCode: "invalid_argument",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command(executable, "-test.run=^TestSyncCheckProcessHelper$")
			command.Env = append(
				os.Environ(),
				syncProcessHelperEnv+"="+syncProcessHelperEnabled,
				syncProcessHelperCaseEnv+"="+test.scenario,
			)
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			runErr := command.Run()
			if command.ProcessState == nil {
				t.Fatalf("helper process did not start: %v", runErr)
			}
			if test.wantExit == 0 && runErr != nil {
				t.Fatalf("helper process failed: %v", runErr)
			}
			if exitCode := command.ProcessState.ExitCode(); exitCode != test.wantExit {
				t.Fatalf(
					"exit = %d, want %d; error = %v; stdout = %q; stderr = %q",
					exitCode, test.wantExit, runErr, stdout.String(), stderr.String(),
				)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			if test.wantJSON {
				var response envelope
				if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
					t.Fatalf("json.Unmarshal() error = %v; stdout = %q", err, stdout.String())
				}
				if response.OK != test.wantOK {
					t.Fatalf("ok = %t, want %t; response = %+v", response.OK, test.wantOK, response)
				}
				if test.wantSyncCheck {
					if response.Data.SyncCheck == nil || response.Data.SyncCheck.Complete != test.wantComplete {
						t.Fatalf("sync_check = %+v, want complete=%t", response.Data.SyncCheck, test.wantComplete)
					}
				}
				if test.wantErrorCode != "" && (response.Error == nil || response.Error.Code != test.wantErrorCode) {
					t.Fatalf("error = %+v, want code %q", response.Error, test.wantErrorCode)
				}
			}
			for _, expected := range test.wantOutputStrings {
				if !strings.Contains(stdout.String(), expected) {
					t.Fatalf("stdout = %q, want %q", stdout.String(), expected)
				}
			}
		})
	}
}

func TestSyncCheckProcessHelper(t *testing.T) {
	if os.Getenv(syncProcessHelperEnv) != syncProcessHelperEnabled {
		return
	}
	result := mail.SyncCheckResult{
		Complete: true,
		Mailboxes: []mail.MailboxDelta{{
			MailboxRef: "mbx_1", AccountRef: "acct_1", Name: "INBOX", Path: []string{"INBOX"},
			LocalMessages: 10, ServerMessages: 12, Delta: 2, Unseen: 1,
		}},
	}
	var gatewayErr error
	var args []string
	switch os.Getenv(syncProcessHelperCaseEnv) {
	case "complete_required_json":
		args = []string{"sync", "--check", "--require-complete", "--json"}
	case "partial_default_json":
		result.Complete = false
		result.Failures = syncProcessFailures()
		args = []string{"sync", "--check", "--json"}
	case "partial_required_json":
		result.Complete = false
		result.Failures = syncProcessFailures()
		args = []string{"sync", "--check", "--require-complete", "--json"}
	case "all_failed_required_human":
		result.Complete = false
		result.Mailboxes = nil
		result.Failures = syncProcessFailures()
		args = []string{"sync", "--check", "--require-complete"}
	case "runtime_error_json":
		gatewayErr = errors.New("sync gateway unavailable")
		args = []string{"sync", "--check", "--require-complete", "--json"}
	case "invalid_required_json":
		args = []string{"sync", "--require-complete", "--json"}
	default:
		os.Exit(125)
	}
	service := mail.NewService(syncCheckGateway{result: result, err: gatewayErr})
	os.Exit(Run(context.Background(), service, args, os.Stdout, os.Stderr))
}

func syncProcessFailures() []mail.SyncCheckFailure {
	return []mail.SyncCheckFailure{{
		Account: "acct@example.com", Mailbox: "INBOX",
		Code: "imap_auth_failed", Message: "IMAP LOGIN failed",
	}}
}
