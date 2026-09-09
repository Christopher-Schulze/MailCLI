package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

type unknownSubmitter struct{}

func (unknownSubmitter) Submit(
	context.Context,
	transport.SubmitConfig,
	string,
	[]string,
	[]byte,
) (transport.SubmitEvidence, error) {
	return transport.SubmitEvidence{}, &transport.SubmissionError{Stage: "final_reply"}
}

func TestRequiresMailService(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "no command"},
		{name: "help", args: []string{"help"}},
		{name: "version", args: []string{"version", "--json"}},
		{name: "update", args: []string{"update", "--json"}},
		{name: "capabilities", args: []string{"capabilities", "--json"}},
		{name: "global JSON version", args: []string{"--json", "version"}},
		{name: "batch without input", args: []string{"batch"}},
		{name: "batch execution", args: []string{"batch", "--input", "-"}, want: true},
		{name: "unknown command", args: []string{"unknown"}},
		{name: "command help", args: []string{"messages", "--help"}},
		{name: "command help with JSON", args: []string{"drafts", "help", "--json"}},
		{name: "subcommand help", args: []string{"messages", "search", "--help"}},
		{name: "global JSON subcommand help", args: []string{"--json", "attachments", "save", "-h"}},
		{name: "missing group subcommand", args: []string{"messages"}},
		{name: "unknown group subcommand", args: []string{"messages", "missing"}},
		{name: "local draft create", args: []string{"drafts", "create", "--input", "-"}},
		{name: "local draft list", args: []string{"drafts", "list"}},
		{name: "local draft inspect", args: []string{"drafts", "inspect", "--ref", "draft"}},
		{name: "local draft preview", args: []string{"drafts", "preview", "--ref", "draft"}},
		{name: "local draft edit", args: []string{"drafts", "edit", "--ref", "draft"}},
		{name: "visible draft handoff", args: []string{"drafts", "handoff", "--ref", "draft"}},
		{name: "local draft update", args: []string{"drafts", "update", "--ref", "draft", "--input", "-"}},
		{name: "local draft discard", args: []string{"drafts", "discard", "--ref", "draft", "--confirm"}},
		{name: "local draft save", args: []string{"drafts", "save", "--ref", "draft"}},
		{name: "direct draft send", args: []string{"drafts", "send", "--ref", "draft", "--confirm"}},
		{name: "send group", args: []string{"send"}},
		{name: "send setup", args: []string{"send", "setup", "--from", "user@icloud.com"}},
		{name: "send setup remove", args: []string{"send", "setup", "--from", "user@icloud.com", "--remove"}},
		{name: "reply draft resolves source", args: []string{"messages", "reply", "--message", "ref"}, want: true},
		{name: "forward draft resolves source", args: []string{"messages", "forward", "--message", "ref"}, want: true},
		{name: "doctor", args: []string{"doctor"}, want: true},
		{name: "list accounts", args: []string{"accounts", "list"}, want: true},
		{name: "read message", args: []string{"--json", "messages", "get", "--ref", "ref"}, want: true},
		{name: "open Mail draft", args: []string{"drafts", "open", "--message", "ref"}, want: true},
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

func TestRequiresSignalContext(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{args: []string{"version"}},
		{args: []string{"help"}},
		{args: []string{"capabilities", "--json"}},
		{args: []string{"batch"}},
		{args: []string{"batch", "--input", "-"}, want: true},
		{args: []string{"drafts", "list"}},
		{args: []string{"drafts", "create", "--to", "recipient@example.com", "--body", "body"}, want: true},
		{args: []string{"drafts", "edit", "--ref", "draft"}, want: true},
		{args: []string{"drafts", "update", "--ref", "draft"}, want: true},
		{args: []string{"messages", "reply", "--message", "ref"}, want: true},
		{args: []string{"messages", "forward", "--message", "ref"}, want: true},
		{args: []string{"drafts", "handoff", "--ref", "draft"}, want: true},
		{args: []string{"drafts", "save", "--ref", "draft"}, want: true},
		{args: []string{"drafts", "send", "--ref", "draft", "--confirm"}, want: true},
		{args: []string{"drafts", "reconcile", "--ref", "draft"}, want: true},
		{args: []string{"update"}, want: true},
		{args: []string{"--json", "update"}, want: true},
		{args: []string{"messages", "search", "--query", "text"}, want: true},
		{args: []string{"sync"}, want: true},
	}
	for _, test := range tests {
		if got := RequiresSignalContext(test.args); got != test.want {
			t.Fatalf("RequiresSignalContext(%q) = %t, want %t", test.args, got, test.want)
		}
	}
}

func TestRequiresMainThread(t *testing.T) {
	for _, test := range []struct {
		args []string
		want bool
	}{
		{args: []string{"drafts", "list"}},
		{args: []string{"drafts", "handoff", "--ref", "draft"}, want: true},
		{args: []string{"--json", "drafts", "handoff", "--ref", "draft"}, want: true},
	} {
		if got := RequiresMainThread(test.args); got != test.want {
			t.Fatalf("RequiresMainThread(%q) = %t, want %t", test.args, got, test.want)
		}
	}
}

func TestRequiresMailServiceSkipsDirectReconcile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configRoot, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir() error = %v", err)
	}
	root := filepath.Join(configRoot, "MailCLI", "drafts")
	service := mail.NewServiceWithTransport(nil, root, mail.SendTransport{
		Submitter:   unknownSubmitter{},
		Mirror:      &cliMirror{},
		Credentials: cliCredentials{},
	})
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		From: "sender@icloud.com", To: []mail.Recipient{{Address: "recipient@example.com"}},
		Subject: "Direct recovery", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	if _, err := service.SendDraft(context.Background(), draft.Ref); err == nil {
		t.Fatal("SendDraft() error = nil, want unknown submission")
	} else {
		var submissionErr *transport.SubmissionError
		if !errors.As(err, &submissionErr) {
			t.Fatalf("SendDraft() error = %v, want %s", err, transport.CodeSMTPSubmissionUnknown)
		}
	}
	args := []string{"drafts", "reconcile", "--ref", draft.Ref}
	if RequiresMailService(args) {
		t.Fatalf("RequiresMailService(%q) = true for a direct unknown claim", args)
	}
	if !RequiresSignalContext(args) {
		t.Fatalf("RequiresSignalContext(%q) = false for direct IMAP recovery", args)
	}
}
