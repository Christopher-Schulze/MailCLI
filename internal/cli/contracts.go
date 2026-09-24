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
	mailAppDependency  string
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
		ID:                contract.ID,
		Schema:            schemaForCommand(contract.ID),
		EffectClass:       contract.effectClass,
		Confirmation:      contract.confirmation,
		StoreDependency:   contract.storeDependency,
		MailAppDependency: contract.mailAppDependency,
		ResultStates:      slices.Clone(contract.resultStates),
	}
}

// commandContracts stays ordered so the human and JSON manifests remain
// deterministic. Every command ID is also resolved here for pre-dispatch
// dependency checks.
var commandContracts = []commandContract{
	{
		ID: "capabilities", effectClass: "read", confirmation: "none",
		storeDependency: "none", mailAppDependency: "none",
		resultStates: []string{"available"},
		mailService:  mailServiceNotRequired,
		published:    true,
	},
	{
		ID: "version", effectClass: "read", confirmation: "none",
		storeDependency: "none", mailAppDependency: "none",
		resultStates: []string{"available"},
		mailService:  mailServiceNotRequired,
		published:    true,
	},
	{
		ID: "update", effectClass: "local-write", confirmation: "none",
		storeDependency: "none", mailAppDependency: "none",
		resultStates:   []string{"updated", "up_to_date"},
		mailService:    mailServiceNotRequired,
		published:      true,
		requiresSignal: true,
	},
	{
		ID: "doctor", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store", mailAppDependency: "optional-automation",
		resultStates: []string{"healthy", "unhealthy"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "batch", effectClass: "batch", confirmation: "operation-dependent",
		storeDependency: "mail-store", mailAppDependency: "none",
		resultStates: []string{"complete", "partial"},
		mailService:  mailServiceForArguments,
		published:    true,
	},
	{
		ID: "accounts.list", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store", mailAppDependency: "fallback-automation",
		resultStates: []string{"complete", "partial", "bounded_identity_coverage"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "mailboxes.list", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store", mailAppDependency: "none",
		resultStates: []string{"complete"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "mailboxes.resolve", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store", mailAppDependency: "none",
		resultStates: []string{"resolved"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "messages.list", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store", mailAppDependency: "fallback-automation",
		resultStates: []string{"complete"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "messages.filter", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store", mailAppDependency: "none",
		resultStates: []string{"complete", "partial", "search_cursor_stale", "search_index_changed", "search_count_limit_exceeded", "search_budget_too_small"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "messages.search", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store", mailAppDependency: "none",
		resultStates: []string{"complete", "partial", "search_cursor_stale", "search_index_changed", "search_count_limit_exceeded", "search_budget_too_small"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "messages.get", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store", mailAppDependency: "none",
		resultStates: []string{"complete", "partial"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "messages.raw", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store", mailAppDependency: "none",
		resultStates: []string{"complete"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "messages.state", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store", mailAppDependency: "none",
		resultStates: []string{"resolved"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "messages.thread", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store", mailAppDependency: "none",
		resultStates: []string{"complete", "partial"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "attachments.list", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store", mailAppDependency: "none",
		resultStates: []string{"complete", "partial"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "attachments.save", effectClass: "filesystem-write", confirmation: "none",
		storeDependency: "mail-store", mailAppDependency: "none",
		resultStates: []string{"saved"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "drafts.create", effectClass: "local-write", confirmation: "none",
		storeDependency: "draft-store", mailAppDependency: "none",
		resultStates:   []string{"created"},
		mailService:    mailServiceNotRequired,
		published:      true,
		requiresSignal: true,
	},
	{
		ID: "drafts.list", effectClass: "read", confirmation: "none",
		storeDependency: "draft-store", mailAppDependency: "none",
		resultStates: []string{"complete"},
		mailService:  mailServiceNotRequired,
		published:    true,
	},
	{
		ID: "drafts.inspect", effectClass: "read", confirmation: "none",
		storeDependency: "draft-store", mailAppDependency: "none",
		resultStates: []string{"complete"},
		mailService:  mailServiceNotRequired,
		published:    true,
	},
	{
		ID: "drafts.preview", effectClass: "read", confirmation: "none",
		storeDependency: "draft-store", mailAppDependency: "none",
		resultStates: []string{"complete"},
		mailService:  mailServiceNotRequired,
		published:    true,
	},
	{
		ID: "drafts.edit", effectClass: "local-write", confirmation: "none",
		storeDependency: "draft-store", mailAppDependency: "none",
		resultStates:   []string{"updated", "up_to_date"},
		mailService:    mailServiceNotRequired,
		published:      true,
		requiresSignal: true,
	},
	{
		ID: "drafts.handoff", effectClass: "visible-compose", confirmation: "none",
		storeDependency: "draft-store", mailAppDependency: "system-compose-service",
		resultStates:       []string{"confirmed_opened", "confirmed_failed", "outcome_unknown", "canceled_before_dispatch"},
		mailService:        mailServiceNotRequired,
		published:          true,
		requiresSignal:     true,
		requiresMainThread: true,
	},
	{
		ID: "drafts.update", effectClass: "local-write", confirmation: "none",
		storeDependency: "draft-store", mailAppDependency: "none",
		resultStates:   []string{"updated", "up_to_date"},
		mailService:    mailServiceNotRequired,
		published:      true,
		requiresSignal: true,
	},
	{
		ID: "drafts.save", effectClass: "unsupported", confirmation: "none",
		storeDependency: "draft-store", mailAppDependency: "none",
		resultStates:   []string{"compose_automation_unsupported"},
		mailService:    mailServiceNotRequired,
		published:      true,
		requiresSignal: true,
	},
	{
		ID: "drafts.open", effectClass: "read", confirmation: "none",
		storeDependency: "mail-store", mailAppDependency: "none",
		resultStates: []string{"complete", "partial"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "drafts.adopt", effectClass: "local-write", confirmation: "none",
		storeDependency: "draft-store+mail-store", mailAppDependency: "none",
		resultStates:   []string{"created"},
		mailService:    mailServiceAlwaysRequired,
		published:      true,
		requiresSignal: true,
	},
	{
		ID: "drafts.send", effectClass: "smtp-send", confirmation: "required-flag",
		storeDependency: "draft-store", mailAppDependency: "none",
		resultStates:   []string{"sent", "sent_mirror_pending"},
		mailService:    mailServiceNotRequired,
		published:      true,
		requiresSignal: true,
	},
	{
		ID: "send.setup", effectClass: "keychain-write", confirmation: "none",
		storeDependency: "none", mailAppDependency: "none",
		resultStates: []string{"stored", "removed"},
		mailService:  mailServiceNotRequired,
		published:    true,
	},
	{
		ID: "drafts.reconcile", effectClass: "local-write+imap-write", confirmation: "none",
		storeDependency: "draft-store+mail-store-if-baseline", mailAppDependency: "none",
		resultStates:   []string{"sent_store_observed", "accepted_by_mail", "sent", "sent_mirror_pending", "outcome_unknown"},
		mailService:    mailServiceForReconcile,
		published:      true,
		requiresSignal: true,
	},
	{
		ID: "drafts.discard", effectClass: "local-write", confirmation: "required-flag",
		storeDependency: "draft-store", mailAppDependency: "none",
		resultStates: []string{"discarded"},
		mailService:  mailServiceNotRequired,
		published:    true,
	},
	{
		ID: "drafts.prune", effectClass: "local-write", confirmation: "required-flag",
		storeDependency: "draft-store", mailAppDependency: "none",
		resultStates: []string{"listed", "pruned"},
		mailService:  mailServiceNotRequired,
		published:    true,
	},
	{
		ID: "messages.reply", effectClass: "local-write", confirmation: "none",
		storeDependency: "mail-store", mailAppDependency: "none",
		resultStates: []string{"created"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "messages.forward", effectClass: "local-write", confirmation: "none",
		storeDependency: "mail-store", mailAppDependency: "none",
		resultStates: []string{"created"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "messages.mark", effectClass: "imap-write", confirmation: "draft-flag",
		storeDependency: "mail-store", mailAppDependency: "none",
		resultStates: []string{"updated", "up_to_date"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "messages.move", effectClass: "imap-write", confirmation: "draft-flag",
		storeDependency: "mail-store", mailAppDependency: "none",
		resultStates: []string{"moved"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "messages.copy", effectClass: "imap-write", confirmation: "none",
		storeDependency: "mail-store", mailAppDependency: "none",
		resultStates: []string{"copied"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "messages.delete", effectClass: "imap-write", confirmation: "required-and-draft-flags",
		storeDependency: "mail-store", mailAppDependency: "none",
		resultStates: []string{"deleted"},
		mailService:  mailServiceAlwaysRequired,
		published:    true,
	},
	{
		ID: "sync", effectClass: "mail-write", confirmation: "none",
		storeDependency: "mail-store", mailAppDependency: "optional",
		resultStates:   []string{"triggered", "checked_complete", "checked_incomplete"},
		mailService:    mailServiceAlwaysRequired,
		published:      true,
		requiresSignal: true,
	},
	{
		ID: "drafts.handoff-reconcile", effectClass: "local-write", confirmation: "required-flag",
		storeDependency: "draft-store", mailAppDependency: "none",
		resultStates:   []string{"confirmed_opened", "confirmed_failed"},
		mailService:    mailServiceNotRequired,
		published:      true,
		requiresSignal: true,
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
