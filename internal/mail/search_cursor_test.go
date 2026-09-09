package mail

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"mailcli/internal/mailref"
)

func TestEncodeSearchCursorRoundtrip(t *testing.T) {
	fingerprint := "abc123query"
	storeUUID := "store-uuid-xyz"
	receivedAt := int64(1700000000)
	rowID := int64(42)
	encoded, err := EncodeSearchCursor(fingerprint, storeUUID, receivedAt, false, rowID)
	if err != nil {
		t.Fatalf("EncodeSearchCursor error = %v", err)
	}
	if len(encoded) == 0 {
		t.Fatal("encoded cursor is empty")
	}
	cursor, err := DecodeSearchCursor(encoded, fingerprint)
	if err != nil {
		t.Fatalf("DecodeSearchCursor error = %v", err)
	}
	if cursor.Fingerprint != fingerprint {
		t.Errorf("Fingerprint = %q, want %q", cursor.Fingerprint, fingerprint)
	}
	if cursor.StoreUUID != storeUUID {
		t.Errorf("StoreUUID = %q, want %q", cursor.StoreUUID, storeUUID)
	}
	if cursor.ReceivedAt != receivedAt {
		t.Errorf("ReceivedAt = %d, want %d", cursor.ReceivedAt, receivedAt)
	}
	if cursor.RowID != rowID {
		t.Errorf("RowID = %d, want %d", cursor.RowID, rowID)
	}
}

func TestEncodeSearchCursorWithRevisionRoundtrip(t *testing.T) {
	encoded, err := EncodeSearchCursorWithRevision("fp", "store", "revision-42", 1700000000, false, 42)
	if err != nil {
		t.Fatalf("EncodeSearchCursorWithRevision error = %v", err)
	}
	cursor, err := DecodeSearchCursor(encoded, "fp")
	if err != nil {
		t.Fatalf("DecodeSearchCursor error = %v", err)
	}
	if cursor.IndexRevision != "revision-42" {
		t.Fatalf("IndexRevision = %q, want revision-42", cursor.IndexRevision)
	}
}

func TestEncodeSearchCursorInclusiveRoundtrip(t *testing.T) {
	encoded, err := EncodeSearchCursorInclusive("fp", "store", 1700000000, false, 42)
	if err != nil {
		t.Fatalf("EncodeSearchCursorInclusive error = %v", err)
	}
	cursor, err := DecodeSearchCursor(encoded, "fp")
	if err != nil {
		t.Fatalf("DecodeSearchCursor error = %v", err)
	}
	if !cursor.Inclusive {
		t.Fatal("Inclusive = false, want true")
	}
}

func TestDecodeSearchCursorRejectsMissingPrefix(t *testing.T) {
	_, err := DecodeSearchCursor("no-prefix-here", "abc")
	if err == nil {
		t.Fatal("DecodeSearchCursor error = nil, want prefix error")
	}
}

func TestDecodeSearchCursorRejectsInvalidBase64(t *testing.T) {
	_, err := DecodeSearchCursor("scur_!!!not-valid-base64!!!", "abc")
	if err == nil {
		t.Fatal("DecodeSearchCursor error = nil, want base64 decode error")
	}
}

func TestDecodeSearchCursorRejectsFingerprintMismatch(t *testing.T) {
	encoded, err := EncodeSearchCursor("correct-fp", "store-uuid", 1700000000, false, 1)
	if err != nil {
		t.Fatalf("EncodeSearchCursor error = %v", err)
	}
	_, err = DecodeSearchCursor(encoded, "wrong-fp")
	if err == nil {
		t.Fatal("DecodeSearchCursor error = nil, want fingerprint mismatch error")
	}
}

func TestDecodeSearchCursorRejectsRowIDZero(t *testing.T) {
	encoded, err := EncodeSearchCursor("fp", "store-uuid", 1700000000, false, 0)
	if err != nil {
		t.Fatalf("EncodeSearchCursor error = %v", err)
	}
	_, err = DecodeSearchCursor(encoded, "fp")
	if err == nil {
		t.Fatal("DecodeSearchCursor error = nil, want row ID zero rejection")
	}
}

func TestDecodeSearchCursorRejectsEmptyStoreUUID(t *testing.T) {
	encoded, err := EncodeSearchCursor("fp", "", 1700000000, false, 1)
	if err != nil {
		t.Fatalf("EncodeSearchCursor error = %v", err)
	}
	_, err = DecodeSearchCursor(encoded, "fp")
	if err == nil {
		t.Fatal("DecodeSearchCursor error = nil, want empty store UUID rejection")
	}
}

func TestDecodeSearchCursorAcceptsLegacyJSONFixture(t *testing.T) {
	t.Parallel()
	want := SearchCursor{
		Version: legacySearchCursorVersion, Fingerprint: "fp", StoreUUID: "store",
		IndexRevision: "revision", ReceivedAt: 1700000000, RowID: 42, Inclusive: true,
	}
	payload, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	token := "scur_" + base64.RawURLEncoding.EncodeToString(payload)
	got, err := DecodeSearchCursor(token, "fp")
	if err != nil {
		t.Fatalf("DecodeSearchCursor() error = %v", err)
	}
	if *got != want {
		t.Fatalf("legacy cursor = %+v, want %+v", *got, want)
	}
}

func TestCompactSearchCursorPreservesFlagsAndReducesSize(t *testing.T) {
	t.Parallel()
	input := SearchCursor{
		Version: searchCursorVersion, Fingerprint: "query-fingerprint-with-unicode-ä✅",
		StoreUUID: "store-uuid", IndexRevision: "revision-2026-09-09", ReceivedAt: -1700000000,
		ReceivedAtNull: true, RowID: 424242, Inclusive: true,
	}
	compact, err := EncodeSearchCursorInclusiveWithRevision(
		input.Fingerprint, input.StoreUUID, input.IndexRevision,
		input.ReceivedAt, input.ReceivedAtNull, input.RowID,
	)
	if err != nil {
		t.Fatalf("EncodeSearchCursor() error = %v", err)
	}
	got, err := DecodeSearchCursor(compact, input.Fingerprint)
	if err != nil {
		t.Fatalf("DecodeSearchCursor() error = %v", err)
	}
	if got.Version != searchCursorVersion || got.StoreUUID != input.StoreUUID || got.IndexRevision != input.IndexRevision || got.ReceivedAt != input.ReceivedAt || !got.ReceivedAtNull || !got.Inclusive || got.RowID != input.RowID {
		t.Fatalf("compact cursor = %+v, want fields from %+v", *got, input)
	}
	legacyPayload, err := json.Marshal(SearchCursor{
		Version: legacySearchCursorVersion, Fingerprint: input.Fingerprint, StoreUUID: input.StoreUUID,
		IndexRevision: input.IndexRevision, ReceivedAt: input.ReceivedAt,
		ReceivedAtNull: input.ReceivedAtNull, RowID: input.RowID, Inclusive: input.Inclusive,
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	compactPayload, err := mailref.DecodeTokenPayload("scur_", compact)
	if err != nil {
		t.Fatalf("DecodeTokenPayload() error = %v", err)
	}
	if len(compactPayload) >= len(legacyPayload) {
		t.Fatalf("compact payload size = %d, legacy JSON size = %d; want reduction", len(compactPayload), len(legacyPayload))
	}
	if (len(compactPayload)+3)/4 >= (len(legacyPayload)+3)/4 {
		t.Fatalf("compact token estimate = %d, legacy token estimate = %d; want reduction", (len(compactPayload)+3)/4, (len(legacyPayload)+3)/4)
	}
}

func TestDecodeSearchCursorRejectsUnknownFlagsAndTrailingBytes(t *testing.T) {
	t.Parallel()
	payload, err := mailref.EncodeCompactPayload(mailref.CompactPayload{
		Fingerprint: "fp", StoreUUID: "store", IndexRevision: "revision", ReceivedAt: 1, Flags: 4, RowID: 1,
	}, searchCursorVersion)
	if err != nil {
		t.Fatalf("EncodeCompactPayload() error = %v", err)
	}
	token, err := mailref.EncodeToken("scur_", payload)
	if err != nil {
		t.Fatalf("EncodeToken() error = %v", err)
	}
	if _, err := DecodeSearchCursor(token, "fp"); err == nil {
		t.Fatal("DecodeSearchCursor(unknown flags) error = nil")
	}
	payload, err = mailref.EncodeCompactPayload(mailref.CompactPayload{
		Fingerprint: "fp", StoreUUID: "store", IndexRevision: "revision", ReceivedAt: 1, RowID: 1,
	}, searchCursorVersion)
	if err != nil {
		t.Fatalf("EncodeCompactPayload() error = %v", err)
	}
	token, err = mailref.EncodeToken("scur_", append(payload, 0))
	if err != nil {
		t.Fatalf("EncodeToken() error = %v", err)
	}
	if _, err := DecodeSearchCursor(token, "fp"); err == nil {
		t.Fatal("DecodeSearchCursor(trailing) error = nil")
	}
}

func FuzzDecodeSearchCursor(f *testing.F) {
	valid, _ := EncodeSearchCursorWithRevision("fp", "store", "revision", 1, false, 1)
	for _, seed := range []string{valid, "", "scur_!!!", "scur_eyJ2ZXJzaW9uIjozfQ"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, token string) {
		_, _ = DecodeSearchCursor(token, "fp")
	})
}

func BenchmarkSearchCursorEncoding(b *testing.B) {
	fingerprint, storeUUID, revision := "query-fingerprint", "store-uuid", "revision-42"
	b.Run("compact", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			token, err := EncodeSearchCursorWithRevision(fingerprint, storeUUID, revision, 1700000000, false, 42)
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
			payload, err := json.Marshal(SearchCursor{Version: legacySearchCursorVersion, Fingerprint: fingerprint, StoreUUID: storeUUID, IndexRevision: revision, ReceivedAt: 1700000000, RowID: 42})
			if err != nil {
				b.Fatal(err)
			}
			token := "scur_" + base64.RawURLEncoding.EncodeToString(payload)
			if i == 0 {
				b.ReportMetric(float64(len(token)), "bytes/token")
			}
		}
	})
}

func TestQueryFingerprintStable(t *testing.T) {
	query := Query{
		MailboxRef: "INBOX",
		Sender:     "alice@example.com",
		After:      "2026-01-01",
	}
	fp1, err := queryFingerprint(query)
	if err != nil {
		t.Fatalf("queryFingerprint error = %v", err)
	}
	fp2, err := queryFingerprint(query)
	if err != nil {
		t.Fatalf("queryFingerprint error = %v", err)
	}
	if fp1 != fp2 {
		t.Errorf("fingerprint not stable: %q != %q", fp1, fp2)
	}
}

func TestQueryFingerprintExcludesCursor(t *testing.T) {
	query := Query{
		MailboxRef: "INBOX",
		Sender:     "alice@example.com",
	}
	fp1, err := queryFingerprint(query)
	if err != nil {
		t.Fatalf("queryFingerprint error = %v", err)
	}
	query.Cursor = "scur_somethingdifferent"
	fp2, err := queryFingerprint(query)
	if err != nil {
		t.Fatalf("queryFingerprint error = %v", err)
	}
	if fp1 != fp2 {
		t.Errorf("fingerprint changed when cursor changed: %q != %q (cursor must not affect fingerprint)", fp1, fp2)
	}
}

func TestQueryFingerprintDiffersForDifferentQueries(t *testing.T) {
	query1 := Query{MailboxRef: "INBOX", Sender: "alice@example.com"}
	query2 := Query{MailboxRef: "INBOX", Sender: "bob@example.com"}
	fp1, err := queryFingerprint(query1)
	if err != nil {
		t.Fatalf("queryFingerprint error = %v", err)
	}
	fp2, err := queryFingerprint(query2)
	if err != nil {
		t.Fatalf("queryFingerprint error = %v", err)
	}
	if fp1 == fp2 {
		t.Errorf("different queries produced same fingerprint: %q", fp1)
	}
}

func TestParseQueryTimeRFC3339(t *testing.T) {
	got, err := parseQueryTime("2026-01-15T10:30:00Z")
	if err != nil {
		t.Fatalf("parseQueryTime error = %v", err)
	}
	if got <= 0 {
		t.Errorf("parsed timestamp = %d, want positive", got)
	}
}

func TestParseQueryTimeDateOnly(t *testing.T) {
	got, err := parseQueryTime("2026-01-15")
	if err != nil {
		t.Fatalf("parseQueryTime error = %v", err)
	}
	if got <= 0 {
		t.Errorf("parsed timestamp = %d, want positive", got)
	}
}

func TestParseQueryTimeEmpty(t *testing.T) {
	got, err := parseQueryTime("")
	if err != nil {
		t.Fatalf("parseQueryTime error = %v", err)
	}
	if got != 0 {
		t.Errorf("parsed timestamp = %d, want 0 for empty input", got)
	}
}

func TestParseQueryTimeInvalid(t *testing.T) {
	_, err := parseQueryTime("not-a-date")
	if err == nil {
		t.Fatal("parseQueryTime error = nil, want invalid date error")
	}
}
