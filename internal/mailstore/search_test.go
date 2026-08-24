package mailstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

const testAccountID = "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE"

func TestStoreListAndSearchUsesLabelsAndStatelessEMLX(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	defer store.Close()

	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if len(page.Messages) != 3 || page.Messages[0].Subject != "Quarterly Report" {
		t.Fatalf("ListMessages() = %#v", page)
	}

	metadataQuery, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Subject: "Quarterly", Limit: 10,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	metadata, err := store.SearchMessages(context.Background(), metadataQuery)
	if err != nil {
		t.Fatalf("SearchMessages(metadata) error = %v", err)
	}
	if len(metadata.Messages) != 1 || metadata.Coverage.Backend != "envelope_sql" ||
		metadata.Coverage.CandidateMessages != 1 || !metadata.Coverage.Complete {
		t.Fatalf("metadata search = %#v", metadata)
	}

	bodyQuery, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 1,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	bodyPage, err := store.SearchMessages(context.Background(), bodyQuery)
	if err != nil {
		t.Fatalf("SearchMessages(body) error = %v", err)
	}
	if len(bodyPage.Messages) != 1 || bodyPage.Messages[0].Summary.Subject != "Quarterly Report" ||
		bodyPage.NextCursor == "" || bodyPage.Coverage.Complete {
		t.Fatalf("body search page = %#v", bodyPage)
	}

	nextQuery, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 1, Cursor: bodyPage.NextCursor,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(next) error = %v", err)
	}
	nextPage, err := store.SearchMessages(context.Background(), nextQuery)
	if err != nil {
		t.Fatalf("SearchMessages(next) error = %v", err)
	}
	if len(nextPage.Messages) != 1 || nextPage.Messages[0].Summary.Subject != "Status Update" {
		t.Fatalf("next body search page = %#v", nextPage)
	}
}

func TestBodySearchReportsByteLimit(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	defer store.Close()
	query, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 10, MaxBytes: 1,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	page, err := store.SearchMessages(context.Background(), query)
	if err != nil {
		t.Fatalf("SearchMessages() error = %v", err)
	}
	if page.Coverage.Complete || page.Coverage.ScannedBytes != 0 {
		t.Fatalf("limited coverage = %#v", page.Coverage)
	}
}

func TestBodySearchStopsAtFirstCandidateOutsideByteBudget(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	defer store.Close()
	query, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 10, MaxBytes: 256,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	page, err := store.SearchMessages(context.Background(), query)
	if err != nil {
		t.Fatalf("SearchMessages() error = %v", err)
	}
	if len(page.Messages) != 0 || page.Coverage.Complete || page.Coverage.ScannedBytes != 0 {
		t.Fatalf("byte-limited ordered search = %#v", page)
	}
}

func TestAttachmentFilterUsesMIMEWhenEnvelopeCatalogLags(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	defer store.Close()
	attachmentBytes := []byte("catalog-lag attachment")
	writeFixtureEMLX(
		t,
		store,
		101,
		"imap://"+testAccountID+"/%5BGmail%5D/All",
		sentFixtureSource(101, sentMessageFixture{
			Body: "needle alpha", RecipientTypes: []int{0},
			Attachments: []fixtureAttachment{{Name: "report.bin", Content: attachmentBytes}},
		}),
	)
	updateFixtureMessage(t, store, `DELETE FROM attachments WHERE message = 101`)

	hasAttachment := true
	withQuery, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, HasAttachment: &hasAttachment, Limit: 10,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(with attachment) error = %v", err)
	}
	withPage, err := store.SearchMessages(context.Background(), withQuery)
	if err != nil || len(withPage.Messages) != 1 ||
		withPage.Messages[0].Summary.Subject != "Quarterly Report" ||
		withPage.Messages[0].Summary.AttachmentCount != 1 ||
		withPage.Coverage.Backend != "emlx_stream" || !withPage.Coverage.Complete {
		t.Fatalf("attachment=true page = %#v, error = %v", withPage, err)
	}

	hasAttachment = false
	withoutQuery, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, HasAttachment: &hasAttachment, Limit: 10,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(without attachment) error = %v", err)
	}
	withoutPage, err := store.SearchMessages(context.Background(), withoutQuery)
	if err != nil || len(withoutPage.Messages) != 2 ||
		withoutPage.Coverage.Backend != "emlx_stream" || !withoutPage.Coverage.Complete {
		t.Fatalf("attachment=false page = %#v, error = %v", withoutPage, err)
	}
}

func TestAttachmentFilterUsesPositiveCatalogWhenSourceIsMissing(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	defer store.Close()
	location, err := parseMailboxURL("imap://" + testAccountID + "/%5BGmail%5D/All")
	if err != nil {
		t.Fatalf("parseMailboxURL() error = %v", err)
	}
	base, err := store.messageBasePath(location, 101)
	if err != nil {
		t.Fatalf("messageBasePath() error = %v", err)
	}
	if err := os.Remove(base + ".emlx"); err != nil {
		t.Fatalf("remove fixture source: %v", err)
	}

	hasAttachment := true
	query, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, HasAttachment: &hasAttachment, Limit: 10,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	page, err := store.SearchMessages(context.Background(), query)
	if err != nil || len(page.Messages) != 1 ||
		page.Messages[0].Summary.Subject != "Quarterly Report" ||
		page.Messages[0].Summary.AttachmentCount != 1 ||
		page.Coverage.MissingSources != 1 || page.Coverage.Complete {
		t.Fatalf("attachment=true missing-source page = %#v, error = %v", page, err)
	}
}

func TestSearchCursorIsBoundToMailStore(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	defer store.Close()
	first, err := mail.PrepareQuery(mail.Query{MailboxRef: inboxRef, Text: "needle", Limit: 1})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	page, err := store.SearchMessages(context.Background(), first)
	if err != nil || page.NextCursor == "" {
		t.Fatalf("SearchMessages() = %+v, error = %v", page, err)
	}
	cursor, err := mail.DecodeSearchCursor(page.NextCursor, first.Fingerprint)
	if err != nil {
		t.Fatalf("DecodeSearchCursor() error = %v", err)
	}
	wrongStore, err := mail.EncodeSearchCursor(
		first.Fingerprint, "different-store", cursor.ReceivedAt, cursor.RowID,
	)
	if err != nil {
		t.Fatalf("EncodeSearchCursor() error = %v", err)
	}
	next, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Text: "needle", Limit: 1, Cursor: wrongStore,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(next) error = %v", err)
	}
	if _, err := store.SearchMessages(context.Background(), next); errorCodeForTest(err) != "invalid_cursor" {
		t.Fatalf("SearchMessages(wrong store) error = %v", err)
	}
}

func TestSnippetForDoesNotSplitUTF8(t *testing.T) {
	t.Parallel()
	value := strings.Repeat("ä", maximumSnippetRunes+20)
	got := snippetFor(value, "")
	if !utf8.ValidString(got) {
		t.Fatalf("snippetFor() returned invalid UTF-8: %q", got)
	}
	if want := strings.Repeat("ä", maximumSnippetRunes) + "…"; got != want {
		t.Fatalf("snippetFor() = %q, want %q", got, want)
	}
}

func newSearchFixture(t *testing.T) (*Store, string) {
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
		`INSERT INTO mailboxes(ROWID,url,total_count,unread_count,deleted_count,source) VALUES (1,'imap://` + testAccountID + `/INBOX',3,0,0,1)`,
		`INSERT INTO mailboxes(ROWID,url,total_count,unread_count,deleted_count,source) VALUES (2,'imap://` + testAccountID + `/%5BGmail%5D/All',1,0,0,1)`,
		`INSERT INTO addresses(ROWID,address,comment) VALUES (1,'alice@example.com','Alice')`,
		`INSERT INTO addresses(ROWID,address,comment) VALUES (2,'christopher@example.com','Christopher')`,
		`INSERT INTO subjects(ROWID,subject) VALUES (1,'Quarterly Report'),(2,'Status Update'),(3,'Noise')`,
		`INSERT INTO summaries(ROWID,summary) VALUES (1,'report summary'),(2,'status summary'),(3,'noise summary')`,
		`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (101,1001,2001,1,1,1,300,300,2,0,0,1,0,100,1,0,300,0)`,
		`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (102,1002,2002,1,2,2,200,200,1,0,1,0,0,100,2,0,200,0)`,
		`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (103,1003,2003,1,3,3,100,100,1,0,1,0,0,100,3,0,100,0)`,
		`INSERT INTO labels(message_id,mailbox_id) VALUES (101,1)`,
		`INSERT INTO recipients(message,address,type,position) VALUES (101,2,0,0),(102,2,0,0),(103,2,0,0)`,
		`INSERT INTO attachments(message,attachment_id,name) VALUES (101,'2','invoice.pdf')`,
	}
	for _, statement := range statements {
		if _, err := writer.Exec(statement); err != nil {
			writer.Close()
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
	locations := map[int64]string{101: "imap://" + testAccountID + "/%5BGmail%5D/All", 102: "imap://" + testAccountID + "/INBOX", 103: "imap://" + testAccountID + "/INBOX"}
	bodies := map[int64]string{
		101: "needle alpha " + strings.Repeat("x", 512),
		102: "needle beta",
		103: "ordinary content",
	}
	for rowID, mailboxURL := range locations {
		location, err := parseMailboxURL(mailboxURL)
		if err != nil {
			store.Close()
			t.Fatalf("parseMailboxURL() error = %v", err)
		}
		base, err := store.messageBasePath(location, rowID)
		if err != nil {
			store.Close()
			t.Fatalf("messageBasePath() error = %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
			store.Close()
			t.Fatalf("MkdirAll(message) error = %v", err)
		}
		source := []byte(fmt.Sprintf(
			"From: Alice <alice@example.com>\r\nTo: Christopher <christopher@example.com>\r\nSubject: Test\r\nMessage-ID: <%d@example.com>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n",
			rowID, bodies[rowID],
		))
		framed := append([]byte(fmt.Sprintf("%-10d\n", len(source))), source...)
		framed = append(framed, validPlistTrailer()...)
		if err := os.WriteFile(base+".emlx", framed, 0o600); err != nil {
			store.Close()
			t.Fatalf("WriteFile(message) error = %v", err)
		}
	}
	inboxRef, err := mailref.EncodeMailbox(testAccountID, []string{"INBOX"})
	if err != nil {
		store.Close()
		t.Fatalf("EncodeMailbox() error = %v", err)
	}
	return store, inboxRef
}
