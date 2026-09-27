package cli

import (
	"slices"
	"strings"
)

// commandContract is the source of truth for command metadata, help, and
// executable dispatch.
type commandContract struct {
	ID                 string
	handler            commandRunner
	familyHandler      commandRunner
	helpDescription    string
	emptyFamilyHelp    bool
	effectClass        string
	confirmation       string
	storeDependency    string
	dependencies       []commandDependency
	resultStates       []string
	mailService        mailServiceRequirement
	published          bool
	requiresSignal     bool
	requiresMainThread bool
}

type mailServiceRequirement uint8

const (
	mailServiceNotRequired mailServiceRequirement = iota
	mailServiceAlwaysRequired
	mailServiceForArguments
	mailServiceForReconcile
)

func commandRequiresMailService(contract commandContract, args []string) bool {
	switch contract.mailService {
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
	return contract.published
}

func commandNeedsSignalFor(contract commandContract) bool {
	return contract.requiresSignal
}

func commandNeedsMainThreadFor(contract commandContract) bool {
	return contract.requiresMainThread
}

func commandCapabilityFor(contract commandContract) (commandCapability, error) {
	schema, err := checkedSchemaForCommand(contract.ID)
	if err != nil {
		return commandCapability{}, err
	}
	return commandCapability{
		ID:              contract.ID,
		Schema:          schema,
		EffectClass:     contract.effectClass,
		Confirmation:    contract.confirmation,
		StoreDependency: contract.storeDependency,
		Dependencies:    capabilityDependencyList(contract.dependencies),
		ResultStates:    slices.Clone(contract.resultStates),
	}, nil
}

func capabilityDependencyList(dependencies []commandDependency) []commandDependency {
	if len(dependencies) == 0 {
		return []commandDependency{}
	}
	return slices.Clone(dependencies)
}

// commandContracts stays ordered so the human and JSON manifests remain
// deterministic. Every command ID is also resolved here for pre-dispatch
// dependency checks.
var commandContracts = []commandContract{
	{
		ID: "capabilities", helpDescription: "Print the machine-readable command contract",
		effectClass: "read", confirmation: "none",
		storeDependency: "none",
		resultStates:    []string{"available"},
		mailService:     mailServiceNotRequired,
		published:       true,
	},
	{
		ID: "version", handler: runVersionCommand, helpDescription: "Print the installed version",
		effectClass: "read", confirmation: "none",
		storeDependency: "none",
		resultStates:    []string{"available"},
		mailService:     mailServiceNotRequired,
		published:       true,
	},
	{
		ID: "update", handler: runUpdateCommand, helpDescription: "Check GitHub and install the latest verified release",
		effectClass: "local-write", confirmation: "none",
		storeDependency: "none",
		dependencies:    []commandDependency{{Kind: dependencyKindNetwork, Target: dependencyTargetGitHubRelease, Condition: dependencyConditionAlways}},
		resultStates:    []string{"updated", "up_to_date"},
		mailService:     mailServiceNotRequired,
		published:       true,
		requiresSignal:  true,
	},
	{
		ID: "doctor", handler: runDoctor, helpDescription: "Verify the local MailCLI environment",
		effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		dependencies:    []commandDependency{{Kind: dependencyKindApp, Target: dependencyTargetMailApp, Condition: dependencyConditionIfDoctorLive}},
		resultStates:    []string{"healthy", "unhealthy"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "batch", handler: runBatch, helpDescription: "Execute bounded explicit reads, attachment saves, and marks",
		effectClass: "batch", confirmation: "operation-dependent",
		storeDependency: "mail-store",
		dependencies: []commandDependency{
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfBatchItemRequiresIMAP},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfBatchItemRequiresIMAP},
		},
		resultStates: []string{"complete", "partial"},
		mailService:  mailServiceForArguments,
		published:    true,
	},
	{
		ID: "accounts.list", handler: runAccountsList, helpDescription: "List configured accounts and sender identities",
		effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		dependencies:    []commandDependency{{Kind: dependencyKindApp, Target: dependencyTargetMailApp, Condition: dependencyConditionIfLocalStoreUnavailable}},
		resultStates:    []string{"complete", "partial", "bounded_identity_coverage"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "mailboxes.list", handler: runMailboxesList, helpDescription: "List and resolve exact mailbox paths",
		emptyFamilyHelp: true,
		effectClass:     "read", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"complete"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "mailboxes.resolve", handler: runMailboxResolve,
		effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"resolved"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "messages.list", handler: runMessagesList, helpDescription: "List, search, read, reply, forward, and organize messages",
		effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		dependencies:    []commandDependency{{Kind: dependencyKindApp, Target: dependencyTargetMailApp, Condition: dependencyConditionIfLocalStoreUnavailable}},
		resultStates:    []string{"complete"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "messages.filter", handler: runMessagesFilter,
		effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"complete", "partial", "search_cursor_stale", "search_index_changed", "search_count_limit_exceeded"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "messages.search", handler: runMessagesSearch,
		effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"complete", "partial", "search_cursor_stale", "search_index_changed", "search_count_limit_exceeded", "search_budget_too_small"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "messages.get", handler: runMessagesGet,
		effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		dependencies: []commandDependency{
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfLocalSourceIncomplete},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfLocalSourceIncomplete},
		},
		resultStates: []string{"complete", "partial"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "messages.raw", handler: runMessagesRaw,
		effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		dependencies: []commandDependency{
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfLocalSourceIncomplete},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfLocalSourceIncomplete},
		},
		resultStates: []string{"complete"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "messages.state", handler: runMessageState,
		effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		dependencies: []commandDependency{
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionAlways},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionAlways},
		},
		resultStates: []string{"resolved"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "messages.thread", handler: runMessageThread,
		effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"complete", "partial"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "attachments.list", handler: runAttachmentsList, helpDescription: "List and save received attachments",
		effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		dependencies: []commandDependency{
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfLocalSourceIncomplete},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfLocalSourceIncomplete},
		},
		resultStates: []string{"complete", "partial"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "attachments.save", handler: runAttachmentsSaveCommand,
		effectClass: "filesystem-write", confirmation: "none",
		storeDependency: "mail-store",
		dependencies: []commandDependency{
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfLocalAttachmentBytesUnavailable},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfLocalAttachmentBytesUnavailable},
		},
		resultStates: []string{"saved"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "drafts.create", handler: runDraftCreateContext, helpDescription: "Create, preview, edit, hand off, and prune drafts",
		effectClass: "local-write", confirmation: "none",
		storeDependency: "draft-store",
		resultStates:    []string{"created"},
		mailService:     mailServiceNotRequired,
		published:       true,
		requiresSignal:  true,
	},
	{
		ID: "drafts.list", handler: runDraftList,
		effectClass: "read", confirmation: "none",
		storeDependency: "draft-store",
		resultStates:    []string{"complete"},
		mailService:     mailServiceNotRequired,
		published:       true,
	},
	{
		ID: "drafts.inspect", handler: runDraftInspectCommand,
		effectClass: "read", confirmation: "none",
		storeDependency: "draft-store",
		resultStates:    []string{"complete"},
		mailService:     mailServiceNotRequired,
		published:       true,
	},
	{
		ID: "drafts.preview", handler: runDraftPreviewCommand,
		effectClass: "read", confirmation: "none",
		storeDependency: "draft-store",
		resultStates:    []string{"complete"},
		mailService:     mailServiceNotRequired,
		published:       true,
	},
	{
		ID: "drafts.edit", handler: runDraftEdit,
		effectClass: "local-write", confirmation: "none",
		storeDependency: "draft-store",
		dependencies:    []commandDependency{{Kind: dependencyKindApp, Target: dependencyTargetEditor, Condition: dependencyConditionAlways}},
		resultStates:    []string{"updated", "up_to_date"},
		mailService:     mailServiceNotRequired,
		published:       true,
		requiresSignal:  true,
	},
	{
		ID: "drafts.handoff", handler: runDraftHandoff,
		effectClass: "visible-compose", confirmation: "none",
		storeDependency:    "draft-store",
		dependencies:       []commandDependency{{Kind: dependencyKindApp, Target: dependencyTargetSystemComposeService, Condition: dependencyConditionAlways}},
		resultStates:       []string{"handed_off", "not_handed_off", "unknown"},
		mailService:        mailServiceNotRequired,
		published:          true,
		requiresSignal:     true,
		requiresMainThread: true,
	},
	{
		ID: "drafts.update", handler: runDraftUpdate,
		effectClass: "local-write", confirmation: "none",
		storeDependency: "draft-store",
		resultStates:    []string{"updated", "up_to_date"},
		mailService:     mailServiceNotRequired,
		published:       true,
		requiresSignal:  true,
	},
	{
		ID: "drafts.save", handler: runDraftSave,
		effectClass: "unsupported", confirmation: "none",
		storeDependency: "draft-store",
		resultStates:    []string{"compose_automation_unsupported"},
		mailService:     mailServiceNotRequired,
		published:       true,
		requiresSignal:  true,
	},
	{
		ID: "drafts.open", handler: runMailDraftOpen,
		effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		dependencies: []commandDependency{
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfLocalSourceIncomplete},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfLocalSourceIncomplete},
		},
		resultStates: []string{"complete", "partial"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "drafts.adopt", handler: runDraftAdopt,
		effectClass: "local-write", confirmation: "none",
		storeDependency: "draft-store+mail-store",
		dependencies: []commandDependency{
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfLocalSourceIncomplete},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfLocalSourceIncomplete},
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfLocalAttachmentBytesUnavailable},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfLocalAttachmentBytesUnavailable},
		},
		resultStates:   []string{"created"},
		mailService:    mailServiceAlwaysRequired,
		published:      true,
		requiresSignal: true,
	},
	{
		ID: "drafts.send", handler: runDraftSend,
		effectClass: "smtp-send", confirmation: "required-flag",
		storeDependency: "draft-store",
		dependencies: []commandDependency{
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfSend},
			{Kind: dependencyKindNetwork, Target: dependencyTargetSMTP, Condition: dependencyConditionIfSend},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfSMTPAccepted},
		},
		resultStates:   []string{"sent", "sent_mirror_pending"},
		mailService:    mailServiceNotRequired,
		published:      true,
		requiresSignal: true,
	},
	{
		ID: "send.setup", handler: runSendSetupCommand, helpDescription: "Store or remove app-specific SMTP send credentials",
		effectClass: "keychain-write", confirmation: "none",
		storeDependency: "none",
		dependencies:    []commandDependency{{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionAlways}},
		resultStates:    []string{"stored", "removed"},
		mailService:     mailServiceNotRequired,
		published:       true,
	},
	{
		ID: "drafts.reconcile", handler: runDraftReconcile,
		effectClass: "local-write+imap-write", confirmation: "none",
		storeDependency: "draft-store+mail-store-if-baseline",
		dependencies: []commandDependency{
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfTransportClaimNeedsIMAPReconciliation},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfTransportClaimNeedsIMAPReconciliation},
		},
		resultStates:   []string{"sent_store_observed", "accepted_by_mail", "sent", "sent_mirror_pending", "outcome_unknown"},
		mailService:    mailServiceForReconcile,
		published:      true,
		requiresSignal: true,
	},
	{
		ID: "drafts.discard", handler: runDraftDiscard,
		effectClass: "local-write", confirmation: "required-flag",
		storeDependency: "draft-store",
		resultStates:    []string{"discarded"},
		mailService:     mailServiceNotRequired,
		published:       true,
	},
	{
		ID: "drafts.prune", handler: runDraftPrune,
		effectClass: "local-write", confirmation: "required-flag",
		storeDependency: "draft-store",
		resultStates:    []string{"listed", "pruned"},
		mailService:     mailServiceNotRequired,
		published:       true,
	},
	{
		ID: "messages.reply", handler: runMessageReply,
		effectClass: "local-write", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"created"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "messages.forward", handler: runMessageForward,
		effectClass: "local-write", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"created"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "messages.mark", handler: runMessageMark,
		effectClass: "imap-write", confirmation: "draft-flag",
		storeDependency: "mail-store",
		dependencies: []commandDependency{
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionAlways},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionAlways},
		},
		resultStates: []string{"updated", "up_to_date"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "messages.move", handler: runMessageMove,
		effectClass: "imap-write", confirmation: "draft-flag",
		storeDependency: "mail-store",
		dependencies: []commandDependency{
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionAlways},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionAlways},
		},
		resultStates: []string{"moved"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "messages.copy", handler: runMessageCopy,
		effectClass: "imap-write", confirmation: "none",
		storeDependency: "mail-store",
		dependencies: []commandDependency{
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionAlways},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionAlways},
		},
		resultStates: []string{"copied"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "messages.delete", handler: runMessageDelete,
		effectClass: "imap-write", confirmation: "required-and-draft-flags",
		storeDependency: "mail-store",
		dependencies: []commandDependency{
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionAlways},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionAlways},
		},
		resultStates: []string{"deleted"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "sync", handler: runSync, helpDescription: "Synchronize with Mail.app or check server status over IMAP (--check)",
		effectClass: "mail-write", confirmation: "none",
		storeDependency: "mail-store",
		dependencies: []commandDependency{
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfSyncCheck},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfSyncCheck},
			{Kind: dependencyKindApp, Target: dependencyTargetMailApp, Condition: dependencyConditionIfSyncDefault},
		},
		resultStates:   []string{"triggered", "checked_complete", "checked_incomplete"},
		mailService:    mailServiceAlwaysRequired,
		published:      true,
		requiresSignal: true,
	},
	{
		ID: "drafts.handoff-reconcile", handler: runDraftHandoffReconcile,
		effectClass: "local-write", confirmation: "required-flag",
		storeDependency: "draft-store",
		resultStates:    []string{"confirmed_opened", "confirmed_failed"},
		mailService:     mailServiceNotRequired,
		published:       true,
		requiresSignal:  true,
	},
}

func init() {
	// Capabilities reads this table, so its handler is assigned after table initialization.
	for index := range commandContracts {
		switch commandContracts[index].ID {
		case "capabilities":
			commandContracts[index].handler = runCapabilitiesCommand
		case "accounts.list":
			commandContracts[index].familyHandler = runAccounts
		case "mailboxes.list":
			commandContracts[index].familyHandler = runMailboxes
		case "messages.list":
			commandContracts[index].familyHandler = runMessages
		case "attachments.list":
			commandContracts[index].familyHandler = runAttachments
		case "drafts.create":
			commandContracts[index].familyHandler = runDrafts
		case "send.setup":
			commandContracts[index].familyHandler = runSendCommandFamily
		}
	}
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

func commandFamilyChoices(family string) []string {
	prefix := family + "."
	choices := make([]string, 0)
	for _, contract := range commandContracts {
		if !strings.HasPrefix(contract.ID, prefix) {
			continue
		}
		choice := strings.TrimPrefix(contract.ID, prefix)
		if !strings.Contains(choice, ".") {
			choices = append(choices, choice)
		}
	}
	return choices
}

func commandFamilyContract(family string) *commandContract {
	prefix := family + "."
	for index := range commandContracts {
		if strings.HasPrefix(commandContracts[index].ID, prefix) {
			return &commandContracts[index]
		}
	}
	return nil
}

func commandContractForFamily(family, subcommand string) *commandContract {
	wanted := family + "." + subcommand
	for index := range commandContracts {
		if commandContracts[index].ID == wanted {
			return &commandContracts[index]
		}
	}
	return nil
}

func commandFamilyShowsHelpWhenEmpty(family string) bool {
	contract := commandFamilyContract(family)
	return contract != nil && contract.emptyFamilyHelp
}

func commandRootContracts() []commandContract {
	roots := make([]commandContract, 0, len(commandContracts))
	seen := make(map[string]struct{}, len(commandContracts))
	for _, contract := range commandContracts {
		parts := strings.SplitN(contract.ID, ".", 2)
		if _, exists := seen[parts[0]]; exists {
			continue
		}
		seen[parts[0]] = struct{}{}
		roots = append(roots, contract)
	}
	return roots
}

func capabilityCommandsForScope(command, family string) ([]commandCapability, error) {
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
		capability, err := commandCapabilityFor(contract)
		if err != nil {
			return nil, err
		}
		commands = append(commands, capability)
	}
	return commands, nil
}
