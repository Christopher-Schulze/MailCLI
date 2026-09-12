package mailstore

import (
	"context"
	"strings"
	"testing"
)

func TestMIMEHTMLFailurePreservesOtherParts(t *testing.T) {
	for _, subtype := range []string{"mixed", "alternative"} {
		for _, htmlFirst := range []bool{false, true} {
			plain := "--boundary\r\nContent-Type: text/plain\r\n\r\nReadable plain body\r\n"
			html := "--boundary\r\nContent-Type: text/html\r\n\r\n" + strings.Repeat("<b>x</b>", 100000) + "\r\n"
			parts := plain + html
			if htmlFirst {
				parts = html + plain
			}
			source := "Content-Type: multipart/" + subtype + "; boundary=boundary\r\n\r\n" + parts + "--boundary--\r\n"
			document, err := parseMIMEDocumentWithContext(context.Background(), strings.NewReader(source), false, false, false)
			if err != nil || document.Complete || document.Content != "Readable plain body" || len(document.MissingParts) != 1 || document.MissingParts[0] != "mime:html:tokens" {
				t.Fatalf("subtype=%s htmlFirst=%v content=%q complete=%v missing=%v error=%v", subtype, htmlFirst, document.Content, document.Complete, document.MissingParts, err)
			}
		}
	}
}

func TestMIMEHTMLExpansionConsumesTextBudget(t *testing.T) {
	for _, limit := range []int64{1, 3} {
		limits := defaultMIMEParseBudgetLimits()
		limits.textBytes = limit
		document, err := parseMIMEDocumentWithLimits(context.Background(), strings.NewReader("Content-Type: text/html\r\n\r\n\xff"), false, false, false, limits)
		if err != nil {
			t.Fatal(err)
		}
		if limit == 1 {
			if document.Complete || document.Content != "" || len(document.MissingParts) != 1 || document.MissingParts[0] != "mime:html:render" {
				t.Fatalf("unbounded output: %+v", document)
			}
		} else if !document.Complete || document.Content != "�" || document.budget.textBytes != 3 {
			t.Fatalf("expanded text not charged: %+v, bytes=%d", document, document.budget.textBytes)
		}
	}
}
