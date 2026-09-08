package mail

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareDraftContent(t *testing.T) {
	tests := []struct {
		name      string
		format    DraftBodyFormat
		source    string
		wantPlain string
		wantHTML  string
		wantError bool
	}{
		{name: "plain", source: "Hello\n", wantPlain: "Hello\n"},
		{name: "markdown", format: DraftBodyMarkdown, source: "**Hello**\n\n- One\n- Two\n", wantPlain: "Hello\n- One\n- Two", wantHTML: "<strong>Hello</strong>"},
		{name: "markdown link", format: DraftBodyMarkdown, source: "[Read report](https://example.com/report)", wantPlain: "Read report (https://example.com/report)", wantHTML: `href="https://example.com/report"`},
		{name: "safe html", format: DraftBodyHTML, source: `<p>Hello <a href="https://example.com">there</a></p>`, wantPlain: "Hello there (https://example.com)", wantHTML: `href="https://example.com"`},
		{name: "safe table", format: DraftBodyHTML, source: `<table onclick="alert(1)"><tr><td>A</td><td>2</td></tr></table>`, wantPlain: "| A | 2 |", wantHTML: "<table>"},
		{name: "remote content removed", format: DraftBodyHTML, source: `<p>Hello</p><img src="https://example.com/track.png"><script>alert(1)</script>`, wantPlain: "Hello"},
		{name: "unsafe link removed", format: DraftBodyHTML, source: `<p><a href="javascript:alert(1)" onclick="alert(2)">Click</a></p>`, wantPlain: "Click"},
		{name: "style subtree removed", format: DraftBodyHTML, source: `<style>body { display: none }</style><p>Visible</p>`, wantPlain: "Visible"},
		{name: "invalid format", format: "rtf", source: "Hello", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content, err := prepareDraftContent(test.format, test.source)
			if test.wantError {
				if err == nil {
					t.Fatal("prepareDraftContent() error = nil")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if content.Plain != test.wantPlain {
				t.Fatalf("plain = %q, want %q", content.Plain, test.wantPlain)
			}
			if test.wantHTML != "" && !strings.Contains(content.HTML, test.wantHTML) {
				t.Fatalf("html = %q, want substring %q", content.HTML, test.wantHTML)
			}
			if strings.Contains(content.HTML, "img") || strings.Contains(content.HTML, "script") ||
				strings.Contains(content.HTML, "track.png") || strings.Contains(content.HTML, "javascript:") ||
				strings.Contains(content.HTML, "onclick") || strings.Contains(content.HTML, "display: none") {
				t.Fatalf("unsafe HTML survived: %q", content.HTML)
			}
		})
	}
}

func TestHTMLToPlainTextGolden(t *testing.T) {
	source := `<p><a href="https://example.com/report">Read report</a>; <a href="https://example.com">https://example.com</a>; <a href="mailto:alice@example.com">alice@example.com</a>; <a href="mailto:alice@example.com?subject=Hello">alice@example.com</a></p>` +
		`<p>Run <code>go  test</code></p><pre>  first` + "\n" + `    second` + "\n" + `</pre>` +
		`<ul><li>One<ul><li>Nested</li></ul></li></ul>` +
		`<ol><li>First</li><li>Second</li></ol>` +
		`<table><tr><th>Item</th><th>Qty</th><th></th></tr><tr><td><strong>Widget</strong></td><td>2</td><td></td></tr></table>`
	want, err := os.ReadFile(filepath.Join("testdata", "golden", "semantic-rich-text.golden"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if got := HTMLToPlainText([]byte(source)); got != strings.TrimSuffix(string(want), "\n") {
		t.Fatalf("HTMLToPlainText() = %q, want %q", got, strings.TrimSuffix(string(want), "\n"))
	}
}

func TestDraftMIMEFingerprintPreservesCodeWhitespace(t *testing.T) {
	withTwoLeadingSpaces := Draft{Body: "  first\n    second"}
	withOneLeadingSpace := Draft{Body: " first\n    second"}
	first, err := draftMIMEFingerprint(withTwoLeadingSpaces)
	if err != nil {
		t.Fatalf("draftMIMEFingerprint(first) error = %v", err)
	}
	second, err := draftMIMEFingerprint(withOneLeadingSpace)
	if err != nil {
		t.Fatalf("draftMIMEFingerprint(second) error = %v", err)
	}
	if first == second {
		t.Fatalf("draftMIMEFingerprint() erased leading code whitespace: %q = %q", first, second)
	}
	if got := normalizeMIMEText("  first\r\n    second  "); got != "  first\n    second  " {
		t.Fatalf("normalizeMIMEText() = %q, want preserved indentation and trailing code spaces", got)
	}
}

func TestHTMLToPlainTextSkipsUnsafeSubtreesAndEscapesTablePipes(t *testing.T) {
	source := `<script>alert(1)</script><p><a href="javascript:alert(2)">Click</a> <a href="data:text/plain,hidden">Data</a></p>` +
		`<img alt="must not be copied" src="https://example.com/pixel.png">` +
		`<table><tr><td>A|B</td><td>1</td></tr></table>`
	want := "Click Data\n| A\\|B | 1 |"
	if got := HTMLToPlainText([]byte(source)); got != want {
		t.Fatalf("HTMLToPlainText() = %q, want %q", got, want)
	}
}

func TestValidateStoredDraftContentRejectsTampering(t *testing.T) {
	content, err := prepareDraftContent(DraftBodyMarkdown, "**Hello**")
	if err != nil {
		t.Fatal(err)
	}
	draft := Draft{
		BodyFormat: content.Format,
		BodySource: content.Source,
		Body:       content.Plain,
		BodyHTML:   content.HTML + "changed",
	}
	if err := validateStoredDraftContent(draft); err == nil {
		t.Fatal("validateStoredDraftContent() error = nil")
	}
}

func TestCollapseHorizontalSpace(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "ascii spaces", input: "hello   world", want: "hello world"},
		{name: "tabs and newlines", input: "hello\t\t\nworld", want: "hello world"},
		{name: "leading trailing", input: "  hello  ", want: "hello"},
		{name: "only spaces", input: "   ", want: ""},
		{name: "empty", input: "", want: ""},
		{name: "unicode nbsp", input: "hello\u00a0world", want: "hello world"},
		{name: "mixed unicode", input: "café  résumé", want: "café résumé"},
		{name: "no collapse needed", input: "hello", want: "hello"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := collapseHorizontalSpace(test.input)
			if got != test.want {
				t.Fatalf("collapseHorizontalSpace(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestNormalizeDraftTextSkipsReplaceAllWhenNoCRLF(t *testing.T) {
	// Input with no \r\n should pass through without ReplaceAll.
	input := "Hello\nWorld\n"
	got := normalizeDraftText(input)
	if got != "Hello\nWorld" {
		t.Fatalf("normalizeDraftText(%q) = %q, want %q", input, got, "Hello\nWorld")
	}
}

func TestNormalizeDraftTextWithCRLF(t *testing.T) {
	input := "Hello\r\nWorld\r\n"
	got := normalizeDraftText(input)
	if got != "Hello\nWorld" {
		t.Fatalf("normalizeDraftText(%q) = %q, want %q", input, got, "Hello\nWorld")
	}
}
