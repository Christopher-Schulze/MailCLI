package mailstore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

type fixtureAttachment struct {
	Name    string
	Content []byte
}

type sentMessageFixture struct {
	Body           string
	RecipientTypes []int
	Attachments    []fixtureAttachment
}

func TestSentObservationUsesSpecialUseMailboxAndNewRowIdentity(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installSentMailboxFixture(t, store)
	baseline, err := store.captureSendBaseline(context.Background())
	if err != nil {
		t.Fatalf("captureSendBaseline() error = %v", err)
	}
	insertSentMessageFixture(t, store, 104)
	draft := observedDraft("Body")
	message, found, err := store.findSentCandidate(context.Background(), baseline, draft)
	if err != nil || !found || message.Subject != draft.Subject || message.Ref == "" {
		t.Fatalf("findSentCandidate() = %+v, found = %t, error = %v", message, found, err)
	}
}

func TestSentObservationUsesCompleteMIMEWhenAttachmentCatalogLags(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installSentMailboxFixture(t, store)
	baseline, err := store.captureSendBaseline(context.Background())
	if err != nil {
		t.Fatalf("captureSendBaseline() error = %v", err)
	}
	attachmentBytes := []byte("exact attachment bytes")
	insertSentMessageFixtureWithDetails(t, store, 104, sentMessageFixture{
		Body: "Reviewed body", RecipientTypes: []int{0},
		Attachments: []fixtureAttachment{{Name: "report.bin", Content: attachmentBytes}},
	})
	updateFixtureMessage(t, store, `DELETE FROM attachments WHERE message = 104`)
	draft := observedDraft("Reviewed body")
	draft.Attachments = []mail.DraftAttachment{
		writeDraftAttachmentFixture(t, "report.bin", attachmentBytes),
	}
	observed, found, err := store.findSentCandidate(context.Background(), baseline, draft)
	if err != nil || !found {
		t.Fatalf("findSentCandidate() = %+v, found = %t, error = %v", observed, found, err)
	}
	message, err := store.GetMessage(context.Background(), observed.Ref)
	if err != nil || len(message.Attachments) != 1 || !message.Attachments[0].Downloaded ||
		message.Attachments[0].Size != int64(len(attachmentBytes)) {
		t.Fatalf("GetMessage() attachments = %+v, error = %v", message.Attachments, err)
	}
	output := filepath.Join(t.TempDir(), "saved.bin")
	if err := store.SaveAttachmentTo(context.Background(), observed.Ref, "2", output); err != nil {
		t.Fatalf("SaveAttachmentTo() error = %v", err)
	}
	saved, err := os.ReadFile(output)
	if err != nil || string(saved) != string(attachmentBytes) {
		t.Fatalf("saved attachment = %q, error = %v", saved, err)
	}
}

func TestBodyMatchesExactNativeMaterialization(t *testing.T) {
	expected := "Hello Christopher,\n\nReviewed body.\n\nRegards\nChristopher\n\n--\nMail signature"
	draft := observedDraft("Reviewed body")
	draft.ExpectedBody = &expected
	actual := "\uFFFC\n> Hello Christopher,\n>\n> Reviewed body.\n>\n> Regards\n> Christopher\n\n--\nMail signature"
	if !bodyMatchesDraft(actual, draft) {
		t.Fatal("bodyMatchesDraft() rejected an exact normalized native body")
	}
	if bodyMatchesDraft(actual+" changed", draft) {
		t.Fatal("bodyMatchesDraft() accepted a changed native suffix")
	}
}

func TestBodyMatchRequiresNativeMaterialization(t *testing.T) {
	draft := observedDraft("Body")
	draft.ExpectedBody = nil
	if bodyMatchesDraft("Body", draft) || bodyMatchesDraft("Body\n\n--\nMail signature", draft) {
		t.Fatal("bodyMatchesDraft() accepted a body without exact native materialization")
	}
}

func TestBodyMatchesExactEmptyNativeMaterialization(t *testing.T) {
	expected := ""
	draft := observedDraft("")
	draft.ExpectedBody = &expected
	if !bodyMatchesDraft("", draft) {
		t.Fatal("bodyMatchesDraft() rejected an exact empty body")
	}
	if bodyMatchesDraft("--\nMail signature", draft) {
		t.Fatal("bodyMatchesDraft() accepted an unmaterialized signature for an empty body")
	}
}

func TestAttachmentFingerprintsAllowOnlyMaterializedMailOwnedExtras(t *testing.T) {
	reviewed := writeDraftAttachmentFixture(t, "reviewed.bin", []byte("reviewed"))
	reviewedDigest, err := hashRegularFile(reviewed.Path)
	if err != nil {
		t.Fatalf("hashRegularFile() error = %v", err)
	}
	actual := map[attachmentFingerprint]int{
		{Name: "reviewed.bin", Size: reviewed.Size, SHA256: hex.EncodeToString(reviewedDigest[:])}: 1,
		{Name: "signature.png", Size: 3, SHA256: strings.Repeat("a", sha256.Size*2)}:               1,
	}
	if !attachmentFingerprintsContain(actual, []mail.DraftAttachment{reviewed}, true) {
		t.Fatal("materialized Mail-owned attachment was not allowed")
	}
	if attachmentFingerprintsContain(actual, []mail.DraftAttachment{reviewed}, false) {
		t.Fatal("unmaterialized extra attachment was allowed")
	}
}

func TestSentObservationFailsClosedWhenCandidatesAreAmbiguous(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installSentMailboxFixture(t, store)
	baseline, err := store.captureSendBaseline(context.Background())
	if err != nil {
		t.Fatalf("captureSendBaseline() error = %v", err)
	}
	insertSentMessageFixture(t, store, 104)
	insertSentMessageFixture(t, store, 105)
	_, found, err := store.findSentCandidate(context.Background(), baseline, mail.Draft{
		From:    "alice@example.com",
		To:      []mail.Recipient{{Address: "christopher@example.com"}},
		Subject: "Observed send", Body: "Body",
	})
	if err != nil || found {
		t.Fatalf("findSentCandidate() found = %t, error = %v", found, err)
	}
}

func TestSentObservationRejectsDuplicateRecipientAcrossRoles(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installSentMailboxFixture(t, store)
	baseline, err := store.captureSendBaseline(context.Background())
	if err != nil {
		t.Fatalf("captureSendBaseline() error = %v", err)
	}
	insertSentMessageFixtureWithDetails(t, store, 104, sentMessageFixture{
		Body: "Body", RecipientTypes: []int{0, 1, 2},
	})
	draft := observedDraft("Body")
	draft.CC = []mail.Recipient{{Address: "christopher@example.com"}}
	draft.BCC = []mail.Recipient{{Address: "christopher@example.com"}}
	_, found, err := store.findSentCandidate(context.Background(), baseline, draft)
	if err != nil || found {
		t.Fatalf("duplicate recipient found = %t, error = %v", found, err)
	}
}

func TestSentObservationRejectsIncompleteFingerprint(t *testing.T) {
	tests := []struct {
		name       string
		fixture    sentMessageFixture
		draftFrom  string
		draftBody  string
		draftName  string
		draftBytes []byte
	}{
		{
			name: "from differs", fixture: sentMessageFixture{Body: "Reviewed body"},
			draftFrom: "other@example.com", draftBody: "Reviewed body",
		},
		{
			name: "body differs", fixture: sentMessageFixture{Body: "Different body"},
			draftBody: "Reviewed body",
		},
		{
			name: "attachment name differs",
			fixture: sentMessageFixture{
				Body: "Reviewed body", Attachments: []fixtureAttachment{{Name: "other.bin", Content: []byte("expected")}},
			},
			draftBody: "Reviewed body", draftName: "report.bin", draftBytes: []byte("expected"),
		},
		{
			name: "attachment size differs",
			fixture: sentMessageFixture{
				Body: "Reviewed body", Attachments: []fixtureAttachment{{Name: "report.bin", Content: []byte("short")}},
			},
			draftBody: "Reviewed body", draftName: "report.bin", draftBytes: []byte("expected"),
		},
		{
			name: "attachment hash differs",
			fixture: sentMessageFixture{
				Body: "Reviewed body", Attachments: []fixtureAttachment{{Name: "report.bin", Content: []byte("rejected")}},
			},
			draftBody: "Reviewed body", draftName: "report.bin", draftBytes: []byte("expected"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, _ := newSearchFixture(t)
			closeTestResource(t, store, "test store")
			installSentMailboxFixture(t, store)
			baseline, err := store.captureSendBaseline(context.Background())
			if err != nil {
				t.Fatalf("captureSendBaseline() error = %v", err)
			}
			test.fixture.RecipientTypes = []int{0}
			insertSentMessageFixtureWithDetails(t, store, 104, test.fixture)
			draft := observedDraft(test.draftBody)
			if test.draftFrom != "" {
				draft.From = test.draftFrom
			}
			if test.draftName != "" {
				draft.Attachments = []mail.DraftAttachment{
					writeDraftAttachmentFixture(t, test.draftName, test.draftBytes),
				}
			}
			_, found, err := store.findSentCandidate(context.Background(), baseline, draft)
			if err != nil || found {
				t.Fatalf("findSentCandidate() found = %t, error = %v", found, err)
			}
		})
	}
}

func TestSentObservationSelectsOnlyExactParallelFingerprint(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installSentMailboxFixture(t, store)
	baseline, err := store.captureSendBaseline(context.Background())
	if err != nil {
		t.Fatalf("captureSendBaseline() error = %v", err)
	}
	correctBytes := []byte("expected")
	wrongBytes := []byte("rejected")
	insertSentMessageFixtureWithDetails(t, store, 104, sentMessageFixture{
		Body: "Parallel body from another send", RecipientTypes: []int{0},
		Attachments: []fixtureAttachment{{Name: "report.bin", Content: correctBytes}},
	})
	insertSentMessageFixtureWithDetails(t, store, 105, sentMessageFixture{
		Body: "Reviewed body", RecipientTypes: []int{0},
		Attachments: []fixtureAttachment{{Name: "report.bin", Content: wrongBytes}},
	})
	insertSentMessageFixtureWithDetails(t, store, 106, sentMessageFixture{
		Body: "Reviewed body", RecipientTypes: []int{0},
		Attachments: []fixtureAttachment{{Name: "report.bin", Content: correctBytes}},
	})
	draft := observedDraft("Reviewed body")
	draft.Attachments = []mail.DraftAttachment{
		writeDraftAttachmentFixture(t, "report.bin", correctBytes),
	}
	message, found, err := store.findSentCandidate(context.Background(), baseline, draft)
	if err != nil || !found {
		t.Fatalf("findSentCandidate() = %+v, found = %t, error = %v", message, found, err)
	}
	decoded, err := mailref.DecodeMessage(message.Ref)
	if err != nil || decoded.LibraryID != "106" {
		t.Fatalf("observed message ref = %+v, error = %v", decoded, err)
	}
}

func TestSentObservationRevalidatesMailboxMembership(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installSentMailboxFixture(t, store)
	baseline, err := store.captureSendBaseline(context.Background())
	if err != nil {
		t.Fatalf("captureSendBaseline() error = %v", err)
	}
	draft := observedDraft("Body")
	insertSentMessageFixture(t, store, 104)
	candidate := firstMailboxCandidate(t, store, baseline, draft)
	updateFixtureMessage(t, store, `DELETE FROM labels WHERE message_id = 104 AND mailbox_id = 4`)
	matches, err := store.recordMatchesDraft(
		context.Background(), candidate, draft, baseline.SentMailboxIDs,
	)
	if err != nil || matches {
		t.Fatalf("recordMatchesDraft() = %t, error = %v", matches, err)
	}
}

func observedDraft(body string) mail.Draft {
	expectedBody := body + "\n\n--\nMail signature"
	return mail.Draft{
		From:    "Alice <alice@example.com>",
		To:      []mail.Recipient{{Address: "christopher@example.com"}},
		Subject: "Observed send", Body: body, ExpectedBody: &expectedBody,
	}
}

func firstMailboxCandidate(
	t *testing.T,
	store *Store,
	baseline sendBaseline,
	draft mail.Draft,
) messageRecord {
	t.Helper()
	query, arguments := mailboxCandidateQuery(baseline, draft, true)
	rows, err := store.database.QueryContext(context.Background(), query, arguments...)
	if err != nil {
		t.Fatalf("query mailbox candidate: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close mailbox candidate rows: %v", err)
		}
	}()
	if !rows.Next() {
		t.Fatalf("mailbox candidate missing: %v", rows.Err())
	}
	record, err := scanMessageRecord(rows)
	if err != nil {
		t.Fatalf("scan mailbox candidate: %v", err)
	}
	return record
}

func installSentMailboxFixture(t *testing.T, store *Store) {
	t.Helper()
	databasePath := filepath.Join(store.versionRoot, "MailData", envelopeIndexName)
	writer := openTestWriter(t, databasePath)
	if _, err := writer.Exec(
		`INSERT INTO mailboxes(ROWID,url,total_count,unread_count,deleted_count,source)
		 VALUES (4,'imap://` + testAccountID + `/%5BGmail%5D/Sent',0,0,0,1)`,
	); err != nil {
		closeTestResourceNow(t, writer, "Sent mailbox writer")
		t.Fatalf("insert Sent mailbox: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close Sent mailbox writer: %v", err)
	}
	accountRoot := filepath.Join(store.versionRoot, testAccountID)
	if err := os.MkdirAll(accountRoot, 0o700); err != nil {
		t.Fatalf("create account root: %v", err)
	}
	cache := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>mboxes</key><dict><key>[Gmail]</key><dict>
<key>MailboxPathComponent</key><string>[Gmail]</string>
<key>IMAPMailboxChildren</key><dict><key>Sent</key><dict>
<key>MailboxPathComponent</key><string>Sent</string>
<key>IMAPMailboxAttributes</key><integer>32768</integer>
<key>IMAPMailboxChildren</key><dict/>
</dict></dict></dict></dict></dict></plist>`)
	if err := os.WriteFile(filepath.Join(accountRoot, ".mboxCache.plist"), cache, 0o600); err != nil {
		t.Fatalf("write mailbox cache: %v", err)
	}
}

func insertSentMessageFixture(t *testing.T, store *Store, rowID int64) {
	insertSentMessageFixtureWithDetails(t, store, rowID, sentMessageFixture{
		Body: "Body", RecipientTypes: []int{0},
	})
}

func insertSentMessageFixtureWithDetails(
	t *testing.T,
	store *Store,
	rowID int64,
	fixture sentMessageFixture,
) {
	t.Helper()
	databasePath := filepath.Join(store.versionRoot, "MailData", envelopeIndexName)
	writer := openTestWriter(t, databasePath)
	subjectID := rowID + 1000
	summaryID := rowID + 2000
	statements := []struct {
		query string
		args  []any
	}{
		{query: `INSERT INTO subjects(ROWID,subject) VALUES (?, 'Observed send')`, args: []any{subjectID}},
		{query: `INSERT INTO summaries(ROWID,summary) VALUES (?, ?)`, args: []any{summaryID, fixture.Body}},
		{
			query: `INSERT INTO messages(
				ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,
				mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			args: []any{
				rowID, rowID + 3000, rowID + 4000, 1, subjectID, summaryID,
				time.Now().Unix(), time.Now().Unix(), 2, 0, 1, 0, 0, 100, rowID, 0,
				time.Now().Unix(), 0,
			},
		},
		{query: `INSERT INTO labels(message_id,mailbox_id) VALUES (?,4)`, args: []any{rowID}},
	}
	for _, statement := range statements {
		if _, err := writer.Exec(statement.query, statement.args...); err != nil {
			closeTestResourceNow(t, writer, "sent-message writer")
			t.Fatalf("insert sent-message fixture: %v", err)
		}
	}
	for position, recipientType := range fixture.RecipientTypes {
		if _, err := writer.Exec(
			`INSERT INTO recipients(message,address,type,position) VALUES (?,2,?,?)`,
			rowID, recipientType, position,
		); err != nil {
			closeTestResourceNow(t, writer, "sent-message writer")
			t.Fatalf("insert sent-message recipient: %v", err)
		}
	}
	for index, attachment := range fixture.Attachments {
		if _, err := writer.Exec(
			`INSERT INTO attachments(message,attachment_id,name) VALUES (?,?,?)`,
			rowID, fmt.Sprintf("%d", index+2), attachment.Name,
		); err != nil {
			closeTestResourceNow(t, writer, "sent-message writer")
			t.Fatalf("insert sent-message attachment: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close sent-message writer: %v", err)
	}
	writeFixtureEMLX(
		t, store, rowID, "imap://"+testAccountID+"/%5BGmail%5D/All",
		sentFixtureSource(rowID, fixture),
	)
}

func sentFixtureSource(rowID int64, fixture sentMessageFixture) []byte {
	var source strings.Builder
	source.WriteString("From: Alice <alice@example.com>\r\n")
	source.WriteString("To: Christopher <christopher@example.com>\r\n")
	source.WriteString("Subject: Observed send\r\n")
	if len(fixture.Attachments) == 0 {
		source.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		source.WriteString(fixture.Body)
		source.WriteString("\r\n\r\n--\r\nMail signature\r\n")
		return []byte(source.String())
	}
	boundary := fmt.Sprintf("mailcli-test-%d", rowID)
	fmt.Fprintf(&source, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", boundary)
	fmt.Fprintf(
		&source,
		"--%s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n\r\n--\r\nMail signature\r\n",
		boundary, fixture.Body,
	)
	for _, attachment := range fixture.Attachments {
		fmt.Fprintf(
			&source,
			"--%s\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=%q\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n",
			boundary, attachment.Name, base64.StdEncoding.EncodeToString(attachment.Content),
		)
	}
	fmt.Fprintf(&source, "--%s--\r\n", boundary)
	return []byte(source.String())
}

func writeDraftAttachmentFixture(t *testing.T, name string, content []byte) mail.DraftAttachment {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write draft attachment: %v", err)
	}
	digest := sha256.Sum256(content)
	return mail.DraftAttachment{
		Path: path, Size: int64(len(content)), SHA256: hex.EncodeToString(digest[:]),
	}
}

func installDraftsMailboxFixture(t *testing.T, store *Store) {
	t.Helper()
	databasePath := filepath.Join(store.versionRoot, "MailData", envelopeIndexName)
	writer := openTestWriter(t, databasePath)
	if _, err := writer.Exec(
		`INSERT INTO mailboxes(ROWID,url,total_count,unread_count,deleted_count,source)
		 VALUES (5,'imap://` + testAccountID + `/%5BGmail%5D/Drafts',0,0,0,1)`,
	); err != nil {
		closeTestResourceNow(t, writer, "Drafts mailbox writer")
		t.Fatalf("insert Drafts mailbox: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close Drafts mailbox writer: %v", err)
	}
	accountRoot := filepath.Join(store.versionRoot, testAccountID)
	if err := os.MkdirAll(accountRoot, 0o700); err != nil {
		t.Fatalf("create account root: %v", err)
	}
	cache := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>mboxes</key><dict><key>[Gmail]</key><dict>
<key>MailboxPathComponent</key><string>[Gmail]</string>
<key>IMAPMailboxChildren</key><dict><key>Drafts</key><dict>
<key>MailboxPathComponent</key><string>Drafts</string>
<key>IMAPMailboxAttributes</key><integer>4096</integer>
<key>IMAPMailboxChildren</key><dict/>
</dict></dict></dict></dict></dict></plist>`)
	if err := os.WriteFile(filepath.Join(accountRoot, ".mboxCache.plist"), cache, 0o600); err != nil {
		t.Fatalf("write mailbox cache: %v", err)
	}
}

func insertDraftMessageFixture(t *testing.T, store *Store, rowID int64) {
	t.Helper()
	databasePath := filepath.Join(store.versionRoot, "MailData", envelopeIndexName)
	writer := openTestWriter(t, databasePath)
	subjectID := rowID + 5000
	summaryID := rowID + 6000
	statements := []struct {
		query string
		args  []any
	}{
		{query: `INSERT INTO subjects(ROWID,subject) VALUES (?, 'Observed draft')`, args: []any{subjectID}},
		{query: `INSERT INTO summaries(ROWID,summary) VALUES (?, 'Body')`, args: []any{summaryID}},
		{
			query: `INSERT INTO messages(
				ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,
				mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			args: []any{
				rowID, rowID + 7000, rowID + 8000, 1, subjectID, summaryID,
				0, time.Now().Unix(), 5, 0, 1, 0, 0, 100, rowID, 0, time.Now().Unix(), 0,
			},
		},
		{query: `INSERT INTO recipients(message,address,type,position) VALUES (?,2,0,0)`, args: []any{rowID}},
		{query: `INSERT INTO server_messages(message,mailbox,junk_level,draft,replied,forwarded) VALUES (?,5,0,1,0,0)`, args: []any{rowID}},
	}
	for _, statement := range statements {
		if _, err := writer.Exec(statement.query, statement.args...); err != nil {
			closeTestResourceNow(t, writer, "draft-message writer")
			t.Fatalf("insert draft-message fixture: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close draft-message writer: %v", err)
	}
	writeFixtureEMLX(t, store, rowID, "imap://"+testAccountID+"/%5BGmail%5D/Drafts", []byte(
		"From: Alice <alice@example.com>\r\n"+
			"To: Christopher <christopher@example.com>\r\n"+
			"Subject: Observed draft\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nBody\r\n",
	))
}

func writeFixtureEMLX(
	t *testing.T,
	store *Store,
	rowID int64,
	mailboxURL string,
	source []byte,
) {
	t.Helper()
	location, err := parseMailboxURL(mailboxURL)
	if err != nil {
		t.Fatalf("parseMailboxURL() error = %v", err)
	}
	base, err := store.messageBasePath(location, rowID)
	if err != nil {
		t.Fatalf("messageBasePath() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatalf("create message directory: %v", err)
	}
	framed := append([]byte(fmt.Sprintf("%-10d\n", len(source))), source...)
	framed = append(framed, validPlistTrailer()...)
	if err := os.WriteFile(base+".emlx", framed, 0o600); err != nil {
		t.Fatalf("write message source: %v", err)
	}
}
