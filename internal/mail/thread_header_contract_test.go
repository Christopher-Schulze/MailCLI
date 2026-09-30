package mail

import (
	"reflect"
	"strings"
	"testing"
)

func TestThreadHeaderParserAndComposerAgree(t *testing.T) {
	for _, test := range []struct {
		name     string
		value    string
		ids      []string
		complete bool
	}{
		{name: "comments", value: "(before) <a@b> (nested (comment)) <c@d>", ids: []string{"<a@b>", "<c@d>"}, complete: true},
		{name: "adjacent", value: "<a@b><c@d>", ids: []string{"<a@b>", "<c@d>"}, complete: true},
		{name: "legacy comma separators", value: "<a@b>, <c@d>", ids: []string{"<a@b>", "<c@d>"}, complete: true},
		{name: "domain literal", value: "<a@[IPv6:::1]>", ids: []string{"<a@[IPv6:::1]>"}, complete: true},
		{name: "literal dtext", value: "<a@[(server@id)<tag>]>", ids: []string{"<a@[(server@id)<tag>]>"}, complete: true},
		{name: "internationalized ID", value: "<ä@例え.test>", ids: []string{"<ä@例え.test>"}, complete: true},
		{name: "multiple at", value: "<a@@b> <good@id>", ids: []string{"<good@id>"}},
		{name: "leading dot", value: "<.a@b> <good@id>", ids: []string{"<good@id>"}},
		{name: "trailing dot", value: "<a.@b> <good@id>", ids: []string{"<good@id>"}},
		{name: "double dot", value: "<a..b@domain> <good@id>", ids: []string{"<good@id>"}},
		{name: "bad domain atom", value: "<a@b..c> <good@id>", ids: []string{"<good@id>"}},
		{name: "bad atom character", value: "<a:b@c> <good@id>", ids: []string{"<good@id>"}},
		{name: "invalid utf8", value: "<a\xff@b> <good@id>", ids: []string{"<good@id>"}},
		{name: "garbage", value: "<a@b> lost <c@d>", ids: []string{"<a@b>", "<c@d>"}},
		{name: "comment inside ID", value: "<a(comment)@b> <good@id>", ids: []string{"<good@id>"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ids, complete := scanMessageIDs(test.value)
			if !reflect.DeepEqual(ids, test.ids) || complete != test.complete {
				t.Fatalf("parsed=%v complete=%t; want %v %t", ids, complete, test.ids, test.complete)
			}
			chain, err := canonicalThreadReferences(test.value, "<parent@id>")
			if test.complete {
				want := strings.Join(append(append([]string(nil), test.ids...), "<parent@id>"), " ")
				if err != nil || chain != want {
					t.Fatalf("composer chain=%q error=%v; want %q", chain, err, want)
				}
			} else if errorCode(err) != "invalid_message_source" {
				t.Fatalf("composer accepted incomplete source: chain=%q error=%v", chain, err)
			}
		})
	}
}

func TestThreadHeaderPresentEmptyValuesAreIncomplete(t *testing.T) {
	for _, name := range []string{"References", "In-Reply-To"} {
		for _, value := range []string{"", "(comment only)"} {
			var summary MessageSummary
			ApplyThreadingHeaders(&summary, name+": "+value+"\r\n\r\n")
			if summary.ThreadingComplete {
				t.Fatalf("present %s without an ID became complete", name)
			}
		}
	}
	var summary MessageSummary
	ApplyThreadingHeaders(&summary, "Subject: no threading fields\r\n\r\n")
	if !summary.ThreadingComplete {
		t.Fatal("absent optional fields became malformed")
	}
	if _, err := canonicalThreadReferences("(comment only)", "<parent@id>"); errorCode(err) != "invalid_message_source" {
		t.Fatalf("present References without an ID accepted: %v", err)
	}
}

func TestThreadHeaderParentValidation(t *testing.T) {
	for _, parent := range []string{"<a@@b>", "<a..b@c>", "<a@b> <c@d>", "<a(comment)@b>", "<a\xff@b>"} {
		if _, err := canonicalThreadReferences("<good@id>", parent); errorCode(err) != "invalid_message_source" {
			t.Fatalf("invalid parent accepted: %q error=%v", parent, err)
		}
	}
	parent := "<a@[(server@id)<tag>]>"
	if chain, err := canonicalThreadReferences("<good@id>", parent); err != nil || chain != "<good@id> "+parent {
		t.Fatalf("valid literal parent chain=%q error=%v", chain, err)
	}
}

func TestThreadHeaderFoldingAndInjection(t *testing.T) {
	raw := "References: (before) <a@b>\r\n\t(nested (comment)) <c@d>\r\n\r\n"
	fields, complete := ParseHeaderFields(raw)
	if !complete || len(fields) != 1 {
		t.Fatalf("folded header fields=%+v complete=%t", fields, complete)
	}
	chain, err := canonicalThreadReferences(fields[0].Value, "<parent@id>")
	if err != nil || chain != "<a@b> <c@d> <parent@id>" {
		t.Fatalf("unfolded chain=%q error=%v", chain, err)
	}
	for _, value := range []string{"<a@b>\r\nBcc: injected@example.com", "<a@b>\n<c@d>"} {
		if _, err := canonicalThreadReferences(value, "<parent@id>"); errorCode(err) != "invalid_message_source" {
			t.Fatalf("raw control characters accepted: %v", err)
		}
	}
}
