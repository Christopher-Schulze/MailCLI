package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/mailstore"
	"mailcli/internal/transport"
)

func TestJSONFailureContainsFiniteValidationGuidance(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), newTestService(), []string{"messages", "get", "--json"}, &stdout, &stderr)
	if code != 2 || stderr.Len() != 0 {
		t.Fatalf("Run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if response.Error == nil || response.Error.Guidance == nil {
		t.Fatalf("response error guidance = %+v", response.Error)
	}
	guidance := response.Error.Guidance
	if guidance.Phase != mail.OperationPhaseValidation || guidance.EffectCertainty != mail.EffectNone ||
		guidance.Retryability != mail.RetryUserInputRequired || guidance.ReplayAllowed ||
		guidance.Recovery.Action != mail.RecoveryCorrect {
		t.Fatalf("validation guidance = %+v", guidance)
	}
}

func TestSendFailureGuidanceRequiresReconciliation(t *testing.T) {
	result := mail.SendResult{
		DraftRef: "draft_ref", AttemptID: "send_attempt", Outcome: mail.SendOutcomeUnknown,
		InvocationStarted: true, DraftRetained: true,
	}
	guidance := guidanceForResponse("drafts.send", responseData{SendResult: &result}, &transport.SubmissionError{Stage: "final reply"})
	if guidance.Phase != mail.OperationPhaseSubmission || guidance.EffectCertainty != mail.EffectUnknown ||
		guidance.Retryability != mail.RetryObserveRequired || guidance.ReplayAllowed {
		t.Fatalf("submission guidance = %+v", guidance)
	}
	if guidance.Recovery.Action != mail.RecoveryReconcile || guidance.Recovery.Command != "drafts.reconcile" ||
		guidance.Recovery.OperationID != "send_attempt" {
		t.Fatalf("submission recovery = %+v", guidance.Recovery)
	}
	wantArgs := []string{"--ref", "draft_ref", "--json"}
	if !equalStrings(guidance.Recovery.Args, wantArgs) {
		t.Fatalf("recovery args = %v, want %v", guidance.Recovery.Args, wantArgs)
	}
}

func TestDraftInspectionRecoveryBuildersUseConsistentContract(t *testing.T) {
	ref := "draft_abcdefghijklmnopqrstuvwx"
	draft := mail.Draft{Ref: ref}
	fullArgs := []string{"--ref", ref, "--view", "full", "--json"}
	plainArgs := []string{"--ref", ref, "--json"}
	tests := []struct {
		name        string
		command     string
		data        responseData
		err         error
		wantAction  mail.RecoveryAction
		wantCommand string
		wantArgs    []string
		wantID      string
	}{
		{
			name: "revision conflict", command: "drafts.update",
			err:        &mail.DraftRevisionConflict{Ref: ref, ExpectedRevision: "expected", CurrentRevision: "current"},
			wantAction: mail.RecoveryInspect, wantCommand: "drafts.inspect", wantArgs: fullArgs,
		},
		{
			name: "editor failure", command: "drafts.edit",
			err:        &draftEditorError{cause: errors.New("editor failed"), evidence: draftEditorEvidence{Ref: ref}, updateAttempted: true},
			wantAction: mail.RecoveryInspect, wantCommand: "drafts.inspect", wantArgs: fullArgs,
		},
		{
			name: "unclaimed send spool", command: "drafts.send",
			err: &mail.OperationError{
				Code: "send_recovery_spool_changed", Message: "spool changed", DraftRef: ref,
				UnclaimedSpool: &mail.UnclaimedSpoolObservation{Path: "/tmp/spool", ObjectType: "regular", OwnerUID: 501, Mode: "0600"},
			},
			wantAction: mail.RecoveryInspect, wantCommand: "drafts.inspect", wantArgs: plainArgs,
		},
		{
			name: "invalid SMTP source", command: "drafts.send", data: responseData{draftRef: ref},
			err:        &transport.TransportError{Code: transport.CodeSMTPSourceInvalid, Message: "invalid source"},
			wantAction: mail.RecoveryInspect, wantCommand: "drafts.inspect", wantArgs: plainArgs,
		},
		{
			name: "completed draft mutation", command: "drafts.update",
			data:       responseData{Draft: &draft, draftMutationCompleted: true},
			err:        &outputTooLargeError{target: "draft", completedDraftRef: ref},
			wantAction: mail.RecoveryInspect, wantCommand: "drafts.inspect", wantArgs: fullArgs,
		},
		{
			name: "busy draft with ref", command: "drafts.send",
			err:        &mail.OperationError{Code: "draft_busy", Message: "busy", DraftRef: ref},
			wantAction: mail.RecoveryInspect, wantCommand: "drafts.inspect", wantArgs: plainArgs,
		},
		{
			name: "completed send", command: "drafts.send",
			data:       responseData{SendResult: &mail.SendResult{DraftRef: ref, AttemptID: "send_attempt", Outcome: mail.SendOutcomeSent}},
			err:        &testCodedError{code: "send_cleanup_failed", message: "cleanup failed"},
			wantAction: mail.RecoveryInspect, wantCommand: "drafts.inspect", wantArgs: plainArgs, wantID: "send_attempt",
		},
		{
			name: "revision conflict without ref", command: "drafts.update",
			err:        &mail.DraftRevisionConflict{ExpectedRevision: "expected", CurrentRevision: "current"},
			wantAction: mail.RecoveryInspect,
		},
		{
			name: "busy draft without ref", command: "drafts.send",
			err:        &mail.OperationError{Code: "draft_busy", Message: "busy"},
			wantAction: mail.RecoveryObserve,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := newErrorData(test.command, test.data, test.err)
			recovery := result.Guidance.Recovery
			if recovery.Action != test.wantAction || recovery.Command != test.wantCommand ||
				!equalStrings(recovery.Args, test.wantArgs) || recovery.OperationID != test.wantID {
				t.Fatalf("recovery = %+v, want action=%q command=%q args=%v operation_id=%q",
					recovery, test.wantAction, test.wantCommand, test.wantArgs, test.wantID)
			}
			if recovery.Command == "drafts.inspect" &&
				(len(recovery.Args) < 3 || recovery.Args[0] != "--ref" || recovery.Args[1] == "" || recovery.Args[len(recovery.Args)-1] != "--json") {
				t.Fatalf("inspect recovery is missing its ref or JSON output: %+v", recovery)
			}
		})
	}
}

type sourceInvalidSubmitter struct {
	calls int
}

func (s *sourceInvalidSubmitter) Submit(context.Context, transport.SubmitConfig, string, []string, []byte) (transport.SubmitEvidence, error) {
	s.calls++
	return transport.SubmitEvidence{}, &transport.TransportError{
		Code: transport.CodeSMTPSourceInvalid, Message: "message source is shorter than declared",
	}
}

func TestDraftSendSourceInvalidGuidanceInspectsRetainedDraft(t *testing.T) {
	submitter := &sourceInvalidSubmitter{}
	mirror := &cliMirror{}
	service := mail.NewServiceWithTransport(nil, filepath.Join(t.TempDir(), "drafts"), mail.SendTransport{
		Submitter: submitter, Mirror: mirror, Credentials: cliCredentials{},
	})
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		From: "sender@icloud.com", To: []mail.Recipient{{Address: "recipient@example.com"}},
		Subject: "Source integrity", Body: "Body",
	}})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runDraftSend(context.Background(), service,
		[]string{"--ref", draft.Ref, "--expected-revision", draft.Revision, "--confirm", "--json"}, &stdout, &stderr)
	if code != 1 || stderr.Len() != 0 || submitter.calls != 1 || mirror.calls != 0 {
		t.Fatalf("send code=%d submit=%d mirror=%d stdout=%q stderr=%q", code, submitter.calls, mirror.calls, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error == nil || response.Error.Code != transport.CodeSMTPSourceInvalid || response.Error.Guidance == nil || response.Data.SendResult != nil {
		t.Fatalf("source-invalid response = %+v", response)
	}
	guidance := response.Error.Guidance
	if guidance.Phase != mail.OperationPhaseSubmission || guidance.EffectCertainty != mail.EffectNone ||
		guidance.Retryability != mail.RetryObserveRequired || guidance.ReplayAllowed ||
		guidance.Recovery.Action != mail.RecoveryInspect || guidance.Recovery.Command != "drafts.inspect" ||
		guidance.Recovery.OperationID != "" || !equalStrings(guidance.Recovery.Args, []string{"--ref", draft.Ref, "--json"}) {
		t.Fatalf("source-invalid guidance = %+v", guidance)
	}
	if strings.Contains(stdout.String(), `"draftRef"`) || strings.Contains(stdout.String(), `"attempt_id"`) {
		t.Fatalf("invented source-invalid evidence: %s", stdout.String())
	}
	stdout.Reset()
	args := append([]string{"drafts", "inspect"}, guidance.Recovery.Args...)
	if code := Run(context.Background(), service, args, &stdout, &stderr); code != 0 {
		t.Fatalf("emitted inspection command code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Draft == nil || response.Data.Draft.Ref != draft.Ref || response.Data.Draft.SendAttempt != nil {
		t.Fatalf("retained draft inspection = %+v", response.Data.Draft)
	}
}

func TestDraftSendForeignSpoolGuidanceCarriesNoFollowEvidence(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter := &cliSubmitter{}
	service := mail.NewServiceWithTransport(nil, root, mail.SendTransport{
		Submitter: submitter, Mirror: &cliMirror{}, Credentials: cliCredentials{},
	})
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		From: "sender@icloud.com", To: []mail.Recipient{{Address: "recipient@example.com"}},
		Subject: "Foreign spool", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	foreignPath := filepath.Join(t.TempDir(), "foreign-spool")
	foreignBytes := []byte("preserve this target")
	if err := os.WriteFile(foreignPath, foreignBytes, 0o600); err != nil {
		t.Fatalf("WriteFile(foreign spool) error = %v", err)
	}
	spoolPath := filepath.Join(root, draft.Ref+".send-spool")
	if err := os.Symlink(foreignPath, spoolPath); err != nil {
		t.Fatalf("Symlink(spool) error = %v", err)
	}
	linkBefore, err := os.Lstat(spoolPath)
	if err != nil {
		t.Fatalf("Lstat(spool before send) error = %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runDraftSend(context.Background(), service,
		[]string{"--ref", draft.Ref, "--expected-revision", draft.Revision, "--confirm", "--json"},
		&stdout, &stderr,
	)
	if code != 1 || stderr.Len() != 0 || submitter.calls != 0 {
		t.Fatalf("send code=%d SMTP calls=%d stdout=%q stderr=%q", code, submitter.calls, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if response.OK || response.Error == nil || response.Error.Code != "send_recovery_spool_changed" ||
		response.Error.UnclaimedSpool == nil || response.Error.Guidance == nil {
		t.Fatalf("foreign-spool response = %+v", response)
	}
	observation := response.Error.UnclaimedSpool
	if observation.Path != spoolPath || !filepath.IsAbs(observation.Path) ||
		observation.ObjectType != "symlink" || observation.OwnerUID != uint32(os.Geteuid()) ||
		observation.Mode != fmt.Sprintf("%04o", linkBefore.Mode().Perm()) {
		t.Fatalf("unclaimed spool evidence = %+v", observation)
	}
	guidance := response.Error.Guidance
	if guidance.EffectCertainty != mail.EffectNone || guidance.Retryability != mail.RetryUserInputRequired ||
		guidance.ReplayAllowed || guidance.Recovery.Action != mail.RecoveryInspect ||
		guidance.Recovery.Command != "drafts.inspect" ||
		!equalStrings(guidance.Recovery.Args, []string{"--ref", draft.Ref, "--json"}) {
		t.Fatalf("foreign-spool guidance = %+v", guidance)
	}
	instruction := strings.ToLower(guidance.Recovery.Instruction)
	for _, required := range []string{"smtp was not contacted", "lock is free", "send claim", "symlink", "never its target"} {
		if !strings.Contains(instruction, required) {
			t.Errorf("recovery instruction %q omits %q", guidance.Recovery.Instruction, required)
		}
	}
	linkAfter, err := os.Lstat(spoolPath)
	if err != nil || !os.SameFile(linkBefore, linkAfter) {
		t.Fatalf("send changed foreign spool link identity: info=%v error=%v", linkAfter, err)
	}
	linkTarget, err := os.Readlink(spoolPath)
	if err != nil || linkTarget != foreignPath {
		t.Fatalf("send changed foreign spool link target = %q, error = %v", linkTarget, err)
	}
	after, err := os.ReadFile(foreignPath)
	if err != nil || !bytes.Equal(after, foreignBytes) {
		t.Fatalf("send changed foreign target bytes = %q, error = %v", after, err)
	}
}

func TestDraftReconcileForeignAcceptedSpoolKeepsReconciliationGuidance(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := newTransportTestService(root, mirrorFailure())
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		From: "sender@icloud.com", To: []mail.Recipient{{Address: "recipient@example.com"}},
		Subject: "Accepted spool", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	first, err := service.SendDraft(context.Background(), mail.SendDraftRequest{
		Ref: draft.Ref, ExpectedRevision: draft.Revision,
	})
	if errorCode(err) != transport.CodeIMAPAppendFailed || first.AttemptID == "" ||
		first.Outcome != mail.SendOutcomeMirrorPending || !first.DraftRetained {
		t.Fatalf("SendDraft() = %+v, error = %v, want retained accepted mirror", first, err)
	}
	spoolPath := filepath.Join(root, draft.Ref+".send-spool")
	foreignPath := filepath.Join(t.TempDir(), "accepted-spool-target")
	foreignBytes := []byte("accepted bytes stay untouched")
	if err := os.WriteFile(foreignPath, foreignBytes, 0o600); err != nil {
		t.Fatalf("WriteFile(foreign target) error = %v", err)
	}
	if err := os.Remove(spoolPath); err != nil {
		t.Fatalf("Remove(accepted spool) error = %v", err)
	}
	if err := os.Symlink(foreignPath, spoolPath); err != nil {
		t.Fatalf("Symlink(accepted spool) error = %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runDraftReconcile(context.Background(), service, []string{"--ref", draft.Ref, "--json"}, &stdout, &stderr)
	if code != 1 || stderr.Len() != 0 {
		t.Fatalf("reconcile code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if response.Error == nil || response.Error.Code != "send_recovery_spool_changed" ||
		response.Error.UnclaimedSpool != nil || response.Error.Guidance == nil || response.Data.SendResult == nil {
		t.Fatalf("accepted-spool response = %+v", response)
	}
	guidance := response.Error.Guidance
	if guidance.Phase != mail.OperationPhaseMirror || guidance.EffectCertainty != mail.EffectPartial ||
		guidance.Retryability != mail.RetryObserveRequired || guidance.ReplayAllowed ||
		guidance.Recovery.Action != mail.RecoveryReconcile || guidance.Recovery.Command != "drafts.reconcile" ||
		guidance.Recovery.OperationID != first.AttemptID ||
		!equalStrings(guidance.Recovery.Args, []string{"--ref", draft.Ref, "--json"}) {
		t.Fatalf("accepted-spool guidance = %+v", guidance)
	}
	instruction := strings.ToLower(guidance.Recovery.Instruction)
	if strings.Contains(instruction, "remove") || strings.Contains(instruction, "delete") || strings.Contains(instruction, "unlink") {
		t.Fatalf("accepted-spool guidance recommends deletion: %q", guidance.Recovery.Instruction)
	}
	linkTarget, err := os.Readlink(spoolPath)
	if err != nil || linkTarget != foreignPath {
		t.Fatalf("reconcile changed accepted spool link = %q, error = %v", linkTarget, err)
	}
	after, err := os.ReadFile(foreignPath)
	if err != nil || !bytes.Equal(after, foreignBytes) {
		t.Fatalf("reconcile changed accepted target bytes = %q, error = %v", after, err)
	}
}

func TestHandoffUnknownGuidanceKeepsRecoveryWithoutAttachments(t *testing.T) {
	result := draftHandoffResult{
		DraftRef: "draft_ref", AttemptID: "handoff_123456789012345678901234", Outcome: draftHandoffUnknown,
		DispatchStarted: true, DraftRetained: true,
	}
	guidance := guidanceForResponse("drafts.handoff", responseData{DraftHandoff: &result}, &testCodedError{code: "handoff_outcome_unknown", message: "unknown"})
	if guidance.Recovery.Action != mail.RecoveryReconcile || guidance.Recovery.Command != "drafts.handoff-reconcile" ||
		guidance.Recovery.OperationID != result.AttemptID || !equalStrings(guidance.Recovery.Args, []string{"--ref", result.DraftRef, "--attempt", result.AttemptID, "--confirm", "--json"}) {
		t.Fatalf("handoff recovery = %+v", guidance.Recovery)
	}
}

func TestHandoffConfirmedFailureDoesNotInventReconciliation(t *testing.T) {
	result := draftHandoffResult{
		DraftRef: "draft_ref", AttemptID: "handoff_123456789012345678901234", Outcome: draftHandoffNotHandedOff,
		DispatchStarted: true, DraftRetained: true,
	}
	guidance := guidanceForResponse("drafts.handoff", responseData{DraftHandoff: &result}, &testCodedError{code: "handoff_failed", message: "native failure"})
	if guidance.Recovery.Action == mail.RecoveryReconcile || guidance.Recovery.Command != "" || guidance.Recovery.OperationID != "" {
		t.Fatalf("confirmed handoff failure guidance = %+v, must not require reconciliation without retained evidence", guidance)
	}
}

func TestMirrorPendingGuidanceRequiresReconciliation(t *testing.T) {
	result := mail.SendResult{
		DraftRef: "draft_ref", AttemptID: "send_attempt", Outcome: mail.SendOutcomeMirrorPending,
		SubmissionAccepted: true, DraftRetained: true,
	}
	guidance := guidanceForResponse("drafts.send", responseData{SendResult: &result}, &transport.TransportError{
		Code: transport.CodeIMAPAppendFailed, Message: "NO mailbox",
	})
	if guidance.Phase != mail.OperationPhaseMirror || guidance.EffectCertainty != mail.EffectPartial ||
		guidance.Retryability != mail.RetryObserveRequired || guidance.ReplayAllowed {
		t.Fatalf("mirror guidance = %+v", guidance)
	}
}

func TestMutationUnknownGuidanceRetainsOperationIdentity(t *testing.T) {
	err := &transport.MutationOutcomeError{
		Code:     transport.CodeIMAPCopyOutcomeUnknown,
		Message:  "COPY response lost",
		Evidence: transport.MutationEvidence{OperationID: "copy_abc", Outcome: transport.MutationOutcomeUnknown},
	}
	guidance := guidanceForResponse("messages.copy", responseData{}, err)
	if guidance.Phase != mail.OperationPhaseMutation || guidance.EffectCertainty != mail.EffectUnknown ||
		guidance.Retryability != mail.RetryObserveRequired || guidance.ReplayAllowed ||
		guidance.Recovery.Action != mail.RecoveryObserve || guidance.Recovery.OperationID != "copy_abc" {
		t.Fatalf("mutation guidance = %+v", guidance)
	}
}

func TestPartialHydrationGuidanceUsesSupportedCommandArguments(t *testing.T) {
	message := failedHydrationMessage()
	err := &testCodedError{code: "imap_auth_failed", message: "authentication failed"}
	guidance := guidanceForResponse("messages.get", responseData{Message: &message}, err)
	if guidance.Phase != mail.OperationPhaseHydration || guidance.EffectCertainty != mail.EffectNone ||
		guidance.Retryability != mail.RetryUserInputRequired || guidance.ReplayAllowed {
		t.Fatalf("hydration guidance = %+v", guidance)
	}
	if guidance.Recovery.Command != "messages.get" || !equalStrings(guidance.Recovery.Args, []string{"--ref", "msg_ref", "--json"}) {
		t.Fatalf("messages.get recovery = %+v", guidance.Recovery)
	}
	guidance = guidanceForResponse("drafts.open", responseData{Message: &message}, err)
	if guidance.Recovery.Command != "drafts.open" || !equalStrings(guidance.Recovery.Args, []string{"--ref", "msg_ref", "--json"}) {
		t.Fatalf("drafts.open recovery = %+v", guidance.Recovery)
	}
}

func TestGuidanceDoesNotEmitInvalidRecoveryCommands(t *testing.T) {
	sourceGuidance := guidanceForResponse("drafts.send", responseData{}, &transport.TransportError{Code: transport.CodeSMTPSourceInvalid, Message: "invalid source"})
	if sourceGuidance.Recovery.Action != mail.RecoveryInspect || sourceGuidance.Recovery.Command != "" || len(sourceGuidance.Recovery.Args) != 0 {
		t.Fatalf("source recovery without draft ref = %+v", sourceGuidance.Recovery)
	}
	result := mail.SendResult{AttemptID: "send_attempt", Outcome: mail.SendOutcomeUnknown}
	guidance := guidanceForResponse("drafts.send", responseData{SendResult: &result}, &transport.SubmissionError{Stage: "reply"})
	if guidance.Recovery.Command != "" || len(guidance.Recovery.Args) != 0 || guidance.Recovery.OperationID != "send_attempt" {
		t.Fatalf("send recovery without draft ref = %+v", guidance.Recovery)
	}

	message := mail.Message{Hydration: &mail.HydrationDiagnostic{State: mail.HydrationStateFailed}}
	guidance = guidanceForResponse("messages.get", responseData{Message: &message}, &testCodedError{code: "imap_timeout", message: "timeout"})
	if guidance.Recovery.Command != "" || len(guidance.Recovery.Args) != 0 {
		t.Fatalf("hydration recovery without message ref = %+v", guidance.Recovery)
	}
}

func TestRawSourceLimitGuidanceStopsRetry(t *testing.T) {
	guidance := mail.GuidanceForError("messages.get", &testCodedError{code: "raw_source_too_large", message: "too large"})
	if guidance.Retryability != mail.RetryTerminal || guidance.ReplayAllowed ||
		guidance.Recovery.Action != mail.RecoveryInspect {
		t.Fatalf("raw source limit guidance = %+v", guidance)
	}
}

func TestResourceLimitErrorEnvelopeRetainsBoundAndCertainty(t *testing.T) {
	resource := &transport.TransportError{
		Code: transport.CodeIMAPResourceLimitExceeded, Message: "IMAP response exceeded its bound",
		Limit: &transport.ResourceLimit{Name: "cumulative response bytes", Value: 32 << 20}, ObservedAtLeast: (32 << 20) + 1,
	}
	for _, test := range []struct {
		name      string
		command   string
		data      responseData
		err       error
		effect    mail.EffectCertainty
		retry     mail.Retryability
		wantLimit bool
	}{
		{name: "read", command: "mailboxes.list", err: fmt.Errorf("lookup: %w", resource), effect: mail.EffectNone, retry: mail.RetryTerminal, wantLimit: true},
		{name: "mutation resolution", command: "messages.delete", err: resource, effect: mail.EffectNone, retry: mail.RetryTerminal, wantLimit: true},
		{name: "APPEND pre-dispatch", command: "drafts.reconcile", err: resource, effect: mail.EffectNone, retry: mail.RetryTerminal, wantLimit: true},
		{name: "SMTP already accepted", command: "drafts.send", err: resource, data: responseData{SendResult: &mail.SendResult{AttemptID: "attempt", Outcome: mail.SendOutcomeMirrorPending, SubmissionAccepted: true}}, effect: mail.EffectPartial, retry: mail.RetryObserveRequired, wantLimit: true},
		{name: "APPEND unknown", command: "drafts.reconcile", err: errors.Join(resource, &transport.TransportError{Code: transport.CodeIMAPAppendOutcomeUnknown}), effect: mail.EffectPartial, retry: mail.RetryObserveRequired},
		{name: "ordinary failure", command: "messages.get", err: &transport.TransportError{Code: transport.CodeIMAPTimeout}, effect: mail.EffectNone, retry: mail.RetrySafe},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if code := writeJSON(&output, envelope{SchemaVersion: schemaVersion, Command: test.command, Error: newErrorData(test.command, test.data, test.err)}); code != 0 {
				t.Fatalf("write failure envelope: %d", code)
			}
			var response envelope
			if err := json.Unmarshal(output.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			failure := response.Error
			if failure == nil || failure.Guidance == nil || failure.Code != transport.ErrorCode(test.err) ||
				failure.Guidance.EffectCertainty != test.effect || failure.Guidance.Retryability != test.retry ||
				failure.Guidance.ReplayAllowed != (test.retry == mail.RetrySafe) {
				t.Fatalf("failure certainty = %+v", failure)
			}
			if test.wantLimit {
				if failure.Limit == nil || *failure.Limit != *resource.Limit || failure.ObservedAtLeast == nil ||
					*failure.ObservedAtLeast != resource.ObservedAtLeast {
					t.Fatalf("limit evidence missing: %s", output.String())
				}
				if test.effect == mail.EffectNone && failure.Guidance.Recovery.Action != mail.RecoveryInspect {
					t.Fatalf("overflow recovery = %+v", failure.Guidance)
				}
			} else if failure.Limit != nil || failure.ObservedAtLeast != nil ||
				strings.Contains(output.String(), `"observed_at_least"`) || strings.Contains(output.String(), `"limit"`) {
				t.Fatalf("unrelated failure received resource evidence: %s", output.String())
			}
		})
	}
}

func TestReadTimeoutRemainsReplayable(t *testing.T) {
	guidance := mail.GuidanceForError("messages.get", &testCodedError{code: transport.CodeIMAPTimeout, message: "timeout"})
	if guidance.Phase != mail.OperationPhaseRead || guidance.Retryability != mail.RetrySafe ||
		!guidance.ReplayAllowed || guidance.Recovery.Action != mail.RecoveryRetry {
		t.Fatalf("read timeout guidance = %+v", guidance)
	}
}

func TestDraftInspectPermissionEnvelopePreservesCorrectiveRecovery(t *testing.T) {
	root := t.TempDir()
	service := mail.NewServiceWithDraftRoot(nil, root)
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		To: []mail.Recipient{{Address: "recipient@example.com"}}, Subject: "Permission", Body: "Body",
	}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, draft.Ref+".json")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Error(err)
		}
	})
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), service, []string{"drafts", "inspect", "--ref", draft.Ref, "--json"}, &stdout, &stderr); code != 1 || stderr.Len() != 0 {
		t.Fatalf("permission read code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error == nil || response.Error.Code != "draft_state_error" || response.Error.Guidance == nil {
		t.Fatalf("permission envelope = %+v", response)
	}
	guidance := response.Error.Guidance
	if guidance.Phase != mail.OperationPhaseRead || guidance.EffectCertainty != mail.EffectNone ||
		guidance.Retryability != mail.RetryUserInputRequired || guidance.ReplayAllowed ||
		guidance.Recovery.Action != mail.RecoveryCorrect || guidance.Recovery.Instruction == "" ||
		strings.Contains(stdout.String(), root) {
		t.Fatalf("permission envelope policy or sanitation = %s", stdout.String())
	}
}

func TestHydrationReadContextComposesBudgetsAndHonorsCallerLimits(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	ctx, cancel := hydrationReadContext(parent)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("hydration read context has no deadline")
	}
	want := readTimeout + resolveTimeout + transport.TransferCommandBudget +
		transport.TransferBudgetForSize(mail.MaximumRawSourceBytes) + hydrationParseMargin
	if hydrationReadBudget() != want || want != 254*time.Second {
		t.Fatalf("hydration budget = %v, want component formula %v and 254s", hydrationReadBudget(), want)
	}
	remaining := time.Until(deadline)
	if readTimeout != 60*time.Second || remaining > want || want-remaining > 5*time.Second {
		t.Fatalf("hydration timeout = %v, local timeout = %v, want bounded %v", remaining, readTimeout, want)
	}
	cancelParent()
	if ctx.Err() != context.Canceled {
		t.Fatalf("caller cancellation = %v, want context.Canceled", ctx.Err())
	}

	deadlineParent, cancelDeadlineParent := context.WithTimeout(context.Background(), time.Second)
	defer cancelDeadlineParent()
	deadlineCtx, cancelDeadline := hydrationReadContext(deadlineParent)
	defer cancelDeadline()
	parentDeadline, parentHasDeadline := deadlineParent.Deadline()
	childDeadline, childHasDeadline := deadlineCtx.Deadline()
	if !parentHasDeadline || !childHasDeadline || !childDeadline.Equal(parentDeadline) {
		t.Fatalf("hydration deadline = %v/%v, want caller deadline %v/%v", childDeadline, childHasDeadline, parentDeadline, parentHasDeadline)
	}
}

func TestHydrationTimeoutReportsNoMutationAndRetainsPartialEvidence(t *testing.T) {
	message := failedHydrationMessage()
	message.Hydration.State = mail.HydrationStateFailed
	message.Hydration.Remote = &mail.HydrationCause{
		Code: "operation_timeout", Message: "IMAP hydration timed out; no external mutation was attempted",
	}
	err := hydrationCommandError(message.Hydration, context.DeadlineExceeded)
	if transport.ErrorCode(err) != "operation_timeout" || !strings.Contains(err.Error(), "no external mutation was attempted") {
		t.Fatalf("hydration timeout error = %v, code %q", err, transport.ErrorCode(err))
	}
	guidance := guidanceForResponse("messages.get", responseData{Message: &message}, err)
	if guidance.EffectCertainty != mail.EffectNone || guidance.Phase != mail.OperationPhaseHydration ||
		message.Content == "" || message.Hydration.Local == nil || message.Hydration.Local.Code != "raw_source_partial" {
		t.Fatalf("timeout recovery evidence = guidance %+v, message %+v", guidance, message)
	}
}

func TestRealMailStoreMissingMessageGetEmitsCorrectRecovery(t *testing.T) {
	const accountID = "951FB9AB-537B-4E97-8DCC-B241B71AD9DD"
	const storeUUID = "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE"
	config := createNotFoundRecoveryStore(t, storeUUID, accountID)
	ctx := context.Background()
	client := mailstore.NewClient(ctx, nil, config, mail.SendTransport{})
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close Mail-store client: %v", err)
		}
	})
	if _, opened := client.StoreProfile(); !opened {
		t.Fatal("fixture Mail store did not open")
	}
	ref, err := mailref.EncodeMessage(mailref.Message{
		AccountID: accountID, MailboxPath: []string{"INBOX"}, LibraryID: "404",
		ExpectedStoreUUID: storeUUID, ExpectedStoreMailboxID: 1,
	})
	if err != nil {
		t.Fatalf("encode missing-message ref: %v", err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(ctx, mail.NewService(client), []string{"messages", "get", "--ref", ref, "--json"}, &stdout, &stderr)
	if code != 1 || stderr.Len() != 0 {
		t.Fatalf("Run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if response.OK || response.Command != "messages.get" || response.Error == nil ||
		response.Error.Code != "not_found" || response.Data.Message != nil || response.Error.Guidance == nil {
		t.Fatalf("missing-message envelope = %+v", response)
	}
	guidance := response.Error.Guidance
	if guidance.Phase != mail.OperationPhaseRead || guidance.EffectCertainty != mail.EffectNone ||
		guidance.Retryability != mail.RetryUserInputRequired || guidance.ReplayAllowed ||
		guidance.Recovery.Action != mail.RecoveryCorrect || guidance.Recovery.Command != "" || len(guidance.Recovery.Args) != 0 {
		t.Fatalf("missing-message guidance = %+v", guidance)
	}
}

func TestUnavailableMailStoreMessageGetEmitsUserRecovery(t *testing.T) {
	ctx := context.Background()
	mailRoot := filepath.Join(t.TempDir(), "Library", "Mail")
	// An incomplete generation still needs store repair rather than onboarding.
	if err := os.MkdirAll(filepath.Join(mailRoot, "V10", "MailData"), 0o700); err != nil {
		t.Fatal(err)
	}
	client := mailstore.NewClient(ctx, nil, mailstore.Config{MailRoot: mailRoot}, mail.SendTransport{})
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close unavailable Mail-store client: %v", err)
		}
	})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	// A malformed ref is caller input even while the store is unavailable.
	if code := Run(ctx, mail.NewService(client), []string{"messages", "get", "--ref", "missing", "--json"}, &stdout, &stderr); code != 2 ||
		!strings.Contains(stdout.String(), `"code":"invalid_reference"`) || !strings.Contains(stdout.String(), `"do":"fix_input"`) {
		t.Fatalf("malformed ref with unavailable store: code=%d stdout=%s", code, stdout.String())
	}
	stdout.Reset()
	validRef, err := mailref.EncodeMessage(mailref.Message{AccountID: "account", MailboxPath: []string{"INBOX"}, LibraryID: "1", ExpectedMessageID: "<m@example.com>"})
	if err != nil {
		t.Fatal(err)
	}
	code := Run(ctx, mail.NewService(client), []string{"messages", "get", "--ref", validRef, "--json"}, &stdout, &stderr)
	if code != 1 || stderr.Len() != 0 {
		t.Fatalf("Run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if response.OK || response.Command != "messages.get" || response.Error == nil ||
		response.Error.Code != "mail_store_unavailable" || response.Error.Guidance == nil {
		t.Fatalf("unavailable-store envelope = %+v", response)
	}
	guidance := response.Error.Guidance
	if guidance.Phase != mail.OperationPhaseRead || guidance.EffectCertainty != mail.EffectNone ||
		guidance.Retryability != mail.RetryUserInputRequired || guidance.ReplayAllowed ||
		guidance.Recovery.Action != mail.RecoveryCorrect || guidance.Recovery.Instruction !=
		"Grant Full Disk Access to the calling app, or open Mail once to create its local store, then retry the read." {
		t.Fatalf("unavailable-store guidance = %+v", guidance)
	}
}

func createNotFoundRecoveryStore(t *testing.T, storeUUID, accountID string) mailstore.Config {
	t.Helper()
	mailRoot := t.TempDir()
	versionRoot := filepath.Join(mailRoot, "V10")
	mailData := filepath.Join(versionRoot, "MailData")
	if err := os.MkdirAll(mailData, 0o700); err != nil {
		t.Fatalf("create fixture MailData: %v", err)
	}
	database, err := sql.Open("sqlite3", filepath.Join(mailData, "Envelope Index"))
	if err != nil {
		t.Fatalf("open fixture Envelope Index: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close fixture Envelope Index: %v", err)
		}
	})
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`CREATE TABLE properties (ROWID INTEGER PRIMARY KEY, key, value)`,
		`CREATE TABLE mailboxes (ROWID INTEGER PRIMARY KEY, url TEXT NOT NULL, total_count INTEGER, unread_count INTEGER, deleted_count INTEGER, source INTEGER)`,
		`CREATE TABLE messages (ROWID INTEGER PRIMARY KEY, message_id INTEGER, global_message_id INTEGER, remote_id INTEGER, remote_mailbox INTEGER, sender INTEGER, subject INTEGER, summary INTEGER, date_sent INTEGER, date_received INTEGER, mailbox INTEGER, flags INTEGER, read INTEGER, flagged INTEGER, deleted INTEGER, size INTEGER, conversation_id INTEGER, type INTEGER, display_date INTEGER, flag_color INTEGER)`,
		`CREATE TABLE addresses (ROWID INTEGER PRIMARY KEY, address TEXT, comment TEXT)`,
		`CREATE TABLE subjects (ROWID INTEGER PRIMARY KEY, subject TEXT)`,
		`CREATE TABLE summaries (ROWID INTEGER PRIMARY KEY, summary TEXT)`,
		`CREATE TABLE recipients (ROWID INTEGER PRIMARY KEY, message INTEGER, address INTEGER, type INTEGER, position INTEGER)`,
		`CREATE TABLE attachments (ROWID INTEGER PRIMARY KEY, message INTEGER, attachment_id TEXT, name TEXT)`,
		`CREATE TABLE labels (message_id INTEGER, mailbox_id INTEGER)`,
		`CREATE TABLE server_messages (message INTEGER, mailbox INTEGER, junk_level INTEGER, draft INTEGER, replied INTEGER, forwarded INTEGER)`,
		`CREATE INDEX messages_mailbox_date_received ON messages(mailbox, date_received)`,
		`CREATE INDEX messages_deleted_date_received ON messages(deleted, date_received)`,
		`CREATE INDEX labels_mailbox ON labels(mailbox_id)`,
		`CREATE INDEX recipients_message ON recipients(message, position, type, address)`,
		`CREATE INDEX attachments_message ON attachments(message, attachment_id)`,
		`INSERT INTO properties(key, value) VALUES ('version', '4')`,
		`INSERT INTO properties(key, value) VALUES ('minor_version', '74003')`,
		`INSERT INTO properties(key, value) VALUES ('last_write_framework_version', '3826.700.81')`,
		`INSERT INTO properties(key, value) VALUES ('WriteTransactionGeneration', '1')`,
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("execute fixture Envelope Index statement %q: %v", statement, err)
		}
	}
	if _, err := database.Exec(`INSERT INTO properties(key, value) VALUES ('UUID', ?)`, storeUUID); err != nil {
		t.Fatalf("insert fixture Envelope Index UUID: %v", err)
	}
	return mailstore.Config{
		MailRoot: mailRoot, MailStorePath: versionRoot,
		ActiveAccountURLs: []string{"imap://" + accountID + "/"},
	}
}

func TestIMAPWireFailureGuidanceKeepsReadsSafeAndWritesUncertain(t *testing.T) {
	for _, code := range []string{transport.CodeIMAPCanceled, transport.CodeIMAPDisconnected} {
		err := &transport.TransportError{Code: code, Message: "injected IMAP wire failure"}
		read := guidanceForResponse("messages.get", responseData{}, err)
		if read.Phase != mail.OperationPhaseRead || read.EffectCertainty != mail.EffectNone ||
			read.Retryability != mail.RetrySafe || !read.ReplayAllowed || read.Recovery.Action != mail.RecoveryRetry {
			t.Fatalf("read guidance for %s = %+v", code, read)
		}
		write := guidanceForResponse("messages.move", responseData{}, err)
		if write.Phase != mail.OperationPhaseMutation || write.EffectCertainty != mail.EffectUnknown ||
			write.Retryability != mail.RetryObserveRequired || write.ReplayAllowed || write.Recovery.Action != mail.RecoveryInspect {
			t.Fatalf("write guidance for %s = %+v", code, write)
		}
	}
	for _, code := range []string{transport.CodeIMAPMutationFailed, transport.CodeIMAPMailboxNotFound} {
		write := guidanceForResponse("messages.move", responseData{}, &transport.TransportError{Code: code, Message: "rejection without mutation evidence"})
		if write.EffectCertainty != mail.EffectUnknown || write.Retryability != mail.RetryObserveRequired || write.ReplayAllowed {
			t.Fatalf("unproven mutation guidance for %s = %+v", code, write)
		}
	}
}

func TestWriteTimeoutRequiresObservation(t *testing.T) {
	for _, command := range []string{"sync", "drafts.handoff", "drafts.reconcile"} {
		guidance := mail.GuidanceForError(command, &testCodedError{code: "operation_timeout", message: "timeout"})
		if guidance.Retryability != mail.RetryObserveRequired || guidance.ReplayAllowed ||
			guidance.Recovery.Action != mail.RecoveryInspect {
			t.Fatalf("%s timeout guidance = %+v", command, guidance)
		}
	}
}

func TestMirrorPendingCodeRequiresReconciliation(t *testing.T) {
	guidance := mail.GuidanceForError("drafts.send", &testCodedError{code: "send_mirror_pending", message: "mirror pending"})
	if guidance.Phase != mail.OperationPhaseMirror || guidance.EffectCertainty != mail.EffectPartial ||
		guidance.Retryability != mail.RetryObserveRequired || guidance.ReplayAllowed ||
		guidance.Recovery.Action != mail.RecoveryReconcile {
		t.Fatalf("mirror pending guidance = %+v", guidance)
	}
}

func TestTerminalHydrationDoesNotEmitReplayCommand(t *testing.T) {
	message := failedHydrationMessage()
	guidance := guidanceForResponse("messages.get", responseData{Message: &message}, &testCodedError{code: "raw_source_too_large", message: "too large"})
	if guidance.Retryability != mail.RetryTerminal || guidance.Recovery.Action != mail.RecoveryInspect ||
		guidance.Recovery.Command != "" || len(guidance.Recovery.Args) != 0 {
		t.Fatalf("terminal hydration guidance = %+v", guidance)
	}
}

func TestOutputLimitGuidanceRequestsCorrection(t *testing.T) {
	guidance := mail.GuidanceForError("messages.get", &testCodedError{code: "output_too_large", message: "too large"})
	if guidance.Phase != mail.OperationPhaseExecution || guidance.EffectCertainty != mail.EffectNone ||
		guidance.Retryability != mail.RetryUserInputRequired || guidance.ReplayAllowed ||
		guidance.Recovery.Action != mail.RecoveryCorrect {
		t.Fatalf("output limit guidance = %+v", guidance)
	}
}

func TestRecoveryErrorEnvelopesUseOnlyRetainedTargets(t *testing.T) {
	draft := mail.Draft{Ref: "draft_abcdefghijklmnopqrstuvwx"}
	conflict := func(ref string) error {
		return &mail.DraftRevisionConflict{Ref: ref, ExpectedRevision: "expected", CurrentRevision: "current"}
	}
	tests := []struct {
		name                string
		command             string
		code                string
		data                responseData
		err                 error
		target              projectionTarget
		view                string
		phase               mail.OperationPhase
		effect              mail.EffectCertainty
		retryability        mail.Retryability
		replayAllowed       bool
		action              mail.RecoveryAction
		recoveryCmd         string
		recoveryArgs        []string
		instructionContains string
	}{
		{
			name: "completed draft output with ref", command: "drafts.update", code: "output_too_large",
			data:   responseData{Draft: &draft, draftMutationCompleted: true},
			err:    &outputTooLargeError{actual: 512, limit: 128, target: "draft", completedDraftRef: draft.Ref},
			target: projectionTargetDraft, view: outputViewMetadata,
			phase: mail.OperationPhaseExecution, effect: mail.EffectComplete,
			retryability: mail.RetryObserveRequired, action: mail.RecoveryInspect,
			recoveryCmd: "drafts.inspect", recoveryArgs: []string{"--ref", draft.Ref, "--view", "full", "--json"},
		},
		{
			name: "completed draft output without ref", command: "drafts.update", code: "output_too_large",
			data:   responseData{draftMutationCompleted: true},
			err:    &outputTooLargeError{actual: 512, limit: 128, target: "draft"},
			target: projectionTargetDraft, view: outputViewMetadata,
			phase: mail.OperationPhaseExecution, effect: mail.EffectComplete,
			retryability: mail.RetryObserveRequired, action: mail.RecoveryObserve,
		},
		{
			name: "read projection without original args", command: "messages.raw", code: "output_too_large",
			err:    &outputTooLargeError{actual: 512, limit: 128, target: "raw message"},
			target: projectionTargetRaw, view: outputViewFull,
			phase: mail.OperationPhaseExecution, effect: mail.EffectNone,
			retryability: mail.RetryUserInputRequired, action: mail.RecoveryCorrect,
		},
		{
			name: "stale search cursor", command: "messages.search", code: "search_cursor_stale",
			err:   &mail.OperationError{Code: "search_cursor_stale", Message: "stale cursor"},
			phase: mail.OperationPhaseRead, effect: mail.EffectNone,
			retryability: mail.RetryUserInputRequired, action: mail.RecoveryCorrect,
		},
		{
			name: "search index changed", command: "messages.filter", code: "search_index_changed",
			err:   &mail.OperationError{Code: "search_index_changed", Message: "index changed"},
			phase: mail.OperationPhaseRead, effect: mail.EffectNone,
			retryability: mail.RetrySafe, replayAllowed: true, action: mail.RecoveryRetry,
		},
		{
			name: "draft revision conflict with ref", command: "drafts.update", code: "draft_revision_conflict", err: conflict(draft.Ref),
			phase: mail.OperationPhaseValidation, effect: mail.EffectNone,
			retryability: mail.RetryObserveRequired, action: mail.RecoveryInspect,
			recoveryCmd: "drafts.inspect", recoveryArgs: []string{"--ref", draft.Ref, "--view", "full", "--json"},
		},
		{
			name: "draft revision conflict without ref", command: "drafts.update", code: "draft_revision_conflict", err: conflict(""),
			phase: mail.OperationPhaseValidation, effect: mail.EffectNone,
			retryability: mail.RetryObserveRequired, action: mail.RecoveryInspect,
		},
		{
			name: "draft busy with ref", command: "drafts.send", code: "draft_busy",
			err:   &mail.OperationError{Code: "draft_busy", Message: "busy", DraftRef: draft.Ref},
			phase: mail.OperationPhaseExecution, effect: mail.EffectNone,
			retryability: mail.RetryObserveRequired, action: mail.RecoveryInspect,
			recoveryCmd: "drafts.inspect", recoveryArgs: []string{"--ref", draft.Ref, "--json"},
		},
		{
			name: "draft busy without ref", command: "drafts.send", code: "draft_busy",
			err:   &mail.OperationError{Code: "draft_busy", Message: "busy"},
			phase: mail.OperationPhaseExecution, effect: mail.EffectNone,
			retryability: mail.RetryObserveRequired, action: mail.RecoveryObserve,
		},
		{
			// Binding resolution precedes composition and SMTP, so no effect exists.
			name: "stale account binding", command: "drafts.send", code: "account_binding_stale",
			err:   &mail.OperationError{Code: "account_binding_stale", Message: "stale binding"},
			phase: mail.OperationPhaseValidation, effect: mail.EffectNone,
			retryability: mail.RetryUserInputRequired, action: mail.RecoveryObserve,
			recoveryCmd: "accounts.list", recoveryArgs: []string{"--json"},
		},
		{
			name: "stale read account binding", command: "messages.get", code: "account_binding_stale",
			err:   &mail.OperationError{Code: "account_binding_stale", Message: "stale binding"},
			phase: mail.OperationPhaseRead, effect: mail.EffectNone,
			retryability: mail.RetryUserInputRequired, action: mail.RecoveryCorrect,
			instructionContains: "enabled account",
		},
		{
			name: "Mail recovery requires user action", command: "drafts.handoff", code: "mail_recovery_required",
			err:   &mail.OperationError{Code: "mail_recovery_required", Message: "reopen Mail"},
			phase: mail.OperationPhaseExecution, effect: mail.EffectUnknown,
			retryability: mail.RetryObserveRequired, action: mail.RecoveryInspect,
		},
		{
			name: "stopped Mail gate repair does not dispatch send", command: "drafts.send", code: "mail_not_running",
			err:   &mail.OperationError{Code: "mail_not_running", Message: "Mail is stopped"},
			phase: mail.OperationPhaseExecution, effect: mail.EffectNone,
			retryability: mail.RetryUserInputRequired, action: mail.RecoveryCorrect,
			instructionContains: "no action was dispatched",
		},
		{
			name: "corrupt Mail gate requires stopped repair", command: "drafts.send", code: "mail_access_gate_corrupt",
			err:   &mail.OperationError{Code: "mail_access_gate_corrupt", Message: "invalid retained state"},
			phase: mail.OperationPhaseExecution, effect: mail.EffectNone,
			retryability: mail.RetryUserInputRequired, action: mail.RecoveryCorrect,
			instructionContains: "Never delete or replace mail-access.lock",
		},
		{
			name: "unsafe Mail gate fails before dispatch", command: "drafts.send", code: "mail_access_gate_unsafe",
			err:   &mail.OperationError{Code: "mail_access_gate_unsafe", Message: "unsafe path"},
			phase: mail.OperationPhaseExecution, effect: mail.EffectNone,
			retryability: mail.RetryTerminal, action: mail.RecoveryInspect,
			instructionContains: "use a verified recovery plan",
		},
		{
			name: "ambiguous mutation mailbox requires correction", command: "messages.move", code: "imap_ambiguous_mailbox",
			err:    &mail.OperationError{Code: "imap_ambiguous_mailbox", Message: "multiple wire names match"},
			target: projectionTargetRaw, view: outputViewFull,
			phase: mail.OperationPhaseValidation, effect: mail.EffectNone,
			retryability: mail.RetryUserInputRequired, action: mail.RecoveryCorrect,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			code := writeProjectedFailure(&output, test.command, test.data,
				outputOptions{target: test.target, view: test.view, maxBytes: defaultJSONOutputBytes}, test.err, true)
			if code != 1 {
				t.Fatalf("writeProjectedFailure() = %d, want 1; envelope=%s", code, output.String())
			}
			var response envelope
			if err := json.Unmarshal(output.Bytes(), &response); err != nil {
				t.Fatalf("json.Unmarshal() error = %v; envelope=%s", err, output.String())
			}
			if response.OK || response.Command != test.command || response.Error == nil ||
				response.Error.Code != test.code || response.Error.Guidance == nil {
				t.Fatalf("failure envelope = %+v", response)
			}
			guidance := response.Error.Guidance
			if guidance.Phase != test.phase || guidance.EffectCertainty != test.effect ||
				guidance.Retryability != test.retryability || guidance.ReplayAllowed != test.replayAllowed ||
				guidance.Recovery.Action != test.action || guidance.Recovery.Command != test.recoveryCmd ||
				!equalStrings(guidance.Recovery.Args, test.recoveryArgs) ||
				(test.instructionContains != "" && !strings.Contains(guidance.Recovery.Instruction, test.instructionContains)) ||
				(test.name == "ambiguous mutation mailbox requires correction" &&
					!strings.Contains(guidance.Recovery.Instruction, "Correct the conflicting IMAP mailbox identities")) {
				t.Fatalf("%s envelope guidance = %+v", test.name, guidance)
			}
		})
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
