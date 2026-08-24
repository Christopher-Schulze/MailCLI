package mailstore

import (
	"reflect"
	"testing"
)

var benchmarkMailboxLocation mailboxLocation

func TestParseMailboxURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		value       string
		wantRaw     []string
		wantVisible []string
	}{
		{
			name:    "nested gmail path",
			value:   "imap://951FB9AB-537B-4E97-8DCC-B241B71AD9DD/%5BGmail%5D/Alle%20Nachrichten",
			wantRaw: []string{"[Gmail]", "Alle Nachrichten"}, wantVisible: []string{"Alle Nachrichten"},
		},
		{
			name:    "decomposed unicode",
			value:   "imap://951FB9AB-537B-4E97-8DCC-B241B71AD9DD/%5BGmail%5D/Entwu%CC%88rfe",
			wantRaw: []string{"[Gmail]", "Entwürfe"}, wantVisible: []string{"Entwürfe"},
		},
		{
			name:    "single path",
			value:   "local://74D628F1-FF28-4691-AF4F-3679DFB2A397/Outbox",
			wantRaw: []string{"Outbox"}, wantVisible: []string{"Outbox"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseMailboxURL(test.value)
			if err != nil {
				t.Fatalf("parseMailboxURL() error = %v", err)
			}
			if !reflect.DeepEqual(got.RawPath, test.wantRaw) || !reflect.DeepEqual(got.VisiblePath, test.wantVisible) {
				t.Fatalf("parseMailboxURL() = %#v", got)
			}
		})
	}
}

func TestParseMailboxURLRejectsEscapes(t *testing.T) {
	t.Parallel()
	tests := []string{
		"smtp://951FB9AB-537B-4E97-8DCC-B241B71AD9DD/Inbox",
		"imap://not-a-uuid/Inbox",
		"imap://951FB9AB-537B-4E97-8DCC-B241B71AD9DD",
		"imap://user@951FB9AB-537B-4E97-8DCC-B241B71AD9DD/Inbox",
		"imap://951FB9AB-537B-4E97-8DCC-B241B71AD9DD/Inbox?query=true",
		"imap://951FB9AB-537B-4E97-8DCC-B241B71AD9DD/Inbox#fragment",
		"imap://951FB9AB-537B-4E97-8DCC-B241B71AD9DD/Inbox%0AChild",
		"imap://951FB9AB-537B-4E97-8DCC-B241B71AD9DD/..",
		"imap://951FB9AB-537B-4E97-8DCC-B241B71AD9DD/Inbox%2FChild",
		"imap://951FB9AB-537B-4E97-8DCC-B241B71AD9DD/Inbox%00",
	}
	for _, value := range tests {
		value := value
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			if _, err := parseMailboxURL(value); err == nil {
				t.Fatal("parseMailboxURL() error = nil")
			}
		})
	}
}

func BenchmarkParseMailboxURL(b *testing.B) {
	const value = "imap://951FB9AB-537B-4E97-8DCC-B241B71AD9DD/%5BGmail%5D/Alle%20Nachrichten"
	b.ReportAllocs()
	b.SetBytes(int64(len(value)))
	for b.Loop() {
		location, err := parseMailboxURL(value)
		if err != nil {
			b.Fatal(err)
		}
		benchmarkMailboxLocation = location
	}
}
