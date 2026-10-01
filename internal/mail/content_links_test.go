package mail

import (
	"strings"
	"testing"
)

func TestShortenLinks(t *testing.T) {
	t.Parallel()
	const long = "https://click.example.com/f/a/upYx533wyWJ-C15_1C36yg~~/AAAmIhA~/6m7yqwKk6656jrzYqqLXL1faPlsHRF6"
	tests := []struct {
		name string
		mode LinkMode
		in   string
		want string
	}{
		{"full keeps everything", LinkModeFull, "see " + long + "\n\n\n\nnext", "see " + long + "\n\n\n\nnext"},
		{"host replaces a long bare url", LinkModeHost, "Upload\n" + long + "\nDone", "Upload\n<click.example.com>\nDone"},
		{"host keeps a short url", LinkModeHost, "read https://example.com/a now", "read https://example.com/a now"},
		{"host keeps the label form", LinkModeHost, "Report (" + long + ") end", "Report (<click.example.com>) end"},
		{"none drops the label suffix", LinkModeNone, "Report (" + long + ") end", "Report end"},
		{"none drops a short url too", LinkModeNone, "read https://example.com/a now", "read  now"},
		{"trailing punctuation stays", LinkModeHost, "Go to " + long + ".", "Go to <click.example.com>."},
		{"angle brackets are reused", LinkModeHost, "mail <" + long + "> ok", "mail <click.example.com> ok"},
		{"none drops angle brackets", LinkModeNone, "mail <" + long + "> ok", "mail  ok"},
		{"two urls on one line", LinkModeHost, long + " and " + long + "x", "<click.example.com> and <click.example.com>"},
		{"internationalized host", LinkModeHost, "https://bücher.example/pfad/mit/einem/sehr/langen/namen/der/nicht/endet", "<bücher.example>"},
		{"balanced parentheses belong to the url", LinkModeHost, "(see https://en.wikipedia.org/wiki/Go_(programming_language))", "(see <en.wikipedia.org>)"},
		{"none drops an uppercase scheme", LinkModeNone, "see HTTPS://Example.com/some/path now", "see  now"},
		{"none drops a mixed case scheme in brackets", LinkModeNone, "mail <Http://example.com/x> ok", "mail  ok"},
		{"host shortens an uppercase scheme", LinkModeHost, "go HTTPS://Click.Example.com/f/a/upYx533wyWJ-C15_1C36yg~~/AAAmIhA~ now", "go <Click.Example.com> now"},
		{"other schemes are not links", LinkModeHost, "ftp://files.example.com/a/very/long/path/that/keeps/going/on/and/on and mailto:a@b.c", "ftp://files.example.com/a/very/long/path/that/keeps/going/on/and/on and mailto:a@b.c"},
		{"blank lines collapse", LinkModeHost, "a\n\n\n\n\nb\n\nc", "a\n\nb\n\nc"},
		{"none collapses blank lines", LinkModeNone, "a\n\n\n\nb", "a\n\nb"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := ShortenLinks(test.in, test.mode); got != test.want {
				t.Fatalf("ShortenLinks(%q, %s) = %q, want %q", test.in, test.mode, got, test.want)
			}
		})
	}
}

// Removing wrapped links must not copy the accumulated output again. Measure
// allocated bytes rather than elapsed time: shared-runner scheduling and GC
// can distort timing ratios even when the production pass stays linear.
func TestShortenLinksDoesNotCopyAccumulatedOutput(t *testing.T) {
	const unit = "Click <https://example.com/a?x=1> now (https://example.com/b?y=2) ok.\n"
	allocated := func(repeat int) int64 {
		text := strings.Repeat(unit, repeat)
		want := strings.Repeat("Click  now ok.\n", repeat)
		result := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			var got string
			for range b.N {
				got = ShortenLinks(text, LinkModeNone)
			}
			b.StopTimer()
			if strings.ReplaceAll(got, "\n\n", "\n") != want {
				b.Fatalf("unexpected result prefix %q", got[:min(len(got), 80)])
			}
		})
		return result.AllocedBytesPerOp()
	}
	small, large := allocated(1024), allocated(4096)
	t.Logf("wrapped-link allocated bytes: small=%d large=%d", small, large)
	if small <= 0 || large > 8*small {
		t.Fatalf("four times the links allocated %d versus %d bytes; repeated whole-output copies must be refused", large, small)
	}
}

func TestParseLinkMode(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]LinkMode{"": LinkModeFull, "full": LinkModeFull, " Host ": LinkModeHost, "NONE": LinkModeNone} {
		got, err := ParseLinkMode(value)
		if err != nil || got != want {
			t.Errorf("ParseLinkMode(%q) = %q, %v; want %q", value, got, err, want)
		}
	}
	if _, err := ParseLinkMode("short"); err == nil {
		t.Fatal("ParseLinkMode(short) error = nil")
	}
}

func TestSplitLinkTailBracketBalances(t *testing.T) {
	const base = "https://example.com/path"
	for _, test := range []struct {
		name, suffix, addressSuffix, tail string
	}{
		{"no tail", "", "", ""},
		{"punctuation", ".,;:!?'", "", ".,;:!?'"},
		{"balanced parentheses", "(a)", "(a)", ""},
		{"excess parentheses", "((a)))))", "((a))", ")))"},
		{"balanced square brackets", "[a]", "[a]", ""},
		{"excess square brackets", "[[a]]]]]", "[[a]]", "]]]"},
		{"balanced braces", "{a}", "{a}", ""},
		{"excess braces", "{{a}}}}}", "{{a}}", "}}}"},
		{"mixed balances", "([a])}].)", "([a])", "}].)"},
		{"interleaved punctuation", "),].;)}!'", "", "),].;)}!'"},
		{"unbalanced long tail", strings.Repeat(")]}", 4096), "", strings.Repeat(")]}", 4096)},
		{"balanced long tail", strings.Repeat("(", 4096) + "a" + strings.Repeat(")", 4096), strings.Repeat("(", 4096) + "a" + strings.Repeat(")", 4096), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			address, tail := splitLinkTail(base + test.suffix)
			if address != base+test.addressSuffix || tail != test.tail {
				t.Fatalf("address=%q tail=%q, want %q and %q", address, tail, base+test.addressSuffix, test.tail)
			}
			if got := ShortenLinks(base+test.suffix, LinkModeNone); got != test.tail {
				t.Fatalf("shortened link retained %q, want tail %q", got, test.tail)
			}
		})
	}
}

func BenchmarkSplitLinkTailUnbalancedClosings(b *testing.B) {
	const base = "https://example.com/path"
	for _, fixture := range []struct {
		name  string
		count int
	}{{"ordinary", 1}, {"1024", 1024}, {"4096", 4096}, {"16384", 16384}} {
		b.Run(fixture.name, func(b *testing.B) {
			tail := strings.Repeat(")", fixture.count)
			text := base + tail
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				address, gotTail := splitLinkTail(text)
				if address != base || gotTail != tail {
					b.Fatalf("tail parsing changed: address=%q tail bytes=%d", address, len(gotTail))
				}
			}
		})
	}
}
