package mailref

import (
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// MessageIdentity excludes token versions and mutable verification hints.
type MessageIdentity struct {
	AccountID string
	StoreUUID string
	MailboxID int64
	MessageID int64
	LibraryID string
}

func MessageIdentityKey(token string) (MessageIdentity, error) {
	message, err := DecodeMessage(token)
	if err != nil {
		return MessageIdentity{}, err
	}
	key := MessageIdentity{
		AccountID: message.AccountID, StoreUUID: message.ExpectedStoreUUID,
		MailboxID: message.ExpectedStoreMailboxID, MessageID: message.ExpectedStoreMessageID,
	}
	if key.MessageID == 0 {
		key.LibraryID = message.LibraryID
	}
	return key, nil
}

type MailboxIdentity struct {
	AccountID string
	Path      string
}

func MailboxIdentityKey(token string) (MailboxIdentity, error) {
	mailbox, err := DecodeMailbox(token)
	if err != nil {
		return MailboxIdentity{}, err
	}
	var path strings.Builder
	for _, component := range mailbox.Path {
		component = norm.NFC.String(component)
		path.WriteString(strconv.Itoa(len(component)))
		path.WriteByte(':')
		path.WriteString(component)
	}
	return MailboxIdentity{AccountID: mailbox.AccountID, Path: path.String()}, nil
}
