package mailapp

import (
	"fmt"

	"mailcli/internal/mailref"
)

type accountReference = mailref.Account
type mailboxReference = mailref.Mailbox
type messageReference = mailref.Message
type listCursor = mailref.ListCursor

func encodeAccountReference(accountID string) (string, error) {
	return mailref.EncodeAccount(accountID)
}

func decodeAccountReference(value string) (accountReference, error) {
	return mailref.DecodeAccount(value)
}

func encodeMailboxReference(accountID string, path []string) (string, error) {
	return mailref.EncodeMailbox(accountID, path)
}

func decodeMailboxReference(value string) (mailboxReference, error) {
	return mailref.DecodeMailbox(value)
}

func encodeMessageReference(ref messageReference) (string, error) {
	return mailref.EncodeMessage(ref)
}

func decodeMessageReference(value string) (messageReference, error) {
	ref, err := mailref.DecodeMessage(value)
	if err != nil {
		return messageReference{}, err
	}
	if ref.IsStoreBound() {
		return messageReference{}, fmt.Errorf("store message refs must be revalidated before Mail.app automation")
	}
	return ref, nil
}

func encodeListCursor(ref listCursor) (string, error) {
	return mailref.EncodeListCursor(ref)
}

func decodeListCursor(value string) (listCursor, error) {
	return mailref.DecodeListCursor(value)
}
