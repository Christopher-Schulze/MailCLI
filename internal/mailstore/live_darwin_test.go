//go:build darwin

package mailstore

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

func TestLiveStoreCapabilities(t *testing.T) {
	if os.Getenv("MAILCLI_LIVE_TESTS") != "1" {
		t.Skip("set MAILCLI_LIVE_TESTS=1 for the local Mail store gate")
	}
	config, err := DefaultConfig()
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	store, err := Open(context.Background(), config)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	closeTestResource(t, store, "live Mail store")
	if store.SchemaFingerprint() == "" {
		t.Fatal("SchemaFingerprint() is empty")
	}
	var messages int
	if err := store.database.QueryRow("SELECT count(*) FROM messages").Scan(&messages); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messages < 1 {
		t.Fatal("live Envelope Index has no messages")
	}
}

func TestLiveClientListsSendersWithoutAutomation(t *testing.T) {
	if os.Getenv("MAILCLI_LIVE_TESTS") != "1" {
		t.Skip("set MAILCLI_LIVE_TESTS=1 for the local Mail store gate")
	}
	config, err := DefaultConfig()
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	store, err := Open(context.Background(), config)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	closeTestResource(t, store, "live Mail store")
	spy := &fallbackSpy{}
	accounts, err := (&Client{store: store, fallback: spy}).ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if spy.accountCalls != 0 {
		t.Fatalf("fallback ListAccounts() calls = %d, want 0", spy.accountCalls)
	}
	addresses := make(map[string]struct{})
	for _, account := range accounts {
		for _, address := range account.EmailAddresses {
			addresses[strings.ToLower(address)] = struct{}{}
		}
	}
	if len(addresses) < 2 {
		t.Fatalf("ListAccounts() sender identities = %v, want both configured senders", addresses)
	}
	t.Logf("store sender identities: %v", addresses)
}

func TestLiveStoreListsMailboxesAndMessages(t *testing.T) {
	if os.Getenv("MAILCLI_LIVE_TESTS") != "1" {
		t.Skip("set MAILCLI_LIVE_TESTS=1 for the local Mail store gate")
	}
	config, err := DefaultConfig()
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	store, err := Open(context.Background(), config)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	closeTestResource(t, store, "live Mail store")
	mailboxes, err := store.ListMailboxes(context.Background(), mail.ListMailboxesRequest{})
	if err != nil {
		t.Fatalf("ListMailboxes() error = %v", err)
	}
	if len(mailboxes) < 1 {
		t.Fatal("ListMailboxes() returned no active mailbox")
	}
	var selected mail.Mailbox
	for _, mailbox := range mailboxes {
		if mailbox.LocalMessagesAvailable && mailbox.MessageCount > 10 {
			selected = mailbox
			break
		}
	}
	if selected.Ref == "" {
		t.Fatal("no populated local mailbox found")
	}
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: selected.Ref, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if len(page.Messages) != 10 {
		t.Fatalf("ListMessages() count = %d", len(page.Messages))
	}
	ref, err := mailref.DecodeMessage(page.Messages[0].Ref)
	if err != nil {
		t.Fatalf("DecodeMessage() error = %v", err)
	}
	if ref.Version != mailref.FormatVersion || !ref.IsStoreBound() {
		t.Fatalf("store message ref = %#v", ref)
	}
	_, source, err := store.openMessageSource(context.Background(), page.Messages[0].Ref)
	if err != nil {
		t.Fatalf("openMessageSource() error = %v", err)
	}
	closeTestResource(t, source, "live message source")
	if source.length < 1 {
		t.Fatal("openMessageSource() returned empty RFC source")
	}
	message, err := store.GetMessage(context.Background(), page.Messages[0].Ref)
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	if message.Summary.Ref == "" || message.ContentSource == "" {
		t.Fatalf("GetMessage() returned incomplete identity metadata")
	}
	if !source.partial {
		raw, err := store.GetRawSource(context.Background(), page.Messages[0].Ref)
		if err != nil {
			t.Fatalf("GetRawSource() error = %v", err)
		}
		if len(raw) != int(source.length) {
			t.Fatalf("GetRawSource() length = %d, want %d", len(raw), source.length)
		}
	}
	metadataQuery, err := mail.PrepareQuery(mail.Query{
		MailboxRef: selected.Ref, Subject: page.Messages[0].Subject, Limit: 10,
	})
	if err != nil {
		t.Fatalf("PrepareQuery(metadata) error = %v", err)
	}
	metadataPage, err := store.SearchMessages(context.Background(), metadataQuery)
	if err != nil {
		t.Fatalf("SearchMessages(metadata) error = %v", err)
	}
	if len(metadataPage.Messages) < 1 || !metadataPage.Coverage.Complete {
		t.Fatal("metadata search did not return a complete local result")
	}
	terms := strings.Fields(page.Messages[0].Subject)
	if len(terms) > 0 {
		bodyQuery, err := mail.PrepareQuery(mail.Query{
			MailboxRef: selected.Ref, Text: terms[0], Limit: 1,
			MaxMessages: 100, MaxBytes: 512 * 1024 * 1024,
		})
		if err != nil {
			t.Fatalf("PrepareQuery(body) error = %v", err)
		}
		bodyPage, err := store.SearchMessages(context.Background(), bodyQuery)
		if err != nil {
			t.Fatalf("SearchMessages(body) error = %v", err)
		}
		if len(bodyPage.Messages) < 1 || bodyPage.Coverage.Backend != "emlx_stream" {
			t.Fatal("body search did not return a stateless EMLX result")
		}
	}
	assertLiveReadP95(t, store, selected.Ref, page.Messages[0].Subject)
}

func assertLiveReadP95(t *testing.T, store *Store, mailboxRef string, subject string) {
	t.Helper()
	const samples = 30
	listDurations := make([]time.Duration, 0, samples)
	searchDurations := make([]time.Duration, 0, samples)
	for range samples {
		started := time.Now()
		_, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
			MailboxRef: mailboxRef, Limit: 10,
		})
		if err != nil {
			t.Fatalf("timed ListMessages() error = %v", err)
		}
		listDurations = append(listDurations, time.Since(started))
		query, err := mail.PrepareQuery(mail.Query{
			MailboxRef: mailboxRef, Subject: subject, Limit: 10,
		})
		if err != nil {
			t.Fatalf("timed PrepareQuery() error = %v", err)
		}
		started = time.Now()
		if _, err := store.SearchMessages(context.Background(), query); err != nil {
			t.Fatalf("timed SearchMessages() error = %v", err)
		}
		searchDurations = append(searchDurations, time.Since(started))
	}
	listP95 := durationP95(listDurations)
	searchP95 := durationP95(searchDurations)
	if listP95 >= 50*time.Millisecond {
		t.Fatalf("warm metadata list p95 = %s, want <50ms", listP95)
	}
	if searchP95 >= 100*time.Millisecond {
		t.Fatalf("warm metadata search p95 = %s, want <100ms", searchP95)
	}
	t.Logf("warm read p95: list=%s metadata_search=%s", listP95, searchP95)
}

func durationP95(values []time.Duration) time.Duration {
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(left int, right int) bool { return sorted[left] < sorted[right] })
	index := (len(sorted)*95+99)/100 - 1
	return sorted[index]
}
