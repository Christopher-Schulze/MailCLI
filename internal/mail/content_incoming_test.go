package mail

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestIncomingHTMLPreservesRendering(t *testing.T) {
	for _, source := range []string{
		"", "Hello &amp; goodbye", "\xff\x00 text",
		`<p>A<b><i>B</p>C</b>D</i>`,
		`<table><caption>Summary</caption>outside<tr><td>A|B</td><td>C</table>`,
		`<a href="https://outer.example"><table><a href="https://inner.example">Inner</a><tr><td>Cell</td></tr></table>Outer</a>`,
		`<svg><desc><p>Foreign text</p></desc></svg><img alt="Logo"><script>hidden</script>`,
		"<pre>\r\n  code\n</pre><ul><li>One<ul><li>Two</li></ul></li></ul>",
		strings.Repeat("<b>x</b>", 65536),
	} {
		t.Run(source[:min(len(source), 30)], func(t *testing.T) {
			got, err := HTMLToPlainTextContext(context.Background(), []byte(source), maximumIncomingTextBytes)
			if want := HTMLToPlainText([]byte(source)); err != nil || got != want {
				t.Fatalf("converted %d bytes: output differs=%v, error=%v", len(source), got != want, err)
			}
		})
	}
}

func TestIncomingHTMLBudgetBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, source, stage string
		limit               int
	}{
		{"exact output", "<pre>1234</pre>", "", 4},
		{"output overflow", "<pre>12345</pre>", "render", 4},
		{"invalid UTF8 expansion", "\xff", "render", 1},
		{"dense early stop", strings.Repeat("<b>x</b>", 100000), "tokens", maximumIncomingTextBytes},
		{"deep parser stop", strings.Repeat("<div>", 600), "parse", maximumIncomingTextBytes},
		{"source overflow", strings.Repeat("x", maximumIncomingHTMLBytes+1), "source", maximumIncomingTextBytes},
	} {
		t.Run(test.name, func(t *testing.T) {
			text, err := HTMLToPlainTextContext(context.Background(), []byte(test.source), test.limit)
			if test.stage == "" {
				if err != nil || text != "1234" {
					t.Fatalf("text=%q error=%v", text, err)
				}
				return
			}
			var conversion *HTMLConversionError
			if text != "" || !errors.As(err, &conversion) || conversion.Stage != test.stage {
				t.Fatalf("output bytes=%d error=%v, want %s", len(text), err, test.stage)
			}
		})
	}
	for _, limit := range []int{2, 3} {
		err := checkIncomingHTMLTokens(context.Background(), []byte("<b>x</b>"), limit)
		if (err == nil) != (limit == 3) {
			t.Fatalf("token boundary %d: %v", limit, err)
		}
	}
	root, err := html.Parse(strings.NewReader("text"))
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{4, 5} {
		err := checkIncomingHTMLTree(context.Background(), root, limit)
		if (err == nil) != (limit == 5) {
			t.Fatalf("tree boundary %d: %v", limit, err)
		}
	}
}

func TestIncomingHTMLCancellation(t *testing.T) {
	for _, test := range []struct {
		name    string
		source  string
		trigger int
	}{
		{"before input", "text", 1},
		{"during parse", strings.Repeat("word ", 20000), 4},
		{"during lexical checks", strings.Repeat("<b>x</b>", 65536), 1000},
		{"during tree checks", strings.Repeat("<b>x</b>", 1024), 1000},
		{"during rendering", strings.Repeat("<b>x</b>", 1024), 2500},
	} {
		base, cancel := context.WithCancel(context.Background())
		ctx := &draftListBoundaryContext{Context: base, trigger: test.trigger, action: cancel}
		text, err := HTMLToPlainTextContext(ctx, []byte(test.source), maximumIncomingTextBytes)
		cancel()
		if text != "" || !errors.Is(err, context.Canceled) || ctx.checks < test.trigger {
			t.Fatalf("%s: checks=%d bytes=%d error=%v", test.name, ctx.checks, len(text), err)
		}
	}
}

func BenchmarkIncomingHTML(b *testing.B) {
	for _, fixture := range []struct {
		name, source string
		rejected     bool
	}{
		{"ordinary", strings.Repeat("<p>Hello <b>world</b>.</p>", 128), false},
		{"dense_512KiB", strings.Repeat("<b>x</b>", 65536), false},
		{"dense_rejected", strings.Repeat("<b>x</b>", 100000), true},
	} {
		for _, bounded := range []bool{false, true} {
			name := fixture.name + "/legacy"
			if bounded {
				name = fixture.name + "/bounded"
			}
			b.Run(name, func(b *testing.B) {
				source := []byte(fixture.source)
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if !bounded {
						if HTMLToPlainText(source) == "" {
							b.Fatal("empty legacy output")
						}
						continue
					}
					text, err := HTMLToPlainTextContext(context.Background(), source, maximumIncomingTextBytes)
					if (err != nil) != fixture.rejected || (text == "") != fixture.rejected {
						b.Fatalf("bytes=%d error=%v", len(text), err)
					}
				}
			})
		}
	}
}
