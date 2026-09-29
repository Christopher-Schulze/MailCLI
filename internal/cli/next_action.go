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
		next.Do, next.Why = "stop", boundedWhy("Stop: ", failure.Message)
		if why, found := terminalWhy[failure.Code]; found {
			next.Why = why
		}
	case failure.environmentRepair || environmentFailure(failure.Code) || (previous != nil && previous.Do == "ask_user"):
		next.Do, next.Why = "ask_user", genericEnvironmentRepairWhy
		if why, found := environmentRepairWhy[failure.Code]; found {
			next.Why = why
		}
	case failure.Code == confirmationRequiredCode:
		next.Do, next.Why = "ask_user", "Ask the user to authorize this action, then rerun with --confirm."
	case guidance.Retryability == mail.RetryObserveRequired:
	case guidance.Retryability == mail.RetrySafe && guidance.ReplayAllowed:
		next.Do, next.Why = "retry", "The evidence permits retrying the original invocation."
		if failure.Code != "search_index_changed" {
			next.WaitSeconds = 1
		}
	case guidance.Retryability == mail.RetryUserInputRequired:
		next.Do, next.Why = "fix_input", boundedWhy("Fix the input: ", failure.Message)
	}
	attachNextRecovery(next, guidance.Recovery)
	if next.Do == "check_state" && next.Command != "" {
		// The command and its refs are separate fields; a ref inside the
		// sentence would push the replay warning past the length bound.
		next.Why = "Do not replay. Inspect the state with next.command and next.args."
	}
	return next
}

// confirmationRequiredCode marks an action that waits for the user's
// authorization; an agent must ask, never add the flag itself.
const confirmationRequiredCode = "confirmation_required"

// terminalWhy names the outcome for terminal codes whose message alone would
// not tell an agent that there is nothing left to do.
var terminalWhy = map[string]string{
	"message_already_trashed":        "The message is already in Trash; nothing to do.",
	"compose_automation_unsupported": "Scripted Mail compose is unavailable; use drafts create and drafts send, or drafts handoff.",
	"update_unsupported_platform":    "Self-update supports only macOS on Apple silicon; stop.",
	"unsupported_platform":           "This platform cannot provide the required safe file access; stop.",
}

// boundedWhy keeps next.why to one line of at most 120 runes.
func boundedWhy(prefix, detail string) string {
	why := []rune(prefix + strings.Join(strings.Fields(detail), " "))
	if len(why) > 120 {
		why = append(why[:119], '…')
	}
	return string(why)
}

const genericEnvironmentRepairWhy = "Repair the environment or permissions using the detailed guidance before continuing."

const fullDiskAccessWhy = "Ask the user to grant Full Disk Access to the calling app, or open Mail.app once, then retry."

// environmentRepairWhy is the single list of environment failures and the
// concrete user action each one needs; keep every sentence at most 120 runes.
var environmentRepairWhy = map[string]string{
	"initialization_failed":              "MailCLI could not start; fix the path or permission named in error.message, then retry.",
	"environment_unhealthy":              "Run `mailcli doctor --json`, fix each failing check it reports, then retry.",
	"imap_mutation_lock_unavailable":     "Create or permit the IMAP mutation lock directory named in error.message, then retry.",
	"mail_not_running":                   "Open Mail.app and let it finish loading, then retry the same command.",
	"mail_automation_denied":             "Ask the user to allow Mail control in System Settings > Privacy & Security > Automation, then retry.",
	"mail_recovery_required":             "Ask the user to quit and reopen Mail.app, then retry; inspect prior writes before replaying them.",
	"mail_access_gate_corrupt":           "Quit Mail.app and retry while it is stopped; never delete or replace mail-access.lock.",
	"mail_access_gate_failed":            "The Mail.app access gate failed; ask the user to quit Mail.app and retry; never delete mail-access.lock.",
	"mail_automation_unavailable":        "Mail.app automation is unavailable; ask the user to open Mail.app and allow automation, then retry.",
	"bridge_cleanup_failed":              "The Mail.app bridge could not confirm cleanup; ask the user to quit and reopen Mail.app, then retry.",
	"account_binding_changed":            "The binding changed while the password was entered; nothing was published. Ask the user, then rerun setup.",
	"special_use_mailbox_unresolved":     "Ask the user to fix the account's special-use mailbox in Mail.app; see `mailcli accounts list --json`.",
	"smtp_auth_failed":                   "SMTP rejected the stored password; ask the user to renew it with `mailcli send setup`, then retry.",
	"imap_auth_failed":                   "IMAP rejected the stored password; ask the user to renew it with `mailcli send setup`, then retry.",
	"smtp_tls_failed":                    "The SMTP TLS handshake failed; check the bound host, port and certificate, then retry.",
	"smtp_credentials_missing":           "No app-specific password is stored; ask the user to run `mailcli send setup`, then retry.",
	"imap_credentials_missing":           "No app-specific password is stored; ask the user to run `mailcli send setup`, then retry.",
	"smtp_utf8_unsupported":              "The SMTP server lacks SMTPUTF8; use ASCII addresses and headers or another account.",
	"transport_unsupported_provider":     "Direct transport supports Gmail and iCloud; bind explicit SMTP/IMAP hosts or use drafts handoff.",
	"editor_unavailable":                 "No editor is available; pass --editor or set VISUAL or EDITOR, then retry.",
	"editor_terminal_unavailable":        "The editor needs an interactive terminal; run this command from a TTY.",
	"account_disabled":                   "Ask the user to enable this account in Mail.app, then retry.",
	"account_degraded":                   "Read the degraded reason in `mailcli accounts list --json`, fix the account in Mail.app, then retry.",
	"account_identity_missing":           "Ask the user to configure the sender identity with `mailcli send setup`, then retry.",
	"account_binding_invalid":            "Correct the account binding with `mailcli send setup`; never replace it with guessed values.",
	"account_binding_host_invalid":       "Correct the bound SMTP/IMAP host and port with `mailcli send setup`, then retry.",
	"account_binding_provider_mismatch":  "Bind the sender alias and credential account to the same provider, then retry.",
	"account_binding_provider_invalid":   "Bind this account to a supported provider or explicit hosts with `mailcli send setup`, then retry.",
	"account_binding_unavailable":        "Fix access to the MailCLI account-binding file or its directory, then retry; keep its contents.",
	"mail_store_unavailable":             fullDiskAccessWhy,
	"mail_store_preferences_unavailable": fullDiskAccessWhy,
	"safe_mailbox_listing_unavailable":   fullDiskAccessWhy,
	"safe_search_unavailable":            fullDiskAccessWhy,
	"safe_message_listing_unavailable":   fullDiskAccessWhy,
	"mail_store_preferences_invalid":     "Ask the user to open Mail.app and restore a valid account configuration, then retry.",
}

func environmentFailure(code string) bool {
	_, found := environmentRepairWhy[code]
	return found
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
	} else if check != nil && !check.CountsMatch {
		return &nextAction{Do: "check_state", Why: "Local and server counts differ; Mail.app has not synced every mailbox yet. Check again after it synced."}
	}
	if result := data.NewMessages; result != nil {
		switch {
		case !result.Complete:
			return &nextAction{Do: "ask_user", Why: "Discovery is incomplete; inspect failures and mailbox reasons for account, credential, network or generation problems."}
		case result.NewCount > 0:
			return &nextAction{Do: "check_state", Why: "The local store lacks these server messages; run sync to have Mail.app fetch them."}
		}
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
