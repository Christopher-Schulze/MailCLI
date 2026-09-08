package imapclient

import (
	"strings"
	"testing"

	"mailcli/internal/transport"
)

func TestModifiedUTF7RoundTrip(t *testing.T) {
	tests := []struct {
		name string
		text string
		wire string
	}{
		{name: "ascii and ampersand", text: "A&B", wire: "A&-B"},
		{name: "latin", text: "Ä", wire: "&AMQ-"},
		{name: "modified slash replacement", text: "ϰ", wire: "&A,A-"},
		{name: "supplementary plane", text: "😀", wire: "&2D3eAA-"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wire, err := encodeModifiedUTF7(test.text)
			if err != nil {
				t.Fatalf("encodeModifiedUTF7() error = %v", err)
			}
			if wire != test.wire {
				t.Fatalf("encodeModifiedUTF7(%q) = %q, want %q", test.text, wire, test.wire)
			}
			decoded, err := decodeModifiedUTF7(wire)
			if err != nil {
				t.Fatalf("decodeModifiedUTF7() error = %v", err)
			}
			if decoded != test.text {
				t.Fatalf("decodeModifiedUTF7(%q) = %q, want %q", wire, decoded, test.text)
			}
		})
	}
}

func TestModifiedUTF7RejectsMalformedValues(t *testing.T) {
	for _, value := range []string{"&unterminated", "&a-", "&AAB-", "&2D3eAA", "\x80"} {
		if _, err := decodeModifiedUTF7(value); err == nil {
			t.Fatalf("decodeModifiedUTF7(%q) error = nil", value)
		}
	}
}

func TestDecodeMailboxWireNamePreservesFlatNamespace(t *testing.T) {
	displayName, displayPath, err := decodeMailboxWireName(
		"A/B", "", transport.MailboxEncodingModifiedUTF7,
	)
	if err != nil {
		t.Fatalf("decodeMailboxWireName() error = %v", err)
	}
	if displayName != "A/B" || len(displayPath) != 1 || displayPath[0] != "A/B" {
		t.Fatalf("flat mailbox = name:%q path:%q, want one component A/B", displayName, strings.Join(displayPath, ","))
	}
}

func TestDecodeMailboxWireNamePreservesEmptyNamespaceRoot(t *testing.T) {
	displayName, displayPath, err := decodeMailboxWireName(
		"", "/", transport.MailboxEncodingModifiedUTF7,
	)
	if err != nil {
		t.Fatalf("decodeMailboxWireName() error = %v", err)
	}
	if displayName != "" || displayPath != nil {
		t.Fatalf("empty namespace root = name:%q path:%v, want empty metadata", displayName, displayPath)
	}
}

func TestDecodeMailboxWireNameUsesNegotiatedEncodingAndDelimiter(t *testing.T) {
	displayName, displayPath, err := decodeMailboxWireName(
		"Parent.&AMQ-", ".", transport.MailboxEncodingModifiedUTF7,
	)
	if err != nil {
		t.Fatalf("modified UTF-7 mailbox = %v", err)
	}
	if displayName != "Parent.Ä" || strings.Join(displayPath, "/") != "Parent/Ä" {
		t.Fatalf("decoded mailbox = name:%q path:%q", displayName, strings.Join(displayPath, "/"))
	}
	utf8Name, utf8Path, err := decodeMailboxWireName(
		"Entwürfe", "/", transport.MailboxEncodingUTF8,
	)
	if err != nil {
		t.Fatalf("UTF-8 mailbox = %v", err)
	}
	if utf8Name != "Entwürfe" || len(utf8Path) != 1 || utf8Path[0] != "Entwürfe" {
		t.Fatalf("UTF-8 mailbox = name:%q path:%q", utf8Name, strings.Join(utf8Path, "/"))
	}
	customName, customPath, err := decodeMailboxWireName(
		"Root.&A+A-+Child", "+", transport.MailboxEncodingModifiedUTF7,
	)
	if err != nil {
		t.Fatalf("custom delimiter mailbox = %v", err)
	}
	if customName != "Root.Ϡ+Child" || strings.Join(customPath, "/") != "Root.Ϡ/Child" || len(customPath) != 2 {
		t.Fatalf("custom delimiter mailbox = name:%q path:%q", customName, strings.Join(customPath, "/"))
	}
}
