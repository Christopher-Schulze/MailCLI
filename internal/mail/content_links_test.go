package mail

import "testing"

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
