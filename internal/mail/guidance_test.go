package mail

import (
	"testing"

	"mailcli/internal/transport"
)

func TestGuidanceForProtocolFailuresBeforeTerminators(t *testing.T) {
	tests := []struct {
		name          string
		command       string
		code          string
		phase         OperationPhase
		effect        EffectCertainty
		retryability  Retryability
		replayAllowed bool
		recovery      RecoveryAction
	}{
		{
			name:          "SMTP DATA incomplete before terminator",
			command:       "drafts.send",
			code:          transport.CodeSMTPDataIncomplete,
			phase:         OperationPhaseSubmission,
			effect:        EffectNone,
			retryability:  RetrySafe,
			replayAllowed: true,
			recovery:      RecoveryRetry,
		},
		{
			name:          "SMTP submission unknown after terminator",
			command:       "drafts.send",
			code:          transport.CodeSMTPSubmissionUnknown,
			phase:         OperationPhaseSubmission,
			effect:        EffectUnknown,
			retryability:  RetryObserveRequired,
			replayAllowed: false,
			recovery:      RecoveryReconcile,
		},
		{
			name:          "APPEND incomplete during send",
			command:       "drafts.send",
			code:          transport.CodeIMAPAppendIncomplete,
			phase:         OperationPhaseMirror,
			effect:        EffectPartial,
			retryability:  RetryObserveRequired,
			replayAllowed: false,
			recovery:      RecoveryReconcile,
		},
		{
			name:          "APPEND incomplete during reconcile",
			command:       "drafts.reconcile",
			code:          transport.CodeIMAPAppendIncomplete,
			phase:         OperationPhaseMirror,
			effect:        EffectNone,
			retryability:  RetrySafe,
			replayAllowed: true,
			recovery:      RecoveryRetry,
		},
		{
			name:          "APPEND outcome unknown after terminator",
			command:       "drafts.reconcile",
			code:          transport.CodeIMAPAppendOutcomeUnknown,
			phase:         OperationPhaseMirror,
			effect:        EffectPartial,
			retryability:  RetryObserveRequired,
			replayAllowed: false,
			recovery:      RecoveryReconcile,
		},
		{
			name:          "composite command stays conservative",
			command:       "batch",
			code:          transport.CodeSMTPDataIncomplete,
			phase:         OperationPhaseExecution,
			effect:        EffectUnknown,
			retryability:  RetryObserveRequired,
			replayAllowed: false,
			recovery:      RecoveryInspect,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := GuidanceForError(test.command, &transport.TransportError{Code: test.code, Message: "injected protocol failure"})
			if got.Phase != test.phase || got.EffectCertainty != test.effect ||
				got.Retryability != test.retryability || got.ReplayAllowed != test.replayAllowed ||
				got.Recovery.Action != test.recovery {
				t.Fatalf("GuidanceForError(%q, %s) = %+v", test.command, test.code, got)
			}
		})
	}
}

type searchBudgetGuidanceError struct {
	requiredBytes int64
}

func (e *searchBudgetGuidanceError) Error() string {
	return "search candidate exceeds byte budget"
}

func (e *searchBudgetGuidanceError) ErrorCode() string {
	return "search_budget_too_small"
}

func (e *searchBudgetGuidanceError) RequiredBytes() int64 {
	return e.requiredBytes
}

func TestGuidanceForSearchBudgetTooSmallRequiresCorrectedRestart(t *testing.T) {
	for _, command := range []string{"messages.search", "messages.filter"} {
		got := GuidanceForError(command, &searchBudgetGuidanceError{requiredBytes: 4096})
		if got.Phase != OperationPhaseRead || got.EffectCertainty != EffectNone ||
			got.Retryability != RetryUserInputRequired || got.ReplayAllowed ||
			got.Recovery.Action != RecoveryCorrect || got.Recovery.Command != command ||
			len(got.Recovery.Args) != 2 || got.Recovery.Args[0] != "--max-bytes" ||
			got.Recovery.Args[1] != "4096" {
			t.Fatalf("GuidanceForError(%q) = %+v", command, got)
		}
	}
}
