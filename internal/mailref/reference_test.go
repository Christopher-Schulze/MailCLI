package mailref

import "testing"

func TestReferenceRoundTrips(t *testing.T) {
	t.Parallel()

	accountToken, err := EncodeAccount("account-id")
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	account, err := DecodeAccount(accountToken)
	if err != nil || account.AccountID != "account-id" {
		t.Fatalf("DecodeAccount() = %#v, %v", account, err)
	}

	mailboxToken, err := EncodeMailbox("account-id", []string{"Nested", "Inbox"})
	if err != nil {
		t.Fatalf("EncodeMailbox() error = %v", err)
	}
	mailbox, err := DecodeMailbox(mailboxToken)
	if err != nil || len(mailbox.Path) != 2 || mailbox.Path[1] != "Inbox" {
		t.Fatalf("DecodeMailbox() = %#v, %v", mailbox, err)
	}
}

func TestMessageReferenceKindsUseInitialFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input Message
	}{
		{
			name: "bridge identity",
			input: Message{AccountID: "account-id", MailboxPath: []string{"Inbox"},
				LibraryID: "42", ExpectedMessageID: "rfc-id"},
		},
		{
			name: "store identity",
			input: Message{AccountID: "account-id", MailboxPath: []string{"Inbox"},
				LibraryID: "42", ExpectedStoreUUID: "store-uuid", ExpectedStoreMailboxID: 5,
				ExpectedStoreMessageID: 7, ExpectedStoreGlobalID: 9},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			token, err := EncodeMessage(test.input)
			if err != nil {
				t.Fatalf("EncodeMessage() error = %v", err)
			}
			got, err := DecodeMessage(token)
			if err != nil {
				t.Fatalf("DecodeMessage() error = %v", err)
			}
			if got.Version != FormatVersion || got.LibraryID != "42" {
				t.Fatalf("DecodeMessage() = %#v", got)
			}
		})
	}
}

func TestDecodeMessageRejectsUnsupportedFormat(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"version":99,"account_id":"a","mailbox_path":["Inbox"],"library_id":"1"}`)
	if _, err := DecodeMessage(encodeToken("msg_", payload)); err == nil {
		t.Fatal("DecodeMessage() error = nil")
	}
}

func TestDecodeMessageRejectsIncompleteStoreIdentity(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"version":1,"account_id":"a","mailbox_path":["Inbox"],"library_id":"1","expected_store_mailbox_id":2}`)
	if _, err := DecodeMessage(encodeToken("msg_", payload)); err == nil {
		t.Fatal("DecodeMessage() error = nil")
	}
}
