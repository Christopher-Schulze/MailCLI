package cli

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

const manualTopics = "Overview|For agents|Install and update|Setup|Commands|Workflows|Output contract|Errors and recovery|Limits|" +
	"Reading and search|Drafts and composition|Sending|Visible handoff|Mailbox mutations and batch|Accounts and bindings|" +
	"Mail.app integration|Security|Platform and compatibility|Release and distribution|Architecture|Development"

func validateManualStructure(content string, commands []string) error {
	var headings []string
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "## ") {
			headings = append(headings, strings.TrimPrefix(line, "## "))
		}
	}
	if strings.Join(headings, "|") != manualTopics {
		return fmt.Errorf("manual topics differ: %v", headings)
	}
	for _, command := range commands {
		marker := "- `" + command + "`:"
		if strings.Count(content, marker) != 1 {
			return fmt.Errorf("command %s must have exactly one guide", command)
		}
		_, block, _ := strings.Cut(content, marker)
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
		if !strings.Contains(content, "["+topic+"](#"+headingAnchor(topic)+")") {
			t.Errorf("manual TOC omits %s", topic)
		}
	}
	anchors := make(map[string]bool)
	for _, line := range strings.Split(content, "\n") {
		if heading, found := strings.CutPrefix(strings.TrimLeft(line, "#"), " "); found && strings.HasPrefix(line, "#") {
			anchors[headingAnchor(heading)] = true
		}
	}
	links := map[string]*regexp.Regexp{
		"docs/documentation.md": regexp.MustCompile(`\]\(#([a-z0-9-]+)\)`),
		"README.md":             regexp.MustCompile(`docs/documentation\.md#([a-z0-9-]+)`),
	}
	for path, pattern := range links {
		for _, match := range pattern.FindAllStringSubmatch(readRepositoryFile(t, path), -1) {
			if !anchors[match[1]] {
				t.Errorf("%s links to missing manual anchor #%s", path, match[1])
			}
		}
	}
}

// headingAnchor mirrors GitHub's heading slugs: lowercase, spaces become
// hyphens, and punctuation other than hyphens is dropped.
func headingAnchor(heading string) string {
	var anchor strings.Builder
	for _, character := range strings.ToLower(heading) {
		switch {
		case character == ' ':
			anchor.WriteRune('-')
		case character == '-' || unicode.IsLetter(character) || unicode.IsDigit(character):
			anchor.WriteRune(character)
		}
	}
	return anchor.String()
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
