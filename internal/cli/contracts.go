package cli

import (
	"slices"
	"strings"
)

// commandContract is the source of truth for command metadata, help, and
// executable dispatch.
type commandContract struct {
	ID                 string
	audience           commandAudience
	handler            commandRunner
	familyHandler      commandRunner
	helpDescription    string
	emptyFamilyHelp    bool
	effectClass        string
	confirmation       string
	storeDependency    string
	dependencies       []commandDependency
	resultStates       []string
	limitRefs          []string
	mailService        mailServiceRequirement
	published          bool
	requiresSignal     bool
	requiresMainThread bool
}

type commandAudience string

const (
	commandAudienceAgent commandAudience = "agent"
	commandAudienceHuman commandAudience = "human"
)

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
	audience := contract.audience
	if audience == "" {
		audience = commandAudienceAgent
	}
	return commandCapability{
		ID:              contract.ID,
		Audience:        audience,
		Schema:          schema,
		EffectClass:     contract.effectClass,
		Confirmation:    contract.confirmation,
		StoreDependency: contract.storeDependency,
		Dependencies:    capabilityDependencyList(contract.dependencies),
		ResultStates:    slices.Clone(contract.resultStates),
		LimitRefs:       commandLimitReferences(contract),
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
		limitRefs:   []string{"batch_operations", "default_batch_concurrency", "maximum_batch_concurrency", "maximum_batch_items", "maximum_batch_input_bytes", "output_projection.message_fields", "output_projection.message_views"},
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
		limitRefs:   []string{"sender_identity_scan_limit", "maximum_sender_identity_scan_limit", "sender_identity_coverage_states", "direct_ops_support_reasons", "supported_providers"},
		effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		dependencies:    []commandDependency{{Kind: dependencyKindApp, Target: dependencyTargetMailApp, Condition: dependencyConditionIfLocalStoreUnavailable}},
		resultStates:    []string{"complete", "partial", "bounded_identity_coverage"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "mailboxes.list", handler: runMailboxesList, helpDescription: "List and resolve exact mailbox paths",
		limitRefs:       []string{"maximum_imap_list_response_bytes", "maximum_imap_list_response_lines", "maximum_imap_list_mailboxes"},
		emptyFamilyHelp: true,
		effectClass:     "read", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"complete"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "mailboxes.resolve", handler: runMailboxResolve,
		limitRefs:   []string{"maximum_imap_list_response_bytes", "maximum_imap_list_response_lines", "maximum_imap_list_mailboxes"},
		effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"resolved"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "messages.list", handler: runMessagesList, helpDescription: "List, search, read, reply, forward, and organize messages",
		limitRefs:   []string{"maximum_page_size"},
		effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		dependencies:    []commandDependency{{Kind: dependencyKindApp, Target: dependencyTargetMailApp, Condition: dependencyConditionIfLocalStoreUnavailable}},
		resultStates:    []string{"complete"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "messages.filter", handler: runMessagesFilter,
		limitRefs:   []string{"maximum_page_size", "search_pagination_consistency", "search_cursor_detects_index_drift", "search_candidate_count_default", "search_exact_count_bounded"},
		effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"complete", "partial", "search_cursor_stale", "search_index_changed", "search_count_limit_exceeded"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "messages.search", handler: runMessagesSearch,
		limitRefs:   []string{"maximum_page_size", "search_pagination_consistency", "search_cursor_detects_index_drift", "search_candidate_count_default", "search_exact_count_bounded"},
		effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"complete", "partial", "search_cursor_stale", "search_index_changed", "search_count_limit_exceeded", "search_budget_too_small"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "messages.get", handler: runMessagesGet,
		limitRefs:   []string{"maximum_raw_source_bytes", "raw_mime_read", "imap_connections_per_account", "maximum_imap_connections_per_account"},
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
		limitRefs:   []string{"maximum_raw_source_bytes", "raw_mime_read"},
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
		limitRefs:   []string{"maximum_page_size"},
		effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"complete", "partial"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "attachments.list", handler: runAttachmentsList, helpDescription: "List and save received attachments",
		limitRefs:   []string{"maximum_raw_source_bytes"},
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
		limitRefs:   []string{"maximum_raw_source_bytes", "imap_connections_per_account", "maximum_imap_connections_per_account"},
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
		limitRefs:   []string{"maximum_draft_input_bytes", "maximum_draft_subject_bytes", "maximum_draft_body_bytes", "maximum_draft_recipients", "maximum_draft_attachments", "maximum_draft_attachment_bytes"},
		effectClass: "local-write", confirmation: "none",
		storeDependency: "draft-store",
		resultStates:    []string{"created"},
		mailService:     mailServiceNotRequired,
		published:       true,
		requiresSignal:  true,
	},
	{
		ID: "drafts.list", handler: runDraftList,
		limitRefs:   []string{"maximum_page_size"},
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
		limitRefs:   []string{"maximum_draft_body_bytes"},
		effectClass: "read", confirmation: "none",
		storeDependency: "draft-store",
		resultStates:    []string{"complete"},
		mailService:     mailServiceNotRequired,
		published:       true,
	},
	{
		ID: "drafts.edit", handler: runDraftEdit,
		limitRefs:   []string{"maximum_draft_input_bytes", "maximum_draft_subject_bytes", "maximum_draft_body_bytes", "maximum_draft_recipients", "maximum_draft_attachments", "maximum_draft_attachment_bytes"},
		audience:    commandAudienceHuman,
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
		limitRefs:   []string{"visible_compose_handoff", "visible_attachment_handoff", "maximum_compose_body_bytes", "maximum_draft_recipients", "maximum_draft_attachments", "maximum_draft_attachment_bytes"},
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
		limitRefs:   []string{"maximum_draft_input_bytes", "maximum_draft_subject_bytes", "maximum_draft_body_bytes", "maximum_draft_recipients", "maximum_draft_attachments", "maximum_draft_attachment_bytes"},
		effectClass: "local-write", confirmation: "none",
		storeDependency: "draft-store",
		resultStates:    []string{"updated", "up_to_date"},
		mailService:     mailServiceNotRequired,
		published:       true,
		requiresSignal:  true,
	},
	{
		ID: "drafts.open", handler: runMailDraftOpen,
		limitRefs:   []string{"maximum_raw_source_bytes", "raw_mime_read", "imap_connections_per_account", "maximum_imap_connections_per_account"},
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
		limitRefs:   []string{"maximum_raw_source_bytes", "maximum_draft_subject_bytes", "maximum_draft_body_bytes", "maximum_draft_recipients", "maximum_draft_attachments", "maximum_draft_attachment_bytes"},
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
		limitRefs:   []string{"send_transport", "raw_mime_send", "supported_providers", "unsupported_provider_code", "provider_support_description", "maximum_draft_recipients", "maximum_draft_attachments", "maximum_draft_attachment_bytes", "imap_connections_per_account", "maximum_imap_connections_per_account"},
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
		limitRefs:   []string{"supported_providers", "unsupported_provider_code", "provider_support_description"},
		effectClass: "keychain-write", confirmation: "none",
		storeDependency: "none",
		dependencies:    []commandDependency{{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionAlways}},
		resultStates:    []string{"stored", "removed"},
		mailService:     mailServiceNotRequired,
		published:       true,
	},
	{
		ID: "drafts.reconcile", handler: runDraftReconcile,
		limitRefs:   []string{"imap_connections_per_account", "maximum_imap_connections_per_account", "maximum_compose_body_bytes"},
		effectClass: "local-write+imap-write", confirmation: "none",
		storeDependency: "draft-store+mail-store-if-baseline",
		dependencies: []commandDependency{
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfTransportClaimNeedsIMAPReconciliation},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfTransportClaimNeedsIMAPReconciliation},
		},
		resultStates:   []string{"sent_store_observed", "accepted_by_mail", "sent", "sent_mirror_pending", "outcome_unknown", "native_draft_observed", "draft_save_outcome_unknown"},
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
		limitRefs:   []string{"maximum_draft_input_bytes", "maximum_draft_subject_bytes", "maximum_draft_body_bytes", "maximum_draft_recipients", "maximum_draft_attachments", "maximum_draft_attachment_bytes"},
		effectClass: "local-write", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"created"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "messages.forward", handler: runMessageForward,
		limitRefs:   []string{"maximum_draft_input_bytes", "maximum_draft_subject_bytes", "maximum_draft_body_bytes", "maximum_draft_recipients", "maximum_draft_attachments", "maximum_draft_attachment_bytes"},
		effectClass: "local-write", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"created"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "messages.mark", handler: runMessageMark,
		limitRefs:   []string{"mutation_transport", "supported_providers", "unsupported_provider_code", "imap_connections_per_account", "maximum_imap_connections_per_account"},
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
		limitRefs:   []string{"mutation_transport", "supported_providers", "unsupported_provider_code", "imap_connections_per_account", "maximum_imap_connections_per_account"},
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
		limitRefs:   []string{"mutation_transport", "supported_providers", "unsupported_provider_code", "imap_connections_per_account", "maximum_imap_connections_per_account"},
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
		limitRefs:   []string{"mutation_transport", "supported_providers", "unsupported_provider_code", "imap_connections_per_account", "maximum_imap_connections_per_account"},
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
		limitRefs:   []string{"imap_connections_per_account", "maximum_imap_connections_per_account"},
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
