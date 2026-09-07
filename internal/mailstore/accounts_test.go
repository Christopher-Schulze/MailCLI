package mailstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mattn/go-sqlite3"
	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

const secondaryCatalogAccountID = "BBBBBBBB-CCCC-4DDD-8EEE-FFFFFFFFFFFF"

func TestListAccountCatalogDegradesAccountLocalSenderSQLFailure(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installSentMailboxFixture(t, store)
	insertMalformedSentIdentityRow(t, store)
	secondary := addLocalCatalogAccount(t, store)

	records, err := store.mailboxRecords(context.Background())
	if err != nil {
		t.Fatalf("mailboxRecords() error = %v", err)
	}
	recordsByPath := make(map[string]mailboxRecord, len(records))
	for _, record := range records {
		recordsByPath[record.pathKey] = record
	}
	_, err = store.loadAccount(context.Background(), store.activeAccounts[0], recordsByPath)
	var issue *accountCatalogIssue
	if !errors.As(err, &issue) {
		t.Fatalf("loadAccount() error = %v, want accountCatalogIssue", err)
	}
	if issue.accountID != testAccountID || issue.account.DegradedReason != accountDegradedSenderIdentity {
		t.Fatalf("account issue = %+v, want sender identity issue for %s", issue, testAccountID)
	}
	if issue.account.DegradedRemediation == "" {
		t.Fatal("account issue remediation is empty")
	}
	if issue.account.IdentityCoverage.Source != "sent_history" ||
		issue.account.IdentityCoverage.State != "unavailable" {
		t.Fatalf("account issue coverage = %+v, want unavailable sent history", issue.account.IdentityCoverage)
	}

	catalog, err := store.ListAccountCatalog(context.Background())
	if err != nil {
		t.Fatalf("ListAccountCatalog() error = %v", err)
	}
	if catalog.Complete || len(catalog.Accounts) != 2 {
		t.Fatalf("catalog = %+v, want two accounts with complete=false", catalog)
	}
	if catalog.Accounts[0].State != "degraded" || catalog.Accounts[0].DegradedReason != accountDegradedSenderIdentity {
		t.Fatalf("primary account = %+v", catalog.Accounts[0])
	}
	if catalog.Accounts[1].Ref != secondary || catalog.Accounts[1].State != "ok" {
		t.Fatalf("unrelated account = %+v, want ok account %s", catalog.Accounts[1], secondary)
	}
}

func TestSenderIdentityScanLimitConfigurationIsBounded(t *testing.T) {
	if got, err := normalizeSenderIdentityScanLimit(0); err != nil || got != mail.DefaultSenderIdentityScanLimit {
		t.Fatalf("default scan limit = %d, error = %v", got, err)
	}
	if got, err := normalizeSenderIdentityScanLimit(4000); err != nil || got != 4000 {
		t.Fatalf("configured scan limit = %d, error = %v", got, err)
	}
	if _, err := normalizeSenderIdentityScanLimit(mail.MaximumSenderIdentityScanLimit + 1); errorCodeForTest(err) != "invalid_argument" {
		t.Fatalf("oversized scan limit error = %v, want invalid_argument", err)
	}
}

func TestListAccountCatalogKeepsGlobalSchemaFailureHardAndWrapped(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "identity@example.com")
	writer := openTestWriter(t, filepath.Join(store.versionRoot, "MailData", envelopeIndexName))
	if _, err := writer.Exec(`DROP TABLE addresses`); err != nil {
		closeTestResourceNow(t, writer, "schema writer")
		t.Fatalf("drop addresses table: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close schema writer: %v", err)
	}

	_, err := store.ListAccountCatalog(context.Background())
	if errorCodeForTest(err) != "account_catalog_incomplete" {
		t.Fatalf("ListAccountCatalog() error = %v, want account_catalog_incomplete", err)
	}
	var sqliteErr sqlite3.Error
	if !errors.As(err, &sqliteErr) {
		t.Fatalf("ListAccountCatalog() error = %v, want wrapped sqlite error", err)
	}
}

func TestAccountCatalogErrorPreservesCause(t *testing.T) {
	cause := errors.New("sentinel catalog cause")
	err := accountCatalogError("account-id", cause)
	if !errors.Is(err, cause) {
		t.Fatalf("accountCatalogError() = %v, errors.Is did not preserve cause", err)
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "account_catalog_incomplete" {
		t.Fatalf("accountCatalogError() = %v, typed error = %+v", err, typed)
	}
}

func insertMalformedSentIdentityRow(t *testing.T, store *Store) {
	t.Helper()
	databasePath := filepath.Join(store.versionRoot, "MailData", envelopeIndexName)
	writer := openTestWriter(t, databasePath)
	statements := []string{
		`INSERT INTO addresses(ROWID,address,comment) VALUES (3,'broken@example.com','Broken')`,
		`INSERT INTO subjects(ROWID,subject) VALUES (1901,'Malformed sent identity')`,
		`INSERT INTO summaries(ROWID,summary) VALUES (2901,'malformed sent identity')`,
		`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (901,3901,4901,3,1901,2901,'not-a-timestamp','not-a-timestamp',4,0,1,0,0,100,901,0,'not-a-timestamp',0)`,
	}
	for _, statement := range statements {
		if _, err := writer.Exec(statement); err != nil {
			closeTestResourceNow(t, writer, "malformed identity writer")
			t.Fatalf("execute malformed identity fixture: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close malformed identity writer: %v", err)
	}
}

func addLocalCatalogAccount(t *testing.T, store *Store) string {
	t.Helper()
	location, err := parseAccountRoot("local://" + secondaryCatalogAccountID + "/")
	if err != nil {
		t.Fatalf("parse secondary account: %v", err)
	}
	store.activeAccounts = append(store.activeAccounts, location)
	store.activeAccountKeys[location.rootKey()] = struct{}{}
	accountRoot := filepath.Join(store.versionRoot, secondaryCatalogAccountID)
	if err := os.MkdirAll(accountRoot, 0o700); err != nil {
		t.Fatalf("create secondary account root: %v", err)
	}
	cache := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>mboxes</key><dict><key>INBOX</key><dict>
<key>MailboxPathComponent</key><string>INBOX</string>
<key>IMAPMailboxChildren</key><dict/>
</dict></dict></dict></plist>`)
	if err := os.WriteFile(filepath.Join(accountRoot, ".mboxCache.plist"), cache, 0o600); err != nil {
		t.Fatalf("write secondary mailbox cache: %v", err)
	}
	ref, err := mailref.EncodeAccount(secondaryCatalogAccountID)
	if err != nil {
		t.Fatalf("encode secondary account: %v", err)
	}
	return ref
}
