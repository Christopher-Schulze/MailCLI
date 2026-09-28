package cli

import (
	"reflect"
	"slices"
	"sync"

	"mailcli/internal/mail"
)

// errorCatalogGuidance is the runtime classification of one code for a group
// of commands, produced by the same classifier that fills error.guidance.
type errorCatalogGuidance struct {
	Commands        []string              `json:"commands"`
	Phase           mail.OperationPhase   `json:"phase"`
	EffectCertainty mail.EffectCertainty  `json:"effect_certainty"`
	Retryability    mail.Retryability     `json:"retryability"`
	ReplayAllowed   bool                  `json:"replay_allowed"`
	Next            string                `json:"next"`
	Recovery        *errorCatalogRecovery `json:"recovery,omitempty"`
}

// errorCatalogRecovery is the observation command a group of failures always
// has at runtime; REF stands for the draft reference the runtime fills in.
type errorCatalogRecovery struct {
	Action  mail.RecoveryAction `json:"action"`
	Command string              `json:"command"`
	Args    []string            `json:"args"`
}

type errorCatalogEntry struct {
	Code     string                 `json:"code"`
	Meaning  string                 `json:"meaning"`
	Commands []string               `json:"commands"`
	Guidance []errorCatalogGuidance `json:"guidance"`
}

var errorCatalog = sync.OnceValue(buildErrorCatalog)

func buildErrorCatalog() []errorCatalogEntry {
	allCommands := make([]string, 0, len(commandContracts))
	for _, contract := range commandContracts {
		allCommands = append(allCommands, contract.ID)
	}
	entries := make([]errorCatalogEntry, 0, len(errorCodeDefinitions))
	for _, definition := range errorCodeDefinitions {
		commands := definition.Commands
		if slices.Equal(commands, errorScopeAll) {
			commands = allCommands
		}
		entry := errorCatalogEntry{Code: definition.Code, Meaning: definition.Meaning, Commands: commands}
		for _, command := range commands {
			guidance := classifyCatalogCode(command, definition.Code)
			index := slices.IndexFunc(entry.Guidance, func(existing errorCatalogGuidance) bool {
				return sameCatalogGuidance(existing, guidance)
			})
			if index >= 0 {
				entry.Guidance[index].Commands = append(entry.Guidance[index].Commands, command)
				continue
			}
			entry.Guidance = append(entry.Guidance, guidance)
		}
		entries = append(entries, entry)
	}
	return entries
}

func classifyCatalogCode(command, code string) errorCatalogGuidance {
	failure := newErrorData(command, responseData{}, &mail.OperationError{Code: code, Message: code})
	guidance := *failure.Guidance
	return errorCatalogGuidance{
		Commands: []string{command}, Phase: guidance.Phase, EffectCertainty: guidance.EffectCertainty,
		Retryability: guidance.Retryability, ReplayAllowed: guidance.ReplayAllowed,
		Next: failureNextAction(failure, nil).Do, Recovery: catalogRecovery(command, code),
	}
}

// draftLevelCommands act on one local draft, so a busy or conflicting draft
// always has a reference to inspect.
var draftLevelCommands = []string{
	"drafts.inspect", "drafts.preview", "drafts.edit", "drafts.update", "drafts.discard", "drafts.send", "drafts.reconcile",
}

// Failures of these commands leave the identity of the installed binary or of
// the sender binding as the only thing to observe.
var (
	updateObservedCodes = []string{"update_install_failed", "operation_failed", "operation_timeout", "finalization_failed"}
	setupObservedCodes  = []string{"operation_failed", "operation_timeout", "finalization_failed"}
)

func catalogRecovery(command, code string) *errorCatalogRecovery {
	fromRuntime := func(recovery mail.RecoveryGuidance) *errorCatalogRecovery {
		return &errorCatalogRecovery{Action: recovery.Action, Command: recovery.Command, Args: recovery.Args}
	}
	switch {
	case code == "draft_revision_conflict":
		return fromRuntime(draftInspectRecovery("REF", true))
	case slices.Contains([]string{"draft_revision_unavailable", "smtp_source_invalid"}, code),
		code == "draft_busy" && slices.Contains(draftLevelCommands, command):
		return fromRuntime(draftInspectRecovery("REF", false))
	case command == "update" && slices.Contains(updateObservedCodes, code):
		return &errorCatalogRecovery{Action: mail.RecoveryObserve, Command: "version", Args: []string{"--json"}}
	case command == "send.setup" && slices.Contains(setupObservedCodes, code):
		return &errorCatalogRecovery{Action: mail.RecoveryObserve, Command: "accounts.list", Args: []string{"--json"}}
	}
	return nil
}

func sameCatalogGuidance(left, right errorCatalogGuidance) bool {
	return left.Phase == right.Phase && left.EffectCertainty == right.EffectCertainty &&
		left.Retryability == right.Retryability && left.ReplayAllowed == right.ReplayAllowed && left.Next == right.Next &&
		reflect.DeepEqual(left.Recovery, right.Recovery)
}

// errorCatalogFor returns the entries whose commands intersect the selected
// commands, restricted to those commands.
func errorCatalogFor(commands []commandCapability) []errorCatalogEntry {
	selected := make(map[string]bool, len(commands))
	for _, command := range commands {
		selected[command.ID] = true
	}
	var entries []errorCatalogEntry
	for _, entry := range errorCatalog() {
		scoped := errorCatalogEntry{Code: entry.Code, Meaning: entry.Meaning}
		for _, command := range entry.Commands {
			if selected[command] {
				scoped.Commands = append(scoped.Commands, command)
			}
		}
		for _, guidance := range entry.Guidance {
			group := guidance
			group.Commands = nil
			for _, command := range guidance.Commands {
				if selected[command] {
					group.Commands = append(group.Commands, command)
				}
			}
			if len(group.Commands) > 0 {
				scoped.Guidance = append(scoped.Guidance, group)
			}
		}
		if len(scoped.Commands) > 0 {
			entries = append(entries, scoped)
		}
	}
	return entries
}
