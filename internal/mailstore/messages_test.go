package mailstore

import (
	"context"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

func TestStoreReadsNullableMessageIdentity(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	defer store.Close()
	updateFixtureMessage(t, store, `UPDATE messages SET message_id = NULL WHERE ROWID = 102`)

	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Status Update")
	decoded, err := mailref.DecodeMessage(messageRef)
	if err != nil {
		t.Fatalf("DecodeMessage() error = %v", err)
	}
	if decoded.ExpectedStoreMessageID != 0 {
		t.Fatalf("ExpectedStoreMessageID = %d, want 0", decoded.ExpectedStoreMessageID)
	}
	if _, err := store.GetMessage(context.Background(), messageRef); err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	query, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Subject: "Status Update", Limit: 10,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	if _, err := store.SearchMessages(context.Background(), query); err != nil {
		t.Fatalf("SearchMessages() error = %v", err)
	}
}

func TestStoreRejectsRefAfterLogicalMailboxMembershipChanges(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	defer store.Close()
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Quarterly Report")
	updateFixtureMessage(t, store, `DELETE FROM labels WHERE message_id = 101 AND mailbox_id = 1`)

	_, err = store.GetMessage(context.Background(), messageRef)
	if errorCodeForTest(err) != "stale_reference" {
		t.Fatalf("GetMessage() error = %v, want stale_reference", err)
	}
}

func TestMessageAttachmentsDoNotClaimMissingFullSourcePartIsDownloaded(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	defer store.Close()
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	ref := messageRefWithSubject(t, page.Messages, "Quarterly Report")
	resolved, source, err := store.openMessageSource(context.Background(), ref)
	if err != nil {
		t.Fatalf("openMessageSource() error = %v", err)
	}
	defer source.Close()
	attachments, err := store.messageAttachments(context.Background(), resolved, source, map[string]mimePart{})
	if err != nil || len(attachments) != 1 || attachments[0].Downloaded {
		t.Fatalf("messageAttachments() = %+v, error = %v", attachments, err)
	}
}

func TestListCursorIsBoundToMailStore(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	defer store.Close()
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 1,
	})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("ListMessages() = %+v, error = %v", page, err)
	}
	if _, err := decodeListCursor(page.NextCursor, inboxRef, "different-store"); errorCodeForTest(err) != "invalid_cursor" {
		t.Fatalf("decodeListCursor() error = %v, want invalid_cursor", err)
	}
}
