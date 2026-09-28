package cli

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

const manualTopics = "Overview|For agents|Install|Setup|Commands|Workflows|Errors and recovery|Limits|Security|Release|Development|Design reference"

func validateManualStructure(content string, commands []string) error {
	var headings []string
	for number, line := range strings.Split(content, "\n") {
		if utf8.RuneCountInString(line) > 400 {
			return fmt.Errorf("manual line %d exceeds 400 characters", number+1)
		}
		if strings.HasPrefix(line, "## ") {
			headings = append(headings, strings.TrimPrefix(line, "## "))
		}
	}
	if strings.Join(headings, "|") != manualTopics {
		return fmt.Errorf("manual topics differ: %v", headings)
	}
	user, _, found := strings.Cut(content, "\n## Design reference\n")
	if !found || len(user) > 40000 {
		return fmt.Errorf("user manual is missing or exceeds 40000 bytes")
	}
	for _, command := range commands {
		marker := "- `" + command + "`:"
		if strings.Count(user, marker) != 1 {
			return fmt.Errorf("command %s must have exactly one guide", command)
		}
		_, block, _ := strings.Cut(user, marker)
		block, _, _ = strings.Cut(block, "\n- `")
		block, _, _ = strings.Cut(block, "\n## ")
		if strings.Count(block, "Example:") != 1 ||
			!strings.Contains(block, "Example: `mailcli "+strings.ReplaceAll(command, ".", " ")) ||
			!strings.Contains(block, "main flag") || !strings.Contains(block, "Output:") {
			return fmt.Errorf("command %s lacks flags, its example or output contract", command)
		}
	}
	return nil
}

func TestManualDocumentationStructure(t *testing.T) {
	content := readRepositoryFile(t, "docs/documentation.md")
	var commands []string
	for _, command := range mustCapabilities(t).Commands {
		commands = append(commands, command.ID)
	}
	if err := validateManualStructure(content, commands); err != nil {
		t.Fatal(err)
	}
	for _, topic := range strings.Split(manualTopics, "|") {
		anchor := strings.ReplaceAll(strings.ToLower(topic), " ", "-")
		if !strings.Contains(content, "["+topic+"](#"+anchor+")") {
			t.Errorf("manual TOC omits %s", topic)
		}
	}
	for _, topic := range []string{
		"Architecture", "Platform and freshness boundaries", "CLI contract", "Composition",
		"Data model", "Account identity bindings", "Local security and permissions",
		"Release distribution", "Scope", "Search", "Setup and usage", "Technical baseline",
	} {
		if !strings.Contains(content, "\n### "+topic+"\n") {
			t.Errorf("design reference lost stable topic %s", topic)
		}
	}
}

func TestManualStructureRejectsBrokenContracts(t *testing.T) {
	var fixture strings.Builder
	for _, topic := range strings.Split(manualTopics, "|") {
		fmt.Fprintf(&fixture, "\n## %s\n", topic)
		if topic == "Commands" {
			fixture.WriteString("- `version`: inspect identity; main flag `--json`.\n")
			fixture.WriteString("  Example: `mailcli version --json`. Output: `name`, `version`.\n")
		}
	}
	valid := fixture.String()
	for _, test := range []struct {
		name    string
		content string
		valid   bool
	}{
		{name: "valid", content: valid, valid: true},
		{name: "missing topic", content: strings.Replace(valid, "## Setup", "### Setup", 1)},
		{name: "overlong line", content: valid + strings.Repeat("x", 401)},
		{name: "oversized manual", content: strings.Replace(valid, "## Overview\n", "## Overview\n"+strings.Repeat("bounded line\n", 4000), 1)},
		{name: "missing command", content: strings.Replace(valid, "- `version`:", "- `other`:", 1)},
		{name: "missing example", content: strings.Replace(valid, "Example:", "Usage:", 1)},
		{name: "wrong example", content: strings.Replace(valid, "mailcli version", "mailcli doctor", 1)},
		{name: "missing outputs", content: strings.Replace(valid, "Output:", "Result:", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateManualStructure(test.content, []string{"version"}); (err == nil) != test.valid {
				t.Fatalf("validation = %v, want valid=%t", err, test.valid)
			}
		})
	}
}

func TestManualCommandExamplesMatchPublishedSchemas(t *testing.T) {
	content := readRepositoryFile(t, "docs/documentation.md")
	for _, command := range mustCapabilities(t).Commands {
		t.Run(command.ID, func(t *testing.T) {
			block, arguments, err := manualCommandExample(content, command.ID)
			if err != nil {
				t.Fatal(err)
			}
			flags := schemaFlagsByName(decodeTestCommandSchema(t, command.Schema))
			for index := 0; index < len(arguments); index++ {
				flag, exists := flags[arguments[index]]
				if !exists {
					t.Fatalf("example has an unpublished flag or operand: %s", arguments[index])
				}
				if flag.TakesValue {
					index++
					if index == len(arguments) || strings.HasPrefix(arguments[index], "--") {
						t.Fatalf("example omits value for %s", flag.Name)
					}
				}
			}
			for _, field := range commandDataFields[command.ID] {
				if !strings.Contains(block, "`"+field+"`") {
					t.Errorf("guide omits its published data field %s", field)
				}
			}
		})
	}
}

func manualCommandExample(content, command string) (string, []string, error) {
	_, block, found := strings.Cut(content, "- `"+command+"`:")
	if !found {
		return "", nil, fmt.Errorf("command guide is missing")
	}
	block, _, _ = strings.Cut(block, "\n- `")
	block, _, _ = strings.Cut(block, "\n## ")
	_, example, found := strings.Cut(block, "Example: `")
	if !found {
		return "", nil, fmt.Errorf("command example is missing")
	}
	example, _, _ = strings.Cut(example, "`")
	prefix := "mailcli " + strings.ReplaceAll(command, ".", " ")
	if example != prefix && !strings.HasPrefix(example, prefix+" ") {
		return "", nil, fmt.Errorf("example uses a different command: %s", example)
	}
	return block, strings.Fields(strings.TrimPrefix(example, prefix)), nil
}
