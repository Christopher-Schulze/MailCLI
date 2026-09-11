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
			input, _, _, err := mail.DeriveReplyInput(mail.ThreadSource{From: headers.From, ReplyTo: headers.ReplyTo, To: headers.To, CC: headers.CC}, mail.DraftKindReply, true, mail.DraftInput{Body: "Reply"})
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
