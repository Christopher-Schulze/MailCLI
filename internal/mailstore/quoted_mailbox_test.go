package mailstore

import (
	"bytes"
	"context"
	stdmail "net/mail"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

func TestQuotedMailboxHeaderRecipientsRemainUsableForReplies(t *testing.T) {
	for _, address := range []string{`"A B"@example.com`, `"Jörg Smith"@example.com`, `"A\"B"@example.com`, `"A@B"@example.com`} {
		t.Run(address, func(t *testing.T) {
			raw := "From: Sender <" + address + ">\r\nReply-To: Reply <" + address + ">\r\nTo: Target <" + address + ">\r\nCc: Copy <copy@example.com>\r\nContent-Type: text/plain\r\n\r\nBody\r\n"
			headers, err := sourceHeadersFromReader(strings.NewReader(raw))
			if err != nil || headers.ReplyToError != nil {
				t.Fatalf("source headers: %+v, %v", headers, err)
			}
			document, err := parseMIMEDocument(strings.NewReader(raw), false, false, false)
			if err != nil || !document.Complete {
				t.Fatalf("MIME document: %+v, %v", document, err)
			}
			if len(headers.To) != 1 || len(headers.ReplyTo) != 1 || headers.To[0].Address != address || headers.ReplyTo[0].Address != address || !reflect.DeepEqual(document.To, headers.To) {
				t.Errorf("header recipients lost mailbox syntax: To=%+v Reply-To=%+v MIME=%+v", headers.To, headers.ReplyTo, document.To)
			}
			input, _, _, err := mail.DeriveReplyInput(mail.ThreadSource{From: headers.From, ReplyTo: headers.ReplyTo, To: headers.To, CC: headers.CC}, mail.DraftKindReply, true, mail.DraftInput{Body: "Reply"}, nil)
			if err != nil || len(input.To) != 1 || len(input.CC) != 1 || input.CC[0].Address != "copy@example.com" {
				t.Fatalf("reply derivation: %+v, %v", input, err)
			}
			payload, err := mail.BuildMessage(mail.Draft{From: "sender@icloud.com", To: input.To, CC: input.CC, Body: input.Body}, "<reply@example.com>")
			if err != nil {
				t.Fatal(err)
			}
			message, err := stdmail.ReadMessage(bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			to, err := message.Header.AddressList("To")
			want, parseErr := stdmail.ParseAddress(address)
			if err != nil || parseErr != nil || len(to) != 1 || to[0].Address != want.Address || to[0].Name != "Reply" {
				t.Fatalf("reply header = %+v, %v", to, err)
			}
		})
	}
}

func TestQuotedMailboxDiscoveredIdentityMatchesBindings(t *testing.T) {
	for _, address := range []string{`"A B"@icloud.com`, `"Jörg Smith"@gmail.com`, `"A\"B"@icloud.com`, `"A@B"@gmail.com`} {
		t.Run(address, func(t *testing.T) {
			store, _ := newSearchFixture(t)
			defer closeTestResource(t, store, "quoted sender store")
			installImapIdentityFixture(t, store, address)
			catalog, err := store.ListAccountCatalog(context.Background())
			if err != nil || !catalog.Complete || len(catalog.Accounts) != 1 {
				t.Fatalf("catalog = %+v, %v", catalog, err)
			}
			account := catalog.Accounts[0]
			if !reflect.DeepEqual(account.EmailAddresses, []string{address}) || !reflect.DeepEqual(account.DiscoveredSenderIdentities, []string{address}) {
				t.Errorf("discovered identity lost syntax: %+v", account)
			}
			for _, discovered := range account.EmailAddresses {
				if _, _, _, _, err := transport.ProviderHosts(discovered); err != nil {
					t.Errorf("discovered provider: %v", err)
				}
			}
			bindings := mail.NewAccountBindingStore(filepath.Join(t.TempDir(), "bindings.json"))
			if err := bindings.UpsertAccountBinding(mail.AccountBinding{AccountID: testAccountID, SenderAliases: []string{address}, CredentialAccount: address}); err != nil {
				t.Fatal(err)
			}
			store.accountBindings = bindings
			catalog, err = store.ListAccountCatalog(context.Background())
			if err != nil || !catalog.Complete || len(catalog.Accounts) != 1 || !reflect.DeepEqual(catalog.Accounts[0].EmailAddresses, []string{address}) {
				t.Fatalf("bound catalog = %+v, %v", catalog, err)
			}
		})
	}
}

func TestReplySourcePreservesMalformedRecipientEvidence(t *testing.T) {
	for _, header := range []string{"To", "Cc"} {
		t.Run(header, func(t *testing.T) {
			store, inboxRef := newSearchFixture(t)
			defer closeTestResource(t, store, "reply source store")
			page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inboxRef, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			ref := messageRefWithSubject(t, page.Messages, "Status Update")
			raw := "From: Alice <alice@example.com>\r\nMessage-ID: <102@example.com>\r\nSubject: Reply\r\n" + header + ": valid@example.com, malformed\r\n\r\nBody\r\n"
			writeFixtureEMLX(t, store, 102, "imap://"+testAccountID+"/INBOX", []byte(raw))
			headers, err := sourceHeadersFromReader(strings.NewReader(raw))
			if err != nil || header == "To" && headers.ToError == nil || header == "Cc" && headers.CCError == nil {
				t.Fatalf("recipient parse evidence = %+v, error = %v", headers, err)
			}
			client := &Client{store: store}
			source, err := client.MessageThreadSource(context.Background(), ref)
			if err != nil || source.RecipientParseError == nil || source.From == "" || source.MessageID == "" {
				t.Fatalf("thread source = %+v, error = %v", source, err)
			}
			document, err := parseMIMEDocument(strings.NewReader(raw), false, false, false)
			if err != nil || document.Complete || !strings.Contains(document.Content, "Body") {
				t.Fatalf("optional recipient failure lost readable detail: %+v, %v", document, err)
			}
			for _, test := range []struct {
				name      string
				all       bool
				input     mail.DraftInput
				wantError bool
			}{
				{"automatic reply-all", true, mail.DraftInput{Body: "Reply"}, true},
				{"ordinary reply", false, mail.DraftInput{Body: "Reply"}, false},
				{"explicit CC", true, mail.DraftInput{Body: "Reply", CCSet: true, CC: []mail.Recipient{{Address: "other@example.com"}}}, false},
				{"explicit empty CC", true, mail.DraftInput{Body: "Reply", CCSet: true, CC: []mail.Recipient{}}, false},
			} {
				t.Run(test.name, func(t *testing.T) {
					input, id, refs, err := mail.DeriveReplyInput(source, mail.DraftKindReply, test.all, test.input, nil)
					if test.wantError {
						if errorCodeForTest(err) != "invalid_message_source" {
							t.Fatalf("automatic reply-all error = %v", err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					service := mail.NewServiceWithDraftRoot(client, t.TempDir())
					draft, err := service.CreateDraft(mail.CreateDraftRequest{Kind: mail.DraftKindReply, SourceRef: ref, ReplyAll: test.all, Input: input, SourceMessageID: id, SourceReferences: refs})
					if err != nil || len(draft.To) != 1 || draft.To[0].Address != "alice@example.com" || len(draft.CC) != len(input.CC) || len(input.CC) > 0 && !reflect.DeepEqual(draft.CC, input.CC) {
						t.Fatalf("supported reply = %+v, error = %v", draft, err)
					}
				})
			}
		})
	}
}
