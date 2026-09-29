package mailstore

import (
	"context"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

// recentImapOperator serves scripted ListRecentMessages answers per mailbox.
type recentImapOperator struct {
	*stubImapOperator
	recent map[string]transport.RecentMailbox
	err    error
	calls  []string
}

func (o *recentImapOperator) ListRecentMessages(
	_ context.Context, _ transport.ImapConfig, mailbox string, count int,
) (transport.RecentMailbox, error) {
	o.calls = append(o.calls, mailbox)
	if o.err != nil {
		return transport.RecentMailbox{}, o.err
	}
	recent := o.recent[mailbox]
	if len(recent.Messages) > count {
		recent.Messages = recent.Messages[len(recent.Messages)-count:]
	}
	return recent, nil
}

func recentMessage(uid uint32, seen bool, subject string) transport.RecentMessage {
	return transport.RecentMessage{
		UID: uid, Seen: seen,
		Header: []byte("From: Sender <s@example.com>\r\nSubject: " + subject + "\r\nMessage-ID: <" + subject + "@example.com>\r\n\r\n"),
	}
}

// newMessagesFixture has two local INBOX rows, holding server UIDs 5001 and 5002.
func newMessagesFixture(t *testing.T, address string, recent map[string]transport.RecentMailbox) (*Client, *recentImapOperator) {
	t.Helper()
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, address)
	updateFixtureMessage(t, store, `UPDATE messages SET remote_id = ROWID - 102 + 5001 WHERE ROWID IN (102, 103)`)
	operator := &recentImapOperator{
		stubImapOperator: &stubImapOperator{boxes: []transport.MailboxInfo{{Name: "INBOX"}, {Name: "All"}, {Name: "Sent", Flags: []string{"\\Sent"}}}},
		recent:           recent,
	}
	client := &Client{
		store: store,
		send:  mail.SendTransport{Imap: operator, Credentials: stubCredentials{address: "secret"}},
	}
	return client, operator
}

func TestNewMessagesReturnsOnlyServerMessagesTheStoreLacks(t *testing.T) {
	client, operator := newMessagesFixture(t, "new-messages@gmail.com", map[string]transport.RecentMailbox{
		"INBOX": {UIDValidity: 900, Exists: 4, Messages: []transport.RecentMessage{
			recentMessage(5001, true, "known-one"), recentMessage(5002, true, "known-two"),
			recentMessage(5003, false, "fresh-three"), recentMessage(5004, true, "fresh-four"),
		}},
	})
	result, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{Limit: 5})
	if err != nil {
		t.Fatalf("NewMessages() error = %v", err)
	}
	if !result.Complete || result.NewCount != 2 || len(result.Mailboxes) != 1 || len(result.Failures) != 0 {
		t.Fatalf("result = %+v", result)
	}
	mailbox := result.Mailboxes[0]
	if mailbox.State != mail.NewMailboxStateChecked || mailbox.Name != "INBOX" || mailbox.ServerMessages != 4 ||
		mailbox.NewCount != 2 || mailbox.Truncated || len(mailbox.Messages) != 2 {
		t.Fatalf("mailbox = %+v", mailbox)
	}
	newest, older := mailbox.Messages[0], mailbox.Messages[1]
	if newest.Subject != "fresh-four" || newest.Unseen || older.Subject != "fresh-three" || !older.Unseen ||
		newest.Sender != "Sender <s@example.com>" || newest.MessageID != "<fresh-four@example.com>" {
		t.Fatalf("rows are not newest first with decoded headers: %+v", mailbox.Messages)
	}
	server, err := mailref.DecodeServer(newest.ServerRef)
	if err != nil || server.UID != 5004 || server.UIDValidity != 900 || len(server.MailboxPath) != 1 || server.MailboxPath[0] != "INBOX" {
		t.Fatalf("server ref = %+v, %v", server, err)
	}
	if len(operator.calls) != 1 || operator.calls[0] != "INBOX" {
		t.Fatalf("IMAP mailboxes listed = %v, want only INBOX", operator.calls)
	}
}

func TestNewMessagesLimitTruncatesAndCountsAll(t *testing.T) {
	client, _ := newMessagesFixture(t, "new-limit@gmail.com", map[string]transport.RecentMailbox{
		"INBOX": {UIDValidity: 900, Exists: 4, Messages: []transport.RecentMessage{
			recentMessage(5001, true, "a"), recentMessage(5002, true, "b"), recentMessage(5003, false, "c"), recentMessage(5004, false, "d"),
		}},
	})
	result, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{Limit: 1})
	if err != nil || len(result.Mailboxes) != 1 {
		t.Fatalf("result = %+v, %v", result, err)
	}
	mailbox := result.Mailboxes[0]
	if mailbox.NewCount != 2 || !mailbox.Truncated || len(mailbox.Messages) != 1 || mailbox.Messages[0].Subject != "d" {
		t.Fatalf("mailbox = %+v", mailbox)
	}
}

func TestNewMessagesMarksAnExhaustedWindowAsTruncated(t *testing.T) {
	client, _ := newMessagesFixture(t, "new-window@gmail.com", map[string]transport.RecentMailbox{
		"INBOX": {UIDValidity: 900, Exists: 900, Messages: []transport.RecentMessage{
			recentMessage(6001, false, "x"), recentMessage(6002, false, "y"),
		}},
	})
	result, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{Limit: 20})
	if err != nil || len(result.Mailboxes) != 1 {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if mailbox := result.Mailboxes[0]; mailbox.NewCount != 2 || !mailbox.Truncated {
		t.Fatalf("every window message is missing locally while more exist, want truncated: %+v", mailbox)
	}
}

func TestNewMessagesReportsAChangedUIDValidityWithoutRows(t *testing.T) {
	client, _ := newMessagesFixture(t, "new-validity@gmail.com", map[string]transport.RecentMailbox{
		"INBOX": {UIDValidity: 901, Exists: 3, Messages: []transport.RecentMessage{recentMessage(5003, false, "fresh")}},
	})
	location, err := parseMailboxURL("imap://" + testAccountID + "/INBOX")
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureMailboxInfo(t, client.store, location, 900)
	result, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{Limit: 5})
	if err != nil || len(result.Mailboxes) != 1 {
		t.Fatalf("result = %+v, %v", result, err)
	}
	mailbox := result.Mailboxes[0]
	if mailbox.State != mail.NewMailboxStateUIDValidityChange || len(mailbox.Messages) != 0 || mailbox.NewCount != 0 || mailbox.Reason == "" {
		t.Fatalf("mailbox = %+v", mailbox)
	}
}

func TestNewMessagesKeepsGoingAfterAFailureAndReportsIt(t *testing.T) {
	client, operator := newMessagesFixture(t, "new-failure@gmail.com", nil)
	operator.err = &transport.TransportError{Code: transport.CodeIMAPTimeout, Message: "IMAP FETCH deadline"}
	result, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{Limit: 5})
	if err != nil {
		t.Fatalf("NewMessages() error = %v", err)
	}
	if result.Complete || len(result.Failures) != 1 || result.Failures[0].Code != transport.CodeIMAPTimeout ||
		result.Failures[0].Mailbox != "INBOX" || len(result.Mailboxes) != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestNewMessagesWithoutCredentialsReportsThemAndStaysSuccessful(t *testing.T) {
	client, operator := newMessagesFixture(t, "new-credentials@gmail.com", nil)
	client.send.Credentials = strictCredentials{}
	result, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{Limit: 5})
	if err != nil || result.Complete || len(result.Failures) != 1 || result.Failures[0].Code != "imap_credentials_missing" {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if len(operator.calls) != 0 {
		t.Fatalf("IMAP was contacted without credentials: %v", operator.calls)
	}
}

func TestNewMessagesRejectsAnUnknownAccountAndAnOperatorWithoutTheLister(t *testing.T) {
	client, _ := newMessagesFixture(t, "new-account@gmail.com", nil)
	if _, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{AccountRef: "acct_missing", Limit: 5}); err == nil ||
		!strings.Contains(err.Error(), "account ref not found") {
		t.Fatalf("unknown account error = %v", err)
	}
	client.send.Imap = &stubImapOperator{}
	if _, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{Limit: 5}); err == nil {
		t.Fatal("an operator without ListRecentMessages was accepted")
	}
	unavailable := (&Client{}).safeWriteUnavailableError()
	if _, err := (&Client{}).NewMessages(context.Background(), mail.NewMessagesRequest{Limit: 5}); err == nil || err.Error() != unavailable.Error() {
		t.Fatalf("no store error = %v, want %v", err, unavailable)
	}
}
