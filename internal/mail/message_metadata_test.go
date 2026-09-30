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
		// Malformed values make threading incomplete instead of claiming "no reply".
		{"malformed ID", "In-Reply-To: <broken\r\nReferences: (unclosed <x@y>\r\n\r\n", []string{}, []string{}, Recipient{}, false},
		{"valid IDs kept beside broken ones", "In-Reply-To: <a@b> <nodomain>\r\nReferences: <x@example.com> <broken@example.com\r\n\r\n", []string{"<a@b>"}, []string{"<x@example.com>"}, Recipient{}, false},
		{"stray closing parenthesis", "References: <x@y>) <z@y>\r\n\r\n", []string{}, []string{"<x@y>", "<z@y>"}, Recipient{}, false},
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

func TestThreadingIDsAccountForEveryInputSpan(t *testing.T) {
	for _, test := range []struct {
		name, value string
		ids         []string
		complete    bool
	}{
		{"garbage before", "lost <a@b>", []string{"<a@b>"}, false},
		{"garbage after", "<a@b> lost", []string{"<a@b>"}, false},
		{"garbage between", "<a@b> lost <c@d>", []string{"<a@b>", "<c@d>"}, false},
		{"only garbage", "lost", []string{}, false},
		{"valid invalid valid", "<a@b> <nodomain> <c@d>", []string{"<a@b>", "<c@d>"}, false},
		{"empty mailbox", "<@b> <c@d>", []string{"<c@d>"}, false},
		{"empty domain", "<a@> <c@d>", []string{"<c@d>"}, false},
		{"comma and whitespace", " ,\t<a@b>, <c@d> , ", []string{"<a@b>", "<c@d>"}, true},
		{"adjacent IDs", "<a@b><c@d>", []string{"<a@b>", "<c@d>"}, true},
		{"empty optional", "", []string{}, true},
		{"escaped and nested comments", `<a@b> (ignore \) <fake@id> (nested)) , <c@d>`, []string{"<a@b>", "<c@d>"}, true},
		{"comments only", "(ignore <fake@id>)", []string{}, true},
		{"unclosed comment", "<a@b> (ignored <fake@id>", []string{"<a@b>"}, false},
		{"unfinished comment escape", `<a@b> (ignored\`, []string{"<a@b>"}, false},
		{"unmatched closing comment", "<a@b>) <c@d>", []string{"<a@b>", "<c@d>"}, false},
		{"unfinished bracket", "<a@b> <broken", []string{"<a@b>"}, false},
		{"nested opening resynchronizes", "<broken <a@b>> <c@d>", []string{"<a@b>", "<c@d>"}, false},
		{"multiple nested openings", "<<<a@b>>", []string{"<a@b>"}, false},
		{"stray closing bracket", "> <a@b> > <c@d> >", []string{"<a@b>", "<c@d>"}, false},
		{"comment cannot fabricate an ID", "<a(ignored)@b> <c@d>", []string{"<c@d>"}, false},
		{"closing comment cannot fabricate an ID", "<a)@b> <c@d>", []string{"<c@d>"}, false},
		{"comment cannot repair garbage", "lost(ignored)<a@b>", []string{"<a@b>"}, false},
		{"duplicates preserve source order", "<a@b> <c@d> <a@b>", []string{"<a@b>", "<c@d>", "<a@b>"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ids, complete := scanMessageIDs(test.value)
			if !reflect.DeepEqual(ids, test.ids) || complete != test.complete {
				t.Fatalf("IDs = %#v, complete = %t; want %#v, %t", ids, complete, test.ids, test.complete)
			}
			metadataComplete := test.complete
			// Empty optional parser input is valid; a present threading field
			// requires at least one msg-id under RFC 5322 section 3.6.4.
			if test.name == "empty optional" || test.name == "comments only" {
				metadataComplete = false
			}
			for _, field := range []string{"In-Reply-To", "References"} {
				summary := MessageSummary{Sender: "unchanged", EnrichmentError: "imap_timeout"}
				ApplyThreadingHeaders(&summary, field+": "+test.value+"\r\n\r\n")
				actual := summary.References
				if field == "In-Reply-To" {
					actual = summary.InReplyTo
				}
				if !reflect.DeepEqual(actual, test.ids) || summary.ThreadingComplete != metadataComplete || !summary.ThreadingRequested || summary.Sender != "unchanged" || summary.EnrichmentError != "imap_timeout" {
					t.Fatalf("%s metadata = %+v", field, summary)
				}
			}
		})
	}
}

func TestThreadingFromCompletenessPreservesAvailableEvidence(t *testing.T) {
	for _, test := range []struct {
		name, headers string
		from          Recipient
		complete      bool
	}{
		{"absent optional From", "", Recipient{}, true},
		{"malformed present From", "From: malformed\r\n", Recipient{}, false},
		{"empty present From", "From: \r\n", Recipient{}, false},
		{"partial From list", "From: valid@example.com, malformed\r\n", Recipient{}, false},
		{"multiple senders", "From: First <Local@EXAMPLE.COM>, Other <other@example.com>\r\n", Recipient{Name: "First", Address: "Local@example.com"}, false},
		{"incompatible duplicates", "From: Local@EXAMPLE.COM\r\nFrom: other@example.com\r\n", Recipient{Address: "Local@example.com"}, false},
		{"duplicate local case differs", "From: Local@example.com\r\nFrom: local@example.com\r\n", Recipient{Address: "Local@example.com"}, false},
		{"compatible duplicate domains", "From: Local@EXAMPLE.COM\r\nFrom: Local@example.com\r\n", Recipient{Address: "Local@example.com"}, true},
		{"incompatible duplicate names", "From: First <Local@example.com>\r\nFrom: Second <Local@example.com>\r\n", Recipient{Name: "First", Address: "Local@example.com"}, false},
		{"valid then malformed", "From: Local@example.com\r\nFrom: malformed\r\n", Recipient{Address: "Local@example.com"}, false},
		{"malformed then valid", "From: malformed\r\nFrom: Local@example.com\r\n", Recipient{Address: "Local@example.com"}, false},
		{"quoted singular", "From: Named <\"A B\"@EXAMPLE.COM>\r\n", Recipient{Name: "Named", Address: `"A B"@example.com`}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			summary := MessageSummary{Sender: "unchanged", From: Recipient{Address: "stale@example.com"}}
			ApplyThreadingHeaders(&summary, test.headers+"References: <a@b>\r\n\r\n")
			if summary.From != test.from || summary.ThreadingComplete != test.complete || summary.Sender != "unchanged" || !reflect.DeepEqual(summary.References, []string{"<a@b>"}) {
				t.Fatalf("metadata = %+v; want sender %+v, complete %t", summary, test.from, test.complete)
			}
			payload, err := json.Marshal(summary)
			if err != nil {
				t.Fatal(err)
			}
			var serialized struct {
				From       Recipient `json:"from"`
				Complete   *bool     `json:"threading_complete"`
				References []string  `json:"references"`
			}
			if err := json.Unmarshal(payload, &serialized); err != nil || serialized.Complete == nil || *serialized.Complete != test.complete || serialized.From != test.from || !reflect.DeepEqual(serialized.References, []string{"<a@b>"}) {
				t.Fatalf("serialized metadata = %s, error = %v", payload, err)
			}
		})
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
	// Requested reply metadata and excerpts keep their empty values: empty means unknown.
	payload, err := json.Marshal(MessageSummary{Sender: "unchanged", ThreadingRequested: true, ExcerptRequested: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"in_reply_to":[]`, `"references":[]`, `"from":{"name":"","address":""}`, `"threading_complete":false`, `"excerpt":""`, `"excerpt_complete":false`, `"excerpt_source":"unavailable"`, `"sender":"unchanged"`} {
		if !strings.Contains(string(payload), field) {
			t.Fatalf("missing %s in %s", field, payload)
		}
	}
}

func TestUnrequestedEnrichmentAndEmptyMessageIDStayOutOfTheJSON(t *testing.T) {
	payload, err := json.Marshal(MessageSummary{Ref: "ref", Sender: "s", Subject: "subject"})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"message_id", "in_reply_to", "references", `"from"`, "threading_complete", "excerpt"} {
		if strings.Contains(string(payload), field) {
			t.Fatalf("unrequested %s present in %s", field, payload)
		}
	}
	payload, err = json.Marshal(MessageSummary{MessageID: "<id@example.com>", ThreadingRequested: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"message_id":"`) || !strings.Contains(string(payload), "id@example.com") ||
		!strings.Contains(string(payload), `"in_reply_to":[]`) || strings.Contains(string(payload), "excerpt") {
		t.Fatalf("threading-only summary = %s", payload)
	}
}
