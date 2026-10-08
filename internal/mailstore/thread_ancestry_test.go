package mailstore

import (
	"bytes"
	"context"
	stdmail "net/mail"
	"path/filepath"
	"testing"

	"mailcli/internal/mail"
)

func TestDerivedReplyPreservesFallbackAncestry(t *testing.T) {
	for _, test := range []struct {
		name    string
		headers string
		chain   string
		invalid bool
	}{
		{name: "single ancestor", headers: "In-Reply-To: <ancestor@id>\r\n", chain: "<ancestor@id> <102@example.com>"},
		{name: "commented ancestor", headers: "In-Reply-To: (before) <ancestor@id> (after)\r\n", chain: "<ancestor@id> <102@example.com>"},
		{name: "no fallback", chain: "<102@example.com>"},
		{name: "multiple ancestors", headers: "In-Reply-To: <first@id> <second@id>\r\n", chain: "<102@example.com>"},
		{name: "duplicate parent", headers: "In-Reply-To: <102@example.com>\r\n", chain: "<102@example.com>"},
		{name: "References precedence", headers: "References: <root@id>\r\nIn-Reply-To: malformed\r\n", chain: "<root@id> <102@example.com>"},
		{name: "malformed fallback", headers: "In-Reply-To: <bad@@id>\r\n", invalid: true},
		{name: "empty In-Reply-To", headers: "In-Reply-To:\r\n", chain: "<102@example.com>"},
		{name: "whitespace In-Reply-To", headers: "In-Reply-To:  \r\n", chain: "<102@example.com>"},
		{name: "empty References and In-Reply-To", headers: "References:\r\nIn-Reply-To:\r\n", chain: "<102@example.com>"},
		{name: "References with empty In-Reply-To", headers: "References: <root@id>\r\nIn-Reply-To:\r\n", chain: "<root@id> <102@example.com>"},
		{name: "comment-only fallback", headers: "In-Reply-To: (no id)\r\n", invalid: true},
		{name: "empty References with fallback", headers: "References:\r\nIn-Reply-To: <ancestor@id>\r\n", chain: "<ancestor@id> <102@example.com>"},
		{name: "empty References only", headers: "References: \r\n", chain: "<102@example.com>"},
		{name: "References without Message-ID", headers: "References: garbage\r\n", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, inbox := newSearchFixture(t)
			closeTestResource(t, store, "ancestry fixture")
			page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inbox, Limit: 3})
			if err != nil {
				t.Fatal(err)
			}
			ref := messageRefWithSubject(t, page.Messages, "Status Update")
			writeFixtureEMLX(t, store, 102, "imap://"+testAccountID+"/INBOX", []byte(
				"From: Alice <alice@example.com>\r\nSubject: Status Update\r\nMessage-ID: <102@example.com>\r\n"+test.headers+"\r\nbody\r\n"))
			client := &Client{store: store}
			source, err := client.MessageThreadSource(context.Background(), ref)
			if err != nil {
				t.Fatal(err)
			}
			input, parent, chain, err := mail.DeriveReplyInput(source, mail.DraftKindReply, false, mail.DraftInput{From: "reply@gmail.com", Body: "Reply"}, nil)
			if test.invalid {
				if nestedErrorCode(err) != "invalid_message_source" {
					t.Fatalf("malformed ancestry accepted: chain=%q error=%v", chain, err)
				}
				return
			}
			if err != nil || chain != test.chain {
				t.Fatalf("derived chain=%q error=%v; want %q", chain, err, test.chain)
			}
			service := mail.NewServiceWithDraftRoot(client, filepath.Join(t.TempDir(), "drafts"))
			draft, err := service.CreateDraft(mail.CreateDraftRequest{
				Kind: mail.DraftKindReply, SourceRef: ref, Input: input,
				SourceMessageID: parent, SourceReferences: chain,
			})
			if err != nil {
				t.Fatal(err)
			}
			payload, err := mail.BuildMessage(draft, "<reply@example.com>")
			if err != nil {
				t.Fatal(err)
			}
			message, err := stdmail.ReadMessage(bytes.NewReader(payload))
			if err != nil || message.Header.Get("References") != test.chain || message.Header.Get("In-Reply-To") != "<102@example.com>" {
				t.Fatalf("composed threading message=%+v error=%v", message, err)
			}
		})
	}
}
