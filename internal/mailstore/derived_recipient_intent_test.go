package mailstore

import (
	"bytes"
	"context"
	stdmail "net/mail"
	"reflect"
	"testing"

	"mailcli/internal/mail"
)

func TestDerivedRecipientsValidateOnlyRequiredSourceAddresses(t *testing.T) {
	for _, test := range []struct {
		name, headers                        string
		forward, explicit, implicit, emptyTo bool
		all, overrideCC, invalid, missingID  bool
		wantTo, wantCC                       []string
	}{
		{name: "automatic malformed Reply-To", headers: "From: alice@example.com\r\nReply-To: malformed\r\n", invalid: true},
		{name: "automatic empty Reply-To", headers: "From: alice@example.com\r\nReply-To:\r\n", invalid: true},
		{name: "automatic duplicate Reply-To", headers: "From: alice@example.com\r\nReply-To: bob@example.com\r\nReply-To: carol@example.com\r\n", invalid: true},
		{name: "explicit malformed Reply-To", headers: "From: alice@example.com\r\nReply-To: malformed\r\n", explicit: true, wantTo: []string{"chosen@example.com"}},
		{name: "explicit missing From", explicit: true, wantTo: []string{"chosen@example.com"}},
		{name: "explicit malformed From", headers: "From: malformed\r\n", explicit: true, wantTo: []string{"chosen@example.com"}},
		{name: "nonempty To without presence flag", headers: "Reply-To: malformed\r\n", explicit: true, implicit: true, wantTo: []string{"chosen@example.com"}},
		{name: "explicit empty To", headers: "Reply-To: malformed\r\n", explicit: true, emptyTo: true},
		{name: "forward ignores Reply-To", headers: "Reply-To: malformed\r\n", forward: true, explicit: true, wantTo: []string{"chosen@example.com"}},
		{name: "reply-all explicit To promotes complete recipients", headers: "Reply-To: malformed\r\nTo: chosen@example.com, me@example.com, to@example.com\r\nCc: copy@example.com\r\n", explicit: true, all: true, wantTo: []string{"chosen@example.com"}, wantCC: []string{"to@example.com", "copy@example.com"}},
		{name: "reply-all incomplete CC remains blocked", headers: "Reply-To: malformed\r\nCc: malformed\r\n", explicit: true, all: true, invalid: true},
		{name: "reply-all explicit empty CC", headers: "Reply-To: malformed\r\nCc: malformed\r\n", explicit: true, all: true, overrideCC: true, wantTo: []string{"chosen@example.com"}},
		{name: "reply-all explicit nonempty CC", headers: "Reply-To: malformed\r\nTo: malformed\r\nCc: malformed\r\n", explicit: true, all: true, overrideCC: true, wantTo: []string{"chosen@example.com"}, wantCC: []string{"other@example.com"}},
		{name: "source identity remains required", explicit: true, missingID: true, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, inbox := newSearchFixture(t)
			closeTestResource(t, store, "recipient intent fixture")
			page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inbox, Limit: 3})
			if err != nil {
				t.Fatal(err)
			}
			ref := messageRefWithSubject(t, page.Messages, "Status Update")
			headers := test.headers
			if !test.missingID {
				headers += "Message-ID: <102@example.com>\r\n"
			}
			writeFixtureEMLX(t, store, 102, "imap://"+testAccountID+"/INBOX", []byte(headers+"\r\nbody\r\n"))
			client := &Client{store: store}
			source, err := client.MessageThreadSource(context.Background(), ref)
			input := mail.DraftInput{From: "sender@example.com", Body: "Reply"}
			if test.explicit {
				input.ToSet = !test.implicit
				if !test.emptyTo {
					input.To = []mail.Recipient{{Address: "chosen@example.com"}}
				}
			}
			input.CCSet = test.overrideCC
			if test.overrideCC {
				for _, address := range test.wantCC {
					input.CC = append(input.CC, mail.Recipient{Address: address})
				}
			}
			kind := mail.DraftKindReply
			if test.forward {
				kind = mail.DraftKindForward
			}
			var parent, chain string
			if err == nil {
				input, parent, chain, err = mail.DeriveReplyInput(source, kind, test.all, input, []string{"me@example.com"})
			}
			if test.invalid {
				if nestedErrorCode(err) != "invalid_message_source" {
					t.Fatalf("required malformed source accepted: input=%+v error=%v", input, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			service := mail.NewServiceWithDraftRoot(client, t.TempDir())
			draft, err := service.CreateDraft(mail.CreateDraftRequest{Kind: kind, SourceRef: ref, ReplyAll: test.all, Input: input, SourceMessageID: parent, SourceReferences: chain})
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
			for _, field := range []struct {
				name string
				want []string
			}{{"To", test.wantTo}, {"Cc", test.wantCC}} {
				var got []string
				if message.Header.Get(field.name) != "" {
					addresses, err := message.Header.AddressList(field.name)
					if err != nil {
						t.Fatal(err)
					}
					for _, address := range addresses {
						got = append(got, address.Address)
					}
				}
				if !reflect.DeepEqual(got, field.want) {
					t.Fatalf("composed %s=%v; want %v", field.name, got, field.want)
				}
			}
		})
	}
}
