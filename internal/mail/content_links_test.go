package mail

import (
	"strings"
	"testing"
	"time"
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

// Removing wrapped links must not rewrite the text seen so far: 40,000 of them
// took 1.16 s when every removal copied the whole output again.
func TestShortenLinksStaysLinearForManyWrappedLinks(t *testing.T) {
	text := strings.Repeat("Click <https://example.com/a?x=1> now (https://example.com/b?y=2) ok.\n", 40000)
	start := time.Now()
	got := ShortenLinks(text, LinkModeNone)
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("ShortenLinks took %v for %d KiB with 80,000 wrapped links", elapsed, len(text)/1024)
	}
	if want := strings.Repeat("Click  now ok.\n", 40000); strings.ReplaceAll(got, "\n\n", "\n") != want {
		t.Fatalf("unexpected result prefix %q", got[:min(len(got), 80)])
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
