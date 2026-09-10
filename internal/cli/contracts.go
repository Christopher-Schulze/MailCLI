package cli

import "strings"

// commandContract is the single source of truth for the public command
// manifest and the process dependencies selected before dispatch. The
// runner registry below owns execution only; it does not repeat capability
// metadata or dependency decisions.
type commandContract struct {
	ID string
	// metadata stores capability values and dispatch flags as compact IDs; the
	// public strings are materialized only when the manifest is published.
	metadata [7]uint8
}

const (
	metadataEffectClass = iota
	metadataConfirmation
	metadataStoreDependency
	metadataMailAppDependency
	metadataResultStates
	metadataMailService
	metadataFlags
)

const (
	textNone uint8 = iota
	textRead
	textLocalWrite
	textLocalWriteIMAPWrite
	textFilesystemWrite
	textVisibleCompose
	textUnsupported
	textSMTPSend
	textKeychainWrite
	textIMAPWrite
	textMailWrite
	textBatch
	textRequiredFlag
	textDraftFlag
	textRequiredAndDraftFlags
	textMailStore
	textDraftStore
	textDraftStoreMailStoreIfBaseline
	textOptionalAutomation
	textFallbackAutomation
	textSystemComposeService
	textOptional
)

func contractTextString(text uint8) string {
	switch text {
	case textRead:
		return "read"
	case textLocalWrite:
		return "local-write"
	case textLocalWriteIMAPWrite:
		return "local-write+imap-write"
	case textFilesystemWrite:
		return "filesystem-write"
	case textVisibleCompose:
		return "visible-compose"
	case textUnsupported:
		return "unsupported"
	case textSMTPSend:
		return "smtp-send"
	case textKeychainWrite:
		return "keychain-write"
	case textIMAPWrite:
		return "imap-write"
	case textMailWrite:
		return "mail-write"
	case textBatch:
		return "batch"
	case textRequiredFlag:
		return "required-flag"
	case textDraftFlag:
		return "draft-flag"
	case textRequiredAndDraftFlags:
		return "required-and-draft-flags"
	case textMailStore:
		return "mail-store"
	case textDraftStore:
		return "draft-store"
	case textDraftStoreMailStoreIfBaseline:
		return "draft-store+mail-store-if-baseline"
	case textOptionalAutomation:
		return "optional-automation"
	case textFallbackAutomation:
		return "fallback-automation"
	case textSystemComposeService:
		return "system-compose-service"
	case textOptional:
		return "optional"
	default:
		return "none"
	}
}

const (
	resultAvailable uint8 = iota
	resultUpdated
	resultHealthy
	resultAccounts
	resultComplete
	resultResolved
	resultSearch
	resultCompletePartial
	resultSaved
	resultCreated
	resultOpened
	resultComposeUnsupported
	resultSent
	resultSetup
	resultReconcile
	resultDiscarded
	resultPruned
	resultMoved
	resultCopied
	resultSync
	resultDeleted
	resultHandoff
	resultHandoffReconcile
)

// NUL-delimited state sets avoid one slice header per command in the release
// image while preserving the manifest's ordered result-state contract.
var resultStateTable = []string{
	"available",
	"updated\x00up_to_date",
	"healthy\x00unhealthy",
	"complete\x00partial\x00bounded_identity_coverage",
	"complete",
	"resolved",
	"complete\x00partial\x00search_cursor_stale\x00search_index_changed\x00search_count_limit_exceeded",
	"complete\x00partial",
	"saved",
	"created",
	"opened",
	"compose_automation_unsupported",
	"sent\x00sent_mirror_pending",
	"stored\x00removed",
	"sent_store_observed\x00accepted_by_mail\x00sent\x00sent_mirror_pending\x00outcome_unknown",
	"discarded",
	"listed\x00pruned",
	"moved",
	"copied",
	"triggered\x00checked_complete\x00checked_incomplete",
	"deleted",
	"confirmed_opened\x00confirmed_failed\x00outcome_unknown\x00canceled_before_dispatch",
	"confirmed_opened\x00confirmed_failed",
}

func resultStateValues(states uint8) []string {
	if states < uint8(len(resultStateTable)) {
		return strings.Split(resultStateTable[states], "\x00")
	}
	return strings.Split(resultStateTable[resultComplete], "\x00")
}

const (
	mailServiceNotRequired uint8 = iota
	mailServiceAlwaysRequired
	mailServiceForArguments
	mailServiceForReconcile
)

const (
	commandPublished uint8 = 1 << iota
	commandNeedsSignal
	commandNeedsMainThread
)

func newCommandContract(
	id string,
	effectClass uint8,
	confirmation uint8,
	storeDependency uint8,
	mailAppDependency uint8,
	mailService uint8,
	requiresSignal bool,
	requiresMainThread bool,
	resultStates uint8,
) commandContract {
	return commandContract{
		ID: id,
		metadata: [7]uint8{
			effectClass, confirmation, storeDependency, mailAppDependency,
			resultStates, mailService,
			commandPublished | boolFlag(commandNeedsSignal, requiresSignal) | boolFlag(commandNeedsMainThread, requiresMainThread),
		},
	}
}

func boolFlag(flag uint8, enabled bool) uint8 {
	if enabled {
		return flag
	}
	return 0
}

func commandRequiresMailService(contract commandContract, args []string) bool {
	switch contract.metadata[metadataMailService] {
	case mailServiceAlwaysRequired:
		return true
	case mailServiceForArguments:
		return len(args) > 0 && !helpOnly(args)
	case mailServiceForReconcile:
		return draftReconcileCommandRequired(args)
	default:
		return false
	}
}

func commandIsPublished(contract commandContract) bool {
	return contract.metadata[metadataFlags]&commandPublished != 0
}

func commandNeedsSignalFor(contract commandContract) bool {
	return contract.metadata[metadataFlags]&commandNeedsSignal != 0
}

func commandNeedsMainThreadFor(contract commandContract) bool {
	return contract.metadata[metadataFlags]&commandNeedsMainThread != 0
}

func commandCapabilityFor(contract commandContract) commandCapability {
	return commandCapability{
		ID:                contract.ID,
		Schema:            schemaForCommand(contract.ID),
		EffectClass:       contractTextString(contract.metadata[metadataEffectClass]),
		Confirmation:      contractTextString(contract.metadata[metadataConfirmation]),
		StoreDependency:   contractTextString(contract.metadata[metadataStoreDependency]),
		MailAppDependency: contractTextString(contract.metadata[metadataMailAppDependency]),
		ResultStates:      resultStateValues(contract.metadata[metadataResultStates]),
	}
}

// commandContracts stays ordered so the human and JSON manifests remain
// deterministic. Every command ID is also resolved here for pre-dispatch
// dependency checks.
var commandContracts = []commandContract{
	newCommandContract("capabilities", textRead, textNone, textNone, textNone, mailServiceNotRequired, false, false, resultAvailable),
	newCommandContract("version", textRead, textNone, textNone, textNone, mailServiceNotRequired, false, false, resultAvailable),
	newCommandContract("update", textLocalWrite, textNone, textNone, textNone, mailServiceNotRequired, true, false, resultUpdated),
	newCommandContract("doctor", textRead, textNone, textMailStore, textOptionalAutomation, mailServiceAlwaysRequired, false, false, resultHealthy),
	newCommandContract("batch", textBatch, textNone, textMailStore, textNone, mailServiceForArguments, false, false, resultCompletePartial),
	newCommandContract("accounts.list", textRead, textNone, textMailStore, textFallbackAutomation, mailServiceAlwaysRequired, false, false, resultAccounts),
	newCommandContract("mailboxes.list", textRead, textNone, textMailStore, textNone, mailServiceAlwaysRequired, false, false, resultComplete),
	newCommandContract("mailboxes.resolve", textRead, textNone, textMailStore, textNone, mailServiceAlwaysRequired, false, false, resultResolved),
	newCommandContract("messages.list", textRead, textNone, textMailStore, textFallbackAutomation, mailServiceAlwaysRequired, false, false, resultComplete),
	newCommandContract("messages.filter", textRead, textNone, textMailStore, textNone, mailServiceAlwaysRequired, false, false, resultSearch),
	newCommandContract("messages.search", textRead, textNone, textMailStore, textNone, mailServiceAlwaysRequired, false, false, resultSearch),
	newCommandContract("messages.get", textRead, textNone, textMailStore, textNone, mailServiceAlwaysRequired, false, false, resultCompletePartial),
	newCommandContract("messages.raw", textRead, textNone, textMailStore, textNone, mailServiceAlwaysRequired, false, false, resultComplete),
	newCommandContract("attachments.list", textRead, textNone, textMailStore, textNone, mailServiceAlwaysRequired, false, false, resultCompletePartial),
	newCommandContract("attachments.save", textFilesystemWrite, textNone, textMailStore, textNone, mailServiceAlwaysRequired, false, false, resultSaved),
	newCommandContract("drafts.create", textLocalWrite, textNone, textDraftStore, textNone, mailServiceNotRequired, true, false, resultCreated),
	newCommandContract("drafts.list", textRead, textNone, textDraftStore, textNone, mailServiceNotRequired, false, false, resultComplete),
	newCommandContract("drafts.inspect", textRead, textNone, textDraftStore, textNone, mailServiceNotRequired, false, false, resultComplete),
	newCommandContract("drafts.preview", textRead, textNone, textDraftStore, textNone, mailServiceNotRequired, false, false, resultComplete),
	newCommandContract("drafts.edit", textLocalWrite, textNone, textDraftStore, textNone, mailServiceNotRequired, true, false, resultUpdated),
	newCommandContract("drafts.handoff", textVisibleCompose, textNone, textDraftStore, textSystemComposeService, mailServiceNotRequired, true, true, resultHandoff),
	newCommandContract("drafts.update", textLocalWrite, textNone, textDraftStore, textNone, mailServiceNotRequired, true, false, resultUpdated),
	newCommandContract("drafts.save", textUnsupported, textNone, textDraftStore, textNone, mailServiceNotRequired, true, false, resultComposeUnsupported),
	newCommandContract("drafts.open", textRead, textNone, textMailStore, textNone, mailServiceAlwaysRequired, false, false, resultCompletePartial),
	newCommandContract("drafts.send", textSMTPSend, textRequiredFlag, textDraftStore, textNone, mailServiceNotRequired, true, false, resultSent),
	newCommandContract("send.setup", textKeychainWrite, textNone, textNone, textNone, mailServiceNotRequired, false, false, resultSetup),
	newCommandContract("drafts.reconcile", textLocalWriteIMAPWrite, textNone, textDraftStoreMailStoreIfBaseline, textNone, mailServiceForReconcile, true, false, resultReconcile),
	newCommandContract("drafts.discard", textLocalWrite, textRequiredFlag, textDraftStore, textNone, mailServiceNotRequired, false, false, resultDiscarded),
	newCommandContract("drafts.prune", textLocalWrite, textRequiredFlag, textDraftStore, textNone, mailServiceNotRequired, false, false, resultPruned),
	newCommandContract("messages.reply", textLocalWrite, textNone, textMailStore, textNone, mailServiceAlwaysRequired, false, false, resultCreated),
	newCommandContract("messages.forward", textLocalWrite, textNone, textMailStore, textNone, mailServiceAlwaysRequired, false, false, resultCreated),
	newCommandContract("messages.mark", textIMAPWrite, textDraftFlag, textMailStore, textNone, mailServiceAlwaysRequired, false, false, resultUpdated),
	newCommandContract("messages.move", textIMAPWrite, textDraftFlag, textMailStore, textNone, mailServiceAlwaysRequired, false, false, resultMoved),
	newCommandContract("messages.copy", textIMAPWrite, textNone, textMailStore, textNone, mailServiceAlwaysRequired, false, false, resultCopied),
	newCommandContract("messages.delete", textIMAPWrite, textRequiredAndDraftFlags, textMailStore, textNone, mailServiceAlwaysRequired, false, false, resultDeleted),
	newCommandContract("sync", textMailWrite, textNone, textMailStore, textOptional, mailServiceAlwaysRequired, true, false, resultSync),
	newCommandContract("drafts.handoff-reconcile", textLocalWrite, textRequiredFlag, textDraftStore, textNone, mailServiceNotRequired, true, false, resultHandoffReconcile),
}

func commandContractForArgs(args []string) (*commandContract, []string) {
	if len(args) == 0 {
		return nil, nil
	}
	for index := range commandContracts {
		contract := &commandContracts[index]
		parts := strings.SplitN(contract.ID, ".", 2)
		if parts[0] != args[0] {
			continue
		}
		if len(parts) == 1 {
			return contract, args[1:]
		}
		if len(args) > 1 && args[1] == parts[1] {
			return contract, args[2:]
		}
	}
	return nil, nil
}

func capabilityCommandsForScope(command, family string) []commandCapability {
	commands := make([]commandCapability, 0, len(commandContracts))
	for _, contract := range commandContracts {
		if !commandIsPublished(contract) {
			continue
		}
		if command != "" && contract.ID != command {
			continue
		}
		if family != "" && !strings.HasPrefix(contract.ID, family+".") && contract.ID != family {
			continue
		}
		commands = append(commands, commandCapabilityFor(contract))
	}
	return commands
}
