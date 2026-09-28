package mailref

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

const (
	testAccountUUID = "DD23A835-CDBE-49A1-84AA-32589740D66C"
	testStoreUUID   = "38BF1CB5-482E-4470-9418-2C3F146C62F1"
)

func storeBoundTestMessage() Message {
	return Message{
		AccountID: testAccountUUID, MailboxPath: []string{"INBOX"}, LibraryID: "75568",
		ExpectedIMAPUID: 10069, ExpectedIMAPMailboxID: 1, ExpectedSubject: "Neue Job-Chance für dich",
		ExpectedStoreUUID: testStoreUUID, ExpectedStoreMailboxID: 1,
		ExpectedStoreMessageID: -5363372559753559471, ExpectedStoreGlobalID: 66868,
	}
}

func binaryPayloadOf(t *testing.T, prefix string, token string) []byte {
	t.Helper()
	payload, err := DecodeTokenPayload(prefix, token)
	if err != nil {
		t.Fatalf("DecodeTokenPayload(%s) error = %v", prefix, err)
	}
	if !isBinaryPayload(payload) {
		t.Fatalf("%s token is not a version 3 binary payload: % x", prefix, payload)
	}
	return payload
}

func TestEncodersEmitBinaryTokensWithinTheLengthBudget(t *testing.T) {
	t.Parallel()
	message, err := EncodeMessage(storeBoundTestMessage())
	if err != nil {
		t.Fatalf("EncodeMessage() error = %v", err)
	}
	mailbox, err := EncodeMailbox(testAccountUUID, []string{"INBOX"})
	if err != nil {
		t.Fatalf("EncodeMailbox() error = %v", err)
	}
	account, err := EncodeAccount(testAccountUUID)
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	for prefix, token := range map[string]string{"msg_": message, "mbx_": mailbox, "acct_": account} {
		binaryPayloadOf(t, prefix, token)
	}
	if len(message) > 92 || len(mailbox) > 40 || len(account) > 30 {
		t.Fatalf("token lengths msg=%d mbx=%d acct=%d; want <=92, <=40, <=30", len(message), len(mailbox), len(account))
	}
	if strings.Contains(message, "Neue") || bytes.Contains(binaryPayloadOf(t, "msg_", message), []byte("Neue Job")) {
		t.Fatal("message ref still carries the subject in clear text")
	}
}

func TestBinaryMessageRoundTrips(t *testing.T) {
	t.Parallel()
	subjectHash := SubjectHash("Projektstatus ✅ 日本語 — März 2026")
	tests := []struct {
		name string
		ref  Message
	}{
		{name: "store bound with a wide message id", ref: storeBoundTestMessage()},
		{name: "store bound with a small message id", ref: Message{
			AccountID: testAccountUUID, MailboxPath: []string{"Parent", "Inbox"}, LibraryID: "9",
			ExpectedStoreUUID: testStoreUUID, ExpectedStoreMailboxID: 5, ExpectedStoreMessageID: 7,
		}},
		{name: "non UUID account, textual library id and message id", ref: Message{
			AccountID: "account-äöü", MailboxPath: []string{"Projects", "非常に長いメールボックス名"},
			LibraryID: "lib-42", ExpectedMessageID: "<message@example.com>",
			ExpectedIMAPUID: 77, ExpectedIMAPUIDValidity: 12345, ExpectedIMAPMailboxID: 9,
			ExpectedSubject: "Projektstatus ✅ 日本語 — März 2026",
		}},
		{name: "lowercase UUID stays a string", ref: Message{
			AccountID: strings.ToLower(testAccountUUID), MailboxPath: []string{"INBOX"}, LibraryID: "1",
			ExpectedStoreUUID: strings.ToLower(testStoreUUID), ExpectedStoreMailboxID: 2,
			ExpectedStoreMessageID: 3, ExpectedStoreGlobalID: -4,
		}},
		{name: "leading zero library id stays a string", ref: Message{
			AccountID: testAccountUUID, MailboxPath: []string{"INBOX"}, LibraryID: "007",
		}},
		{name: "hash only", ref: Message{
			AccountID: testAccountUUID, MailboxPath: []string{"INBOX"}, LibraryID: "5", ExpectedSubjectHash: subjectHash,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			token, err := EncodeMessage(test.ref)
			if err != nil {
				t.Fatalf("EncodeMessage() error = %v", err)
			}
			binaryPayloadOf(t, "msg_", token)
			got, err := DecodeMessage(token)
			if err != nil {
				t.Fatalf("DecodeMessage() error = %v", err)
			}
			want := test.ref
			want.Version = BinaryFormatVersion
			if want.ExpectedSubject != "" {
				want.ExpectedSubjectHash = SubjectHash(want.ExpectedSubject)
				want.ExpectedSubject = ""
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("decoded message = %+v, want %+v", got, want)
			}
			again, err := EncodeMessage(got)
			if err != nil || again != token {
				t.Fatalf("re-encoded token = %q, %v; want the original %q", again, err, token)
			}
		})
	}
}

func TestBinaryMailboxAndAccountRoundTrip(t *testing.T) {
	t.Parallel()
	for _, accountID := range []string{testAccountUUID, "plain-account", strings.ToLower(testAccountUUID)} {
		account, err := EncodeAccount(accountID)
		if err != nil {
			t.Fatalf("EncodeAccount(%q) error = %v", accountID, err)
		}
		binaryPayloadOf(t, "acct_", account)
		decodedAccount, err := DecodeAccount(account)
		if err != nil || decodedAccount != (Account{Version: BinaryFormatVersion, AccountID: accountID}) {
			t.Fatalf("DecodeAccount(%q) = %+v, %v", accountID, decodedAccount, err)
		}
		path := []string{"Parent", "Ünïcode ✅", "Inbox"}
		mailbox, err := EncodeMailbox(accountID, path)
		if err != nil {
			t.Fatalf("EncodeMailbox(%q) error = %v", accountID, err)
		}
		binaryPayloadOf(t, "mbx_", mailbox)
		decodedMailbox, err := DecodeMailbox(mailbox)
		if err != nil || !reflect.DeepEqual(decodedMailbox, Mailbox{Version: BinaryFormatVersion, AccountID: accountID, Path: path}) {
			t.Fatalf("DecodeMailbox(%q) = %+v, %v", accountID, decodedMailbox, err)
		}
	}
}

func TestVersionTwoRefsStillDecode(t *testing.T) {
	t.Parallel()
	payload := &compactPayload{
		AccountID: testAccountUUID, MailboxPath: []string{"INBOX"}, LibraryID: "75568", ExpectedIMAPUID: 10069,
		ExpectedSubject: "Betreff", ExpectedStoreUUID: testStoreUUID, ExpectedStoreMailboxID: 1,
		ExpectedStoreMessageID: 12, ExpectedStoreGlobalID: 66868,
	}
	token, err := EncodeCompactTokenPayload("msg_", payload, byte(FormatVersion))
	if err != nil {
		t.Fatal(err)
	}
	message, err := DecodeMessage(token)
	if err != nil || message.Version != FormatVersion || message.ExpectedSubject != "Betreff" ||
		message.ExpectedStoreGlobalID != 66868 || !message.SubjectMatches("Betreff") || message.SubjectMatches("Anders") {
		t.Fatalf("version 2 message = %+v, %v", message, err)
	}
	mailboxToken, err := EncodeCompactTokenPayload("mbx_", &compactPayload{AccountID: testAccountUUID, MailboxPath: []string{"INBOX"}}, byte(FormatVersion))
	if err != nil {
		t.Fatal(err)
	}
	if mailbox, err := DecodeMailbox(mailboxToken); err != nil || mailbox.AccountID != testAccountUUID || mailbox.Path[0] != "INBOX" {
		t.Fatalf("version 2 mailbox = %+v, %v", mailbox, err)
	}
	accountToken, err := EncodeCompactTokenPayload("acct_", &compactPayload{AccountID: testAccountUUID}, byte(FormatVersion))
	if err != nil {
		t.Fatal(err)
	}
	if account, err := DecodeAccount(accountToken); err != nil || account.AccountID != testAccountUUID {
		t.Fatalf("version 2 account = %+v, %v", account, err)
	}
}

func TestMessageIdentityKeyIsEqualAcrossVersions(t *testing.T) {
	t.Parallel()
	ref := storeBoundTestMessage()
	binaryToken, err := EncodeMessage(ref)
	if err != nil {
		t.Fatal(err)
	}
	jsonToken, err := EncodeCompactTokenPayload("msg_", &compactPayload{
		AccountID: ref.AccountID, MailboxPath: ref.MailboxPath, LibraryID: ref.LibraryID,
		ExpectedIMAPUID: ref.ExpectedIMAPUID, ExpectedIMAPMailboxID: ref.ExpectedIMAPMailboxID,
		ExpectedSubject: ref.ExpectedSubject, ExpectedStoreUUID: ref.ExpectedStoreUUID,
		ExpectedStoreMailboxID: ref.ExpectedStoreMailboxID, ExpectedStoreMessageID: ref.ExpectedStoreMessageID,
		ExpectedStoreGlobalID: ref.ExpectedStoreGlobalID,
	}, byte(FormatVersion))
	if err != nil {
		t.Fatal(err)
	}
	fromBinary, err := MessageIdentityKey(binaryToken)
	if err != nil {
		t.Fatal(err)
	}
	fromJSON, err := MessageIdentityKey(jsonToken)
	if err != nil || fromBinary != fromJSON {
		t.Fatalf("identity keys differ: binary=%+v json=%+v err=%v", fromBinary, fromJSON, err)
	}
}

func TestSubjectHashComparison(t *testing.T) {
	t.Parallel()
	token, err := EncodeMessage(storeBoundTestMessage())
	if err != nil {
		t.Fatal(err)
	}
	message, err := DecodeMessage(token)
	if err != nil {
		t.Fatal(err)
	}
	if !message.HasSubjectCheck() || !message.SubjectMatches("Neue Job-Chance für dich") || message.SubjectMatches("Neue Job-Chance für euch") {
		t.Fatalf("subject hash check failed for %+v", message)
	}
	noSubject := storeBoundTestMessage()
	noSubject.ExpectedSubject = ""
	token, err = EncodeMessage(noSubject)
	if err != nil {
		t.Fatal(err)
	}
	if message, err = DecodeMessage(token); err != nil || message.HasSubjectCheck() {
		t.Fatalf("message without a subject carries a check: %+v, %v", message, err)
	}
	if SubjectHash("") == 0 || SubjectHash("a") == 0 {
		t.Fatal("SubjectHash must never return the absent marker 0")
	}
}

func TestBinaryPayloadRejectsEveryTruncation(t *testing.T) {
	t.Parallel()
	valid := map[string]string{}
	var err error
	if valid["msg_"], err = EncodeMessage(storeBoundTestMessage()); err != nil {
		t.Fatal(err)
	}
	if valid["mbx_"], err = EncodeMailbox(testAccountUUID, []string{"Parent", "Inbox"}); err != nil {
		t.Fatal(err)
	}
	if valid["acct_"], err = EncodeAccount(testAccountUUID); err != nil {
		t.Fatal(err)
	}
	for prefix, token := range valid {
		payload := binaryPayloadOf(t, prefix, token)
		for length := 1; length < len(payload); length++ {
			if err := decodeByPrefix(prefix, encodeToken(prefix, payload[:length])); err == nil {
				t.Errorf("%s payload truncated to %d of %d bytes decoded without an error", prefix, length, len(payload))
			}
		}
		trailing := append(append([]byte{}, payload...), 0)
		if err := decodeByPrefix(prefix, encodeToken(prefix, trailing)); err == nil {
			t.Errorf("%s payload with a trailing byte decoded without an error", prefix)
		}
	}
}

func TestBinaryPayloadRejectsCorruptForms(t *testing.T) {
	t.Parallel()
	uuidBytes := bytes.Repeat([]byte{0xAB}, 16)
	wideSmall := append([]byte{binaryPayloadMarker, 0x01 | 0x02 | 0x04 | 0x40}, uuidBytes...)
	wideSmall = append(wideSmall, 1, 5, 'I', 'N', 'B', 'O', 'X', 1)
	wideSmall = append(wideSmall, uuidBytes...)
	wideSmall = append(wideSmall, 2, 0, 0, 0, 0, 0, 0, 0, 5, 0)
	tests := []struct {
		prefix  string
		payload []byte
	}{
		{"acct_", []byte{binaryPayloadMarker}},
		{"acct_", []byte{binaryPayloadMarker, 0x80}},
		{"acct_", append([]byte{binaryPayloadMarker, 0x01}, uuidBytes[:15]...)},
		{"acct_", []byte{binaryPayloadMarker, 0x00, 0x00}},
		{"acct_", []byte{binaryPayloadMarker, 0x00, 0x02, 0xff, 0xfe}},
		{"acct_", []byte{binaryPayloadMarker, 0x00, 0xff, 0xff, 0xff, 0xff, 0x0f, 'x'}},
		{"acct_", []byte{binaryPayloadMarker, 0x80, 0x00, 0x01, 'a'}},
		{"acct_", []byte{binaryPayloadMarker, 0x02, 0x01, 'a'}},
		{"mbx_", []byte{binaryPayloadMarker, 0x00, 0x01, 'a', 0x00}},
		{"mbx_", []byte{binaryPayloadMarker, 0x00, 0x01, 'a', 0xff, 0xff, 0x7f}},
		{"msg_", wideSmall},
	}
	for _, test := range tests {
		if err := decodeByPrefix(test.prefix, encodeToken(test.prefix, test.payload)); err == nil {
			t.Errorf("%s payload % x decoded without an error", test.prefix, test.payload)
		}
	}
}

func TestBinaryMessageRejectsIncompleteIdentities(t *testing.T) {
	t.Parallel()
	if _, err := EncodeMessage(Message{
		AccountID: testAccountUUID, MailboxPath: []string{"INBOX"}, LibraryID: "1", ExpectedStoreMailboxID: 2,
	}); err == nil {
		t.Fatal("EncodeMessage() accepted a store mailbox without a store UUID")
	}
	if _, err := EncodeMessage(Message{
		AccountID: testAccountUUID, MailboxPath: []string{"INBOX"}, LibraryID: "1", ExpectedIMAPUIDValidity: 3,
	}); err == nil {
		t.Fatal("EncodeMessage() accepted UIDVALIDITY without a UID")
	}
}

func TestBinaryEncodingKeepsInvalidUTF8Decodable(t *testing.T) {
	t.Parallel()
	ref := Message{AccountID: testAccountUUID, MailboxPath: []string{"IN\xffBOX"}, LibraryID: "1", ExpectedMessageID: "<a\xfe@x>"}
	token, err := EncodeMessage(ref)
	if err != nil {
		t.Fatalf("EncodeMessage() error = %v", err)
	}
	got, err := DecodeMessage(token)
	if err != nil || got.MailboxPath[0] != "IN�BOX" || got.ExpectedMessageID != "<a�@x>" {
		t.Fatalf("decoded message = %+v, %v", got, err)
	}
	if subject := "Sub\xffject"; SubjectHash(subject) == SubjectHash("Sub�ject") {
		t.Fatal("SubjectHash must hash the raw subject bytes")
	}
}

func decodeByPrefix(prefix string, token string) error {
	var err error
	switch prefix {
	case "acct_":
		_, err = DecodeAccount(token)
	case "mbx_":
		_, err = DecodeMailbox(token)
	default:
		_, err = DecodeMessage(token)
	}
	return err
}
