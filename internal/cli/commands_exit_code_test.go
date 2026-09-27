package cli

import (
	"bytes"
	"errors"
	"io/fs"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

func TestCommandExitCodeRequiresProvenCallerInputAndSafeRecovery(t *testing.T) {
	tests := []struct {
		name              string
		command           string
		err               error
		partialEffects    bool
		wantCode          int
		wantCorrection    bool
		wantPartialEffect bool
	}{
		{name: "invalid argument", command: "capabilities", err: &commandError{code: "invalid_argument", message: "bad selector"}, wantCode: 2},
		{name: "invalid structured input", command: "drafts.create", err: &commandError{code: "invalid_input", message: "bad input"}, wantCode: 2},
		{name: "missing required input", command: "messages.get", err: &commandError{code: "missing_required", message: "missing ref"}, wantCode: 2},
		{name: "unknown command", command: "messages.get", err: &commandError{code: "unknown_command", message: "unknown command"}, wantCode: 2},
		{name: "invalid caller reference", command: "messages.list", err: &mail.ValidationError{Code: "invalid_reference", Message: "invalid mailbox ref"}, wantCode: 2, wantCorrection: true},
		{name: "invalid caller cursor", command: "drafts.list", err: &mail.ValidationError{Code: "invalid_cursor", Message: "invalid draft cursor"}, wantCode: 2, wantCorrection: true},
		{name: "internal reference failure", command: "messages.list", err: &mail.OperationError{Code: "invalid_reference", Message: "Mail returned an invalid ref"}, wantCode: 1, wantCorrection: true},
		{name: "pagination state changed", command: "drafts.list", err: &mail.OperationError{Code: "invalid_cursor", Message: "draft directory changed"}, wantCode: 1, wantCorrection: true},
		{name: "invalid local message source", command: "messages.get", err: &mail.OperationError{Code: "invalid_emlx", Message: "malformed source"}, wantCode: 1},
		{name: "invalid mailbox cache", command: "messages.list", err: &mail.OperationError{Code: "invalid_mailbox_cache", Message: "malformed cache"}, wantCode: 1},
		{name: "configuration needs correction", command: "accounts.list", err: &mail.OperationError{Code: "account_binding_unavailable", Message: "binding unavailable"}, wantCode: 1, wantCorrection: true},
		{name: "permission needs correction", command: "messages.get", err: fs.ErrPermission, wantCode: 1, wantCorrection: true},
		{name: "transport address needs correction", command: "drafts.send", err: &transport.TransportError{Code: transport.CodeInvalidAddress, Message: "invalid configured sender"}, wantCode: 1, wantCorrection: true},
		{name: "transport timeout", command: "messages.get", err: &transport.TransportError{Code: transport.CodeIMAPTimeout, Message: "read timed out"}, wantCode: 1},
		{name: "missing destructive confirmation", command: "messages.delete", err: &commandError{code: "confirmation_required", message: "--confirm is required"}, wantCode: 1, wantCorrection: true},
		{name: "internal capability schema error", command: "capabilities", err: &commandError{code: "capability_schema_invalid", message: "invalid internal schema"}, wantCode: 1},
		{
			name: "partial mutation retains operational exit", command: "messages.copy",
			err: &transport.MutationOutcomeError{
				Code: "invalid_reference", Message: "copy stopped after a verified effect",
				Evidence: transport.MutationEvidence{
					Command: "COPY", Outcome: transport.MutationOutcomePartial,
					CompletedEffects: []string{"copy"},
				},
				Err: &mail.ValidationError{Code: "invalid_reference", Message: "invalid source ref"},
			},
			wantCode: 1, wantPartialEffect: true,
		},
		{
			name: "response records a partial effect", command: "send.setup",
			err:            &commandError{code: "invalid_argument", message: "binding invalid"},
			partialEffects: true, wantCode: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			guidance := mail.GuidanceForError(test.command, test.err)
			if test.wantCorrection && (guidance.Retryability != mail.RetryUserInputRequired || guidance.Recovery.Action != mail.RecoveryCorrect) {
				t.Fatalf("guidance = %+v, want correction guidance", guidance)
			}
			if test.wantPartialEffect && guidance.EffectCertainty != mail.EffectPartial {
				t.Fatalf("guidance = %+v, want partial-effect evidence", guidance)
			}
			if got := commandExitCodeFor(test.command, test.err, test.partialEffects); got != test.wantCode {
				t.Fatalf("commandExitCodeFor() = %d, want %d; guidance = %+v", got, test.wantCode, guidance)
			}
		})
	}

	if got := commandExitCode(errors.New("untyped failure")); got != 1 {
		t.Fatalf("untyped failure exit = %d, want 1", got)
	}

	var stdout, stderr bytes.Buffer
	code := failCommandWithData(
		"send.setup", true,
		responseData{PartialEffects: []sendSetupPartialEffect{{Step: "keychain_store", Status: "complete"}}},
		&commandError{code: "invalid_argument", message: "binding publication failed"}, &stdout, &stderr,
	)
	if code != 1 || stdout.Len() == 0 || stderr.Len() != 0 {
		t.Fatalf("partial-effect command exit = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = failProjectedDraft(
		"drafts.create", false, mail.Draft{},
		outputOptions{draftMutationCompleted: true},
		&commandError{code: "invalid_argument", message: "export failed"}, &stdout, &stderr,
	)
	if code != 1 {
		t.Fatalf("completed-draft export failure exit = %d, want 1", code)
	}
}
