package mailapp

import (
	"encoding/base64"
	"reflect"
	"testing"
)

func TestReferenceRoundTrips(t *testing.T) {
	mailboxPath := []string{"Parent", "Inbox"}
	accountToken, err := encodeAccountReference("account-id")
	if err != nil {
		t.Fatalf("encodeAccountReference() error = %v", err)
	}
	account, err := decodeAccountReference(accountToken)
	if err != nil || account.AccountID != "account-id" {
		t.Fatalf("account = %+v, error = %v", account, err)
	}

	mailboxToken, err := encodeMailboxReference("account-id", mailboxPath)
	if err != nil {
		t.Fatalf("encodeMailboxReference() error = %v", err)
	}
	mailbox, err := decodeMailboxReference(mailboxToken)
	if err != nil || mailbox.AccountID != "account-id" || !reflect.DeepEqual(mailbox.Path, mailboxPath) {
		t.Fatalf("mailbox = %+v, error = %v", mailbox, err)
	}

	messageToken, err := encodeMessageReference(messageReference{
		AccountID: "account-id", MailboxPath: mailboxPath,
		LibraryID: "42", ExpectedMessageID: "message-id",
	})
	if err != nil {
		t.Fatalf("encodeMessageReference() error = %v", err)
	}
	message, err := decodeMessageReference(messageToken)
	if err != nil || message.LibraryID != "42" || message.ExpectedMessageID != "message-id" {
		t.Fatalf("message = %+v, error = %v", message, err)
	}
}

func TestReferenceRejectsWrongKindsAndUnsupportedFormats(t *testing.T) {
	accountToken, err := encodeAccountReference("account-id")
	if err != nil {
		t.Fatalf("encodeAccountReference() error = %v", err)
	}
	if _, err := decodeMailboxReference(accountToken); err == nil {
		t.Fatal("decodeMailboxReference() error = nil, want wrong-prefix error")
	}

	payload := []byte(`{"version":99,"account_id":"account-id"}`)
	invalidVersionToken := "acct_" + base64.RawURLEncoding.EncodeToString(payload)
	if _, err := decodeAccountReference(invalidVersionToken); err == nil {
		t.Fatal("decodeAccountReference() error = nil, want version error")
	}
}

func TestMessageReferenceRejectsStoreBoundIdentity(t *testing.T) {
	token, err := encodeMessageReference(messageReference{
		AccountID: "account-id", MailboxPath: []string{"Inbox"}, LibraryID: "42",
		ExpectedStoreUUID: "store-uuid", ExpectedStoreMailboxID: 7,
	})
	if err != nil {
		t.Fatalf("encodeMessageReference() error = %v", err)
	}
	if _, err := decodeMessageReference(token); err == nil {
		t.Fatal("decodeMessageReference() error = nil, want store-bound rejection")
	}
}

func TestCursorRoundTrips(t *testing.T) {
	listToken, err := encodeListCursor(listCursor{MailboxRef: "mbx_ref", Offset: 10, PreviousID: "42"})
	if err != nil {
		t.Fatalf("encodeListCursor() error = %v", err)
	}
	list, err := decodeListCursor(listToken)
	if err != nil || list.MailboxRef != "mbx_ref" || list.Offset != 10 || list.PreviousID != "42" {
		t.Fatalf("list cursor = %+v, error = %v", list, err)
	}
}
