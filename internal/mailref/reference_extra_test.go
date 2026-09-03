package mailref

import "testing"

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
