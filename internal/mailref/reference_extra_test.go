package mailref

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestListCursorRoundTrip(t *testing.T) {
	t.Parallel()

	token, err := EncodeListCursor(ListCursor{MailboxRef: "mbox-ref", Offset: 7, PreviousID: "4242"})
	if err != nil {
		t.Fatalf("EncodeListCursor() error = %v", err)
	}
	got, err := DecodeListCursor(token)
	if err != nil {
		t.Fatalf("DecodeListCursor() error = %v", err)
	}
	if got.Version != FormatVersion || got.MailboxRef != "mbox-ref" || got.Offset != 7 || got.PreviousID != "4242" {
		t.Fatalf("DecodeListCursor() = %#v", got)
	}
}

func TestDecodeListCursorRejectsInvalid(t *testing.T) {
	t.Parallel()

	valid, err := EncodeListCursor(ListCursor{MailboxRef: "mbox-ref", Offset: 3, PreviousID: "9"})
	if err != nil {
		t.Fatalf("EncodeListCursor() error = %v", err)
	}
	_ = valid
	cases := map[string]string{
		"garbage":        "not-a-token",
		"wrong prefix":   encodeToken("msg_", []byte(`{"version":1}`)),
		"bad base64":     "cur_!!!",
		"bad json":       encodeToken("cur_", []byte(`{broken`)),
		"wrong version":  encodeToken("cur_", []byte(`{"version":99,"mailbox_ref":"m","offset":1,"previous_id":"p"}`)),
		"empty ref":      encodeToken("cur_", []byte(`{"version":1,"mailbox_ref":"","offset":1,"previous_id":"p"}`)),
		"zero offset":    encodeToken("cur_", []byte(`{"version":1,"mailbox_ref":"m","offset":0,"previous_id":"p"}`)),
		"empty previous": encodeToken("cur_", []byte(`{"version":1,"mailbox_ref":"m","offset":1,"previous_id":""}`)),
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeListCursor(token); err == nil {
				t.Fatalf("DecodeListCursor(%q) error = nil", token)
			}
		})
	}
}

func TestDecodersRejectForeignTokens(t *testing.T) {
	t.Parallel()

	accountToken, err := EncodeAccount("a")
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	if _, err := DecodeMailbox(accountToken); err == nil {
		t.Error("DecodeMailbox(account token) error = nil")
	}
	if _, err := DecodeMessage(accountToken); err == nil {
		t.Error("DecodeMessage(account token) error = nil")
	}
	if _, err := DecodeAccount("garbage"); err == nil {
		t.Error("DecodeAccount(garbage) error = nil")
	}
	if _, err := DecodeMailbox("cur_b64!!!"); err == nil {
		t.Error("DecodeMailbox(bad token) error = nil")
	}
}

func TestDecodeAccountClassifiesFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		token string
		kind  AccountReferenceErrorKind
	}{
		{name: "corrupt token", token: "acct_!!!", kind: AccountReferenceCorrupt},
		{
			name:  "unsupported version",
			token: encodeToken("acct_", []byte(`{"version":99,"account_id":"account-id"}`)),
			kind:  AccountReferenceVersionUnsupported,
		},
		{
			name:  "empty account",
			token: encodeToken("acct_", []byte(`{"version":1,"account_id":""}`)),
			kind:  AccountReferenceCorrupt,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := DecodeAccount(test.token)
			var typed *AccountReferenceError
			if !errors.As(err, &typed) || typed.Kind != test.kind {
				t.Fatalf("DecodeAccount() error = %v, typed = %+v, want %s", err, typed, test.kind)
			}
			if typed.ErrorCode() != string(test.kind) {
				t.Fatalf("ErrorCode() = %q, want %q", typed.ErrorCode(), test.kind)
			}
		})
	}
}

func TestEncodeMessageRejectsPartialStoreIdentity(t *testing.T) {
	t.Parallel()

	partial := Message{AccountID: "a", MailboxPath: []string{"Inbox"}, LibraryID: "1", ExpectedStoreMailboxID: 2}
	if _, err := EncodeMessage(partial); err == nil {
		t.Error("EncodeMessage(partial identity) error = nil")
	}
}

func TestIsStoreBound(t *testing.T) {
	t.Parallel()

	if (Message{}).IsStoreBound() {
		t.Error("IsStoreBound() = true for empty message")
	}
	if (!Message{ExpectedStoreUUID: "u", ExpectedStoreMailboxID: 3}.IsStoreBound()) {
		t.Error("IsStoreBound() = false for full store identity")
	}
	if (Message{ExpectedStoreUUID: "u"}).IsStoreBound() {
		t.Error("IsStoreBound() = true for UUID without mailbox ID")
	}
}

func TestLegacyReferenceFixturesDecode(t *testing.T) {
	t.Parallel()
	accountToken := encodeToken("acct_", []byte(`{"version":1,"account_id":"legacy-account"}`))
	account, err := DecodeAccount(accountToken)
	if err != nil || account != (Account{Version: LegacyFormatVersion, AccountID: "legacy-account"}) {
		t.Fatalf("legacy account = %+v, error = %v", account, err)
	}
	mailboxToken := encodeToken("mbx_", []byte(`{"version":1,"account_id":"legacy-account","path":["Parent","Inbox"]}`))
	mailbox, err := DecodeMailbox(mailboxToken)
	if err != nil || !reflect.DeepEqual(mailbox, Mailbox{Version: LegacyFormatVersion, AccountID: "legacy-account", Path: []string{"Parent", "Inbox"}}) {
		t.Fatalf("legacy mailbox = %+v, error = %v", mailbox, err)
	}
	messageToken := encodeToken("msg_", []byte(`{"version":1,"account_id":"legacy-account","mailbox_path":["Parent","Inbox"],"library_id":"42","expected_message_id":"<id@example.com>","expected_imap_uid":7,"expected_imap_uid_validity":9,"expected_imap_mailbox_id":11,"expected_subject":"Betreff","expected_store_uuid":"store","expected_store_mailbox_id":13,"expected_store_message_id":17,"expected_store_global_id":19}`))
	message, err := DecodeMessage(messageToken)
	if err != nil || message.Version != LegacyFormatVersion || message.ExpectedStoreGlobalID != 19 || message.ExpectedSubject != "Betreff" {
		t.Fatalf("legacy message = %+v, error = %v", message, err)
	}
	cursorToken := encodeToken("cur_", []byte(`{"version":1,"mailbox_ref":"mbx_legacy","offset":4,"previous_id":"42"}`))
	cursor, err := DecodeListCursor(cursorToken)
	if err != nil || cursor != (ListCursor{Version: LegacyFormatVersion, MailboxRef: "mbx_legacy", Offset: 4, PreviousID: "42"}) {
		t.Fatalf("legacy cursor = %+v, error = %v", cursor, err)
	}
}

func TestCompactMessagePreservesIdentityAndReducesSize(t *testing.T) {
	t.Parallel()
	ref := Message{
		AccountID: "account-äöü",
		MailboxPath: []string{
			"Projects", "2026", "非常に長いメールボックス名", "Team", "Subteam",
			"Archive", "Reports", "Nested", "Level-9", "Inbox",
		},
		LibraryID: "424242", ExpectedMessageID: "<message-424242@example.com>",
		ExpectedIMAPUID: 77, ExpectedIMAPUIDValidity: 12345, ExpectedIMAPMailboxID: 9,
		ExpectedSubject:   "Projektstatus ✅ 日本語 — März 2026",
		ExpectedStoreUUID: "store-uuid-424242", ExpectedStoreMailboxID: 5,
		ExpectedStoreMessageID: 7, ExpectedStoreGlobalID: 9,
	}
	token, err := EncodeMessage(ref)
	if err != nil {
		t.Fatalf("EncodeMessage() error = %v", err)
	}
	decoded, err := DecodeMessage(token)
	if err != nil {
		t.Fatalf("DecodeMessage() error = %v", err)
	}
	ref.Version = FormatVersion
	if !reflect.DeepEqual(decoded, ref) {
		t.Fatalf("decoded message = %+v, want %+v", decoded, ref)
	}
	legacyPayload, err := json.Marshal(Message{
		Version: LegacyFormatVersion, AccountID: ref.AccountID, MailboxPath: ref.MailboxPath,
		LibraryID: ref.LibraryID, ExpectedMessageID: ref.ExpectedMessageID,
		ExpectedIMAPUID: ref.ExpectedIMAPUID, ExpectedIMAPUIDValidity: ref.ExpectedIMAPUIDValidity,
		ExpectedIMAPMailboxID: ref.ExpectedIMAPMailboxID, ExpectedSubject: ref.ExpectedSubject,
		ExpectedStoreUUID: ref.ExpectedStoreUUID, ExpectedStoreMailboxID: ref.ExpectedStoreMailboxID,
		ExpectedStoreMessageID: ref.ExpectedStoreMessageID, ExpectedStoreGlobalID: ref.ExpectedStoreGlobalID,
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	compactPayload, err := DecodeTokenPayload("msg_", token)
	if err != nil {
		t.Fatalf("DecodeTokenPayload() error = %v", err)
	}
	if len(compactPayload) >= len(legacyPayload) {
		t.Fatalf("compact payload size = %d, legacy JSON size = %d; want reduction", len(compactPayload), len(legacyPayload))
	}
	if estimatedTokenCount(len(compactPayload)) >= estimatedTokenCount(len(legacyPayload)) {
		t.Fatalf("compact token estimate = %d, legacy token estimate = %d; want reduction", estimatedTokenCount(len(compactPayload)), estimatedTokenCount(len(legacyPayload)))
	}
	if len(token) >= len(encodeToken("msg_", legacyPayload)) {
		t.Fatalf("compact token size = %d, legacy token size = %d; want reduction", len(token), len(encodeToken("msg_", legacyPayload)))
	}
}

func estimatedTokenCount(byteCount int) int {
	return (byteCount + 3) / 4
}

func TestCompactReferenceRejectsUnsupportedVersionAndTrailingBytes(t *testing.T) {
	t.Parallel()
	unsupported := encodeToken("acct_", []byte{compactPayloadMarker, FormatVersion + 1, 0})
	_, err := DecodeAccount(unsupported)
	if err == nil {
		t.Fatal("DecodeAccount(unsupported) error = nil")
	}
	var typed *AccountReferenceError
	if !errors.As(err, &typed) || typed.Kind != AccountReferenceVersionUnsupported || typed.Version != FormatVersion+1 {
		t.Fatalf("unsupported account error = %v, typed = %+v", err, typed)
	}
	valid, err := EncodeAccount("account")
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	payload, err := DecodeTokenPayload("acct_", valid)
	if err != nil {
		t.Fatalf("DecodeTokenPayload() error = %v", err)
	}
	trailing := encodeToken("acct_", append(payload, 0))
	if _, err := DecodeAccount(trailing); err == nil {
		t.Fatal("DecodeAccount(trailing) error = nil")
	}
}

func TestCompactEncodingRejectsUnboundedValues(t *testing.T) {
	t.Parallel()
	tooLong := strings.Repeat("x", MaxCompactStringBytes+1)
	if _, err := EncodeAccount(tooLong); err == nil {
		t.Fatal("EncodeAccount(oversized ID) error = nil")
	}
	if _, err := EncodeMailbox("account", []string{tooLong}); err == nil {
		t.Fatal("EncodeMailbox(oversized path component) error = nil")
	}
	path := make([]string, MaxCompactListLength+1)
	if _, err := EncodeMailbox("account", path); err == nil {
		t.Fatal("EncodeMailbox(oversized path) error = nil")
	}
	if _, err := EncodeCompactTokenPayload("acct_", nil, compactReferenceVersion); err == nil {
		t.Fatal("EncodeCompactTokenPayload(nil) error = nil")
	}
}

func FuzzDecodeReferenceTokens(f *testing.F) {
	account, _ := EncodeAccount("seed")
	mailbox, _ := EncodeMailbox("seed", []string{"Inbox"})
	message, _ := EncodeMessage(Message{AccountID: "seed", MailboxPath: []string{"Inbox"}, LibraryID: "1"})
	cursor, _ := EncodeListCursor(ListCursor{MailboxRef: mailbox, Offset: 1, PreviousID: "1"})
	for _, seed := range []string{account, mailbox, message, cursor, "", "acct_!!!", "mbx_", "msg_", "cur_"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, token string) {
		_, _ = DecodeAccount(token)
		_, _ = DecodeMailbox(token)
		_, _ = DecodeMessage(token)
		_, _ = DecodeListCursor(token)
	})
}

func BenchmarkReferenceEncoding(b *testing.B) {
	ref := Message{
		AccountID: "account-id", MailboxPath: []string{"Projects", "2026", "Inbox"}, LibraryID: "424242",
		ExpectedMessageID: "<message@example.com>", ExpectedSubject: "Projektstatus ✅ 日本語",
		ExpectedStoreUUID: "store-uuid", ExpectedStoreMailboxID: 5, ExpectedStoreMessageID: 7,
	}
	b.Run("compact", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			token, err := EncodeMessage(ref)
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
			payload, err := json.Marshal(Message{Version: LegacyFormatVersion, AccountID: ref.AccountID, MailboxPath: ref.MailboxPath, LibraryID: ref.LibraryID, ExpectedMessageID: ref.ExpectedMessageID, ExpectedSubject: ref.ExpectedSubject, ExpectedStoreUUID: ref.ExpectedStoreUUID, ExpectedStoreMailboxID: ref.ExpectedStoreMailboxID, ExpectedStoreMessageID: ref.ExpectedStoreMessageID})
			if err != nil {
				b.Fatal(err)
			}
			token := encodeToken("msg_", payload)
			if i == 0 {
				b.ReportMetric(float64(len(token)), "bytes/token")
			}
		}
	})
}
