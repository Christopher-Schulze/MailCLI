package mail

import (
	"io/fs"
	"strings"
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
			name: "mutation lock setup requires correction", command: "drafts.send",
			code: transport.CodeIMAPLockUnavailable, phase: OperationPhaseValidation,
			effect: EffectNone, retryability: RetryUserInputRequired, recovery: RecoveryCorrect,
		},
		{
			name: "mutation lock contention permits retry", command: "messages.mark",
			code: transport.CodeIMAPAccountBusy, phase: OperationPhaseMutation,
			effect: EffectNone, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry,
		},
		{
			name: "SMTP source integrity requires inspection", command: "drafts.send",
			code: transport.CodeSMTPSourceInvalid, phase: OperationPhaseSubmission,
			effect: EffectNone, retryability: RetryObserveRequired, recovery: RecoveryInspect,
		},
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

func TestGuidanceForNotFoundRequiresFreshInputForReads(t *testing.T) {
	err := &transport.TransportError{Code: "not_found", Message: "missing message"}
	tests := []struct {
		name    string
		command string
	}{
		{name: "messages.get", command: "messages.get"},
		{name: "messages.raw", command: "messages.raw"},
		{name: "drafts.inspect", command: "drafts.inspect"},
		{name: "drafts.open", command: "drafts.open"},
		{name: "attachments.list", command: "attachments.list"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := GuidanceForError(test.command, err)
			if got.Phase != OperationPhaseRead || got.EffectCertainty != EffectNone ||
				got.Retryability != RetryUserInputRequired || got.ReplayAllowed ||
				got.Recovery.Action != RecoveryCorrect || got.Recovery.Command != "" || len(got.Recovery.Args) != 0 {
				t.Fatalf("GuidanceForError(%q, not_found) = %+v", test.command, got)
			}
		})
	}
}

func TestGuidanceForKnownReadErrorsHasExplicitPolicy(t *testing.T) {
	tests := []struct {
		code                string
		phase               OperationPhase
		retryability        Retryability
		replayAllowed       bool
		recovery            RecoveryAction
		instruction         bool
		instructionContains string
	}{
		{code: transport.CodeIMAPConnectFailed, phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{code: transport.CodeIMAPCanceled, phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{code: transport.CodeIMAPDisconnected, phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{code: transport.CodeIMAPTimeout, phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{code: transport.CodeIMAPFetchFailed, phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{code: "operation_canceled", phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{code: "draft_operation_canceled", phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{code: "operation_timeout", phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{code: "mail_store_unavailable", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "mail_store_preferences_unavailable", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "mail_store_preferences_invalid", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "safe_mailbox_listing_unavailable", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "safe_search_unavailable", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "account_disabled", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "Enable this account in Mail.app"},
		{code: "account_degraded", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "mailcli accounts list --json"},
		{code: "account_identity_missing", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "mailcli send setup"},
		{code: "account_binding_invalid", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "preserve existing binding data"},
		{code: "account_binding_host_invalid", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "host and port"},
		{code: "account_binding_provider_mismatch", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "same supported provider"},
		{code: "account_binding_version_unsupported", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "preserve the binding file"},
		{code: "account_binding_unavailable", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "preserve the file contents"},
		{code: "mail_automation_denied", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "Privacy & Security > Automation"},
		{code: "mail_not_running", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "Open Mail.app"},
		{code: "mail_recovery_required", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "Quit and reopen Mail.app"},
		{code: transport.CodeIMAPAuthFailed, phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "ambiguous_attachment", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "search_count_limit_exceeded", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "stale_reference", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "message_source_missing", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "content_unavailable", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "remove `--export`"},
		{code: "content_incomplete", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "finish downloading this message"},
		{code: "local_only_mailbox", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "local store"},
		{code: "store_bound_reference_required", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "store-bound message reference"},
		{code: "mailbox_uidvalidity_changed", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "fresh message reference"},
		{code: transport.CodeIMAPMailboxNotFound, phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: transport.CodeIMAPMessageNotFound, phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: transport.CodeIMAPAmbiguousMailbox, phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "unsupported_mail_store_schema", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "ambiguous_mail_store_generation", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "unsafe_message_source", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "raw_source_too_large", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "attachment_resource_limit", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "mime_resource_limit", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "invalid_emlx", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "invalid_message_source", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "ambiguous_message_source", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "store_changed", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "mail_store_not_read_only", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "invalid_mailbox_cache", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "mailbox_cache_malformed", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "imap_flag_read_unsupported", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "account_catalog_incomplete", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "mailbox_catalog_incomplete", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "mailbox_info_malformed", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "imap_message_uid_mismatch", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "draft_state_error", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "send_receipt_invalid", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "imap_response_malformed", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "mailbox_uidvalidity_unknown", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "search_unavailable", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "search_cursor_stale", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect},
		{code: "search_index_changed", phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{code: "invalid_argument", phase: OperationPhaseValidation, retryability: RetryUserInputRequired, recovery: RecoveryCorrect},
		{code: "invalid_cursor", phase: OperationPhaseValidation, retryability: RetryUserInputRequired, recovery: RecoveryCorrect},
	}
	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			got := GuidanceForError("messages.get", &transport.TransportError{Code: test.code, Message: "injected read failure"})
			if got.Phase != test.phase || got.EffectCertainty != EffectNone ||
				got.Retryability != test.retryability || got.ReplayAllowed != test.replayAllowed ||
				got.Recovery.Action != test.recovery || (got.Recovery.Instruction != "") != test.instruction ||
				(test.instructionContains != "" && !strings.Contains(got.Recovery.Instruction, test.instructionContains)) {
				t.Fatalf("GuidanceForError(%q) = %+v", test.code, got)
			}
		})
	}
	permission := GuidanceForError("messages.get", fs.ErrPermission)
	if permission.Phase != OperationPhaseRead || permission.EffectCertainty != EffectNone ||
		permission.Retryability != RetryUserInputRequired || permission.ReplayAllowed ||
		permission.Recovery.Action != RecoveryCorrect || permission.Recovery.Instruction == "" {
		t.Fatalf("permission guidance = %+v", permission)
	}
	unknown := GuidanceForError("messages.get", &transport.TransportError{Code: "future_read_failure", Message: "new failure"})
	if unknown.Phase != OperationPhaseRead || unknown.EffectCertainty != EffectNone ||
		unknown.Retryability != RetryObserveRequired || unknown.ReplayAllowed || unknown.Recovery.Action != RecoveryInspect {
		t.Fatalf("unknown read guidance = %+v", unknown)
	}
}

func TestGuidanceForAmbiguousMailboxMutationRequiresCorrection(t *testing.T) {
	ambiguous := &transport.TransportError{
		Code: transport.CodeIMAPAmbiguousMailbox, Message: "multiple mailbox identities match the requested path",
	}
	for _, command := range []string{
		"messages.mark", "messages.move", "messages.copy", "messages.delete",
		BatchOperationMark, BatchOperationMove, BatchOperationCopy, BatchOperationDelete,
	} {
		t.Run(command, func(t *testing.T) {
			got := GuidanceForError(command, ambiguous)
			if got.Phase != OperationPhaseValidation || got.EffectCertainty != EffectNone ||
				got.Retryability != RetryUserInputRequired || got.ReplayAllowed ||
				got.Recovery.Action != RecoveryCorrect ||
				!strings.Contains(got.Recovery.Instruction, "Correct the conflicting IMAP mailbox identities") {
				t.Fatalf("GuidanceForError(%q) = %+v", command, got)
			}
		})
	}

	batch := GuidanceForError("batch", ambiguous)
	if batch.EffectCertainty != EffectUnknown || batch.Retryability != RetryObserveRequired ||
		batch.ReplayAllowed || batch.Recovery.Action != RecoveryInspect {
		t.Fatalf("batch guidance = %+v; want conservative inspection", batch)
	}
	for _, operation := range []BatchOperation{
		BatchOperationMark, BatchOperationMove, BatchOperationCopy, BatchOperationDelete,
	} {
		t.Run("batch item "+operation, func(t *testing.T) {
			item := (&batchExecution{request: BatchRequest{Operation: operation}}).itemError(ambiguous)
			if item.Retryable || item.Guidance == nil ||
				item.Guidance.Phase != OperationPhaseValidation || item.Guidance.EffectCertainty != EffectNone ||
				item.Guidance.Retryability != RetryUserInputRequired || item.Guidance.ReplayAllowed ||
				item.Guidance.Recovery.Action != RecoveryCorrect ||
				!strings.Contains(item.Guidance.Recovery.Instruction, "duplicate special-use assignments") {
				t.Fatalf("batch item guidance = %+v; want pre-dispatch correction guidance", item)
			}
		})
	}

	uncertain := &transport.MutationOutcomeError{
		Code:     transport.CodeIMAPMoveOutcomeUnknown,
		Evidence: transport.MutationEvidence{Command: "MOVE", OperationID: "op_1"},
		Err:      ambiguous,
	}
	got := GuidanceForError("messages.move", uncertain)
	if got.EffectCertainty != EffectUnknown || got.Retryability != RetryObserveRequired ||
		got.ReplayAllowed || got.Recovery.Action == RecoveryCorrect {
		t.Fatalf("wrapped uncertain mutation guidance = %+v; mailbox ambiguity must not erase outcome uncertainty", got)
	}
}
