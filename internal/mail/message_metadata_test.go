package mail

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReplyMetadataHeaders(t *testing.T) {
	for _, test := range []struct {
		name, raw   string
		reply, refs []string
		sender      Recipient
		complete    bool
	}{
		{"folded and comments", "From: =?UTF-8?Q?J=C3=B6rg?= <Local@EXAMPLE.COM>\r\nIn-Reply-To: <a@b>, (ignore <bad@id>) <c@d>\r\nReferences: <first@x>\r\n\t(second (nested)) <next@x>, <last@x>\r\n\r\n", []string{"<a@b>", "<c@d>"}, []string{"<first@x>", "<next@x>", "<last@x>"}, Recipient{Name: "Jörg", Address: "Local@example.com"}, true},
		{"missing", "From: person@example.com\n\n", []string{}, []string{}, Recipient{Address: "person@example.com"}, true},
		{"malformed ID", "In-Reply-To: <broken\r\nReferences: (unclosed <x@y>\r\n\r\n", []string{}, []string{}, Recipient{}, true},
		{"header boundary missing", "From: a@b\r\n", []string{}, []string{}, Recipient{Address: "a@b"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var summary MessageSummary
			ApplyThreadingHeaders(&summary, test.raw)
			if !reflect.DeepEqual(summary.InReplyTo, test.reply) || !reflect.DeepEqual(summary.References, test.refs) || summary.From != test.sender || summary.ThreadingComplete != test.complete {
				t.Fatalf("metadata = %+v", summary)
			}
		})
	}
}

func TestOrderedHeaderFields(t *testing.T) {
	fields, complete := ParseHeaderFields("x-MiXeD: one\r\n two\r\nx-MiXeD: second\r\nSubject: third\r\n\r\nbody")
	want := []HeaderField{{"x-MiXeD", "one two"}, {"x-MiXeD", "second"}, {"Subject", "third"}}
	if !complete || !reflect.DeepEqual(fields, want) {
		t.Fatalf("fields = %+v, complete=%t", fields, complete)
	}
}

func TestBoundedExcerptText(t *testing.T) {
	for _, test := range []struct {
		name, input string
		length      int
		want        string
	}{
		{"quotes and signature", "Hello\r\n> old reply\r\n  > more\r\nworld\r\n-- \r\nSignature", 240, "Hello world"},
		{"Unicode boundary", "Ä🙂界x", 3, "Ä🙂界"},
		{"collapse", " a\t\nb  c ", 240, "a b c"},
		{"not a signature", "a\n--\nb", 240, "a -- b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := BuildExcerpt(test.input, test.length)
			if got != test.want || utf8.RuneCountInString(got) > test.length || !utf8.ValidString(got) {
				t.Fatalf("excerpt = %q", got)
			}
		})
	}
}

func TestEmptySummaryMetadataStableJSON(t *testing.T) {
	payload, err := json.Marshal(MessageSummary{Sender: "unchanged"})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"in_reply_to":[]`, `"references":[]`, `"from":{"name":"","address":""}`, `"threading_complete":false`, `"excerpt_source":"unavailable"`, `"sender":"unchanged"`} {
		if !strings.Contains(string(payload), field) {
			t.Fatalf("missing %s in %s", field, payload)
		}
	}
}
