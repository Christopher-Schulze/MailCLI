package mailstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mattn/go-sqlite3"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
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

// BenchmarkSearchFixture603 measures the first result page over a generated
// 603-message store. It keeps metadata, body, and explicit-count comparisons
// reproducible without opening Mail.app or reading the user's Mail store.
func BenchmarkSearchFixture603(b *testing.B) {
	benchmarks := []struct {
		name  string
		query mail.Query
	}{
		{name: "metadata_subject", query: mail.Query{Subject: "Status", Limit: 25}},
		{name: "metadata_subject_exact_count", query: mail.Query{Subject: "Status", Limit: 25, MaxMessages: 1000, ExactCount: true}},
		{name: "body_default", query: mail.Query{Text: "needle", Limit: 25, MaxMessages: 1000}},
		{name: "body_exact_count", query: mail.Query{Text: "needle", Limit: 25, MaxMessages: 1000, ExactCount: true}},
	}
	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			store, mailboxRef := newSearchFixture(b, 600)
			closeTestResource(b, store, "benchmark store")
			query := benchmark.query
			query.MailboxRef = mailboxRef
			prepared, err := mail.PrepareQuery(query)
			if err != nil {
				b.Fatalf("prepare search query: %v", err)
			}
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				page, err := store.SearchMessages(ctx, prepared)
				if err != nil {
					b.Fatalf("search fixture: %v", err)
				}
				if len(page.Messages) != 25 {
					b.Fatalf("search fixture returned %d messages, want 25", len(page.Messages))
				}
			}
		})
	}
}

func reportGeneratedStoreFixture(b *testing.B, fixture searchFixtureData) {
	b.ReportMetric(float64(fixture.messageCount), "messages")
	b.ReportMetric(float64(fixture.indexBytes), "index_B")
	b.ReportMetric(float64(fixture.sourceBytes), "source_B")
}

func openGeneratedStoreFixture(tb testing.TB, fixture searchFixtureData) *Store {
	tb.Helper()
	store, err := Open(context.Background(), Config{
		MailRoot: fixture.mailRoot, ActiveAccountURLs: fixture.activeAccountURLs,
	})
	if err != nil {
		tb.Fatalf("open generated benchmark store: %v", err)
	}
	return store
}

// BenchmarkGeneratedStoreInitialOpen times the first Store open on an
// unopened generated fixture. Close is excluded from this measurement.
func BenchmarkGeneratedStoreInitialOpen(b *testing.B) {
	fixture := newGeneratedStoreFixture(b)
	config := Config{MailRoot: fixture.mailRoot, ActiveAccountURLs: fixture.activeAccountURLs}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		store, err := Open(ctx, config)
		if err != nil {
			b.Fatalf("open generated benchmark store: %v", err)
		}
		b.StopTimer()
		closeErr := store.Close()
		b.StartTimer()
		if closeErr != nil {
			b.Fatalf("close generated benchmark store: %v", closeErr)
		}
	}
	reportGeneratedStoreFixture(b, fixture)
}

// BenchmarkGeneratedStoreLifecycle keeps catalog, page, and full-read paths
// deterministic when a user Mail store is unavailable.
func BenchmarkGeneratedStoreLifecycle(b *testing.B) {
	fixture := newGeneratedStoreFixture(b)
	b.Run("open_close", func(b *testing.B) { benchmarkGeneratedStoreOpenClose(b, fixture) })
	b.Run("list_accounts", func(b *testing.B) { benchmarkGeneratedStoreListAccounts(b, fixture) })
	b.Run("list_mailboxes", func(b *testing.B) { benchmarkGeneratedStoreListMailboxes(b, fixture) })
	b.Run("list_messages_25", func(b *testing.B) { benchmarkGeneratedStoreListMessages(b, fixture) })
	b.Run("get_message_full", func(b *testing.B) { benchmarkGeneratedStoreGetMessage(b, fixture) })
}

func newGeneratedStoreFixture(b testing.TB) searchFixtureData {
	b.Helper()
	fixture := createSearchFixtureData(b, generatedStoreFixtureMessageCount-3, true)
	if fixture.messageCount != generatedStoreFixtureMessageCount {
		b.Fatalf("generated fixture has %d messages, want %d", fixture.messageCount, generatedStoreFixtureMessageCount)
	}
	return fixture
}

func benchmarkGeneratedStoreOpenClose(b *testing.B, fixture searchFixtureData) {
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		store := openGeneratedStoreFixture(b, fixture)
		if err := store.Close(); err != nil {
			b.Fatalf("close generated benchmark store: %v", err)
		}
	}
	reportGeneratedStoreFixture(b, fixture)
}

func benchmarkGeneratedStoreListAccounts(b *testing.B, fixture searchFixtureData) {
	store := openGeneratedStoreFixture(b, fixture)
	b.Cleanup(func() {
		if err := store.Close(); err != nil {
			b.Errorf("close generated benchmark store: %v", err)
		}
	})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		accounts, err := store.ListAccounts(ctx)
		if err != nil {
			b.Fatalf("list generated accounts: %v", err)
		}
		if len(accounts) != 2 {
			b.Fatalf("generated account count = %d, want 2", len(accounts))
		}
	}
	reportGeneratedStoreFixture(b, fixture)
}

func benchmarkGeneratedStoreListMailboxes(b *testing.B, fixture searchFixtureData) {
	store := openGeneratedStoreFixture(b, fixture)
	b.Cleanup(func() {
		if err := store.Close(); err != nil {
			b.Errorf("close generated benchmark store: %v", err)
		}
	})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		mailboxes, err := store.ListMailboxes(
			ctx, mail.ListMailboxesRequest{AccountRef: fixture.accountRef},
		)
		if err != nil {
			b.Fatalf("list generated mailboxes: %v", err)
		}
		if len(mailboxes) != 2 {
			b.Fatalf("generated mailbox count = %d, want 2", len(mailboxes))
		}
	}
	reportGeneratedStoreFixture(b, fixture)
}

func benchmarkGeneratedStoreListMessages(b *testing.B, fixture searchFixtureData) {
	ctx := context.Background()
	setupStore := openGeneratedStoreFixture(b, fixture)
	page, err := setupStore.ListMessages(ctx, mail.ListMessagesRequest{
		MailboxRef: fixture.inboxRef, Limit: 25,
	})
	if err != nil {
		b.Fatalf("verify generated message page: %v", err)
	}
	assertGeneratedStorePage(b, fixture, page)
	if err := setupStore.Close(); err != nil {
		b.Fatalf("close generated page verification store: %v", err)
	}
	store := openGeneratedStoreFixture(b, fixture)
	b.Cleanup(func() {
		if err := store.Close(); err != nil {
			b.Errorf("close generated benchmark store: %v", err)
		}
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		page, err := store.ListMessages(ctx, mail.ListMessagesRequest{
			MailboxRef: fixture.inboxRef, Limit: 25,
		})
		if err != nil || len(page.Messages) != 25 {
			b.Fatalf("list generated 25-message page: count=%d error=%v", len(page.Messages), err)
		}
	}
	reportGeneratedStoreFixture(b, fixture)
}

func BenchmarkListSummaryRead(b *testing.B) {
	for _, variant := range []struct {
		name    string
		summary string
	}{
		{name: "short", summary: "status summary"},
		{name: "large", summary: strings.Repeat("s", 1<<20)},
	} {
		b.Run(variant.name, func(b *testing.B) {
			fixture := newGeneratedStoreFixture(b)
			writer := openTestWriter(b, filepath.Join(
				fixture.mailRoot, "V10", "MailData", envelopeIndexName,
			))
			if _, err := writer.Exec(
				"UPDATE summaries SET summary = ? WHERE ROWID = 2", variant.summary,
			); err != nil {
				closeTestResourceNow(b, writer, "generated summary writer")
				b.Fatalf("update generated summary: %v", err)
			}
			closeTestResourceNow(b, writer, "generated summary writer")
			store := openGeneratedStoreFixture(b, fixture)
			b.Cleanup(func() {
				if err := store.Close(); err != nil {
					b.Errorf("close generated summary store: %v", err)
				}
			})
			ctx := context.Background()
			request := mail.ListMessagesRequest{MailboxRef: fixture.inboxRef, Limit: 25}
			page, err := store.ListMessages(ctx, request)
			if err != nil {
				b.Fatalf("verify generated summary page: %v", err)
			}
			assertGeneratedStorePage(b, fixture, page)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				page, err := store.ListMessages(ctx, request)
				if err != nil || len(page.Messages) != 25 || page.NextCursor == "" {
					b.Fatalf("list generated summary page: count=%d error=%v", len(page.Messages), err)
				}
			}
			b.ReportMetric(float64(len(variant.summary)), "summary_row_B")
			reportGeneratedStoreFixture(b, fixture)
		})
	}
}

func benchmarkGeneratedStoreGetMessage(b *testing.B, fixture searchFixtureData) {
	setupStore := openGeneratedStoreFixture(b, fixture)
	ctx := context.Background()
	page, err := setupStore.ListMessages(ctx, mail.ListMessagesRequest{
		MailboxRef: fixture.inboxRef, Limit: 25,
	})
	if err != nil {
		b.Fatalf("list generated messages for full-read reference: %v", err)
	}
	messageRef := assertGeneratedStorePage(b, fixture, page)
	message, err := setupStore.GetMessage(ctx, messageRef)
	if err != nil {
		b.Fatalf("verify generated full-message read: %v", err)
	}
	assertGeneratedFullMessage(b, message)
	if err := setupStore.Close(); err != nil {
		b.Fatalf("close generated full-read verification store: %v", err)
	}
	store := openGeneratedStoreFixture(b, fixture)
	b.Cleanup(func() {
		if err := store.Close(); err != nil {
			b.Errorf("close generated benchmark store: %v", err)
		}
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		message, err := store.GetMessage(ctx, messageRef)
		if err != nil {
			b.Fatalf("get generated full message: %v", err)
		}
		if !message.ContentComplete || !strings.Contains(message.Content, "needle extra 0") {
			b.Fatalf("generated full message is incomplete: %+v", message.Summary)
		}
	}
	reportGeneratedStoreFixture(b, fixture)
}

// BenchmarkSearchTextRepresentations measures search-text preparation and
// snippet offset handling without SQLite or MIME parsing noise.
func BenchmarkSearchTextRepresentations(b *testing.B) {
	normalizationHeavy := strings.Repeat("Cafe\u0301 ", 1<<17)
	benchmarks := []struct {
		name      string
		value     string
		term      string
		wantMatch bool
	}{
		{name: "ascii_nonmatch_1MiB", value: strings.Repeat("a", 1<<20), term: "needle"},
		{name: "ascii_early_match_1MiB", value: "needle " + strings.Repeat("a", 1<<20), term: "needle", wantMatch: true},
		{name: "ascii_late_match_1MiB", value: strings.Repeat("a", 1<<20) + " needle", term: "needle", wantMatch: true},
		{name: "normalization_heavy_nonmatch", value: normalizationHeavy, term: "needle"},
		{name: "normalization_heavy_match", value: normalizationHeavy, term: "café", wantMatch: true},
	}
	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			terms := normalizedSearchTerms(benchmark.term)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				representations := newSearchTextRepresentations(benchmark.value)
				matched, firstTerm := containsAllFoldedSearchTerms(representations.folded, terms)
				if matched != benchmark.wantMatch {
					b.Fatalf("search match = %t, want %t", matched, benchmark.wantMatch)
				}
				if matched && snippetForSearchText(&representations, firstTerm) == "" {
					b.Fatal("matched search text produced an empty snippet")
				}
			}
		})
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

// BenchmarkSearchBodySmallINBOXExactCount measures the explicit bounded
// candidate-count probe separately from the default stream-derived path.
func BenchmarkSearchBodySmallINBOXExactCount(b *testing.B) {
	store := openBenchmarkStore(b)
	_, mailboxRef := resolveGmailINBOX(b, store)
	ctx := context.Background()
	prepared, err := mail.PrepareQuery(mail.Query{
		Text: "invoice", MailboxRef: mailboxRef, Limit: 25,
		MaxMessages: mail.MaximumSearchMaxMessages, ExactCount: true,
	})
	if err != nil {
		b.Fatalf("prepare query: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.SearchMessages(ctx, prepared); err != nil {
			b.Fatalf("search bodies with exact count: %v", err)
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

type mutationIdentityFixture struct {
	searchFixtureData
	bindingPath string
}

type countedMutationBindings struct {
	mail.AccountBindingStore
	loadCalls atomic.Int64
}

func (store *countedMutationBindings) LoadAccountBindings() (mail.AccountBindingFile, error) {
	store.loadCalls.Add(1)
	return store.AccountBindingStore.LoadAccountBindings()
}

type countedMutationCredentials struct {
	strictCredentials
	loadCalls atomic.Int64
}

func (store *countedMutationCredentials) Load(account string) (string, error) {
	store.loadCalls.Add(1)
	return store.strictCredentials.Load(account)
}

func newMutationIdentityFixture(tb testing.TB) mutationIdentityFixture {
	tb.Helper()
	fixture := createSearchFixtureData(tb, generatedStoreFixtureMessageCount-3, true)
	databasePath := filepath.Join(fixture.mailRoot, "V10", "MailData", envelopeIndexName)
	database := openTestWriter(tb, databasePath)
	for _, mailbox := range []struct {
		rowID     int64
		accountID string
	}{
		{rowID: 4, accountID: testAccountID},
		{rowID: 5, accountID: secondaryCatalogAccountID},
	} {
		_, err := database.Exec(
			`INSERT INTO mailboxes(ROWID,url,total_count,unread_count,deleted_count,source) VALUES(?,?,0,0,0,1)`,
			mailbox.rowID, "imap://"+mailbox.accountID+"/Sent",
		)
		if err != nil {
			closeTestResourceNow(tb, database, "mutation identity fixture database")
			tb.Fatalf("add generated Sent mailbox: %v", err)
		}
	}
	if err := database.Close(); err != nil {
		tb.Fatalf("close mutation identity fixture database: %v", err)
	}
	writeMutationIdentityMailboxCache(tb, fixture.mailRoot, testAccountID, []string{"INBOX", "Archive", "Sent"})
	writeMutationIdentityMailboxCache(tb, fixture.mailRoot, secondaryCatalogAccountID, []string{"INBOX", "Sent"})
	indexInfo, err := os.Stat(databasePath)
	if err != nil {
		tb.Fatalf("stat generated mutation identity index: %v", err)
	}
	fixture.indexBytes = indexInfo.Size()
	bindingPath := filepath.Join(tb.TempDir(), "mutation-account-bindings.json")
	bindings := mail.NewAccountBindingStore(bindingPath)
	for _, binding := range []mail.AccountBinding{
		{AccountID: testAccountID, SenderAliases: []string{"identity@gmail.com"}, CredentialAccount: "identity@gmail.com"},
		{AccountID: secondaryCatalogAccountID, SenderAliases: []string{"other@gmail.com"}, CredentialAccount: "other@gmail.com"},
	} {
		if err := bindings.UpsertAccountBinding(binding); err != nil {
			tb.Fatalf("write generated mutation identity binding: %v", err)
		}
	}
	return mutationIdentityFixture{searchFixtureData: fixture, bindingPath: bindingPath}
}

func writeMutationIdentityMailboxCache(
	tb testing.TB,
	mailRoot string,
	accountID string,
	mailboxNames []string,
) {
	tb.Helper()
	var cache strings.Builder
	cache.WriteString(`<?xml version="1.0"?><plist version="1.0"><dict><key>mboxes</key><dict>`)
	for _, name := range mailboxNames {
		attributes := 0
		if name == "Sent" {
			attributes = mailboxAttributeSent
		}
		if _, err := fmt.Fprintf(&cache,
			`<key>%s</key><dict><key>MailboxPathComponent</key><string>%s</string><key>IMAPMailboxAttributes</key><integer>%d</integer><key>IMAPMailboxChildren</key><dict/></dict>`,
			name, name, attributes,
		); err != nil {
			tb.Fatalf("format generated mutation identity mailbox cache: %v", err)
		}
	}
	cache.WriteString(`</dict></dict></plist>`)
	cachePath := filepath.Join(mailRoot, "V10", accountID, ".mboxCache.plist")
	if err := os.WriteFile(cachePath, []byte(cache.String()), 0o600); err != nil {
		tb.Fatalf("write generated mutation identity mailbox cache: %v", err)
	}
}

func openMutationIdentityClient(
	tb testing.TB,
	fixture mutationIdentityFixture,
) (*Store, *Client, *accountIdentityCounters, *countedMutationBindings, *countedMutationCredentials) {
	tb.Helper()
	bindings := &countedMutationBindings{AccountBindingStore: mail.NewAccountBindingStore(fixture.bindingPath)}
	store, err := Open(context.Background(), Config{
		MailRoot: fixture.mailRoot, ActiveAccountURLs: fixture.activeAccountURLs,
		AccountBindings: bindings,
	})
	if err != nil {
		tb.Fatalf("open generated mutation identity store: %v", err)
	}
	counters := &accountIdentityCounters{}
	store.accountIdentityCounters = counters
	credentials := &countedMutationCredentials{strictCredentials: strictCredentials{
		"identity@gmail.com": "generated-test-password",
	}}
	client := &Client{store: store, send: mail.SendTransport{
		Imap:        &stubImapOperator{boxes: []transport.MailboxInfo{{Name: "INBOX"}, {Name: "Archive"}}},
		Credentials: credentials,
	}}
	tb.Cleanup(func() {
		if err := store.Close(); err != nil {
			tb.Errorf("close generated mutation identity store: %v", err)
		}
	})
	return store, client, counters, bindings, credentials
}

func mutationTargetReferences(tb testing.TB, store *Store, fixture searchFixtureData, count int) []string {
	tb.Helper()
	refs := make([]string, 0, count)
	cursor := ""
	for len(refs) < count {
		limit := min(mail.MaximumPageLimit, count-len(refs))
		page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
			MailboxRef: fixture.inboxRef, Cursor: cursor, Limit: limit,
		})
		if err != nil {
			tb.Fatalf("list generated mutation target references: %v", err)
		}
		for _, message := range page.Messages {
			refs = append(refs, message.Ref)
			if len(refs) == count {
				return refs
			}
		}
		if page.NextCursor == "" {
			tb.Fatalf("generated mutation fixture ended after %d of %d references", len(refs), count)
		}
		cursor = page.NextCursor
	}
	return refs
}

func BenchmarkMutationAccountResolution(b *testing.B) {
	fixture := newMutationIdentityFixture(b)
	for _, benchmark := range []struct {
		name  string
		items int
	}{
		{name: "items_1", items: 1},
		{name: "items_100", items: 100},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			store, client, counters, bindings, credentials := openMutationIdentityClient(b, fixture)
			messageRefs := mutationTargetReferences(b, store, fixture.searchFixtureData, benchmark.items)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				for _, messageRef := range messageRefs {
					target, err := client.resolveImapTargetForMutation(context.Background(), messageRef)
					if err != nil || target.accountID != testAccountID || target.uid == 0 || target.uidvalidity == 0 {
						b.Fatalf("resolve generated mutation target: target=%+v error=%v", target, err)
					}
				}
			}
			iterations := float64(b.N)
			b.ReportMetric(float64(counters.fullCatalogBuilds.Load())/iterations, "catalog_builds/op")
			b.ReportMetric(float64(counters.sentScanQueries.Load())/iterations, "sent_scan_queries/op")
			b.ReportMetric(float64(bindings.loadCalls.Load())/iterations, "binding_loads/op")
			b.ReportMetric(float64(credentials.loadCalls.Load())/iterations, "credential_loads/op")
			reportGeneratedStoreFixture(b, fixture.searchFixtureData)
		})
	}
}

var rawSourceBuilderBenchmarkSink string

func BenchmarkRawSourceBuilder(b *testing.B) {
	fixture := createSearchFixtureData(b, 0, true)
	store := openGeneratedStoreFixture(b, fixture)
	defer func() {
		if err := store.Close(); err != nil {
			b.Errorf("close generated raw-source store: %v", err)
		}
	}()
	ctx := context.Background()
	mailboxRef, err := mailref.EncodeMailbox(testAccountID, []string{"Archive"})
	if err != nil {
		b.Fatalf("encode generated raw-source mailbox: %v", err)
	}
	page, err := store.ListMessages(ctx, mail.ListMessagesRequest{MailboxRef: mailboxRef, Limit: 3})
	if err != nil {
		b.Fatalf("list generated raw-source messages: %v", err)
	}
	messageRef := ""
	for _, message := range page.Messages {
		if message.Subject == "Quarterly Report" {
			messageRef = message.Ref
			break
		}
	}
	if messageRef == "" {
		b.Fatal("generated raw-source message was not listed")
	}
	resolved, err := store.resolveMessage(ctx, messageRef)
	if err != nil {
		b.Fatalf("resolve generated raw-source message: %v", err)
	}
	base, err := store.messageBasePath(resolved.PhysicalLocation, resolved.Record.RowID)
	if err != nil {
		b.Fatalf("resolve generated raw-source path: %v", err)
	}
	for _, sourceSize := range []int{8 << 20, 32 << 20, int(mail.MaximumRawSourceBytes)} {
		b.Run(fmt.Sprintf("source_%d_MiB", sourceSize>>20), func(b *testing.B) {
			rawSourceBuilderBenchmarkSink = ""
			if err := writeRawSourceBenchmarkFrame(b, base+".emlx", sourceSize); err != nil {
				b.Fatalf("write generated raw-source fixture: %v", err)
			}
			runtime.GC()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				raw, err := store.GetRawSource(ctx, messageRef)
				if err != nil {
					b.Fatalf("GetRawSource() error = %v", err)
				}
				if len(raw) != sourceSize {
					b.Fatalf("GetRawSource() bytes = %d, want %d", len(raw), sourceSize)
				}
				rawSourceBuilderBenchmarkSink = raw
			}
			b.ReportMetric(float64(sourceSize), "source_B")
		})
	}
	rawSourceBuilderBenchmarkSink = ""
}

func writeRawSourceBenchmarkFrame(b *testing.B, path string, sourceSize int) error {
	if sourceSize < 0 || sourceSize > int(mail.MaximumRawSourceBytes) {
		b.Fatalf("raw-source fixture size %d is outside the supported range", sourceSize)
	}
	prefix := fmt.Sprintf("%-10d\n", sourceSize)
	if len(prefix) != emlxPrefixBytes {
		b.Fatalf("EMLX prefix length = %d, want %d", len(prefix), emlxPrefixBytes)
	}
	trailer := validPlistTrailer()
	framed := make([]byte, emlxPrefixBytes+sourceSize+len(trailer))
	copy(framed, prefix)
	header := "From: benchmark@example.com\r\nSubject: Raw source benchmark\r\n\r\n"
	if len(header) > sourceSize {
		b.Fatalf("raw-source fixture size %d cannot hold its header", sourceSize)
	}
	copy(framed[emlxPrefixBytes:], header)
	for index := emlxPrefixBytes + len(header); index < emlxPrefixBytes+sourceSize; index++ {
		framed[index] = 'x'
	}
	copy(framed[emlxPrefixBytes+sourceSize:], trailer)
	return os.WriteFile(path, framed, 0o600)
}
