//go:build darwin && cgo

package keychain

import (
	"errors"
	"testing"
)

func TestCFStringAndCFDataWithoutKeychain(t *testing.T) {
	t.Parallel()

	cf, err := cfString("mailcli-test")
	if err != nil {
		t.Fatalf("cfString() error = %v", err)
	}
	if cf == 0 {
		t.Fatal("cfString() = 0")
	}
	releaseCFString(cf)

	data, err := cfData("secret")
	if err != nil {
		t.Fatalf("cfData() error = %v", err)
	}
	if data == 0 {
		t.Fatal("cfData() = 0")
	}
	releaseCFData(data)
}

func TestCFStringAcceptsUnicodeIdentifier(t *testing.T) {
	t.Parallel()

	cf, err := cfString("müller@example.com")
	if err != nil {
		t.Fatalf("cfString() error = %v", err)
	}
	if cf == 0 {
		t.Fatal("cfString() = 0")
	}
	releaseCFString(cf)
}

func TestCFStringRejectsNULIdentifiers(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "beginning", value: "\x00alice@example.com"},
		{name: "middle", value: "alice\x00@example.com"},
		{name: "end", value: "alice@example.com\x00"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cf, err := cfString(test.value)
			if cf != 0 {
				releaseCFString(cf)
				t.Fatalf("cfString() returned a CoreFoundation string for an embedded NUL")
			}
			var typed *KeychainError
			if !errors.As(err, &typed) || typed.Code != CodeInvalidIdentifier {
				t.Fatalf("cfString() error = %v, want %s", err, CodeInvalidIdentifier)
			}
			if typed.Message != "keychain identifier contains an embedded NUL byte" {
				t.Fatalf("cfString() message = %q, must not expose identifier metadata", typed.Message)
			}
		})
	}
}
