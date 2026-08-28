package cli

import "testing"

func TestRequiresMailService(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "no command"},
		{name: "help", args: []string{"help"}},
		{name: "version", args: []string{"version", "--json"}},
		{name: "capabilities", args: []string{"capabilities", "--json"}},
		{name: "global JSON version", args: []string{"--json", "version"}},
		{name: "unknown command", args: []string{"unknown"}},
		{name: "command help", args: []string{"messages", "--help"}},
		{name: "command help with JSON", args: []string{"drafts", "help", "--json"}},
		{name: "subcommand help", args: []string{"messages", "search", "--help"}},
		{name: "global JSON subcommand help", args: []string{"--json", "attachments", "save", "-h"}},
		{name: "local draft create", args: []string{"drafts", "create", "--input", "-"}},
		{name: "local draft list", args: []string{"drafts", "list"}},
		{name: "local draft inspect", args: []string{"drafts", "inspect", "--ref", "draft"}},
		{name: "local draft update", args: []string{"drafts", "update", "--ref", "draft", "--input", "-"}},
		{name: "local draft discard", args: []string{"drafts", "discard", "--ref", "draft", "--confirm"}},
		{name: "doctor", args: []string{"doctor"}, want: true},
		{name: "list accounts", args: []string{"accounts", "list"}, want: true},
		{name: "read message", args: []string{"--json", "messages", "get", "--ref", "ref"}, want: true},
		{name: "save Mail draft", args: []string{"drafts", "save", "--ref", "draft"}, want: true},
		{name: "send draft", args: []string{"drafts", "send", "--ref", "draft", "--confirm"}, want: true},
		{name: "reconcile draft", args: []string{"drafts", "reconcile", "--ref", "draft"}, want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := RequiresMailService(test.args); got != test.want {
				t.Fatalf("RequiresMailService(%q) = %t, want %t", test.args, got, test.want)
			}
		})
	}
}
