package mailstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// newMessagesFixture has two physical local INBOX rows (no label rows, like an
// iCloud or plain IMAP account), holding server UIDs 5001 and 5002.
func newMessagesFixture(t *testing.T, address string, recent map[string]transport.RecentMailbox) (*Client, *recentImapOperator) {
	t.Helper()
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, address)
	updateFixtureMessage(t, store, `UPDATE messages SET remote_id = ROWID - 102 + 5001 WHERE ROWID IN (102, 103)`)
	updateFixtureMessage(t, store, `DELETE FROM labels WHERE mailbox_id = 1`)
	location, err := parseMailboxURL("imap://" + testAccountID + "/INBOX")
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureMailboxInfo(t, store, location, 900)
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

// localIdentityHeader builds the header block a server would send for the
// message a local row stores: same sender address, subject and sent date.
func localIdentityHeader(t *testing.T, store *Store, rowID int64) []byte {
	t.Helper()
	var subject, address string
	var sent int64
	err := store.database.QueryRow(`
		SELECT subject.subject, sender.address, m.date_sent FROM messages m
		JOIN subjects subject ON subject.ROWID = m.subject JOIN addresses sender ON sender.ROWID = m.sender
		WHERE m.ROWID = ?`, rowID).Scan(&subject, &address, &sent)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(fmt.Sprintf("From: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%d@example.com>\r\n\r\n", address, subject, time.Unix(sent, 0).UTC().Format(time.RFC1123Z), rowID))
}

func moveFixtureMessagesToAll(t *testing.T, store *Store, rowIDs ...int64) {
	t.Helper()
	inbox, err := parseMailboxURL("imap://" + testAccountID + "/INBOX")
	if err != nil {
		t.Fatal(err)
	}
	all, err := parseMailboxURL("imap://" + testAccountID + "/%5BGmail%5D/All")
	if err != nil {
		t.Fatal(err)
	}
	for _, rowID := range rowIDs {
		source, err := store.messageBasePath(inbox, rowID)
		if err != nil {
			t.Fatal(err)
		}
		destination, err := store.messageBasePath(all, rowID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(destination + ".emlx"); !os.IsNotExist(err) {
			t.Fatalf("fixture destination exists or cannot be checked: %v", err)
		}
		if err := os.Rename(source+".emlx", destination+".emlx"); err != nil {
			t.Fatal(err)
		}
	}
}

func prefixedSubject(header []byte, prefix string) []byte {
	return []byte(strings.Replace(string(header), "Subject: ", "Subject: "+prefix, 1))
}

// Gmail keeps every message once, in All Mail, and shows a label such as INBOX
// through the labels table: the local rows carry All Mail UIDs, which are not
// comparable with INBOX UIDs, so the comparison uses sender, subject and date.
func TestNewMessagesMatchesLabelBackedMailboxesByHeaderIdentity(t *testing.T) {
	client, operator := newMessagesFixture(t, "new-labels@gmail.com", nil)
	updateFixtureMessage(t, client.store, `UPDATE messages SET mailbox = 2, remote_id = ROWID + 77000 WHERE ROWID IN (102, 103)`)
	moveFixtureMessagesToAll(t, client.store, 102, 103)
	updateFixtureMessage(t, client.store, `INSERT INTO labels(message_id, mailbox_id) VALUES (102, 1), (103, 1)`)
	operator.recent = map[string]transport.RecentMailbox{"INBOX": {UIDValidity: 900, Exists: 3, Messages: []transport.RecentMessage{
		{UID: 41001, Seen: true, Header: localIdentityHeader(t, client.store, 102)},
		// A reply the server shows as "Re: ...": the store keeps the subject without its prefix.
		{UID: 41002, Seen: true, Header: prefixedSubject(localIdentityHeader(t, client.store, 103), "Re: ")},
		{UID: 41003, Header: []byte("From: New Sender <new@example.com>\r\nSubject: Really new\r\nDate: Tue, 29 Sep 2026 10:15:00 +0200\r\nMessage-ID: <new@example.com>\r\n\r\n")},
	}}}
	result, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{Limit: 5})
	if err != nil || len(result.Mailboxes) != 1 {
		t.Fatalf("result = %+v, %v", result, err)
	}
	mailbox := result.Mailboxes[0]
	if mailbox.MatchedBy != mail.NewMatchedByHeaders || mailbox.NewCount != 1 || mailbox.Truncated || len(mailbox.Messages) != 1 || mailbox.Messages[0].Subject != "Really new" {
		t.Fatalf("label-backed mailbox = %+v", mailbox)
	}
	server, err := mailref.DecodeServer(mailbox.Messages[0].ServerRef)
	if err != nil || server.UID != 41003 {
		t.Fatalf("server ref = %+v, %v", server, err)
	}
}

func TestNewMessagesDistinguishesLabelMessagesWithTheSameHeaders(t *testing.T) {
	client, operator := newMessagesFixture(t, "new-collision@gmail.com", nil)
	updateFixtureMessage(t, client.store, `UPDATE messages SET mailbox = 2, remote_id = ROWID + 77000 WHERE ROWID IN (102, 103)`)
	moveFixtureMessagesToAll(t, client.store, 102, 103)
	updateFixtureMessage(t, client.store, `UPDATE messages SET date_sent = 200, subject = 2 WHERE ROWID = 103`)
	updateFixtureMessage(t, client.store, `INSERT INTO labels(message_id, mailbox_id) VALUES (102, 1), (103, 1)`)
	known := localIdentityHeader(t, client.store, 102)
	newHeader := []byte(strings.Replace(string(known), "<102@example.com>", "<different@example.com>", 1))
	operator.recent = map[string]transport.RecentMailbox{"INBOX": {UIDValidity: 900, Exists: 2, Messages: []transport.RecentMessage{
		{UID: 41001, Header: known}, {UID: 41002, Header: newHeader},
	}}}
	result, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{Limit: 5})
	if err != nil || !result.Complete || result.NewCount != 1 || len(result.Mailboxes) != 1 ||
		len(result.Mailboxes[0].Messages) != 1 || result.Mailboxes[0].Messages[0].MessageID != "<different@example.com>" {
		t.Fatalf("colliding header result = %+v, %v", result, err)
	}
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
	if mailbox.State != mail.NewMailboxStateChecked || mailbox.MatchedBy != mail.NewMatchedByUID || mailbox.Name != "INBOX" || mailbox.ServerMessages != 4 ||
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

func TestNewMessagesReportsValidatedWindowScope(t *testing.T) {
	for _, test := range []struct {
		name       string
		exists     int
		scanned    int
		allKnown   bool
		noValidity bool
		limited    bool
		truncated  bool
		newCount   int
		listed     int
	}{
		{name: "all 100 known with older server mail", exists: 900, scanned: 100, allKnown: true, limited: true},
		{name: "window and output limits", exists: 900, scanned: 100, limited: true, truncated: true, newCount: 98, listed: 20},
		{name: "output limit without older server mail", exists: 100, scanned: 100, truncated: true, newCount: 98, listed: 20},
		{name: "unresolved comparison keeps scanned window", exists: 900, scanned: 100, noValidity: true, limited: true},
		{name: "fully scanned small mailbox", exists: 2, scanned: 2},
		{name: "successfully scanned empty mailbox"},
	} {
		t.Run(test.name, func(t *testing.T) {
			messages := make([]transport.RecentMessage, test.scanned)
			for index := range messages {
				messages[index] = recentMessage(uint32(5001+index), false, fmt.Sprintf("window-%d", index))
			}
			client, _ := newMessagesFixture(t, "new-scope@gmail.com", map[string]transport.RecentMailbox{
				"INBOX": {UIDValidity: 900, Exists: test.exists, Messages: messages},
			})
			if test.allKnown {
				updateFixtureMessage(t, client.store, `
					WITH RECURSIVE ids(row_id) AS (VALUES(104) UNION ALL SELECT row_id + 1 FROM ids WHERE row_id < 201)
					INSERT INTO messages(ROWID, remote_id, mailbox, deleted)
					SELECT row_id, row_id - 102 + 5001, 1, 0 FROM ids
				`)
			}
			if test.noValidity {
				location, err := parseMailboxURL("imap://" + testAccountID + "/INBOX")
				if err != nil {
					t.Fatal(err)
				}
				path, err := mailboxInfoPath(client.store.versionRoot, location)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			result, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{Limit: 20})
			if err != nil || len(result.Failures) != 0 || len(result.Mailboxes) != 1 || result.Complete == test.noValidity || result.NewCount != test.newCount {
				t.Fatalf("window result=%+v error=%v", result, err)
			}
			mailbox := result.Mailboxes[0]
			state := mail.NewMailboxStateChecked
			if test.noValidity {
				state = mail.NewMailboxStateUnresolved
			}
			if mailbox.State != state || mailbox.ServerMessages != test.exists || mailbox.ScannedMessages != test.scanned || mailbox.WindowLimited != test.limited ||
				mailbox.NewCount != test.newCount || mailbox.Truncated != test.truncated || len(mailbox.Messages) != test.listed {
				t.Fatalf("window mailbox=%+v; want scanned=%d limited=%t truncated=%t missing=%d listed=%d", mailbox, test.scanned, test.limited, test.truncated, test.newCount, test.listed)
			}
		})
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
	if result.Complete {
		t.Fatalf("changed UIDVALIDITY was reported complete: %+v", result)
	}
}

func TestNewMessagesDoesNotCompareUIDsWithoutLocalValidity(t *testing.T) {
	client, _ := newMessagesFixture(t, "new-unknown-validity@gmail.com", map[string]transport.RecentMailbox{
		"INBOX": {UIDValidity: 900, Exists: 1, Messages: []transport.RecentMessage{recentMessage(5001, false, "known")}},
	})
	location, err := parseMailboxURL("imap://" + testAccountID + "/INBOX")
	if err != nil {
		t.Fatal(err)
	}
	path, err := mailboxInfoPath(client.store.versionRoot, location)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	result, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{Limit: 5})
	if err != nil || result.Complete || len(result.Mailboxes) != 1 ||
		result.Mailboxes[0].State != mail.NewMailboxStateUnresolved || result.Mailboxes[0].MatchedBy != "" {
		t.Fatalf("unknown validity result = %+v, %v", result, err)
	}
}

func TestNewMessagesUsesOnlyVisibleMailboxMembership(t *testing.T) {
	for _, test := range []struct {
		name        string
		sql         string
		validity    uint32
		moveToAll   bool
		wrongID     bool
		serverEmpty bool
		serverNoGen bool
		state       string
		matchedBy   string
		newUIDs     []uint32
	}{
		{name: "deleted physical row is missing", sql: `UPDATE messages SET deleted = 1 WHERE ROWID = 102`, validity: 900, state: mail.NewMailboxStateChecked, matchedBy: mail.NewMatchedByUID, newUIDs: []uint32{5001}},
		{name: "deleted label does not select header matching", sql: `UPDATE messages SET deleted = 1, mailbox = 2 WHERE ROWID = 102; INSERT INTO labels(message_id, mailbox_id) VALUES (102, 1)`, validity: 900, state: mail.NewMailboxStateChecked, matchedBy: mail.NewMatchedByUID, newUIDs: []uint32{5001}},
		{name: "dangling label does not select header matching", sql: `INSERT INTO labels(message_id, mailbox_id) VALUES (999999, 1)`, validity: 900, state: mail.NewMailboxStateChecked, matchedBy: mail.NewMatchedByUID},
		{name: "empty physical membership needs no generation", sql: `UPDATE messages SET deleted = 1 WHERE mailbox = 1`, state: mail.NewMailboxStateChecked, matchedBy: mail.NewMatchedByUID, newUIDs: []uint32{5002, 5001}},
		{name: "empty label membership needs no generation", sql: `UPDATE messages SET deleted = 1, mailbox = 2 WHERE ROWID IN (102, 103); INSERT INTO labels(message_id, mailbox_id) VALUES (102, 1), (103, 1)`, state: mail.NewMailboxStateChecked, matchedBy: mail.NewMatchedByUID, newUIDs: []uint32{5002, 5001}},
		{name: "empty membership ignores stale generation", sql: `UPDATE messages SET deleted = 1 WHERE mailbox = 1`, validity: 901, state: mail.NewMailboxStateChecked, matchedBy: mail.NewMatchedByUID, newUIDs: []uint32{5002, 5001}},
		{name: "empty server and local membership", sql: `UPDATE messages SET deleted = 1 WHERE mailbox = 1`, serverEmpty: true, state: mail.NewMailboxStateChecked, matchedBy: mail.NewMatchedByUID},
		{name: "empty membership still requires server generation", sql: `UPDATE messages SET deleted = 1 WHERE mailbox = 1`, serverNoGen: true, state: mail.NewMailboxStateUnresolved},
		{name: "physical candidate needs generation despite dangling labels", sql: `INSERT INTO labels(message_id, mailbox_id) VALUES (999999, 1)`, state: mail.NewMailboxStateUnresolved},
		{name: "mixed membership needs physical generation", sql: `INSERT INTO labels(message_id, mailbox_id) VALUES (101, 1)`, state: mail.NewMailboxStateUnresolved},
		{name: "mixed membership rejects stale physical generation", sql: `INSERT INTO labels(message_id, mailbox_id) VALUES (101, 1)`, validity: 901, state: mail.NewMailboxStateUIDValidityChange},
		{name: "visible labels need exact ID not physical generation", sql: `UPDATE messages SET mailbox = 2, remote_id = ROWID + 77000 WHERE ROWID IN (102, 103); INSERT INTO labels(message_id, mailbox_id) VALUES (102, 1), (103, 1)`, moveToAll: true, state: mail.NewMailboxStateChecked, matchedBy: mail.NewMatchedByHeaders},
		{name: "label metadata cannot substitute for exact ID", sql: `UPDATE messages SET mailbox = 2, remote_id = ROWID + 77000 WHERE ROWID IN (102, 103); INSERT INTO labels(message_id, mailbox_id) VALUES (102, 1), (103, 1)`, moveToAll: true, wrongID: true, state: mail.NewMailboxStateChecked, matchedBy: mail.NewMatchedByHeaders, newUIDs: []uint32{5001}},
		{name: "mixed membership keeps exact ID verification", sql: `INSERT INTO labels(message_id, mailbox_id) VALUES (101, 1)`, validity: 900, wrongID: true, state: mail.NewMailboxStateChecked, matchedBy: mail.NewMatchedByHeaders, newUIDs: []uint32{5001}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, operator := newMessagesFixture(t, "new-visible@gmail.com", nil)
			first := localIdentityHeader(t, client.store, 102)
			if test.wrongID {
				first = []byte(strings.Replace(string(first), "<102@example.com>", "<other@example.com>", 1))
			}
			recent := transport.RecentMailbox{UIDValidity: 900, Exists: 2, Messages: []transport.RecentMessage{
				{UID: 5001, Header: first}, {UID: 5002, Header: localIdentityHeader(t, client.store, 103)},
			}}
			if test.serverEmpty {
				recent.Exists, recent.Messages = 0, nil
			}
			if test.serverNoGen {
				recent.UIDValidity = 0
			}
			operator.recent = map[string]transport.RecentMailbox{"INBOX": recent}
			updateFixtureMessage(t, client.store, test.sql)
			if test.moveToAll {
				moveFixtureMessagesToAll(t, client.store, 102, 103)
			}
			location, err := parseMailboxURL("imap://" + testAccountID + "/INBOX")
			if err != nil {
				t.Fatal(err)
			}
			if test.validity == 0 {
				path, err := mailboxInfoPath(client.store.versionRoot, location)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else if test.validity != 900 {
				writeFixtureMailboxInfo(t, client.store, location, test.validity)
			}
			result, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{Limit: 5})
			if err != nil || len(result.Failures) != 0 || len(result.Mailboxes) != 1 || result.Complete != (test.state == mail.NewMailboxStateChecked) {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			mailbox := result.Mailboxes[0]
			if mailbox.State != test.state || mailbox.MatchedBy != test.matchedBy || mailbox.NewCount != len(test.newUIDs) || len(mailbox.Messages) != len(test.newUIDs) || mailbox.Truncated {
				t.Fatalf("mailbox=%+v; want state=%s matched_by=%s UIDs=%v", mailbox, test.state, test.matchedBy, test.newUIDs)
			}
			for index, row := range mailbox.Messages {
				server, err := mailref.DecodeServer(row.ServerRef)
				if err != nil || server.UID != test.newUIDs[index] || server.UIDValidity != 900 {
					t.Fatalf("server ref=%+v error=%v; want UID=%d generation=900", server, err, test.newUIDs[index])
				}
			}
		})
	}
}

func TestLocalServerUIDsUsesVisiblePhysicalAndLabelMembers(t *testing.T) {
	client, _ := newMessagesFixture(t, "visible-uids@gmail.com", nil)
	updateFixtureMessage(t, client.store, `UPDATE messages SET deleted = 1 WHERE ROWID = 102; UPDATE messages SET remote_id = 5003 WHERE ROWID = 101; INSERT INTO labels(message_id, mailbox_id) VALUES (101, 1), (102, 1), (999999, 1)`)
	known, err := client.store.localServerUIDs(context.Background(), 1, []uint32{5001, 5002, 5003, 5004})
	if err != nil || len(known) != 2 || known[5001] || !known[5002] || !known[5003] || known[5004] {
		t.Fatalf("visible local UIDs=%v error=%v; want only physical 5002 and label 5003", known, err)
	}
}

func TestNewMessagesKeepsGoingAfterAFailureAndReportsIt(t *testing.T) {
	for _, code := range []string{transport.CodeIMAPTimeout, transport.CodeIMAPResponseMalformed, "mailbox_uidvalidity_changed"} {
		t.Run(code, func(t *testing.T) {
			client, operator := newMessagesFixture(t, "new-failure@gmail.com", nil)
			operator.err = &transport.TransportError{Code: code, Message: "recent FETCH proof failed"}
			result, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{Limit: 5})
			if err != nil {
				t.Fatalf("NewMessages() error = %v", err)
			}
			if result.Complete || len(result.Failures) != 1 || result.Failures[0].Code != code ||
				result.Failures[0].Mailbox != "INBOX" || len(result.Mailboxes) != 0 || result.NewCount != 0 {
				t.Fatalf("failed coverage became a trustworthy empty result: %+v", result)
			}
		})
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
	unknown, err := mailref.EncodeAccount("FFFFFFFF-BBBB-4CCC-8DDD-EEEEEEEEEEEE")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{AccountRef: unknown, Limit: 5}); err == nil ||
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
