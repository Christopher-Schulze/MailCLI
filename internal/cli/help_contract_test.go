package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
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
			want: "mailcli messages filter [options]", notWanted: []string{"max-bytes", "max-messages"},
		},
		{name: "messages search", args: []string{"messages", "search", "--help"}, want: "mailcli messages search [options]"},
		{name: "messages get", args: []string{"messages", "get", "--help"}, want: "mailcli messages get [options]"},
		{name: "messages raw", args: []string{"messages", "raw", "--help"}, want: "mailcli messages raw [options]"},
		{name: "messages reply", args: []string{"messages", "reply", "--help"}, want: "mailcli messages reply [options]"},
		{
			name: "messages forward", args: []string{"messages", "forward", "--help"},
			want: "mailcli messages forward [options]", notWanted: []string{"--all"},
		},
		{name: "messages mark", args: []string{"messages", "mark", "--help"}, want: "mailcli messages mark [options]"},
		{name: "messages move", args: []string{"messages", "move", "--help"}, want: "mailcli messages move [options]"},
		{name: "messages copy", args: []string{"messages", "copy", "--help"}, want: "mailcli messages copy [options]"},
		{name: "messages delete", args: []string{"messages", "delete", "--help"}, want: "mailcli messages delete [options]"},
		{name: "attachments", args: []string{"attachments", "--help"}, want: "mailcli attachments"},
		{name: "attachments list", args: []string{"attachments", "list", "--help"}, want: "mailcli attachments list [options]"},
		{name: "attachments save", args: []string{"attachments", "save", "--help"}, want: "mailcli attachments save [options]"},
		{name: "drafts", args: []string{"drafts", "--help"}, want: "mailcli drafts"},
		{name: "drafts create", args: []string{"drafts", "create", "--help"}, want: "mailcli drafts create [options]"},
		{name: "drafts list", args: []string{"drafts", "list", "--help"}, want: "mailcli drafts list [options]"},
		{name: "drafts inspect", args: []string{"drafts", "inspect", "--help"}, want: "mailcli drafts inspect [options]"},
		{name: "drafts preview", args: []string{"drafts", "preview", "--help"}, want: "mailcli drafts preview [options]"},
		{name: "drafts edit", args: []string{"drafts", "edit", "--help"}, want: "mailcli drafts edit [options]"},
		{name: "drafts handoff", args: []string{"drafts", "handoff", "--help"}, want: "mailcli drafts handoff [options]"},
		{name: "drafts update", args: []string{"drafts", "update", "--help"}, want: "mailcli drafts update [options]"},
		{name: "drafts save", args: []string{"drafts", "save", "--help"}, want: "mailcli drafts save [options]"},
		{name: "drafts open", args: []string{"drafts", "open", "--help"}, want: "mailcli drafts open [options]"},
		{name: "drafts send", args: []string{"drafts", "send", "--help"}, want: "mailcli drafts send [options]"},
		{name: "send", args: []string{"send", "--help"}, want: "mailcli send setup"},
		{name: "send setup", args: []string{"send", "setup", "--help"}, want: "mailcli send setup [options]"},
		{name: "drafts reconcile", args: []string{"drafts", "reconcile", "--help"}, want: "mailcli drafts reconcile [options]"},
		{name: "drafts discard", args: []string{"drafts", "discard", "--help"}, want: "mailcli drafts discard [options]"},
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
	if !strings.Contains(stdout.String(), "Mail 16 scripted draft save remains disabled") {
		t.Fatalf("help omits the Mail 16 compose limitation: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "send ") {
		t.Fatalf("help omits the send command: %s", stdout.String())
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
			want: []string{"Options:", "--mailbox <ref>", "--attachment <true|false>", "--max-bytes <bytes>", "(default: 4 GiB)", "-h, --help"},
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
