package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"mailcli/internal/mail"
)

type mailboxSelectionGateway struct {
	testGateway
	request mail.ListMessagesRequest
}

func (g *mailboxSelectionGateway) ListMessages(_ context.Context, request mail.ListMessagesRequest) (mail.MessagePage, error) {
	g.request = request
	return mail.MessagePage{Messages: []mail.MessageSummary{{
		Ref: "message-reference", MailboxRef: "resolved-mailbox", Account: "resolved-account", Subject: "Inbox subject",
	}}}, nil
}

func TestMessagesListMailboxSelectorAndProjectedAccount(t *testing.T) {
	for _, test := range []struct {
		name    string
		args    []string
		mailbox string
		account string
	}{
		{name: "unified default"},
		{name: "role", args: []string{"--mailbox", "inbox", "--account", "account-reference"}, mailbox: "inbox", account: "account-reference"},
		{name: "exact path", args: []string{"--mailbox", "Projects/Reports"}, mailbox: "Projects/Reports"},
	} {
		t.Run(test.name, func(t *testing.T) {
			gateway := &mailboxSelectionGateway{}
			var stdout, stderr bytes.Buffer
			args := append([]string{"messages", "list", "--json", "--fields", "subject"}, test.args...)
			code := Run(context.Background(), mail.NewService(gateway), args, &stdout, &stderr)
			if code != 0 || stderr.Len() != 0 {
				t.Fatalf("code=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if gateway.request.MailboxRef != test.mailbox || gateway.request.AccountRef != test.account || gateway.request.Limit != mail.DefaultPageLimit {
				t.Fatalf("request=%+v", gateway.request)
			}
			var response struct {
				Data struct {
					Page mail.MessagePage `json:"page"`
				} `json:"data"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Data.Page.Messages) != 1 {
				t.Fatalf("page=%+v", response.Data.Page)
			}
			item := response.Data.Page.Messages[0]
			if item.Ref != "message-reference" || item.MailboxRef != "resolved-mailbox" || item.Account != "resolved-account" || item.Subject != "Inbox subject" {
				t.Fatalf("projected identity lost: %+v", item)
			}
		})
	}
}
