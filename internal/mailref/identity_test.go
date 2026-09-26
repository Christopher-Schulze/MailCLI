package mailref

import (
	"errors"
	"testing"
)

func TestTokenPayloadRequiresCanonicalBase64(t *testing.T) {
	for _, test := range []struct {
		token string
		valid bool
	}{
		{token: "msg_QQ", valid: true}, {token: "msg_QR"}, {token: "msg_QQ\n"},
	} {
		t.Run(test.token, func(t *testing.T) {
			payload, err := DecodeTokenPayload("msg_", test.token)
			if test.valid {
				if err != nil || string(payload) != "A" {
					t.Fatalf("canonical token rejected: %q, %v", payload, err)
				}
				return
			}
			var coded interface{ ErrorCode() string }
			if !errors.As(err, &coded) || coded.ErrorCode() != "invalid_reference" {
				t.Fatalf("noncanonical token accepted or untyped: %q, %v", payload, err)
			}
		})
	}
}

func TestMailboxIdentityNormalizesWithoutComponentCollisions(t *testing.T) {
	for _, test := range []struct {
		name  string
		first []string
		last  []string
		equal bool
	}{
		{name: "NFC", first: []string{"Caf\u00e9"}, last: []string{"Cafe\u0301"}, equal: true},
		{name: "slash", first: []string{"a/b"}, last: []string{"a", "b"}},
		{name: "NUL", first: []string{"a\x00b"}, last: []string{"a", "b"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var identities []MailboxIdentity
			for _, path := range [][]string{test.first, test.last} {
				token, err := EncodeMailbox("account", path)
				if err != nil {
					t.Fatal(err)
				}
				identity, err := MailboxIdentityKey(token)
				if err != nil {
					t.Fatal(err)
				}
				identities = append(identities, identity)
			}
			if (identities[0] == identities[1]) != test.equal {
				t.Fatalf("mailbox identities=%+v", identities)
			}
		})
	}
}
