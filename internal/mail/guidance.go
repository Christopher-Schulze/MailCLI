package mail

import (
	"context"
	"errors"
	"strings"

	"mailcli/internal/transport"
)

// OperationPhase identifies the boundary at which an operation stopped.
type OperationPhase string

const (
	OperationPhaseValidation OperationPhase = "validation"
	OperationPhaseRead       OperationPhase = "read"
	OperationPhaseSubmission OperationPhase = "submission"
	OperationPhaseMirror     OperationPhase = "mirror"
	OperationPhaseMutation   OperationPhase = "mutation"
	OperationPhaseHydration  OperationPhase = "hydration"
	OperationPhaseCleanup    OperationPhase = "cleanup"
	OperationPhaseExecution  OperationPhase = "execution"
)

// EffectCertainty states what the operation boundary proves about external
// effects when an error is returned.
type EffectCertainty string

const (
	EffectNone     EffectCertainty = "none"
	EffectComplete EffectCertainty = "complete"
	EffectPartial  EffectCertainty = "partial"
	EffectUnknown  EffectCertainty = "unknown"
)

// Retryability is the finite policy an agent must apply before replaying an
// operation. observe_required forbids replay until the retained evidence is
// checked; user_input_required needs a correction first.
type Retryability string

const (
	RetrySafe              Retryability = "safe"
	RetryObserveRequired   Retryability = "observe_required"
	RetryUserInputRequired Retryability = "user_input_required"
	RetryTerminal          Retryability = "terminal"
)

// RecoveryAction names the next safe class of action for an operation error.
type RecoveryAction string

const (
	RecoveryRetry     RecoveryAction = "retry"
	RecoveryObserve   RecoveryAction = "observe"
	RecoveryReconcile RecoveryAction = "reconcile"
	RecoveryCorrect   RecoveryAction = "correct"
	RecoveryInspect   RecoveryAction = "inspect"
	RecoveryNone      RecoveryAction = "none"
)

// RecoveryGuidance contains only supported command arguments or a durable
// operation identity. It never carries credentials or raw protocol secrets.
type RecoveryGuidance struct {
	Action      RecoveryAction `json:"action"`
	Command     string         `json:"command,omitempty"`
	Args        []string       `json:"args,omitempty"`
	OperationID string         `json:"operation_id,omitempty"`
}

// OperationGuidance is the machine-readable retry and recovery contract.
type OperationGuidance struct {
	Phase           OperationPhase   `json:"phase"`
	EffectCertainty EffectCertainty  `json:"effect_certainty"`
	Retryability    Retryability     `json:"retryability"`
	ReplayAllowed   bool             `json:"replay_allowed"`
	Recovery        RecoveryGuidance `json:"recovery"`
}

// GuidanceForError classifies an error conservatively at the named command
// boundary. Callers with retained result data can add the exact recovery
// command and operation identity after this base classification.
func GuidanceForError(command string, err error) OperationGuidance {
	code := guidanceErrorCode(err)
	if isInputErrorCode(code) {
		return guidanceForInput()
	}
	switch code {
	case "batch_canceled":
		return OperationGuidance{Phase: OperationPhaseExecution, EffectCertainty: EffectNone, Retryability: RetrySafe, ReplayAllowed: true, Recovery: RecoveryGuidance{Action: RecoveryRetry}}
	case "initialization_failed":
		return guidanceForInput()
	case "finalization_failed":
		return OperationGuidance{Phase: OperationPhaseCleanup, EffectCertainty: EffectUnknown, Retryability: RetryObserveRequired, Recovery: RecoveryGuidance{Action: RecoveryInspect}}
	case "serialization_failed":
		return OperationGuidance{Phase: OperationPhaseExecution, EffectCertainty: EffectUnknown, Retryability: RetryTerminal, Recovery: RecoveryGuidance{Action: RecoveryInspect}}
	case "operation_canceled", "operation_timeout", transport.CodeSMTPTimeout, transport.CodeSMTPTransferTimeout, transport.CodeIMAPConnectFailed, transport.CodeIMAPTimeout, transport.CodeIMAPFetchFailed:
		if effectfulCommand(command) {
			return guidanceForUnknown(defaultPhase(command))
		}
		return guidanceForRead()
	case transport.CodeSMTPRejected:
		return guidanceForSMTPRejection(err)
	case transport.CodeSMTPSubmissionUnknown:
		return guidanceForSendUnknown()
	case transport.CodeIMAPCopyOutcomeUnknown, transport.CodeIMAPMoveOutcomeUnknown:
		return guidanceForMutationUnknown(err)
	case transport.CodeIMAPAppendOutcomeUnknown:
		return guidanceForMirrorUnknown()
	case "send_mirror_pending", "send_mirror_outcome_unknown":
		return guidanceForMirrorUnknown()
	case "send_outcome_unknown", "send_outcome_unverifiable", "send_state_unknown":
		return guidanceForSendUnknown()
	case transport.CodeIMAPAppendFailed:
		if command == "drafts.send" || command == "drafts.reconcile" {
			return guidanceForMirrorUnknown()
		}
	case transport.CodeIMAPRawSourceTooLarge:
		return OperationGuidance{Phase: OperationPhaseHydration, EffectCertainty: EffectNone, Retryability: RetryTerminal, Recovery: RecoveryGuidance{Action: RecoveryInspect}}
	case "output_too_large":
		return OperationGuidance{Phase: OperationPhaseExecution, EffectCertainty: EffectNone, Retryability: RetryUserInputRequired, Recovery: RecoveryGuidance{Action: RecoveryCorrect}}
	case "content_export_too_large":
		return OperationGuidance{Phase: OperationPhaseExecution, EffectCertainty: EffectNone, Retryability: RetryTerminal, Recovery: RecoveryGuidance{Action: RecoveryInspect}}
	case transport.CodeSMTPAuthFailed, transport.CodeSMTPCredentialsMissing, transport.CodeIMAPAuthFailed, transport.CodeSMTPTLSFailed, transport.CodeUnsupportedProvider:
		return guidanceForInput()
	}
	if effectfulCommand(command) {
		return guidanceForUnknown(defaultPhase(command))
	}
	return guidanceForRead()
}

func guidanceErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) && coded.ErrorCode() != "" {
		return coded.ErrorCode()
	}
	if errors.Is(err, context.Canceled) {
		return "operation_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "operation_timeout"
	}
	return "operation_failed"
}

func isInputErrorCode(code string) bool {
	return code == "invalid_argument" || code == "invalid_input" || code == "missing_required" || code == "unknown_command" || code == "confirmation_required" || strings.HasPrefix(code, "invalid_")
}

func effectfulCommand(command string) bool {
	switch command {
	case "batch", "update", "attachments.save", "send.setup", "sync",
		"drafts.create", "drafts.edit", "drafts.handoff", "drafts.update", "drafts.save",
		"drafts.send", "drafts.reconcile", "drafts.discard", "drafts.prune",
		"messages.reply", "messages.forward", "messages.mark", "messages.move",
		"messages.copy", "messages.delete":
		return true
	default:
		return false
	}
}

func defaultPhase(command string) OperationPhase {
	switch {
	case command == "drafts.send":
		return OperationPhaseSubmission
	case command == "drafts.reconcile":
		return OperationPhaseMirror
	case strings.HasPrefix(command, "messages."):
		return OperationPhaseMutation
	default:
		return OperationPhaseExecution
	}
}

func guidanceForInput() OperationGuidance {
	return OperationGuidance{Phase: OperationPhaseValidation, EffectCertainty: EffectNone, Retryability: RetryUserInputRequired, Recovery: RecoveryGuidance{Action: RecoveryCorrect}}
}

func guidanceForRead() OperationGuidance {
	return OperationGuidance{Phase: OperationPhaseRead, EffectCertainty: EffectNone, Retryability: RetrySafe, ReplayAllowed: true, Recovery: RecoveryGuidance{Action: RecoveryRetry}}
}

func guidanceForUnknown(phase OperationPhase) OperationGuidance {
	return OperationGuidance{Phase: phase, EffectCertainty: EffectUnknown, Retryability: RetryObserveRequired, Recovery: RecoveryGuidance{Action: RecoveryInspect}}
}

func guidanceForSendUnknown() OperationGuidance {
	return OperationGuidance{Phase: OperationPhaseSubmission, EffectCertainty: EffectUnknown, Retryability: RetryObserveRequired, Recovery: RecoveryGuidance{Action: RecoveryReconcile}}
}

func guidanceForMirrorUnknown() OperationGuidance {
	return OperationGuidance{Phase: OperationPhaseMirror, EffectCertainty: EffectPartial, Retryability: RetryObserveRequired, Recovery: RecoveryGuidance{Action: RecoveryReconcile}}
}

func guidanceForMutationUnknown(err error) OperationGuidance {
	guidance := OperationGuidance{Phase: OperationPhaseMutation, EffectCertainty: EffectUnknown, Retryability: RetryObserveRequired, Recovery: RecoveryGuidance{Action: RecoveryObserve}}
	var outcome *transport.MutationOutcomeError
	if errors.As(err, &outcome) {
		guidance.Recovery.OperationID = outcome.Evidence.OperationID
		if len(outcome.Evidence.CompletedEffects) > 0 || outcome.Evidence.Outcome == transport.MutationOutcomePartial {
			guidance.EffectCertainty = EffectPartial
		}
	}
	return guidance
}

func guidanceForSMTPRejection(err error) OperationGuidance {
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "transient final rejection") {
		return OperationGuidance{Phase: OperationPhaseSubmission, EffectCertainty: EffectNone, Retryability: RetrySafe, ReplayAllowed: true, Recovery: RecoveryGuidance{Action: RecoveryRetry}}
	}
	return OperationGuidance{Phase: OperationPhaseSubmission, EffectCertainty: EffectNone, Retryability: RetryUserInputRequired, Recovery: RecoveryGuidance{Action: RecoveryCorrect}}
}
