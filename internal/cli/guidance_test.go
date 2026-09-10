package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"mailcli/internal/mail"
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

func TestHandoffUnknownGuidanceKeepsRecoveryWithoutAttachments(t *testing.T) {
	result := draftHandoffResult{
		DraftRef: "draft_ref", AttemptID: "handoff_123456789012345678901234", Outcome: mail.HandoffOutcomeUnknown,
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
		DraftRef: "draft_ref", AttemptID: "handoff_123456789012345678901234", Outcome: mail.HandoffOutcomeConfirmedFailed,
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
	if guidance.Recovery.Command != "drafts.open" || !equalStrings(guidance.Recovery.Args, []string{"--message", "msg_ref", "--json"}) {
		t.Fatalf("drafts.open recovery = %+v", guidance.Recovery)
	}
}

func TestGuidanceDoesNotEmitInvalidRecoveryCommands(t *testing.T) {
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

func TestReadTimeoutRemainsReplayable(t *testing.T) {
	guidance := mail.GuidanceForError("messages.get", &testCodedError{code: transport.CodeIMAPTimeout, message: "timeout"})
	if guidance.Phase != mail.OperationPhaseRead || guidance.Retryability != mail.RetrySafe ||
		!guidance.ReplayAllowed || guidance.Recovery.Action != mail.RecoveryRetry {
		t.Fatalf("read timeout guidance = %+v", guidance)
	}
}

func TestWriteTimeoutRequiresObservation(t *testing.T) {
	for _, command := range []string{"sync", "drafts.handoff", "drafts.save"} {
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
