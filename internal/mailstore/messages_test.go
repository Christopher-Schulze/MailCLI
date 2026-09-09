package mailstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

func TestStoreReadsNullableMessageIdentity(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	updateFixtureMessage(t, store, `UPDATE messages SET message_id = NULL WHERE ROWID = 102`)

	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Status Update")
	decoded, err := mailref.DecodeMessage(messageRef)
	if err != nil {
		t.Fatalf("DecodeMessage() error = %v", err)
	}
	if decoded.ExpectedStoreMessageID != 0 {
		t.Fatalf("ExpectedStoreMessageID = %d, want 0", decoded.ExpectedStoreMessageID)
	}
	if _, err := store.GetMessage(context.Background(), messageRef); err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	query, err := mail.PrepareQuery(mail.Query{
		MailboxRef: inboxRef, Subject: "Status Update", Limit: 10,
	})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	if _, err := store.SearchMessages(context.Background(), query); err != nil {
		t.Fatalf("SearchMessages() error = %v", err)
	}
}

func TestStoreReturnsExactFullRawSource(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	ref := messageRefWithSubject(t, page.Messages, "Status Update")
	raw, err := store.GetRawSource(context.Background(), ref)
	if err != nil {
		t.Fatalf("GetRawSource() error = %v", err)
	}
	if !strings.HasPrefix(raw, "From: Alice <alice@example.com>\r\n") ||
		!strings.HasSuffix(raw, "needle beta\r\n") {
		t.Fatal("GetRawSource() did not preserve the exact framed RFC source")
	}
}

func TestStoreRejectsRefAfterLogicalMailboxMembershipChanges(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Quarterly Report")
	updateFixtureMessage(t, store, `DELETE FROM labels WHERE message_id = 101 AND mailbox_id = 1`)

	_, err = store.GetMessage(context.Background(), messageRef)
	if errorCodeForTest(err) != "stale_reference" {
		t.Fatalf("GetMessage() error = %v, want stale_reference", err)
	}
}

func TestMessageAttachmentsDoNotClaimMissingFullSourcePartIsDownloaded(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	ref := messageRefWithSubject(t, page.Messages, "Quarterly Report")
	resolved, source, err := store.openMessageSource(context.Background(), ref)
	if err != nil {
		t.Fatalf("openMessageSource() error = %v", err)
	}
	closeTestResource(t, source, "message source")
	attachments, err := store.messageAttachments(context.Background(), resolved, source, map[string]mimePart{})
	if err != nil || len(attachments) != 1 || attachments[0].Downloaded {
		t.Fatalf("messageAttachments() = %+v, error = %v", attachments, err)
	}
}

func TestMessageAttachmentsRejectSymlinkedExternalBytes(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	ref := messageRefWithSubject(t, page.Messages, "Quarterly Report")
	resolved, source, err := store.openMessageSource(context.Background(), ref)
	if err != nil {
		t.Fatalf("openMessageSource() error = %v", err)
	}
	closeTestResource(t, source, "message source")
	directory, err := store.attachmentDirectory(resolved, "2")
	if err != nil {
		t.Fatalf("attachmentDirectory() error = %v", err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	target := filepath.Join(t.TempDir(), "target.pdf")
	if err := os.WriteFile(target, []byte("untrusted"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Symlink(target, filepath.Join(directory, "invoice.pdf")); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	_, err = store.messageAttachments(context.Background(), resolved, source, map[string]mimePart{})
	if errorCodeForTest(err) != "ambiguous_attachment" {
		t.Fatalf("messageAttachments() error = %v, want ambiguous_attachment", err)
	}
}

func TestListCursorIsBoundToMailStore(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 1,
	})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("ListMessages() = %+v, error = %v", page, err)
	}
	if _, err := decodeListCursor(page.NextCursor, inboxRef, "different-store"); errorCodeForTest(err) != "invalid_cursor" {
		t.Fatalf("decodeListCursor() error = %v, want invalid_cursor", err)
	}
}

func TestListMessagesRejectsMalformedCursorBeforeCatalogRead(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	if _, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Cursor: "lcur_!!!", Limit: 1,
	}); errorCodeForTest(err) != "invalid_cursor" {
		t.Fatalf("ListMessages() error = %v, want invalid_cursor", err)
	}
	if store.mailboxCatalogQueries != 0 {
		t.Fatalf("malformed cursor triggered %d mailbox catalog queries", store.mailboxCatalogQueries)
	}
}

func TestListCursorAcceptsLegacyJSONFixture(t *testing.T) {
	t.Parallel()
	want := listCursor{
		Version: legacyListCursorVersion, StoreUUID: "store-uuid", MailboxRef: "mbx_ref",
		DateReceived: 1700000000, RowID: 42,
	}
	payload, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	token := "lcur_" + base64.RawURLEncoding.EncodeToString(payload)
	got, err := decodeListCursor(token, want.MailboxRef, want.StoreUUID)
	if err != nil {
		t.Fatalf("decodeListCursor() error = %v", err)
	}
	if *got != want {
		t.Fatalf("legacy list cursor = %+v, want %+v", *got, want)
	}
}

func TestCompactListCursorPreservesNullDateAndReducesSize(t *testing.T) {
	t.Parallel()
	want := listCursor{
		Version: listCursorVersion, StoreUUID: "store-uuid-424242", MailboxRef: "mbx_very-long-nested-mailbox-reference",
		DateReceivedNull: true, RowID: 424242,
	}
	token, err := encodeListCursor(want)
	if err != nil {
		t.Fatalf("encodeListCursor() error = %v", err)
	}
	got, err := decodeListCursor(token, want.MailboxRef, want.StoreUUID)
	if err != nil {
		t.Fatalf("decodeListCursor() error = %v", err)
	}
	if *got != want {
		t.Fatalf("compact list cursor = %+v, want %+v", *got, want)
	}
	legacyPayload, err := json.Marshal(listCursor{
		Version: legacyListCursorVersion, StoreUUID: want.StoreUUID, MailboxRef: want.MailboxRef,
		DateReceived: want.DateReceived, DateReceivedNull: want.DateReceivedNull, RowID: want.RowID,
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if len(token) >= len("lcur_"+base64.RawURLEncoding.EncodeToString(legacyPayload)) {
		t.Fatalf("compact token size = %d, legacy token size = %d; want reduction", len(token), len("lcur_"+base64.RawURLEncoding.EncodeToString(legacyPayload)))
	}
	compactTokenBytes := len(token)
	legacyTokenBytes := len("lcur_" + base64.RawURLEncoding.EncodeToString(legacyPayload))
	if (compactTokenBytes+3)/4 >= (legacyTokenBytes+3)/4 {
		t.Fatalf("compact token estimate = %d, legacy token estimate = %d; want reduction", (compactTokenBytes+3)/4, (legacyTokenBytes+3)/4)
	}
}

func TestListCursorLegacyAndCompactContinueIdentically(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t, 4)
	closeTestResource(t, store, "test store")
	first, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inboxRef, Limit: 1})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("ListMessages(first) = %#v, error = %v", first, err)
	}
	compactCursor, err := decodeListCursor(first.NextCursor, inboxRef, store.storeUUID)
	if err != nil {
		t.Fatalf("decodeListCursor(compact) error = %v", err)
	}
	legacyPayload, err := json.Marshal(listCursor{
		Version: legacyListCursorVersion, StoreUUID: compactCursor.StoreUUID, MailboxRef: compactCursor.MailboxRef,
		DateReceived: compactCursor.DateReceived, DateReceivedNull: compactCursor.DateReceivedNull, RowID: compactCursor.RowID,
	})
	if err != nil {
		t.Fatalf("json.Marshal(legacy cursor) error = %v", err)
	}
	legacyCursor := "lcur_" + base64.RawURLEncoding.EncodeToString(legacyPayload)
	var continuations [][]string
	for _, cursor := range []string{first.NextCursor, legacyCursor} {
		next, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inboxRef, Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("ListMessages(next) error = %v", err)
		}
		ids := make([]string, 0, len(next.Messages))
		for _, item := range next.Messages {
			ref, err := mailref.DecodeMessage(item.Ref)
			if err != nil {
				t.Fatalf("DecodeMessage() error = %v", err)
			}
			ids = append(ids, ref.LibraryID)
		}
		continuations = append(continuations, ids)
	}
	if !reflect.DeepEqual(continuations[0], continuations[1]) {
		t.Fatalf("compact continuation = %v, legacy continuation = %v", continuations[0], continuations[1])
	}
}

func FuzzDecodeListCursor(f *testing.F) {
	compact, _ := encodeListCursor(listCursor{StoreUUID: "store", MailboxRef: "mbx", DateReceived: 1, RowID: 1})
	for _, seed := range []string{compact, "", "lcur_!!!", "lcur_eyJ2ZXJzaW9uIjoyfQ"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, token string) {
		_, _ = decodeListCursor(token, "mbx", "store")
	})
}

func BenchmarkListCursorEncoding(b *testing.B) {
	cursor := listCursor{StoreUUID: "store-uuid", MailboxRef: "mbx_nested_mailbox_reference", DateReceived: 1700000000, RowID: 424242}
	b.Run("compact", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			token, err := encodeListCursor(cursor)
			if err != nil {
				b.Fatal(err)
			}
			if i == 0 {
				b.ReportMetric(float64(len(token)), "bytes/token")
			}
		}
	})
	b.Run("legacy-json", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			payload, err := json.Marshal(listCursor{Version: legacyListCursorVersion, StoreUUID: cursor.StoreUUID, MailboxRef: cursor.MailboxRef, DateReceived: cursor.DateReceived, RowID: cursor.RowID})
			if err != nil {
				b.Fatal(err)
			}
			token := "lcur_" + base64.RawURLEncoding.EncodeToString(payload)
			if i == 0 {
				b.ReportMetric(float64(len(token)), "bytes/token")
			}
		}
	})
}
