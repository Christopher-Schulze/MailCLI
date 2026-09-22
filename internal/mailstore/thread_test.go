package mailstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

const inactiveThreadAccountID = "FFFFFFFF-EEEE-4DDD-8CCC-BBBBBBBBBBBB"

// newThreadFixture builds a store where conversation 42 spans INBOX, Sent,
// and one mailbox of an account that is not active. The ungrouped message
// has a NULL conversation_id like Mail leaves on unthreaded mail.
func newThreadFixture(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	mailRoot := filepath.Join(root, "Mail")
	mailData := filepath.Join(mailRoot, "V10", "MailData")
	if err := os.MkdirAll(mailData, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	databasePath := filepath.Join(mailData, envelopeIndexName)
	writer := openTestWriter(t, databasePath)
	createTestSchema(t, writer, "")
	statements := []string{
		`INSERT INTO mailboxes(ROWID,url,total_count,unread_count,deleted_count,source) VALUES (1,'imap://` + testAccountID + `/INBOX',2,0,0,1)`,
		`INSERT INTO mailboxes(ROWID,url,total_count,unread_count,deleted_count,source) VALUES (2,'imap://` + testAccountID + `/Sent',1,0,0,1)`,
		`INSERT INTO mailboxes(ROWID,url,total_count,unread_count,deleted_count,source) VALUES (3,'imap://` + inactiveThreadAccountID + `/INBOX',1,0,0,1)`,
		`INSERT INTO addresses(ROWID,address,comment) VALUES (1,'alice@example.com','Alice'),(2,'christopher@example.com','Christopher')`,
		`INSERT INTO subjects(ROWID,subject) VALUES (1,'Thread start'),(2,'Thread reply'),(3,'Solo'),(4,'Inactive member'),(5,'Deleted member')`,
		`INSERT INTO summaries(ROWID,summary) VALUES (1,'first'),(2,'reply'),(3,'solo'),(4,'inactive'),(5,'deleted')`,
		`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (11,1101,2101,1,1,1,100,100,1,0,0,0,0,100,42,0,100,0)`,
		`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (12,1102,2102,2,2,2,200,200,2,0,1,0,0,100,42,0,200,0)`,
		`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (13,1103,2103,1,3,3,300,300,1,0,0,0,0,100,NULL,0,300,0)`,
		`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (14,1104,2104,1,5,5,50,50,1,0,0,0,1,100,42,0,50,0)`,
		`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (15,1105,2105,1,4,4,150,150,3,0,0,0,0,100,42,0,150,0)`,
		`INSERT INTO recipients(message,address,type,position) VALUES (11,2,0,0),(12,1,0,0),(13,2,0,0)`,
	}
	for _, statement := range statements {
		if _, err := writer.Exec(statement); err != nil {
			closeTestResourceNow(t, writer, "fixture database")
			t.Fatalf("execute fixture statement: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close fixture database: %v", err)
	}
	store, err := Open(context.Background(), Config{
		MailRoot:          mailRoot,
		ActiveAccountURLs: []string{"imap://" + testAccountID + "/"},
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	inboxRef, err := mailref.EncodeMailbox(testAccountID, []string{"INBOX"})
	if err != nil {
		closeTestResourceNow(t, store, "thread store")
		t.Fatalf("EncodeMailbox() error = %v", err)
	}
	return store, inboxRef
}

func threadSeedRef(t *testing.T, store *Store, mailboxRef string, subject string) string {
	t.Helper()
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: mailboxRef, Limit: mail.MaximumPageLimit,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	for _, message := range page.Messages {
		if message.Subject == subject {
			return message.Ref
		}
	}
	t.Fatalf("no fixture message with subject %q", subject)
	return ""
}

func TestMessageThreadSpansMailboxesChronologically(t *testing.T) {
	store, inboxRef := newThreadFixture(t)
	closeTestResource(t, store, "thread store")
	seedRef := threadSeedRef(t, store, inboxRef, "Thread start")

	thread, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{
		Ref: seedRef, Limit: mail.DefaultPageLimit,
	})
	if err != nil {
		t.Fatalf("MessageThread() error = %v", err)
	}
	if thread.Ref != seedRef || thread.ConversationID != 42 || thread.Truncated {
		t.Fatalf("thread header = %+v", thread)
	}
	// Deleted member 14 and inactive-account member 15 are excluded; the two
	// visible members arrive in chronological order.
	if len(thread.Messages) != 2 {
		t.Fatalf("members = %+v", thread.Messages)
	}
	if thread.Messages[0].Subject != "Thread start" || thread.Messages[1].Subject != "Thread reply" {
		t.Fatalf("member order = %q, %q", thread.Messages[0].Subject, thread.Messages[1].Subject)
	}
	for _, member := range thread.Messages {
		if member.ConversationID != 42 {
			t.Fatalf("member projection = %+v", member)
		}
		decoded, err := mailref.DecodeMessage(member.Ref)
		if err != nil || !decoded.IsStoreBound() {
			t.Fatalf("member ref %q = %v, %v", member.Ref, decoded, err)
		}
	}
	sentRef, err := mailref.EncodeMailbox(testAccountID, []string{"Sent"})
	if err != nil {
		t.Fatalf("EncodeMailbox() error = %v", err)
	}
	if thread.Messages[0].MailboxRef != inboxRef || thread.Messages[1].MailboxRef != sentRef {
		t.Fatalf("member mailbox refs = %q, %q", thread.Messages[0].MailboxRef, thread.Messages[1].MailboxRef)
	}
}

func TestMessageThreadTruncatesAtLimit(t *testing.T) {
	store, inboxRef := newThreadFixture(t)
	closeTestResource(t, store, "thread store")
	seedRef := threadSeedRef(t, store, inboxRef, "Thread start")

	thread, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{
		Ref: seedRef, Limit: 1,
	})
	if err != nil {
		t.Fatalf("MessageThread() error = %v", err)
	}
	if !thread.Truncated || len(thread.Messages) != 1 || thread.Messages[0].Subject != "Thread start" {
		t.Fatalf("truncated thread = %+v", thread)
	}
}

func TestMessageThreadUngroupedSeedReturnsSingleMember(t *testing.T) {
	store, inboxRef := newThreadFixture(t)
	closeTestResource(t, store, "thread store")
	seedRef := threadSeedRef(t, store, inboxRef, "Solo")

	thread, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{
		Ref: seedRef, Limit: mail.DefaultPageLimit,
	})
	if err != nil {
		t.Fatalf("MessageThread() error = %v", err)
	}
	if thread.ConversationID != 0 || len(thread.Messages) != 1 || thread.Messages[0].Ref != seedRef {
		t.Fatalf("ungrouped thread = %+v", thread)
	}
}

func TestMessageThreadValidatesRequest(t *testing.T) {
	store, inboxRef := newThreadFixture(t)
	closeTestResource(t, store, "thread store")
	seedRef := threadSeedRef(t, store, inboxRef, "Thread start")

	for _, limit := range []int{0, mail.MaximumPageLimit + 1} {
		if _, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{
			Ref: seedRef, Limit: limit,
		}); errorCodeForTest(err) != "invalid_argument" {
			t.Fatalf("limit %d error = %v, want invalid_argument", limit, err)
		}
	}
	if _, err := store.MessageThread(context.Background(), mail.MessageThreadRequest{
		Ref: "stale-ref", Limit: mail.DefaultPageLimit,
	}); err == nil {
		t.Fatal("stale ref must fail before reading members")
	}
}

func TestClientMessageThreadFailsClosedWithoutStore(t *testing.T) {
	client := &Client{}
	if _, err := client.MessageThread(context.Background(), mail.MessageThreadRequest{
		Ref: "ref", Limit: mail.DefaultPageLimit,
	}); err == nil {
		t.Fatal("MessageThread without a store must fail closed")
	}
}
