package cli

import (
	"strings"

	"mailcli/internal/mail"
)

type nextAction struct {
	Do          string   `json:"do"`
	Command     string   `json:"command,omitempty"`
	Args        []string `json:"args,omitempty"`
	WaitSeconds int      `json:"wait_seconds,omitempty"`
	Why         string   `json:"why"`
}

// envelopeNextAction is the single decision point; it never authorizes replay
// merely because a recovery command or retained reference exists.
func envelopeNextAction(value envelope) *nextAction {
	if value.Data.Finalization != nil {
		return &nextAction{Do: "check_state", Why: "Cleanup failed; inspect retained operation evidence before taking further action."}
	}
	if value.OK {
		return pendingNextAction(value.Data)
	}
	next := failureNextAction(value.Error, value.Next)
	if next.Do == "check_state" && value.Error != nil && value.Error.Guidance != nil && value.Error.Guidance.Recovery.Command == "drafts.handoff-reconcile" && value.Data.DraftHandoff != nil {
		return pendingDraftAction(value.Data.DraftHandoff.DraftRef, false)
	}
	return next
}

func failureNextAction(failure *errorData, previous *nextAction) *nextAction {
	if failure == nil || failure.Guidance == nil {
		return &nextAction{Do: "check_state", Why: "Recovery evidence is missing; inspect the state without replaying the operation."}
	}
	guidance := *failure.Guidance
	canceled := failure.callerCanceled || strings.Contains(failure.Code, "canceled") || (failure.Code == "operation_failed" && guidance.Retryability == mail.RetrySafe && previous != nil && previous.Do == "stop")
	next := &nextAction{Do: "check_state", Why: "Inspect retained evidence before any replay."}
	switch {
	case guidance.EffectCertainty != mail.EffectNone:
	case canceled:
		next.Do, next.Why = "stop", "The caller canceled the operation; do not restart it automatically."
	case guidance.Retryability == mail.RetryTerminal:
		next.Do, next.Why = "stop", "This failure is terminal; inspect the detailed guidance before further action."
	case failure.environmentRepair || environmentFailure(failure.Code) || (previous != nil && previous.Do == "ask_user"):
		next.Do, next.Why = "ask_user", "Repair the environment or permissions using the detailed guidance before continuing."
	case guidance.Retryability == mail.RetryObserveRequired:
	case guidance.Retryability == mail.RetrySafe && guidance.ReplayAllowed:
		next.Do, next.Why = "retry", "The evidence permits retrying the original invocation."
		if failure.Code != "search_index_changed" {
			next.WaitSeconds = 1
		}
	case guidance.Retryability == mail.RetryUserInputRequired:
		next.Do, next.Why = "fix_input", "Correct the input described by the detailed guidance before resubmitting."
	}
	attachNextRecovery(next, guidance.Recovery)
	return next
}

func environmentFailure(code string) bool {
	switch code {
	case "initialization_failed", "environment_unhealthy", "imap_mutation_lock_unavailable", "mail_not_running", "mail_automation_denied", "mail_recovery_required", "mail_access_gate_corrupt",
		"smtp_auth_failed", "imap_auth_failed", "smtp_tls_failed", "smtp_credentials_missing", "imap_credentials_missing", "smtp_utf8_unsupported", "transport_unsupported_provider",
		"editor_unavailable", "editor_terminal_unavailable", "account_disabled", "account_degraded", "account_identity_missing", "account_binding_invalid", "account_binding_host_invalid", "account_binding_provider_mismatch", "account_binding_provider_invalid", "account_binding_unavailable",
		"mail_store_unavailable", "mail_store_preferences_unavailable", "preferences_unavailable", "safe_mailbox_listing_unavailable", "safe_search_unavailable", "safe_message_listing_unavailable", "mail_store_preferences_invalid":
		return true
	}
	return false
}

func attachNextRecovery(next *nextAction, recovery mail.RecoveryGuidance) {
	if next.Do == "stop" || next.Do == "ask_user" || recovery.Command == "" || len(recovery.Args) == 0 {
		return
	}
	// Handoff reconciliation needs a human-observed outcome, and updates need
	// reviewed stdin. Neither is an executable observation command.
	if next.Do == "check_state" && recovery.Action != mail.RecoveryInspect && recovery.Action != mail.RecoveryObserve && recovery.Command != "drafts.reconcile" {
		return
	}
	if recovery.Command == "drafts.handoff-reconcile" || recovery.Command == "drafts.update" {
		return
	}
	next.Command, next.Args = recovery.Command, append([]string(nil), recovery.Args...)
}

func pendingDraftEvidence(data responseData) *nextAction {
	if result := data.SendResult; result != nil && result.Outcome != mail.SendOutcomeSent && result.Outcome != mail.SendOutcomeObserved {
		return pendingDraftAction(result.DraftRef, result.AttemptID != "")
	}
	if handoffNeedsReconciliation(data.DraftHandoff, nil) {
		return pendingDraftAction(data.DraftHandoff.DraftRef, false)
	}
	if result := data.HandoffReconcile; result != nil && result.SnapshotsRetained {
		return pendingDraftAction(result.DraftRef, false)
	}
	if draft := data.Draft; draft != nil && (draft.SaveAttempt != nil || draft.SendAttempt != nil || draft.HandoffAttempt != nil) {
		return pendingDraftAction(draft.Ref, false)
	}
	if data.Drafts != nil {
		for _, draft := range *data.Drafts {
			if draft.SaveAttempt != nil || draft.SendAttempt != nil || draft.HandoffAttempt != nil || draft.StateError != "" {
				return pendingDraftAction(draft.Ref, false)
			}
		}
	}
	return nil
}

func pendingNextAction(data responseData) *nextAction {
	if next := pendingDraftEvidence(data); next != nil {
		return next
	}
	if check := data.SyncCheck; check != nil && !check.Complete {
		return &nextAction{Do: "ask_user", Why: "Synchronization coverage is incomplete; resolve the reported inaccessible or unresolved identities."}
	}
	if data.Complete != nil && !*data.Complete {
		return &nextAction{Do: "ask_user", Why: "The account catalog is degraded; repair the reported account configuration before relying on complete coverage."}
	}
	if data.ContentComplete != nil && !*data.ContentComplete || data.Message != nil && !data.Message.ContentComplete && len(data.Message.MissingParts) > 0 {
		return &nextAction{Do: "ask_user", Why: "Content is incomplete; obtain the missing parts through a verified source before claiming completeness."}
	}
	if sync := data.SyncResult; sync != nil && sync.Triggered {
		next := &nextAction{Do: "check_state", Command: "sync", Args: []string{"--check"}, Why: "Synchronization was requested; observe server coverage before claiming it is complete."}
		if sync.AccountRef != "" {
			next.Args = append(next.Args, "--account", sync.AccountRef)
		}
		next.Args = append(next.Args, "--json")
		return next
	}
	return nil
}

func pendingDraftAction(ref string, reconcile bool) *nextAction {
	next := &nextAction{Do: "check_state", Why: "A retained attempt needs observation; do not repeat the original write."}
	if ref != "" {
		if reconcile {
			next.Command, next.Args = "drafts.reconcile", []string{"--ref", ref, "--json"}
		} else {
			attachNextRecovery(next, draftInspectRecovery(ref, false))
		}
	}
	return next
}
