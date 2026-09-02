package mail

import (
	"testing"
)

func TestEncodeSearchCursorRoundtrip(t *testing.T) {
	fingerprint := "abc123query"
	storeUUID := "store-uuid-xyz"
	receivedAt := int64(1700000000)
	rowID := int64(42)
	encoded, err := EncodeSearchCursor(fingerprint, storeUUID, receivedAt, rowID)
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
	encoded, err := EncodeSearchCursor("correct-fp", "store-uuid", 1700000000, 1)
	if err != nil {
		t.Fatalf("EncodeSearchCursor error = %v", err)
	}
	_, err = DecodeSearchCursor(encoded, "wrong-fp")
	if err == nil {
		t.Fatal("DecodeSearchCursor error = nil, want fingerprint mismatch error")
	}
}

func TestDecodeSearchCursorRejectsRowIDZero(t *testing.T) {
	encoded, err := EncodeSearchCursor("fp", "store-uuid", 1700000000, 0)
	if err != nil {
		t.Fatalf("EncodeSearchCursor error = %v", err)
	}
	_, err = DecodeSearchCursor(encoded, "fp")
	if err == nil {
		t.Fatal("DecodeSearchCursor error = nil, want row ID zero rejection")
	}
}

func TestDecodeSearchCursorRejectsEmptyStoreUUID(t *testing.T) {
	encoded, err := EncodeSearchCursor("fp", "", 1700000000, 1)
	if err != nil {
		t.Fatalf("EncodeSearchCursor error = %v", err)
	}
	_, err = DecodeSearchCursor(encoded, "fp")
	if err == nil {
		t.Fatal("DecodeSearchCursor error = nil, want empty store UUID rejection")
	}
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
