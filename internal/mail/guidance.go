package mail

import (
	"context"
	"errors"
	"io/fs"
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

// RecoveryGuidance contains supported command arguments, a concrete user
// instruction, or a durable operation identity. It never carries credentials
// or raw protocol secrets.
type RecoveryGuidance struct {
	Action      RecoveryAction `json:"action"`
	Command     string         `json:"command,omitempty"`
	Args        []string       `json:"args,omitempty"`
	OperationID string         `json:"operation_id,omitempty"`
	Instruction string         `json:"instruction,omitempty"`
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
	if command == "drafts.adopt" {
		var adoption *DraftAdoptionError
		if errors.As(err, &adoption) {
			return guidanceForDraftAdoption(adoption)
		}
	}
	if guidance, matched := guidanceForAccessGatePreflight(command, code); matched {
		return guidance
	}
	var attachmentOutcome *AttachmentSaveOutcomeError
	if errors.As(err, &attachmentOutcome) {
		return guidanceForAttachmentSaveOutcome(attachmentOutcome, code)
	}
	var mutation *transport.MutationOutcomeError
	if errors.As(err, &mutation) &&
		(mutation.Evidence.IsStore() || mutation.Evidence.Command == "COPY" || mutation.Evidence.Command == "MOVE") {
		return guidanceForMutationUnknown(err)
	}
	if rejection, rejected := transport.TaggedIMAPRejection(err); rejected {
		if command == "drafts.send" || command == "drafts.reconcile" {
			guidance := guidanceForMirrorUnknown()
			switch {
			case code == transport.CodeIMAPQuotaExceeded:
				guidance.Recovery.Instruction = "Free space in the Sent mailbox or correct its quota, then run drafts.reconcile; never repeat SMTP."
			case code == transport.CodeIMAPMailboxNotFound:
				guidance.Recovery.Instruction = "Restore or select the correct Sent mailbox, then run drafts.reconcile; never repeat SMTP."
			case code == transport.CodeIMAPAuthFailed:
				guidance.Recovery.Instruction = "Correct the bound IMAP credentials, then run drafts.reconcile; never repeat SMTP."
			case rejection.Status == "NO" && rejection.ResponseCode != nil && *rejection.ResponseCode == "UNAVAILABLE":
				guidance.Recovery.Instruction = "Wait for the IMAP service to recover, then run drafts.reconcile; never repeat SMTP."
			default:
				guidance.Recovery.Instruction = "Inspect error.imap_rejection, correct the server-side cause, then run drafts.reconcile; never repeat SMTP."
			}
			return guidance
		}
		if transport.IsTransientIMAPReadRejection(err) {
			return guidanceForRead()
		}
		switch code {
		case transport.CodeIMAPAuthFailed:
			// Preserve the existing credential-correction guidance below.
		case transport.CodeIMAPMailboxNotFound:
			return guidanceForReadCorrection("Correct the requested mailbox on the IMAP server, then retry the operation.")
		case transport.CodeIMAPQuotaExceeded:
			return OperationGuidance{
				Phase: OperationPhaseValidation, EffectCertainty: EffectNone,
				Retryability: RetryUserInputRequired, ReplayAllowed: false,
				Recovery: RecoveryGuidance{
					Action:      RecoveryCorrect,
					Instruction: "Free space in the target mailbox or choose a destination with sufficient quota, then finish the retained Sent mirror with drafts.reconcile; do not resubmit SMTP.",
				},
			}
		default:
			return guidanceForTerminalRead()
		}
	}
	if isMailboxMutationCommand(command) && transport.IsAmbiguousMailbox(err) {
		return OperationGuidance{
			Phase: OperationPhaseValidation, EffectCertainty: EffectNone,
			Retryability: RetryUserInputRequired, ReplayAllowed: false,
			Recovery: RecoveryGuidance{
				Action:      RecoveryCorrect,
				Instruction: "Correct the conflicting IMAP mailbox identities, either by renaming a colliding mailbox or correcting duplicate special-use assignments, refresh the mailbox list, and rerun the command only after the requested mailbox resolves uniquely.",
			},
		}
	}
	if command != "batch" && transport.IsResourceLimitExceeded(err) {
		phase := OperationPhaseRead
		if effectfulCommand(command) {
			phase = defaultPhase(command)
		}
		if isMailboxMutationCommand(command) {
			phase = OperationPhaseMutation
		}
		return OperationGuidance{
			Phase: phase, EffectCertainty: EffectNone,
			Retryability: RetryTerminal, ReplayAllowed: false,
			Recovery: RecoveryGuidance{Action: RecoveryInspect},
		}
	}
	if !effectfulCommand(command) && errors.Is(err, fs.ErrPermission) {
		return guidanceForReadCorrection("Grant Full Disk Access to the calling app, or correct the file permissions before retrying this read.")
	}
	if command == "messages.get" && code == "not_found" {
		return guidanceForReadCorrection("Obtain a fresh valid message reference before retrying this read.")
	}
	if code == "not_found" && !effectfulCommand(command) {
		return guidanceForReadCorrection("Correct the missing or stale reference before retrying this read.")
	}
	if !effectfulCommand(command) {
		switch code {
		case "mail_busy", "mail_automation_timeout", "mail_process_changed":
			return guidanceForRead()
		case "invalid_request":
			// The bridge uses this for malformed internal request envelopes too,
			// so a supported read cannot safely treat it as caller input.
			return guidanceForUnknownRead()
		}
		if code == transport.CodeIMAPFetchFailed && !transport.IsTransientReadFailure(err) {
			return guidanceForTerminalRead()
		}
		if guidance, matched := guidanceForKnownReadError(code); matched {
			return guidance
		}
	}
	if code == "search_budget_too_small" {
		guidance := OperationGuidance{
			Phase: OperationPhaseRead, EffectCertainty: EffectNone,
			Retryability: RetryUserInputRequired, ReplayAllowed: false,
			Recovery: RecoveryGuidance{Action: RecoveryCorrect},
		}
		return guidance
	}
	if isInputErrorCode(code) {
		return guidanceForInput()
	}
	switch {
	case code == "draft_revision_conflict" || code == "draft_revision_unavailable":
		return OperationGuidance{Phase: OperationPhaseValidation, EffectCertainty: EffectNone, Retryability: RetryObserveRequired, ReplayAllowed: false, Recovery: RecoveryGuidance{Action: RecoveryInspect}}
	case code == "draft_busy":
		return OperationGuidance{
			Phase: OperationPhaseExecution, EffectCertainty: EffectNone,
			Retryability: RetryObserveRequired, ReplayAllowed: false,
			Recovery: RecoveryGuidance{Action: RecoveryObserve},
		}
	case code == "search_cursor_stale":
		return OperationGuidance{
			Phase: OperationPhaseRead, EffectCertainty: EffectNone,
			Retryability: RetryUserInputRequired, ReplayAllowed: false,
			Recovery: RecoveryGuidance{Action: RecoveryCorrect},
		}
	case code == "search_index_changed":
		return OperationGuidance{
			Phase: OperationPhaseRead, EffectCertainty: EffectNone,
			Retryability: RetrySafe, ReplayAllowed: true,
			Recovery: RecoveryGuidance{Action: RecoveryRetry},
		}
	case code == "batch_canceled":
		return OperationGuidance{Phase: OperationPhaseExecution, EffectCertainty: EffectNone, Retryability: RetrySafe, ReplayAllowed: true, Recovery: RecoveryGuidance{Action: RecoveryRetry}}
	case code == "initialization_failed":
		return guidanceForInput()
	case code == transport.CodeIMAPLockUnavailable:
		return guidanceForInput()
	case code == transport.CodeIMAPAccountBusy:
		return OperationGuidance{
			Phase: OperationPhaseMutation, EffectCertainty: EffectNone,
			Retryability: RetrySafe, ReplayAllowed: true,
			Recovery: RecoveryGuidance{Action: RecoveryRetry},
		}
	case code == "finalization_failed":
		return OperationGuidance{Phase: OperationPhaseCleanup, EffectCertainty: EffectUnknown, Retryability: RetryObserveRequired, Recovery: RecoveryGuidance{Action: RecoveryInspect}}
	case code == "serialization_failed":
		return OperationGuidance{Phase: OperationPhaseExecution, EffectCertainty: EffectUnknown, Retryability: RetryTerminal, Recovery: RecoveryGuidance{Action: RecoveryInspect}}
	case transport.IsSMTPSourceInvalid(err):
		if command == "drafts.send" {
			return OperationGuidance{
				Phase: OperationPhaseSubmission, EffectCertainty: EffectNone,
				Retryability: RetryObserveRequired,
				Recovery:     RecoveryGuidance{Action: RecoveryInspect},
			}
		}
	case transport.IsSMTPDataIncomplete(err):
		if command == "drafts.send" {
			return OperationGuidance{
				Phase: OperationPhaseSubmission, EffectCertainty: EffectNone,
				Retryability: RetrySafe, ReplayAllowed: true,
				Recovery: RecoveryGuidance{Action: RecoveryRetry},
			}
		}
	case transport.IsAppendIncomplete(err):
		if command == "drafts.send" {
			return guidanceForMirrorUnknown()
		}
		if command == "drafts.reconcile" {
			return OperationGuidance{
				Phase: OperationPhaseMirror, EffectCertainty: EffectNone,
				Retryability: RetrySafe, ReplayAllowed: true,
				Recovery: RecoveryGuidance{Action: RecoveryRetry},
			}
		}
		return guidanceForUnknown(defaultPhase(command))
	case code == "operation_canceled" || code == "draft_operation_canceled" || code == "operation_timeout" || transport.IsTransientTransportFailure(err):
		if effectfulCommand(command) {
			return guidanceForUnknown(defaultPhase(command))
		}
		if code == "operation_canceled" || code == "draft_operation_canceled" || code == "operation_timeout" || transport.IsTransientReadFailure(err) {
			return guidanceForRead()
		}
		return guidanceForUnknownRead()
	case transport.IsRejectedSubmission(err):
		return guidanceForSMTPRejection(err)
	case transport.IsSubmissionOutcomeUnknown(err):
		return guidanceForSendUnknown()
	case transport.IsMutationOutcomeUnknown(err) || transport.IsFlagsStateMismatch(err):
		return guidanceForMutationUnknown(err)
	case transport.IsMessageNotFound(err):
		var outcome *transport.MutationOutcomeError
		if errors.As(err, &outcome) && outcome.Evidence.IsStore() {
			return guidanceForMutationUnknown(err)
		}
	case transport.IsAppendOutcomeUnknown(err):
		return guidanceForMirrorUnknown()
	case code == "send_mirror_pending" || code == "send_mirror_outcome_unknown":
		return guidanceForMirrorUnknown()
	case code == "send_outcome_unknown" || code == "send_outcome_unverifiable" || code == "send_state_unknown":
		return guidanceForSendUnknown()
	case code == "handoff_outcome_unknown" || code == "handoff_retry_blocked" || code == "handoff_attachment_cleanup_failed" || code == "handoff_claim_cleanup_failed":
		return guidanceForHandoffUnknown()
	case code == "handoff_canceled_before_dispatch":
		return OperationGuidance{
			Phase: OperationPhaseExecution, EffectCertainty: EffectNone,
			Retryability: RetrySafe, ReplayAllowed: true,
			Recovery: RecoveryGuidance{Action: RecoveryRetry},
		}
	case transport.IsAppendFailed(err):
		if command == "drafts.send" || command == "drafts.reconcile" {
			return guidanceForMirrorUnknown()
		}
	case transport.IsSourceTooLarge(err):
		return OperationGuidance{Phase: OperationPhaseHydration, EffectCertainty: EffectNone, Retryability: RetryTerminal, Recovery: RecoveryGuidance{Action: RecoveryInspect}}
	case code == "output_too_large":
		return OperationGuidance{Phase: OperationPhaseExecution, EffectCertainty: EffectNone, Retryability: RetryUserInputRequired, Recovery: RecoveryGuidance{Action: RecoveryCorrect}}
	case code == "content_export_too_large":
		return OperationGuidance{Phase: OperationPhaseExecution, EffectCertainty: EffectNone, Retryability: RetryTerminal, Recovery: RecoveryGuidance{Action: RecoveryInspect}}
	case transport.IsConfigurationFailure(err):
		if !effectfulCommand(command) {
			if transport.IsTLSVerificationFailure(err) {
				return guidanceForReadCorrection("Correct the IMAP certificate trust or configured hostname before retrying this read; do not disable TLS verification.")
			}
			return guidanceForReadCorrection("Correct the account credentials or IMAP security configuration before retrying this read.")
		}
		return guidanceForInput()
	}
	if effectfulCommand(command) {
		return guidanceForUnknown(defaultPhase(command))
	}
	return guidanceForUnknownRead()
}

func isMailStoreAvailabilityError(code string) bool {
	return code == "mail_store_unavailable" || code == "mail_store_preferences_unavailable" ||
		code == "safe_mailbox_listing_unavailable" || code == "safe_search_unavailable"
}

func guidanceForAccessGatePreflight(command string, code string) (OperationGuidance, bool) {
	switch code {
	case "mail_access_gate_corrupt", "mail_access_gate_unsafe":
	case "mail_not_running":
		if !effectfulCommand(command) {
			return OperationGuidance{}, false
		}
	default:
		return OperationGuidance{}, false
	}
	phase := OperationPhaseRead
	if effectfulCommand(command) {
		phase = OperationPhaseExecution
	}
	instruction := ""
	retryability := RetryUserInputRequired
	action := RecoveryCorrect
	switch code {
	case "mail_access_gate_corrupt":
		instruction = "Quit Mail.app and retry this same operation while it is stopped. MailCLI clears only verified recovery-state contents and returns mail_not_running; then reopen Mail.app and retry. Never delete or replace mail-access.lock."
	case "mail_access_gate_unsafe":
		retryability = RetryTerminal
		action = RecoveryInspect
		instruction = "No Mail.app action was dispatched. Inspect the existing MailCLI access directory and lock ownership, type, link count, and identity; preserve the lock and use a verified recovery plan before retrying. Never delete or replace mail-access.lock."
	case "mail_not_running":
		instruction = "Open Mail.app, allow it to finish loading, then retry this same operation; no action was dispatched."
	}
	return OperationGuidance{
		Phase: phase, EffectCertainty: EffectNone,
		Retryability: retryability, ReplayAllowed: false,
		Recovery: RecoveryGuidance{Action: action, Instruction: instruction},
	}, true
}

func guidanceForKnownReadError(code string) (OperationGuidance, bool) {
	if isMailStoreAvailabilityError(code) {
		return guidanceForReadCorrection("Grant Full Disk Access to the calling app, or open Mail once to create its local store, then retry the read."), true
	}
	if instruction, matched := guidanceForReadAccessError(code); matched {
		return guidanceForReadCorrection(instruction), true
	}
	if instruction, matched := guidanceForReadSourceError(code); matched {
		return guidanceForReadCorrection(instruction), true
	}
	if isTerminalReadError(code) {
		return guidanceForTerminalRead(), true
	}
	return OperationGuidance{}, false
}

func guidanceForDraftAdoption(adoption *DraftAdoptionError) OperationGuidance {
	if adoption.PublicationStarted || adoption.StagingRetained {
		return OperationGuidance{
			Phase: OperationPhaseExecution, EffectCertainty: EffectUnknown,
			Retryability: RetryObserveRequired, ReplayAllowed: false,
			Recovery: RecoveryGuidance{
				Action: RecoveryInspect, Command: "drafts.list", Args: []string{"--json"},
				Instruction: "Do not replay adoption. Inspect the draft list for the new local reference; if it exists, run drafts inspect --ref REF --json. Preserve and inspect any retained staging artifacts.",
			},
		}
	}
	if adoption.ErrorCode() == "adopt_source_incomplete" {
		return guidanceForReadCorrection("Open the source draft in Mail.app to materialize its full content, then retry adoption.")
	}
	return GuidanceForError("drafts.open", adoption.Err)
}

func guidanceForReadAccessError(code string) (string, bool) {
	switch code {
	case "mail_store_preferences_invalid":
		return "Open Mail.app and restore a valid local store configuration before retrying this read.", true
	case "account_disabled":
		return "Enable this account in Mail.app, then retry the read.", true
	case "account_degraded":
		return "Inspect `mailcli accounts list --json` for the account's degraded reason and remediation, correct the account in Mail.app, then retry the read.", true
	case "account_identity_missing":
		return "Configure the account's verified sender identity and credentials with `mailcli send setup`, then retry the read.", true
	case "account_binding_invalid":
		return "Restore a valid account binding or correct it through the supported `mailcli send setup` flow; preserve existing binding data and do not replace it with guessed values.", true
	case "account_binding_host_invalid":
		return "Correct the account binding's host and port values to satisfy the existing MailCLI endpoint rules, then retry the read.", true
	case "account_binding_provider_mismatch":
		return "Configure the sender alias and credential account for the same supported provider, then retry the read.", true
	case "account_binding_version_unsupported":
		return "Use a MailCLI build that supports the existing account-binding version; preserve the binding file and retry with that build.", true
	case "account_reference_version_unsupported":
		return "Use a MailCLI build that supports the account-reference version in the local catalog; preserve the catalog entry and retry with that build.", true
	case "account_binding_unavailable":
		return "Correct access to the existing account-binding file or its parent directory, then retry the read; preserve the file contents.", true
	case "account_binding_stale":
		return "Inspect `mailcli accounts list --json`, then correct the binding to an enabled account through `mailcli send setup` before retrying this read; preserve existing binding data.", true
	case "mail_automation_denied":
		return "Allow the calling app to control Mail in System Settings > Privacy & Security > Automation, then retry the read.", true
	case "mail_not_running":
		return "Open Mail.app and allow its account catalog to finish loading, then retry the read.", true
	case "mail_recovery_required":
		return "Quit and reopen Mail.app to resolve its retained operation state, then retry this read; inspect any prior effectful operation before replaying it.", true
	default:
		return "", false
	}
}

func guidanceForReadSourceError(code string) (string, bool) {
	switch code {
	case "invalid_reference":
		return "Use a current opaque reference emitted by the matching MailCLI listing command; do not edit the reference manually.", true
	case "ambiguous_reference":
		return "Refresh the account or mailbox listing, resolve any duplicate account IDs or mailbox names in Mail.app, then retry with a current reference that selects one target.", true
	case "stale_cursor":
		return "Restart this Mail.app listing without the stale --cursor value, then continue with the fresh next_cursor returned by the first page.", true
	case "content_unavailable":
		return "A consumed send receipt has no draft body to export; remove `--export` and inspect the retained receipt metadata instead.", true
	case "content_incomplete":
		return "Open Mail.app and let it finish downloading this message, then retry the export after the message reports complete content.", true
	case "local_only_mailbox":
		return "Read this message from Mail.app's local store, or choose a server-backed mailbox before retrying IMAP hydration.", true
	case "store_bound_reference_required":
		return "Resolve a current store-bound message reference from this Mail store, then retry the read.", true
	case "mailbox_uidvalidity_changed":
		return "Resolve a fresh message reference after Mail.app synchronizes the changed mailbox identity, then retry the read.", true
	case "stale_reference", transport.CodeIMAPMailboxNotFound, transport.CodeIMAPMessageNotFound,
		transport.CodeIMAPAmbiguousMailbox:
		return "Correct the mailbox or message reference, then retry this read.", true
	case "message_source_missing":
		return "Open Mail once and allow it to download this message, then retry the read.", true
	case "raw_source_partial":
		return "Make a complete source available in Mail.app or through the configured targeted IMAP read, then retry; use only a verified complete source.", true
	case transport.CodeIMAPMessageUIDUnknown:
		return "Refresh the local Mail catalog or provide a fresh Message-ID-backed reference with a verified mailbox UID, then retry this read.", true
	case transport.CodeIMAPAmbiguousMessageID:
		return "Resolve the ambiguous message matches in the target mailbox, or obtain a fresh reference with a uniquely verified UID and UIDVALIDITY, then retry.", true
	case "search_count_limit_exceeded":
		return "Narrow the search or deliberately raise its candidate limit before retrying.", true
	case "ambiguous_attachment":
		return "Choose an attachment identifier that resolves to one attachment before retrying.", true
	default:
		return "", false
	}
}

func isTerminalReadError(code string) bool {
	switch code {
	case "account_catalog_incomplete", "attachment_resource_limit", "ambiguous_mail_store_generation",
		"account_reference_corrupt", "account_reference_invalid", "ambiguous_message_source",
		"content_export_changed", "imap_flag_read_unsupported", "invalid_emlx", "invalid_mailbox_cache",
		"invalid_message_source", "mail_store_not_read_only", "mailbox_cache_malformed",
		"mail_store_path_mismatch", "mailbox_catalog_incomplete", "mailbox_info_malformed", "mime_resource_limit",
		"raw_source_too_large", "store_changed", "unsafe_message_source", "unsupported_mail_store_schema",
		"search_unavailable", "draft_state_error", "send_receipt_invalid", transport.CodeIMAPMessageUIDMismatch,
		transport.CodeIMAPResponseMalformed, transport.CodeIMAPUIDValidityUnknown:
		return true
	default:
		return false
	}
}

func guidanceForTerminalRead() OperationGuidance {
	return OperationGuidance{
		Phase: OperationPhaseRead, EffectCertainty: EffectNone,
		Retryability: RetryTerminal, ReplayAllowed: false,
		Recovery: RecoveryGuidance{Action: RecoveryInspect},
	}
}

func guidanceForAttachmentSaveOutcome(outcome *AttachmentSaveOutcomeError, code string) OperationGuidance {
	phase := outcome.Phase
	if phase == "" {
		phase = OperationPhaseExecution
	}
	if outcome.EffectCertainty == EffectNone {
		return guidanceForAttachmentSaveNoEffect(outcome.Cause, code, phase)
	}
	certainty := outcome.EffectCertainty
	if certainty != EffectComplete && certainty != EffectPartial && certainty != EffectUnknown {
		certainty = EffectUnknown
	}
	return OperationGuidance{
		Phase: phase, EffectCertainty: certainty, Retryability: RetryObserveRequired,
		ReplayAllowed: false, Recovery: RecoveryGuidance{Action: RecoveryInspect},
	}
}

func guidanceForAttachmentSaveNoEffect(cause error, code string, phase OperationPhase) OperationGuidance {
	if guidance, matched := guidanceForAttachmentSaveCorrection(cause, code); matched {
		return guidance
	}
	if code == "attachment_changed" ||
		(code == transport.CodeIMAPFetchFailed && !transport.IsTransientReadFailure(cause)) {
		return OperationGuidance{
			Phase: phase, EffectCertainty: EffectNone, Retryability: RetryTerminal,
			Recovery: RecoveryGuidance{Action: RecoveryInspect},
		}
	}
	if guidance, matched := guidanceForKnownReadError(code); matched {
		return guidance
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) ||
		code == "operation_canceled" || code == "operation_timeout" ||
		code == "mail_busy" || code == "mail_automation_timeout" || code == "mail_process_changed" ||
		transport.IsTransientReadFailure(cause) {
		guidance := guidanceForRead()
		guidance.Phase = phase
		return guidance
	}
	if transport.IsResourceLimitExceeded(cause) {
		return OperationGuidance{
			Phase: phase, EffectCertainty: EffectNone, Retryability: RetryTerminal,
			Recovery: RecoveryGuidance{Action: RecoveryInspect},
		}
	}
	return OperationGuidance{
		Phase: phase, EffectCertainty: EffectNone, Retryability: RetryObserveRequired,
		Recovery: RecoveryGuidance{Action: RecoveryInspect},
	}
}

func guidanceForAttachmentSaveCorrection(cause error, code string) (OperationGuidance, bool) {
	if isAttachmentSaveInputError(code) || transport.IsConfigurationFailure(cause) {
		guidance := guidanceForInput()
		guidance.EffectCertainty = EffectNone
		return guidance, true
	}
	if errors.Is(cause, fs.ErrPermission) {
		return guidanceForReadCorrection("Correct access to the Mail source or output directory before retrying this attachment save."), true
	}
	if code == "not_found" {
		return guidanceForReadCorrection("Refresh the message and attachment listings, then retry with a current message reference and an attachment ID present on that message."), true
	}
	if code == "attachment_not_downloaded" {
		return guidanceForReadCorrection("Download the attachment in Mail.app or configure targeted IMAP hydration, then retry the save."), true
	}
	return OperationGuidance{}, false
}

func isAttachmentSaveInputError(code string) bool {
	switch code {
	case "invalid_argument", "invalid_input", "missing_required":
		return true
	default:
		return false
	}
}

func guidanceErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if code := transport.ErrorCode(err); code != "" {
		return code
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
	case "batch", "update", "attachments.save", "attachment_save", "send.setup", "sync",
		"drafts.create", "drafts.edit", "drafts.handoff", "drafts.update", "drafts.save", "drafts.adopt",
		"drafts.send", "drafts.reconcile", "drafts.handoff-reconcile", "drafts.discard", "drafts.prune",
		"messages.reply", "messages.forward", "messages.mark", "messages.move",
		"messages.copy", "messages.delete":
		return true
	default:
		return false
	}
}

func isMailboxMutationCommand(command string) bool {
	switch command {
	case "messages.mark", "messages.move", "messages.copy", "messages.delete",
		BatchOperationMark, BatchOperationMove, BatchOperationCopy, BatchOperationDelete:
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
	case command == "drafts.handoff-reconcile":
		return OperationPhaseCleanup
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

func guidanceForReadCorrection(instruction string) OperationGuidance {
	return OperationGuidance{
		Phase: OperationPhaseRead, EffectCertainty: EffectNone,
		Retryability: RetryUserInputRequired, ReplayAllowed: false,
		Recovery: RecoveryGuidance{Action: RecoveryCorrect, Instruction: instruction},
	}
}

func guidanceForUnknownRead() OperationGuidance {
	return OperationGuidance{
		Phase: OperationPhaseRead, EffectCertainty: EffectNone,
		Retryability: RetryObserveRequired, ReplayAllowed: false,
		Recovery: RecoveryGuidance{Action: RecoveryInspect},
	}
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

func guidanceForHandoffUnknown() OperationGuidance {
	return OperationGuidance{
		Phase: OperationPhaseExecution, EffectCertainty: EffectUnknown,
		Retryability: RetryObserveRequired, ReplayAllowed: false,
		Recovery: RecoveryGuidance{Action: RecoveryReconcile},
	}
}

func guidanceForMutationUnknown(err error) OperationGuidance {
	guidance := OperationGuidance{Phase: OperationPhaseMutation, EffectCertainty: EffectUnknown, Retryability: RetryObserveRequired, Recovery: RecoveryGuidance{Action: RecoveryObserve}}
	var outcome *transport.MutationOutcomeError
	if errors.As(err, &outcome) {
		guidance.Recovery.OperationID = outcome.Evidence.OperationID
		if outcome.Evidence.HasPartialEffects() {
			guidance.EffectCertainty = EffectPartial
		} else if outcome.Evidence.StoreRejectedOrNotStarted() ||
			((outcome.Evidence.Command == "COPY" || outcome.Evidence.Command == "MOVE") &&
				(outcome.Evidence.Outcome == transport.MutationOutcomeNotStarted ||
					outcome.Evidence.Outcome == transport.MutationOutcomeRejected)) {
			guidance.EffectCertainty = EffectNone
		}
		if (outcome.Evidence.Command == "COPY" || outcome.Evidence.Command == "MOVE") && outcome.Evidence.Outcome == transport.MutationOutcomeNotStarted &&
			!outcome.Evidence.HasPartialEffects() {
			if isInputErrorCode(guidanceErrorCode(err)) || transport.IsConfigurationFailure(err) {
				guidance.Retryability, guidance.Recovery.Action = RetryUserInputRequired, RecoveryCorrect
			} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || transport.IsTransientTransportFailure(err) {
				guidance.Retryability, guidance.ReplayAllowed = RetrySafe, true
				guidance.Recovery.Action = RecoveryRetry
			}
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
