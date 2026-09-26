package cli

import (
	"context"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

func TestCopyNotStartedGuidanceRetainsOperationWithoutReconciliation(t *testing.T) {
	for _, test := range []struct {
		name         string
		command      string
		code         string
		cause        error
		outcome      string
		effects      []string
		certainty    mail.EffectCertainty
		retryability mail.Retryability
		replay       bool
		action       mail.RecoveryAction
	}{
		{name: "deadline", code: transport.CodeIMAPTimeout, outcome: transport.MutationOutcomeNotStarted, certainty: mail.EffectNone, retryability: mail.RetrySafe, replay: true, action: mail.RecoveryRetry},
		{name: "cancellation", code: transport.CodeIMAPMutationFailed, cause: context.Canceled, outcome: transport.MutationOutcomeNotStarted, certainty: mail.EffectNone, retryability: mail.RetrySafe, replay: true, action: mail.RecoveryRetry},
		{name: "validation", code: transport.CodeIMAPInvalidValue, outcome: transport.MutationOutcomeNotStarted, certainty: mail.EffectNone, retryability: mail.RetryUserInputRequired, action: mail.RecoveryCorrect},
		{name: "unclassified cause", code: transport.CodeIMAPMutationFailed, outcome: transport.MutationOutcomeNotStarted, certainty: mail.EffectNone, retryability: mail.RetryObserveRequired, action: mail.RecoveryObserve},
		{name: "prior partial effect", code: transport.CodeIMAPTimeout, outcome: transport.MutationOutcomeNotStarted, effects: []string{"copy"}, certainty: mail.EffectPartial, retryability: mail.RetryObserveRequired, action: mail.RecoveryObserve},
		{name: "unknown persistence", code: transport.CodeIMAPCopyOutcomeUnknown, cause: context.Canceled, outcome: transport.MutationOutcomeUnknown, certainty: mail.EffectUnknown, retryability: mail.RetryObserveRequired, action: mail.RecoveryObserve},
		{name: "MOVE deadline", command: "messages.move", code: transport.CodeIMAPTimeout, outcome: transport.MutationOutcomeNotStarted, certainty: mail.EffectNone, retryability: mail.RetrySafe, replay: true, action: mail.RecoveryRetry},
		{name: "MOVE cancellation", command: "messages.move", code: transport.CodeIMAPCanceled, cause: context.Canceled, outcome: transport.MutationOutcomeNotStarted, certainty: mail.EffectNone, retryability: mail.RetrySafe, replay: true, action: mail.RecoveryRetry},
		{name: "MOVE validation", command: "messages.move", code: transport.CodeIMAPInvalidValue, outcome: transport.MutationOutcomeNotStarted, certainty: mail.EffectNone, retryability: mail.RetryUserInputRequired, action: mail.RecoveryCorrect},
		{name: "MOVE partial effect", command: "messages.move", code: transport.CodeIMAPTimeout, outcome: transport.MutationOutcomeNotStarted, effects: []string{"copy"}, certainty: mail.EffectPartial, retryability: mail.RetryObserveRequired, action: mail.RecoveryObserve},
		{name: "MOVE unknown persistence", command: "messages.move", code: transport.CodeIMAPMoveOutcomeUnknown, cause: context.Canceled, outcome: transport.MutationOutcomeUnknown, certainty: mail.EffectUnknown, retryability: mail.RetryObserveRequired, action: mail.RecoveryObserve},
	} {
		t.Run(test.name, func(t *testing.T) {
			operation := "COPY"
			if test.command == "messages.move" {
				operation = "MOVE"
			} else {
				test.command = "messages.copy"
			}
			err := &transport.MutationOutcomeError{
				Code:    test.code,
				Message: "IMAP COPY failure with retained dispatch evidence",
				Err:     test.cause,
				Evidence: transport.MutationEvidence{
					OperationID:         "copy-operation",
					Outcome:             test.outcome,
					CompletedEffects:    test.effects,
					Command:             operation,
					UIDValidity:         12345,
					ExpectedUIDValidity: 12345,
				},
			}

			guidance := guidanceForResponse(test.command, responseData{}, err)
			if guidance.Phase != mail.OperationPhaseMutation || guidance.EffectCertainty != test.certainty ||
				guidance.Retryability != test.retryability || guidance.ReplayAllowed != test.replay ||
				guidance.Recovery.Action != test.action || guidance.Recovery.OperationID != "copy-operation" {
				t.Fatalf("COPY guidance = %+v", guidance)
			}
			if guidance.Recovery.Command != "" || len(guidance.Recovery.Args) != 0 {
				t.Fatalf("COPY recovery suggests a command: %+v", guidance.Recovery)
			}
		})
	}
}
