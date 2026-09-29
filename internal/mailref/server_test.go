package mailref

import (
	"reflect"
	"strings"
	"testing"
)

func TestServerRefRoundTrips(t *testing.T) {
	t.Parallel()
	for _, accountID := range []string{testAccountUUID, "plain-account", strings.ToLower(testAccountUUID)} {
		ref := Server{AccountID: accountID, MailboxPath: []string{"Parent", "Ünï ✅"}, UIDValidity: 1650000001, UID: 4294967295}
		token, err := EncodeServer(ref)
		if err != nil {
			t.Fatalf("EncodeServer(%q) error = %v", accountID, err)
		}
		if !strings.HasPrefix(token, "srv_") {
			t.Fatalf("token = %q", token)
		}
		got, err := DecodeServer(token)
		want := ref
		want.Version = BinaryFormatVersion
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("DecodeServer(%q) = %+v, %v; want %+v", token, got, err, want)
		}
	}
}

func TestServerRefIsShort(t *testing.T) {
	t.Parallel()
	token, err := EncodeServer(Server{AccountID: testAccountUUID, MailboxPath: []string{"INBOX"}, UIDValidity: 1650000001, UID: 10069})
	if err != nil || len(token) > 60 {
		t.Fatalf("token = %q (%d), %v; want at most 60 characters", token, len(token), err)
	}
}

func TestServerRefRejectsEmptyFields(t *testing.T) {
	t.Parallel()
	valid := Server{AccountID: testAccountUUID, MailboxPath: []string{"INBOX"}, UIDValidity: 1, UID: 1}
	for name, mutate := range map[string]func(*Server){
		"account":     func(ref *Server) { ref.AccountID = "" },
		"path":        func(ref *Server) { ref.MailboxPath = nil },
		"uidvalidity": func(ref *Server) { ref.UIDValidity = 0 },
		"uid":         func(ref *Server) { ref.UID = 0 },
	} {
		ref := valid
		mutate(&ref)
		if _, err := EncodeServer(ref); err == nil {
			t.Errorf("EncodeServer accepted an empty %s", name)
		}
	}
}

func TestServerRefRejectsCorruptForms(t *testing.T) {
	t.Parallel()
	token, err := EncodeServer(Server{AccountID: testAccountUUID, MailboxPath: []string{"INBOX"}, UIDValidity: 9, UID: 7})
	if err != nil {
		t.Fatal(err)
	}
	payload := binaryPayloadOf(t, "srv_", token)
	for length := 1; length < len(payload); length++ {
		if _, err := DecodeServer(encodeToken("srv_", payload[:length])); err == nil {
			t.Errorf("payload truncated to %d of %d bytes decoded", length, len(payload))
		}
	}
	if _, err := DecodeServer(encodeToken("srv_", append(append([]byte{}, payload...), 0))); err == nil {
		t.Error("payload with a trailing byte decoded")
	}
	zeroUID := append([]byte{}, payload[:len(payload)-1]...)
	zeroUID = append(zeroUID, 0)
	if _, err := DecodeServer(encodeToken("srv_", zeroUID)); err == nil {
		t.Error("payload with UID 0 decoded")
	}
	if _, err := DecodeServer(strings.Replace(token, "srv_", "msg_", 1)); err == nil {
		t.Error("a msg_ prefix decoded as a server ref")
	}
	if _, err := DecodeMessage(strings.Replace(token, "srv_", "msg_", 1)); err == nil {
		t.Error("a server payload decoded as a message ref")
	}
	if _, err := DecodeServer("msg_" + strings.TrimPrefix(token, "srv_")); err == nil {
		t.Error("wrong prefix decoded")
	}
}
