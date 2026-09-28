package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"mailcli/internal/transport"
)

func TestHelpContractTable(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		want      string
		notWanted []string
	}{
		{name: "top level", args: []string{"help"}, want: "Usage:"},
		{name: "update", args: []string{"update", "--help"}, want: "mailcli update [options]"},
		{name: "capabilities", args: []string{"capabilities", "--help"}, want: "mailcli capabilities"},
		{name: "accounts", args: []string{"accounts", "--help"}, want: "mailcli accounts list"},
		{name: "accounts list", args: []string{"accounts", "list", "--help"}, want: "mailcli accounts list [options]"},
		{name: "mailboxes", args: []string{"mailboxes", "--help"}, want: "mailcli mailboxes"},
		{name: "mailboxes list", args: []string{"mailboxes", "list", "--help"}, want: "mailcli mailboxes list [options]"},
		{name: "mailboxes resolve", args: []string{"mailboxes", "resolve", "--help"}, want: "mailcli mailboxes resolve [options]"},
		{name: "messages", args: []string{"messages", "--help"}, want: "mailcli messages"},
		{name: "messages list", args: []string{"messages", "list", "--help"}, want: "mailcli messages list [options]"},
		{
			name: "messages filter", args: []string{"messages", "filter", "--help"},
			want: "mailcli messages filter [options]", notWanted: []string{"max-messages"},
		},
		{name: "messages search", args: []string{"messages", "search", "--help"}, want: "mailcli messages search [options]"},
		{name: "messages get", args: []string{"messages", "get", "--help"}, want: "mailcli messages get [REF] [options]"},
		{name: "messages raw", args: []string{"messages", "raw", "--help"}, want: "mailcli messages raw [REF] [options]"},
		{name: "messages state", args: []string{"messages", "state", "--help"}, want: "mailcli messages state [REF] [options]"},
		{name: "messages thread", args: []string{"messages", "thread", "--help"}, want: "mailcli messages thread [REF] [options]"},
		{name: "messages reply", args: []string{"messages", "reply", "--help"}, want: "mailcli messages reply [REF] [options]"},
		{
			name: "messages forward", args: []string{"messages", "forward", "--help"},
			want: "mailcli messages forward [REF] [options]", notWanted: []string{"--all"},
		},
		{name: "messages mark", args: []string{"messages", "mark", "--help"}, want: "mailcli messages mark [REF] [options]"},
		{name: "messages move", args: []string{"messages", "move", "--help"}, want: "mailcli messages move [REF] [options]"},
		{name: "messages copy", args: []string{"messages", "copy", "--help"}, want: "mailcli messages copy [REF] [options]"},
		{name: "messages delete", args: []string{"messages", "delete", "--help"}, want: "mailcli messages delete [REF] [options]"},
		{name: "attachments", args: []string{"attachments", "--help"}, want: "mailcli attachments"},
		{name: "attachments list", args: []string{"attachments", "list", "--help"}, want: "mailcli attachments list [REF] [options]"},
		{name: "attachments save", args: []string{"attachments", "save", "--help"}, want: "mailcli attachments save [REF] [options]"},
		{name: "drafts", args: []string{"drafts", "--help"}, want: "mailcli drafts"},
		{name: "drafts create", args: []string{"drafts", "create", "--help"}, want: "mailcli drafts create [options]"},
		{name: "drafts list", args: []string{"drafts", "list", "--help"}, want: "mailcli drafts list [options]"},
		{name: "drafts inspect", args: []string{"drafts", "inspect", "--help"}, want: "mailcli drafts inspect [REF] [options]"},
		{name: "drafts preview", args: []string{"drafts", "preview", "--help"}, want: "mailcli drafts preview [REF] [options]"},
		{name: "drafts edit", args: []string{"drafts", "edit", "--help"}, want: "mailcli drafts edit [REF] [options]"},
		{name: "drafts handoff", args: []string{"drafts", "handoff", "--help"}, want: "mailcli drafts handoff [REF] [options]"},
		{name: "drafts handoff reconcile", args: []string{"drafts", "handoff-reconcile", "--help"}, want: "mailcli drafts handoff-reconcile [REF] [options]"},
		{name: "drafts update", args: []string{"drafts", "update", "--help"}, want: "mailcli drafts update [REF] [options]"},
		{name: "drafts open", args: []string{"drafts", "open", "--help"}, want: "mailcli drafts open [REF] [options]"},
		{name: "drafts send", args: []string{"drafts", "send", "--help"}, want: "mailcli drafts send [REF] [options]"},
		{name: "send", args: []string{"send", "--help"}, want: "mailcli send setup"},
		{name: "send setup", args: []string{"send", "setup", "--help"}, want: "mailcli send setup [options]"},
		{name: "drafts reconcile", args: []string{"drafts", "reconcile", "--help"}, want: "mailcli drafts reconcile [REF] [options]"},
		{name: "drafts discard", args: []string{"drafts", "discard", "--help"}, want: "mailcli drafts discard [REF] [options]"},
		{name: "sync", args: []string{"sync", "--help"}, want: "mailcli sync [options]"},
		{name: "doctor", args: []string{"doctor", "--help"}, want: "mailcli doctor"},
		{name: "version", args: []string{"version", "--help"}, want: "mailcli version"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			variants := [][]string{test.args}
			if test.args[len(test.args)-1] == "--help" {
				wordVariant := append([]string(nil), test.args...)
				wordVariant[len(wordVariant)-1] = "help"
				variants = append(variants, wordVariant)
			}
			for _, args := range variants {
				var stdout bytes.Buffer
				var stderr bytes.Buffer
				code := Run(context.Background(), newTestService(), args, &stdout, &stderr)
				if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), test.want) {
					t.Fatalf("args = %q, code = %d, stdout = %q, stderr = %q", args, code, stdout.String(), stderr.String())
				}
				for _, unwanted := range test.notWanted {
					if strings.Contains(stdout.String(), unwanted) {
						t.Fatalf("args = %q, stdout = %q, must not contain %q", args, stdout.String(), unwanted)
					}
				}
			}
		})
	}
}

func TestCommandContractsAreUniqueAndDispatchable(t *testing.T) {
	seen := make(map[string]struct{}, len(commandContracts))
	for _, contract := range commandContracts {
		if _, exists := seen[contract.ID]; exists {
			t.Errorf("duplicate command contract %q", contract.ID)
		}
		seen[contract.ID] = struct{}{}
		if contract.handler == nil {
			t.Errorf("command contract %q has no handler", contract.ID)
		}
		if len(commandFamilyChoices(strings.SplitN(contract.ID, ".", 2)[0])) > 0 &&
			commandFamilyContract(strings.SplitN(contract.ID, ".", 2)[0]).familyHandler == nil {
			t.Errorf("command family %q has no table router", strings.SplitN(contract.ID, ".", 2)[0])
		}
	}
}

func TestHelpAndFamilyChoicesComeFromCommandContracts(t *testing.T) {
	var topLevel bytes.Buffer
	var stderr bytes.Buffer
	if code := Run(context.Background(), newTestService(), []string{"help"}, &topLevel, &stderr); code != 0 {
		t.Fatalf("top-level help code = %d, stderr = %q", code, stderr.String())
	}
	for _, contract := range commandRootContracts() {
		command := strings.SplitN(contract.ID, ".", 2)[0]
		entry := fmt.Sprintf("%s: %s", command, contract.helpDescription)
		if contract.helpDescription == "" || !strings.Contains(topLevel.String(), entry) {
			t.Errorf("top-level help is missing contract entry %q", entry)
		}
		choices := commandFamilyChoices(command)
		if len(choices) == 0 {
			continue
		}
		var familyHelp bytes.Buffer
		if code := Run(context.Background(), newTestService(), []string{command, "--help"}, &familyHelp, &stderr); code != 0 {
			t.Errorf("%s help code = %d, stderr = %q", command, code, stderr.String())
		}
		if !strings.Contains(familyHelp.String(), strings.Join(choices, "|")) {
			t.Errorf("%s help = %q, want table choices %q", command, familyHelp.String(), choices)
		}
	}
}

func TestTopLevelHelpIsCompact(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), newTestService(), []string{"help"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if lines := strings.Count(strings.TrimSpace(stdout.String()), "\n") + 1; lines > 24 {
		t.Fatalf("help has %d lines, want at most 24:\n%s", lines, stdout.String())
	}
	for _, command := range []string{"messages", "drafts", "update", "doctor", "help"} {
		if !strings.Contains(stdout.String(), command) {
			t.Fatalf("help does not contain %q: %s", command, stdout.String())
		}
	}
	if stdout.Len() > 900 {
		t.Fatalf("help has %d bytes, want at most 900: %s", stdout.Len(), stdout.String())
	}
	t.Logf("top-level help: %d bytes", stdout.Len())
	if !strings.Contains(stdout.String(), "send: ") {
		t.Fatalf("help omits the send command: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Output: --json/--human > MAILCLI_OUTPUT=json|human > pipe:JSON, TTY:human.") {
		t.Fatalf("help omits the output rule: %s", stdout.String())
	}
	manual, err := os.ReadFile("../../docs/documentation.md")
	if err != nil {
		t.Fatal(err)
	}
	_, composition, _ := strings.Cut(string(manual), "\n## Drafts and composition\n")
	composition, _, _ = strings.Cut(composition, "\n### ")
	for _, caveat := range []string{"Mail 16", "transport_unsupported_provider", "handoff"} {
		if !strings.Contains(composition, caveat) {
			t.Fatalf("manual composition topic omits caveat %q", caveat)
		}
	}
}

func TestHelpSurvivesJSONFinalization(t *testing.T) {
	for _, contract := range commandContracts {
		path := strings.Split(contract.ID, ".")
		for _, suffix := range [][]string{{"--help"}, {"help"}, {"--help", "--json"}} {
			args := append(append([]string(nil), path...), suffix...)
			var help bytes.Buffer
			var stderr bytes.Buffer
			if code := Run(context.Background(), newTestService(), args, &help, &stderr); code != 0 {
				t.Fatalf("args = %q, code = %d, stderr = %q", args, code, stderr.String())
			}
			var stdout bytes.Buffer
			if code := FinalizeJSON(&stdout, args, help.Bytes(), 0, nil); code != 0 || stdout.String() != help.String() {
				t.Errorf("args = %q, finalized code = %d, stdout = %q", args, code, stdout.String())
			}
		}
	}
}

func TestHelpOmitsRepositoryPaths(t *testing.T) {
	argumentSets := [][]string{{"help"}}
	for _, contract := range commandContracts {
		argumentSets = append(argumentSets, append(strings.Split(contract.ID, "."), "--help"))
	}
	for _, contract := range commandRootContracts() {
		argumentSets = append(argumentSets, []string{strings.SplitN(contract.ID, ".", 2)[0], "--help"})
	}
	for _, args := range argumentSets {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		if code := Run(context.Background(), newTestService(), args, &stdout, &stderr); code != 0 {
			t.Fatalf("args = %q, code = %d, stderr = %q", args, code, stderr.String())
		}
		for _, path := range []string{"docs/", "README", ".md", "skills/", "scripts/"} {
			if strings.Contains(stdout.String(), path) {
				t.Errorf("args = %q help contains repository path %q: %s", args, path, stdout.String())
			}
		}
	}
}

func TestFocusedHelpUsesProfessionalOptionFormatting(t *testing.T) {
	tests := []struct {
		args    []string
		want    []string
		notWant []string
	}{
		{
			args: []string{"messages", "search", "help"},
			want: []string{"Options:", "--mailbox <ref>", "--attachment <true|false>", "--max-bytes <int>", "(default: 1 MiB)", "--max-scan-bytes <bytes>", "(default: 4 GiB)", "-h, --help"},
		},
		{
			args:    []string{"drafts", "preview", "help"},
			want:    []string{"--preview-format"},
			notWant: []string{"--format"},
		},
		{
			args:    []string{"messages", "copy", "help"},
			want:    []string{"Options:", "--ref <ref>", "--mailbox <ref>"},
			notWant: []string{"--allow-draft"},
		},
		{
			args: []string{"messages", "move", "help"},
			want: []string{"--allow-draft", "Allow moving a source message that is a draft"},
		},
		{
			args: []string{"drafts", "prune", "help"},
			want: []string{"Options:", "--older-than <int>", "(default: 30)", "--confirm", "--json"},
		},
		{
			args: []string{"send", "setup", "help"},
			want: []string{transport.ProviderSupportDescription()},
		},
	}
	for _, test := range tests {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		code := Run(context.Background(), newTestService(), test.args, &stdout, &stderr)
		if code != 0 || stderr.Len() != 0 {
			t.Fatalf("args = %q, code = %d, stderr = %q", test.args, code, stderr.String())
		}
		for _, wanted := range test.want {
			if !strings.Contains(stdout.String(), wanted) {
				t.Fatalf("args = %q, stdout = %q, want %q", test.args, stdout.String(), wanted)
			}
		}
		for _, unwanted := range test.notWant {
			if strings.Contains(stdout.String(), unwanted) {
				t.Fatalf("args = %q, stdout = %q, must not contain %q", test.args, stdout.String(), unwanted)
			}
		}
	}
}

func TestOwnedIndexCommandsAreRejected(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "top-level index", args: []string{"index", "status"}},
		{name: "index refresh", args: []string{"index", "refresh"}},
		{name: "message index source", args: []string{"messages", "index-source"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := Run(context.Background(), newTestService(), test.args, &stdout, &stderr)
			if code != 2 || stderr.Len() == 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
			}
		})
	}
}
