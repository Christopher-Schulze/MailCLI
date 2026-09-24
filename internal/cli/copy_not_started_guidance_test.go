package cli

import (
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

func TestCopyNotStartedGuidanceRetainsOperationWithoutReconciliation(t *testing.T) {
	err := &transport.MutationOutcomeError{
		Code:    transport.CodeIMAPTimeout,
		Message: "IMAP COPY was not dispatched; no server-side effect occurred",
		Evidence: transport.MutationEvidence{
			OperationID:         "copy-operation",
			Outcome:             transport.MutationOutcomeNotStarted,
			Command:             "COPY",
			UIDValidity:         12345,
			ExpectedUIDValidity: 12345,
		},
	}

	guidance := guidanceForResponse("messages.copy", responseData{}, err)
	if guidance.Phase != mail.OperationPhaseMutation || guidance.EffectCertainty != mail.EffectNone ||
		guidance.Retryability != mail.RetryObserveRequired || guidance.ReplayAllowed ||
		guidance.Recovery.Action != mail.RecoveryObserve || guidance.Recovery.OperationID != "copy-operation" {
		t.Fatalf("COPY not-started guidance = %+v", guidance)
	}
	if guidance.Recovery.Command != "" || len(guidance.Recovery.Args) != 0 {
		t.Fatalf("COPY not-started recovery suggests a command: %+v", guidance.Recovery)
	}
}
