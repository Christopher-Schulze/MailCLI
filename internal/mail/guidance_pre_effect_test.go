package mail

import "testing"

func TestPreEffectCodesClassifyWithoutEffect(t *testing.T) {
	tests := []struct {
		command, code string
		phase         OperationPhase
		retryability  Retryability
		action        RecoveryAction
	}{
		{"messages.move", "account_disabled", OperationPhaseValidation, RetryUserInputRequired, RecoveryCorrect},
		{"drafts.send", "keychain_load_failed", OperationPhaseValidation, RetryUserInputRequired, RecoveryCorrect},
		{"drafts.send", "account_binding_missing", OperationPhaseValidation, RetryUserInputRequired, RecoveryCorrect},
		{"messages.delete", "draft_mutation_confirmation_required", OperationPhaseValidation, RetryUserInputRequired, RecoveryCorrect},
		{"drafts.edit", "editor_unavailable", OperationPhaseValidation, RetryUserInputRequired, RecoveryCorrect},
		{"drafts.handoff", "handoff_attachment_missing", OperationPhaseValidation, RetryUserInputRequired, RecoveryCorrect},
		{"messages.list", "ambiguous_mailbox", OperationPhaseRead, RetryUserInputRequired, RecoveryCorrect},
		{"update", "update_signature_invalid", OperationPhaseValidation, RetryTerminal, RecoveryInspect},
		{"update", "update_unsupported_platform", OperationPhaseValidation, RetryTerminal, RecoveryInspect},
		{"drafts.inspect", "draft_lock_unsafe", OperationPhaseRead, RetryTerminal, RecoveryInspect},
		{"drafts.inspect", "send_receipt_expired", OperationPhaseRead, RetryTerminal, RecoveryInspect},
		{"accounts.list", "account_binding_ambiguous", OperationPhaseRead, RetryUserInputRequired, RecoveryCorrect},
		{"messages.delete", "message_already_trashed", OperationPhaseValidation, RetryTerminal, RecoveryInspect},
		{"attachments.save", "attachment_resource_limit", OperationPhaseValidation, RetryTerminal, RecoveryInspect},
	}
	for _, test := range tests {
		t.Run(test.command+"/"+test.code, func(t *testing.T) {
			guidance := GuidanceForError(test.command, &OperationError{Code: test.code, Message: test.code})
			if guidance.EffectCertainty != EffectNone || guidance.ReplayAllowed || guidance.Phase != test.phase ||
				guidance.Retryability != test.retryability || guidance.Recovery.Action != test.action {
				t.Fatalf("guidance = %+v", guidance)
			}
		})
	}
}

func TestTransientPreEffectCodesAllowReplay(t *testing.T) {
	for _, test := range []struct{ command, code string }{
		{"drafts.prune", "prune_state_changed"},
		{"send.setup", "account_binding_busy"},
		{"update", "update_busy"},
		{"update", "update_download_failed"},
	} {
		guidance := GuidanceForError(test.command, &OperationError{Code: test.code, Message: test.code})
		if guidance.EffectCertainty != EffectNone || guidance.Retryability != RetrySafe || !guidance.ReplayAllowed || guidance.Recovery.Action != RecoveryRetry {
			t.Errorf("%s/%s guidance = %+v", test.command, test.code, guidance)
		}
	}
}

func TestDraftLockIntegrityStaysUncertainForMutations(t *testing.T) {
	guidance := GuidanceForError("drafts.send", &OperationError{Code: "draft_lock_changed", Message: "draft_lock_changed"})
	if guidance.EffectCertainty == EffectNone {
		t.Fatalf("a draft mutation lost effect uncertainty: %+v", guidance)
	}
}

func TestOutcomeCodesStayConservative(t *testing.T) {
	for _, test := range []struct{ command, code string }{
		{"drafts.send", "send_outcome_unknown"},
		{"drafts.send", "smtp_submission_unknown"},
		{"messages.copy", "imap_copy_outcome_unknown"},
		{"drafts.handoff", "handoff_outcome_unknown"},
	} {
		guidance := GuidanceForError(test.command, &OperationError{Code: test.code, Message: test.code})
		if guidance.EffectCertainty == EffectNone || guidance.ReplayAllowed {
			t.Errorf("%s/%s lost its uncertain-effect guidance: %+v", test.command, test.code, guidance)
		}
	}
}

func TestPreEffectCodeSetsAreDisjoint(t *testing.T) {
	for code := range preEffectInspectionCodes {
		if preEffectCorrectionCodes[code] {
			t.Errorf("%s is both a correction and an inspection code", code)
		}
	}
}
