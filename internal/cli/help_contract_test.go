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
		{name: "capabilities", args: []string{"capabilities", "--help"}, want: "mailcli capabilities"},
		{name: "accounts", args: []string{"accounts", "--help"}, want: "mailcli accounts list"},
		{name: "accounts list", args: []string{"accounts", "list", "--help"}, want: "Usage of accounts list:"},
		{name: "mailboxes", args: []string{"mailboxes", "--help"}, want: "mailcli mailboxes"},
		{name: "mailboxes list", args: []string{"mailboxes", "list", "--help"}, want: "Usage of mailboxes list:"},
		{name: "mailboxes resolve", args: []string{"mailboxes", "resolve", "--help"}, want: "Usage of mailboxes resolve:"},
		{name: "messages", args: []string{"messages", "--help"}, want: "mailcli messages"},
		{name: "messages list", args: []string{"messages", "list", "--help"}, want: "Usage of messages list:"},
		{
			name: "messages filter", args: []string{"messages", "filter", "--help"},
			want: "Usage of messages filter:", notWanted: []string{"max-bytes", "max-messages"},
		},
		{name: "messages search", args: []string{"messages", "search", "--help"}, want: "Usage of messages search:"},
		{name: "messages get", args: []string{"messages", "get", "--help"}, want: "Usage of messages get:"},
		{name: "messages raw", args: []string{"messages", "raw", "--help"}, want: "Usage of messages raw:"},
		{name: "messages reply", args: []string{"messages", "reply", "--help"}, want: "Usage of messages reply:"},
		{
			name: "messages forward", args: []string{"messages", "forward", "--help"},
			want: "Usage of messages forward:", notWanted: []string{"-all"},
		},
		{name: "messages mark", args: []string{"messages", "mark", "--help"}, want: "Usage of messages mark:"},
		{name: "messages move", args: []string{"messages", "move", "--help"}, want: "Usage of messages move:"},
		{name: "messages copy", args: []string{"messages", "copy", "--help"}, want: "Usage of messages copy:"},
		{name: "messages delete", args: []string{"messages", "delete", "--help"}, want: "Usage of messages delete:"},
		{name: "attachments", args: []string{"attachments", "--help"}, want: "mailcli attachments"},
		{name: "attachments list", args: []string{"attachments", "list", "--help"}, want: "Usage of attachments list:"},
		{name: "attachments save", args: []string{"attachments", "save", "--help"}, want: "Usage of attachments save:"},
		{name: "drafts", args: []string{"drafts", "--help"}, want: "mailcli drafts"},
		{name: "drafts create", args: []string{"drafts", "create", "--help"}, want: "Usage of drafts create:"},
		{name: "drafts list", args: []string{"drafts", "list", "--help"}, want: "Usage of drafts list:"},
		{name: "drafts inspect", args: []string{"drafts", "inspect", "--help"}, want: "Usage of drafts inspect:"},
		{name: "drafts update", args: []string{"drafts", "update", "--help"}, want: "Usage of drafts update:"},
		{name: "drafts save", args: []string{"drafts", "save", "--help"}, want: "Usage of drafts save:"},
		{name: "drafts open", args: []string{"drafts", "open", "--help"}, want: "Usage of drafts open:"},
		{name: "drafts send", args: []string{"drafts", "send", "--help"}, want: "Usage of drafts send:"},
		{name: "drafts reconcile", args: []string{"drafts", "reconcile", "--help"}, want: "Usage of drafts reconcile:"},
		{name: "drafts discard", args: []string{"drafts", "discard", "--help"}, want: "Usage of drafts discard:"},
		{name: "sync", args: []string{"sync", "--help"}, want: "Usage of sync:"},
		{name: "doctor", args: []string{"doctor", "--help"}, want: "mailcli doctor"},
		{name: "version", args: []string{"version", "--help"}, want: "mailcli version"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := Run(context.Background(), newTestService(), test.args, &stdout, &stderr)
			if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), test.want) {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
			}
			for _, unwanted := range test.notWanted {
				if strings.Contains(stdout.String(), unwanted) {
					t.Fatalf("stdout = %q, must not contain %q", stdout.String(), unwanted)
				}
			}
		})
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
