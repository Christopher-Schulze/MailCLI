package cli

import (
	"slices"
	"strings"
)

// commandContract is the single source of truth for the public command
// manifest and the process dependencies selected before dispatch. The
// runner registry below owns execution only; it does not repeat capability
// metadata or dependency decisions.
type commandContract struct {
	ID                 string
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

func commandCapabilityFor(contract commandContract) commandCapability {
	return commandCapability{
		ID:              contract.ID,
		Schema:          schemaForCommand(contract.ID),
		EffectClass:     contract.effectClass,
		Confirmation:    contract.confirmation,
		StoreDependency: contract.storeDependency,
		Dependencies:    capabilityDependencyList(contract.dependencies),
		ResultStates:    slices.Clone(contract.resultStates),
	}
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
		ID: "capabilities", effectClass: "read", confirmation: "none",
		storeDependency: "none",
		resultStates:    []string{"available"},
		mailService:     mailServiceNotRequired,
		published:       true,
	},
	{
		ID: "version", effectClass: "read", confirmation: "none",
		storeDependency: "none",
		resultStates:    []string{"available"},
		mailService:     mailServiceNotRequired,
		published:       true,
	},
	{
		ID: "update", effectClass: "local-write", confirmation: "none",
		storeDependency: "none",
		dependencies:    []commandDependency{{Kind: dependencyKindNetwork, Target: dependencyTargetGitHubRelease, Condition: dependencyConditionAlways}},
		resultStates:    []string{"updated", "up_to_date"},
		mailService:     mailServiceNotRequired,
		published:       true,
		requiresSignal:  true,
	},
	{
		ID: "doctor", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		dependencies:    []commandDependency{{Kind: dependencyKindApp, Target: dependencyTargetMailApp, Condition: dependencyConditionIfDoctorLive}},
		resultStates:    []string{"healthy", "unhealthy"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "batch", effectClass: "batch", confirmation: "operation-dependent",
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
		ID: "accounts.list", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		dependencies:    []commandDependency{{Kind: dependencyKindApp, Target: dependencyTargetMailApp, Condition: dependencyConditionIfLocalStoreUnavailable}},
		resultStates:    []string{"complete", "partial", "bounded_identity_coverage"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "mailboxes.list", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"complete"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "mailboxes.resolve", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"resolved"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "messages.list", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		dependencies:    []commandDependency{{Kind: dependencyKindApp, Target: dependencyTargetMailApp, Condition: dependencyConditionIfLocalStoreUnavailable}},
		resultStates:    []string{"complete"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "messages.filter", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"complete", "partial", "search_cursor_stale", "search_index_changed", "search_count_limit_exceeded"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "messages.search", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"complete", "partial", "search_cursor_stale", "search_index_changed", "search_count_limit_exceeded", "search_budget_too_small"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "messages.get", effectClass: "read", confirmation: "none",
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
		ID: "messages.raw", effectClass: "read", confirmation: "none",
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
		ID: "messages.state", effectClass: "read", confirmation: "none",
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
		ID: "messages.thread", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"complete", "partial"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "attachments.list", effectClass: "read", confirmation: "none",
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
		ID: "attachments.save", effectClass: "filesystem-write", confirmation: "none",
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
		ID: "drafts.create", effectClass: "local-write", confirmation: "none",
		storeDependency: "draft-store",
		resultStates:    []string{"created"},
		mailService:     mailServiceNotRequired,
		published:       true,
		requiresSignal:  true,
	},
	{
		ID: "drafts.list", effectClass: "read", confirmation: "none",
		storeDependency: "draft-store",
		resultStates:    []string{"complete"},
		mailService:     mailServiceNotRequired,
		published:       true,
	},
	{
		ID: "drafts.inspect", effectClass: "read", confirmation: "none",
		storeDependency: "draft-store",
		resultStates:    []string{"complete"},
		mailService:     mailServiceNotRequired,
		published:       true,
	},
	{
		ID: "drafts.preview", effectClass: "read", confirmation: "none",
		storeDependency: "draft-store",
		resultStates:    []string{"complete"},
		mailService:     mailServiceNotRequired,
		published:       true,
	},
	{
		ID: "drafts.edit", effectClass: "local-write", confirmation: "none",
		storeDependency: "draft-store",
		dependencies:    []commandDependency{{Kind: dependencyKindApp, Target: dependencyTargetEditor, Condition: dependencyConditionAlways}},
		resultStates:    []string{"updated", "up_to_date"},
		mailService:     mailServiceNotRequired,
		published:       true,
		requiresSignal:  true,
	},
	{
		ID: "drafts.handoff", effectClass: "visible-compose", confirmation: "none",
		storeDependency:    "draft-store",
		dependencies:       []commandDependency{{Kind: dependencyKindApp, Target: dependencyTargetSystemComposeService, Condition: dependencyConditionAlways}},
		resultStates:       []string{"confirmed_opened", "confirmed_failed", "outcome_unknown", "canceled_before_dispatch"},
		mailService:        mailServiceNotRequired,
		published:          true,
		requiresSignal:     true,
		requiresMainThread: true,
	},
	{
		ID: "drafts.update", effectClass: "local-write", confirmation: "none",
		storeDependency: "draft-store",
		resultStates:    []string{"updated", "up_to_date"},
		mailService:     mailServiceNotRequired,
		published:       true,
		requiresSignal:  true,
	},
	{
		ID: "drafts.save", effectClass: "unsupported", confirmation: "none",
		storeDependency: "draft-store",
		resultStates:    []string{"compose_automation_unsupported"},
		mailService:     mailServiceNotRequired,
		published:       true,
		requiresSignal:  true,
	},
	{
		ID: "drafts.open", effectClass: "read", confirmation: "none",
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
		ID: "drafts.adopt", effectClass: "local-write", confirmation: "none",
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
		ID: "drafts.send", effectClass: "smtp-send", confirmation: "required-flag",
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
		ID: "send.setup", effectClass: "keychain-write", confirmation: "none",
		storeDependency: "none",
		dependencies:    []commandDependency{{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionAlways}},
		resultStates:    []string{"stored", "removed"},
		mailService:     mailServiceNotRequired,
		published:       true,
	},
	{
		ID: "drafts.reconcile", effectClass: "local-write+imap-write", confirmation: "none",
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
		ID: "drafts.discard", effectClass: "local-write", confirmation: "required-flag",
		storeDependency: "draft-store",
		resultStates:    []string{"discarded"},
		mailService:     mailServiceNotRequired,
		published:       true,
	},
	{
		ID: "drafts.prune", effectClass: "local-write", confirmation: "required-flag",
		storeDependency: "draft-store",
		resultStates:    []string{"listed", "pruned"},
		mailService:     mailServiceNotRequired,
		published:       true,
	},
	{
		ID: "messages.reply", effectClass: "local-write", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"created"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "messages.forward", effectClass: "local-write", confirmation: "none",
		storeDependency: "mail-store",
		resultStates:    []string{"created"},
		mailService:     mailServiceAlwaysRequired,
		published:       true,
	},
	{
		ID: "messages.mark", effectClass: "imap-write", confirmation: "draft-flag",
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
		ID: "messages.move", effectClass: "imap-write", confirmation: "draft-flag",
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
		ID: "messages.copy", effectClass: "imap-write", confirmation: "none",
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
		ID: "messages.delete", effectClass: "imap-write", confirmation: "required-and-draft-flags",
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
		ID: "sync", effectClass: "mail-write", confirmation: "none",
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
		ID: "drafts.handoff-reconcile", effectClass: "local-write", confirmation: "required-flag",
		storeDependency: "draft-store",
		resultStates:    []string{"confirmed_opened", "confirmed_failed"},
		mailService:     mailServiceNotRequired,
		published:       true,
		requiresSignal:  true,
	},
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
