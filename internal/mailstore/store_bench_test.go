package mailstore

import (
	"context"
	"runtime"
	"testing"

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
