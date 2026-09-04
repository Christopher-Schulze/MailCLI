package mailstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/mattn/go-sqlite3"

	"mailcli/internal/mail"
)

// openBenchmarkStore opens the real local Mail store read-only. The store is
// never mutated, Mail.app is not started, and no Apple Events are sent: these
// benchmarks exercise SQLite reads and .emlx source streaming only.
func openBenchmarkStore(b *testing.B) *Store {
	b.Helper()
	config, err := DefaultConfig()
	if err != nil {
		b.Fatalf("load default config: %v", err)
	}
	ctx := context.Background()
	store, err := Open(ctx, config)
	if err != nil {
		b.Skipf("local Mail store unavailable: %v", err)
	}
	b.Cleanup(func() {
		if err := store.Close(); err != nil {
			b.Fatalf("close store: %v", err)
		}
	})
	return store
}

// resolveGmailINBOX locates the Gmail account's INBOX mailbox so benchmarks
// run against a large, realistic mailbox. Falls back to the INBOX holding the
// most messages when no Gmail account exists.
func resolveGmailINBOX(b *testing.B, store *Store) (accountRef, mailboxRef string) {
	b.Helper()
	ctx := context.Background()
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		b.Fatalf("list accounts: %v", err)
	}
	gmail := ""
	fallback := ""
	bestCount := -1
	fallbackRef := ""
	for _, account := range accounts {
		for _, address := range account.EmailAddresses {
			if gmail == "" && containsFold(address, "@gmail.com") {
				gmail = account.Ref
			}
		}
		if fallback == "" {
			fallback = account.Ref
		}
		mailboxes, err := store.ListMailboxes(ctx, mail.ListMailboxesRequest{AccountRef: account.Ref})
		if err != nil {
			continue
		}
		for _, mailbox := range mailboxes {
			if len(mailbox.Path) == 1 && mailbox.Path[0] == "INBOX" && mailbox.MessageCount > bestCount {
				bestCount = mailbox.MessageCount
				fallbackRef = mailbox.Ref
			}
		}
	}
	if gmail != "" {
		mailboxes, err := store.ListMailboxes(ctx, mail.ListMailboxesRequest{AccountRef: gmail})
		if err != nil {
			b.Fatalf("list gmail mailboxes: %v", err)
		}
		for _, mailbox := range mailboxes {
			if len(mailbox.Path) == 1 && mailbox.Path[0] == "INBOX" {
				return gmail, mailbox.Ref
			}
		}
	}
	if fallbackRef == "" {
		b.Skip("no INBOX mailbox found in local store")
	}
	return fallback, fallbackRef
}

func containsFold(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if equalFold(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func equalFold(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := 0; i < len(left); i++ {
		a, b := left[i], right[i]
		if 'A' <= a && a <= 'Z' {
			a += 'a' - 'A'
		}
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

func openSenderIdentityBenchmarkStore(b *testing.B) *Store {
	b.Helper()
	root := b.TempDir()
	accountRoot := filepath.Join(root, testAccountID)
	if err := os.MkdirAll(accountRoot, 0o700); err != nil {
		b.Fatalf("create sender identity benchmark account root: %v", err)
	}
	database := sql.OpenDB(&sqliteConnector{
		driver: &sqlite3.SQLiteDriver{},
		dsn:    filepath.Join(root, "Envelope Index"),
	})
	statements := []string{
		`CREATE TABLE mailboxes (ROWID INTEGER PRIMARY KEY, url TEXT, total_count INTEGER, unread_count INTEGER)`,
		`CREATE TABLE messages (ROWID INTEGER PRIMARY KEY, sender INTEGER, date_sent INTEGER, date_received INTEGER, mailbox INTEGER, deleted INTEGER)`,
		`CREATE TABLE addresses (ROWID INTEGER PRIMARY KEY, address TEXT, comment TEXT)`,
		`CREATE TABLE labels (message_id INTEGER, mailbox_id INTEGER)`,
		`CREATE INDEX messages_mailbox_date_received ON messages(mailbox, date_received)`,
		`CREATE INDEX labels_mailbox ON labels(mailbox_id)`,
		`INSERT INTO mailboxes(ROWID,url,total_count,unread_count) VALUES (1,'imap://AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE/Sent',50000,0)`,
		`INSERT INTO addresses(ROWID,address,comment) VALUES (1,'sender@example.com','Sender')`,
		`WITH RECURSIVE numbers(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM numbers WHERE n < 50000)
		 INSERT INTO messages(ROWID,sender,date_sent,date_received,mailbox,deleted)
		 SELECT n,1,n,n,1,0 FROM numbers`,
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			_ = database.Close()
			b.Fatalf("create sender identity benchmark fixture: %v", err)
		}
	}
	cache := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>mboxes</key><dict><key>Sent</key><dict>
<key>MailboxPathComponent</key><string>Sent</string>
<key>IMAPMailboxAttributes</key><integer>32768</integer>
<key>IMAPMailboxChildren</key><dict/>
</dict></dict></dict></plist>`)
	if err := os.WriteFile(filepath.Join(accountRoot, ".mboxCache.plist"), cache, 0o600); err != nil {
		_ = database.Close()
		b.Fatalf("write sender identity benchmark cache: %v", err)
	}
	versionDirectory, err := os.Open(root)
	if err != nil {
		_ = database.Close()
		b.Fatalf("open sender identity benchmark root: %v", err)
	}
	location, err := parseAccountRoot("imap://" + testAccountID + "/")
	if err != nil {
		_ = versionDirectory.Close()
		_ = database.Close()
		b.Fatalf("parse sender identity benchmark account: %v", err)
	}
	store := &Store{
		database: database, versionRoot: root, versionDirectory: versionDirectory,
		activeAccounts: []mailboxLocation{location}, activeAccountKeys: map[string]struct{}{location.rootKey(): {}},
	}
	b.Cleanup(func() {
		if err := store.Close(); err != nil {
			b.Errorf("close sender identity benchmark store: %v", err)
		}
	})
	return store
}

func BenchmarkListAccountsSenderIdentity50K(b *testing.B) {
	store := openSenderIdentityBenchmarkStore(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := store.ListAccounts(ctx); err != nil {
			b.Fatalf("list accounts with bounded sender identities: %v", err)
		}
	}
}

func BenchmarkListAccountsSenderIdentity50KUnbounded(b *testing.B) {
	store := openSenderIdentityBenchmarkStore(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := listAccountsUnboundedSenderIdentityBenchmark(ctx, store); err != nil {
			b.Fatalf("list accounts with unbounded sender identities: %v", err)
		}
	}
}

func listAccountsUnboundedSenderIdentityBenchmark(ctx context.Context, store *Store) error {
	records, err := store.mailboxRecords(ctx)
	if err != nil {
		return err
	}
	recordsByPath := make(map[string]mailboxRecord, len(records))
	for _, record := range records {
		recordsByPath[record.pathKey] = record
	}
	location := store.activeAccounts[0]
	cached, err := store.loadMailboxCache(ctx, location.AccountID)
	if err != nil {
		return err
	}
	sentMailboxIDs, foundSent, err := strictSpecialMailboxIDs(
		cached.Mailboxes, location, mailboxAttributeSent, recordsByPath,
	)
	if err != nil {
		return err
	}
	if !foundSent || len(sentMailboxIDs) == 0 {
		return nil
	}
	_, err = loadSenderIdentitiesUnboundedBenchmark(ctx, store.database)
	return err
}

func BenchmarkLoadSenderIdentities50K(b *testing.B) {
	store := openSenderIdentityBenchmarkStore(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := store.loadSenderIdentities(ctx, []int64{1}); err != nil {
			b.Fatalf("load bounded sender identities: %v", err)
		}
	}
}

func BenchmarkLoadSenderIdentities50KUnbounded(b *testing.B) {
	store := openSenderIdentityBenchmarkStore(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := loadSenderIdentitiesUnboundedBenchmark(ctx, store.database); err != nil {
			b.Fatalf("load unbounded sender identities: %v", err)
		}
	}
}

func loadSenderIdentitiesUnboundedBenchmark(ctx context.Context, database *sql.DB) (int64, error) {
	rows, err := database.QueryContext(ctx, `
		WITH membership(id) AS (
			SELECT ROWID FROM messages WHERE mailbox IN (?)
			UNION
			SELECT message_id FROM labels WHERE mailbox_id IN (?)
		)
		SELECT COALESCE(sender.address, ''), COALESCE(sender.comment, ''),
			count(*), max(COALESCE(message.date_sent, 0))
		FROM membership
		JOIN messages message ON message.ROWID = membership.id
		JOIN addresses sender ON sender.ROWID = message.sender
		WHERE message.deleted = 0
		GROUP BY sender.address, sender.comment
	`, 1, 1)
	if err != nil {
		return 0, fmt.Errorf("query unbounded sender identities: %w", err)
	}
	var total int64
	for rows.Next() {
		var address string
		var name string
		var messageCount int64
		var latestSent int64
		if err := rows.Scan(&address, &name, &messageCount, &latestSent); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan unbounded sender identity: %w", err)
		}
		total += messageCount
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate unbounded sender identities: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close unbounded sender identities: %w", err)
	}
	return total, nil
}

func BenchmarkListAccounts(b *testing.B) {
	store := openBenchmarkStore(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.ListAccounts(ctx); err != nil {
			b.Fatalf("list accounts: %v", err)
		}
	}
}

func BenchmarkListMailboxesGmail(b *testing.B) {
	store := openBenchmarkStore(b)
	accountRef, _ := resolveGmailINBOX(b, store)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.ListMailboxes(ctx, mail.ListMailboxesRequest{AccountRef: accountRef}); err != nil {
			b.Fatalf("list mailboxes: %v", err)
		}
	}
}

func BenchmarkListMessagesGmailINBOX25(b *testing.B) {
	store := openBenchmarkStore(b)
	_, mailboxRef := resolveGmailINBOX(b, store)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		page, err := store.ListMessages(ctx, mail.ListMessagesRequest{MailboxRef: mailboxRef, Limit: 25})
		if err != nil {
			b.Fatalf("list messages: %v", err)
		}
		if len(page.Messages) == 0 {
			b.Fatalf("expected messages in INBOX")
		}
	}
}

// BenchmarkSearchMetadataSubject measures the pure SQL metadata path
// (`messages filter --subject ...` equivalent) without any .emlx body scan.
func BenchmarkSearchMetadataSubject(b *testing.B) {
	store := openBenchmarkStore(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		prepared, err := mail.PrepareQuery(mail.Query{Subject: "invoice", Limit: 25})
		if err != nil {
			b.Fatalf("prepare query: %v", err)
		}
		if _, err := store.SearchMessages(ctx, prepared); err != nil {
			b.Fatalf("search metadata: %v", err)
		}
	}
}

// BenchmarkSearchBodySmallINBOX measures a single-term body search scoped to
// the Gmail INBOX (`messages search --mailbox ... --query invoice`).
func BenchmarkSearchBodySmallINBOX(b *testing.B) {
	store := openBenchmarkStore(b)
	_, mailboxRef := resolveGmailINBOX(b, store)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		prepared, err := mail.PrepareQuery(mail.Query{Text: "invoice", MailboxRef: mailboxRef, Limit: 25})
		if err != nil {
			b.Fatalf("prepare query: %v", err)
		}
		if _, err := store.SearchMessages(ctx, prepared); err != nil {
			b.Fatalf("search bodies: %v", err)
		}
	}
}

// BenchmarkSearchBodyLargeStore measures a multi-term body search across all
// active accounts and mailboxes (no scope restriction), the heaviest query
// shape the CLI offers.
func BenchmarkSearchBodyLargeStore(b *testing.B) {
	store := openBenchmarkStore(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		prepared, err := mail.PrepareQuery(mail.Query{
			Text: "invoice receipt payment confirmation", Limit: 25,
		})
		if err != nil {
			b.Fatalf("prepare query: %v", err)
		}
		if _, err := store.SearchMessages(ctx, prepared); err != nil {
			b.Fatalf("search bodies: %v", err)
		}
	}
}

// BenchmarkSearchBodyHeapRebound verifies the body-search path retains no
// heap: it runs repeated full body searches, forces GC, and reports retained
// heap growth per iteration. A persistent index or leaked scan state would
// show up as monotonic heap growth.
func BenchmarkSearchBodyHeapRebound(b *testing.B) {
	store := openBenchmarkStore(b)
	_, mailboxRef := resolveGmailINBOX(b, store)
	ctx := context.Background()
	runtime.GC()
	var baseline uint64

	// Warmup: one full search outside the measured loop.
	prepared, err := mail.PrepareQuery(mail.Query{Text: "invoice", MailboxRef: mailboxRef, Limit: 25})
	if err != nil {
		b.Fatalf("prepare query: %v", err)
	}
	if _, err := store.SearchMessages(ctx, prepared); err != nil {
		b.Fatalf("search bodies: %v", err)
	}
	runtime.GC()
	baseline = heapAlloc()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.SearchMessages(ctx, prepared); err != nil {
			b.Fatalf("search bodies: %v", err)
		}
	}
	b.StopTimer()
	runtime.GC()
	retained := int64(heapAlloc()) - int64(baseline)
	b.ReportMetric(float64(retained)/float64(b.N), "retained-B/op")
	if retained > 1<<20 {
		b.Errorf("body search retained %d bytes of heap after GC across %d iterations; possible leak", retained, b.N)
	}
}

func heapAlloc() uint64 {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}
