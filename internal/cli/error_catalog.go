package cli

import (
	"slices"
	"sync"

	"mailcli/internal/mail"
)

// errorCatalogGuidance is the runtime classification of one code for a group
// of commands, produced by the same classifier that fills error.guidance.
type errorCatalogGuidance struct {
	Commands        []string             `json:"commands"`
	Phase           mail.OperationPhase  `json:"phase"`
	EffectCertainty mail.EffectCertainty `json:"effect_certainty"`
	Retryability    mail.Retryability    `json:"retryability"`
	ReplayAllowed   bool                 `json:"replay_allowed"`
	Next            string               `json:"next"`
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
		Next: failureNextAction(failure, nil).Do,
	}
}

func sameCatalogGuidance(left, right errorCatalogGuidance) bool {
	return left.Phase == right.Phase && left.EffectCertainty == right.EffectCertainty &&
		left.Retryability == right.Retryability && left.ReplayAllowed == right.ReplayAllowed && left.Next == right.Next
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
