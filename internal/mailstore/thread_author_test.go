package mailstore

import (
	"bytes"
	"context"
	stdmail "net/mail"
	"reflect"
	"testing"

	"mailcli/internal/mail"
)

func TestDerivedReplyRequiresUnambiguousAuthor(t *testing.T) {
	for _, test := range []struct {
		name, headers string
		explicit      bool
		invalid       bool
		want          []string
	}{
		{name: "single author", headers: "From: Alice <alice@example.com>\r\n", want: []string{"alice@example.com"}},
		{name: "multiple authors", headers: "From: alice@example.com, bob@example.com\r\n", invalid: true},
		{name: "incompatible duplicate authors", headers: "From: alice@example.com\r\nFrom: bob@example.com\r\n", invalid: true},
		{name: "incompatible duplicate names", headers: "From: Alice <alice@example.com>\r\nFrom: Other <alice@example.com>\r\n", invalid: true},
		{name: "malformed duplicate", headers: "From: alice@example.com\r\nFrom: malformed\r\n", invalid: true},
		{name: "compatible duplicate domains", headers: "From: Alice <alice@EXAMPLE.COM>\r\nFrom: Alice <alice@example.com>\r\n", want: []string{"alice@EXAMPLE.COM"}},
		{name: "Reply-To wins", headers: "From: alice@example.com, bob@example.com\r\nReply-To: carol@example.com, dave@example.com\r\n", want: []string{"carol@example.com", "dave@example.com"}},
		{name: "explicit To wins", headers: "From: alice@example.com, bob@example.com\r\n", explicit: true, want: []string{"chosen@example.com"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, inbox := newSearchFixture(t)
			closeTestResource(t, store, "author fixture")
			page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inbox, Limit: 3})
			if err != nil {
				t.Fatal(err)
			}
			ref := messageRefWithSubject(t, page.Messages, "Status Update")
			writeFixtureEMLX(t, store, 102, "imap://"+testAccountID+"/INBOX", []byte(test.headers+"Message-ID: <102@example.com>\r\n\r\nbody\r\n"))
			client := &Client{store: store}
			source, err := client.MessageThreadSource(context.Background(), ref)
			if err != nil {
				t.Fatal(err)
			}
			input := mail.DraftInput{From: "sender@example.com", Body: "Reply"}
			if test.explicit {
				input.ToSet, input.To = true, []mail.Recipient{{Address: "chosen@example.com"}}
			}
			input, parent, chain, err := mail.DeriveReplyInput(source, mail.DraftKindReply, false, input, nil)
			if test.invalid {
				if nestedErrorCode(err) != "invalid_message_source" {
					t.Fatalf("ambiguous author accepted: input=%+v error=%v", input, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			service := mail.NewServiceWithDraftRoot(client, t.TempDir())
			draft, err := service.CreateDraft(mail.CreateDraftRequest{Kind: mail.DraftKindReply, SourceRef: ref, Input: input, SourceMessageID: parent, SourceReferences: chain})
			if err != nil {
				t.Fatal(err)
			}
			payload, err := mail.BuildMessage(draft, "<reply@example.com>")
			if err != nil {
				t.Fatal(err)
			}
			message, err := stdmail.ReadMessage(bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			addresses, err := message.Header.AddressList("To")
			var got []string
			for _, address := range addresses {
				got = append(got, address.Address)
			}
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("composed To=%v error=%v; want %v", got, err, test.want)
			}
		})
	}
}
