package mail

import "mailcli/internal/transport"

// preEffectCorrectionCodes fail before any external effect in every command
// path (identity, binding, store, credential, editor and precondition checks).
// The caller corrects input, configuration or permissions before a new attempt.
var preEffectCorrectionCodes = map[string]bool{
	"account_binding_account_degraded": true, "account_binding_ambiguous": true, "account_binding_host_invalid": true,
	"account_binding_invalid": true, "account_binding_missing": true, "account_binding_provider_invalid": true,
	"account_binding_provider_mismatch": true, "account_binding_stale": true, "account_binding_version_unsupported": true,
	"account_catalog_incomplete": true, "account_degraded": true, "account_disabled": true, "account_identity_missing": true,
	"account_no_email": true, "account_not_found": true, "account_reference_invalid": true,
	"account_reference_version_unsupported": true, "adopt_source_incomplete": true, "ambiguous_attachment": true,
	"ambiguous_mail_store_generation": true, "attachment_not_downloaded": true, "draft_mutation_confirmation_required": true,
	"editor_canceled": true, "editor_failed": true, "editor_terminal_unavailable": true, "editor_unavailable": true,
	"forward_source_incomplete": true, "handoff_attachment_changed": true, "handoff_attachment_missing": true,
	"handoff_attachment_unreadable": true, "handoff_attempt_mismatch": true, "handoff_not_dispatched": true,
	"handoff_not_found": true, "imap_credentials_missing": true, "interactive_required": true,
	"keychain_delete_failed": true, "keychain_invalid_identifier": true, "keychain_item_duplicate": true,
	"keychain_item_not_found": true, "keychain_load_failed": true, "keychain_store_failed": true,
	"keychain_unsupported": true, "mail_service_unavailable": true, "mail_store_preferences_invalid": true,
	"mail_store_preferences_unavailable": true, "mail_store_unavailable": true, "message_source_missing": true,
	"not_found": true, "prune_candidate_limit_exceeded": true, "raw_source_partial": true,
	"ambiguous_mailbox": true, "stale_reference": true, "attachment_changed": true,
	"safe_write_unavailable": true, "send_transport_unavailable": true, "smtp_credentials_missing": true,
	"unsupported_mail_store_schema":      true,
	transport.CodeIMAPAmbiguousMessageID: true, transport.CodeIMAPMessageUIDUnknown: true,
	transport.CodeLocalOnlyMailbox: true, transport.CodeUnsupportedProvider: true,
}

// preEffectInspectionCodes also fail before any external effect, but a guessed
// correction is unsafe: the caller inspects the reported state first.
var preEffectInspectionCodes = map[string]bool{
	"account_reference_corrupt": true, "ambiguous_message_source": true, "attachment_resource_limit": true,
	"mail_store_not_read_only": true, "mail_store_path_mismatch": true, "mime_resource_limit": true,
	transport.CodeMessageAlreadyTrashed: true,
}

// preEffectRetryCodes are transient contention or network failures before any
// effect; the unchanged invocation may run again.
var preEffectRetryCodes = map[string]bool{
	"account_binding_busy": true, "prune_state_changed": true,
	"update_busy": true, "update_check_failed": true, "update_download_failed": true,
}

// preEffectTerminalCodes are deterministic refusals before any effect: an
// untrusted or invalid release, an unsupported platform or capability, or a
// state file that failed an integrity check. Repeating cannot help.
var preEffectTerminalCodes = map[string]bool{
	"account_binding_unsafe": true, "capability_schema_invalid": true, "compose_automation_unsupported": true,
	"list_identity_invalid": true, "send_receipt_expired": true, "send_receipt_unavailable": true, "unsupported_platform": true,
	"update_checksum_invalid": true, "update_checksum_mismatch": true, "update_host_untrusted": true,
	"update_lock_failed": true, "update_package_invalid": true, "update_package_missing": true,
	"update_redirect_invalid": true, "update_redirect_limit": true, "update_signature_invalid": true,
	"update_unsupported_platform": true, "update_url_insecure": true, "update_url_invalid": true,
	"update_url_invalid_port": true,
}

// readTerminalCodes are integrity refusals that prove no effect only for reads;
// a draft mutation can meet them during cleanup after an external effect.
var readTerminalCodes = map[string]bool{"draft_lock_changed": true, "draft_lock_unsafe": true}

func guidanceForPreEffect(command, code string) (OperationGuidance, bool) {
	phase := OperationPhaseValidation
	if !effectfulCommand(command) {
		phase = OperationPhaseRead
	}
	switch {
	case readTerminalCodes[code] && !effectfulCommand(command):
		return OperationGuidance{
			Phase: phase, EffectCertainty: EffectNone,
			Retryability: RetryTerminal, Recovery: RecoveryGuidance{Action: RecoveryInspect},
		}, true
	case preEffectCorrectionCodes[code]:
		return OperationGuidance{
			Phase: phase, EffectCertainty: EffectNone,
			Retryability: RetryUserInputRequired, Recovery: RecoveryGuidance{Action: RecoveryCorrect},
		}, true
	case preEffectInspectionCodes[code], preEffectTerminalCodes[code]:
		return OperationGuidance{
			Phase: phase, EffectCertainty: EffectNone,
			Retryability: RetryTerminal, Recovery: RecoveryGuidance{Action: RecoveryInspect},
		}, true
	case preEffectRetryCodes[code]:
		return OperationGuidance{
			Phase: phase, EffectCertainty: EffectNone,
			Retryability: RetrySafe, ReplayAllowed: true, Recovery: RecoveryGuidance{Action: RecoveryRetry},
		}, true
	}
	return OperationGuidance{}, false
}
