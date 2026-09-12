package mail

import (
	"context"
	"errors"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestStreamingTextMatchesBufferedReference(t *testing.T) {
	fixtures := []string{
		"<pre>first\n</pre><div>second</div>", "<p></p><br><br>tail",
		"<ul><li> one <ul><li>two</li></ul></li></ul>",
		"<table><caption>Title</caption>fostered<tr><td>A|B</td><td><pre> x\n y</pre></td></tr></table>",
		"<p> Hello <code>  code </code> world </p>",
		"<a href=\"https://outer.example\"><table><a href=\"https://inner.example\">Inner</a><tr><td>Cell</td></tr></table>Outer</a>",
		"<img alt=\"Logo\"><script>ignore()</script>\xff\x00é",
	}
	random := rand.New(rand.NewPCG(290, 17))
	fragments := []string{"<p>", "</p>", "<b>", "</b>", "<pre>", "</pre>", "<ul>", "<li>", "</li>", "</ul>", "<table>", "<tr>", "<td>", "</td>", "</tr>", "</table>", "<br>", " ", "\n", "\r", "A|B", "é", "&amp;", "<a href=\"https://example.com\">", "</a>", "<code>", "</code>"}
	for range 3000 {
		var source strings.Builder
		for range 30 {
			source.WriteString(fragments[random.IntN(len(fragments))])
		}
		fixtures = append(fixtures, source.String())
	}
	for index, source := range fixtures {
		root, err := html.Parse(strings.NewReader(source))
		if err != nil {
			t.Fatal(err)
		}
		for _, limit := range []int{0, 16, MaximumDraftBodyBytes} {
			want, wantErr := bufferedReferenceDraftText(context.Background(), root, limit)
			got, gotErr := renderPlainText(context.Background(), root, limit)
			if got != want || (gotErr != nil) != (wantErr != nil) {
				t.Fatalf("fixture=%d limit=%d source=%q got=%q (%v) want=%q (%v)", index, limit, source, got, gotErr, want, wantErr)
			}
		}
	}
}

func TestStreamingTextStopsAtOutputBudget(t *testing.T) {
	root, err := html.Parse(strings.NewReader(strings.Repeat("<b>long text</b>", 10000)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := &draftListBoundaryContext{Context: context.Background()}
	text, err := renderPlainText(ctx, root, 1)
	var limit *plainTextBudgetError
	if text != "" || !errors.As(err, &limit) || limit.limit != 1 || ctx.checks > 20 {
		t.Fatalf("output bytes=%d, checks=%d, error=%v", len(text), ctx.checks, err)
	}
	_, err = HTMLToPlainTextContext(context.Background(), []byte("long text"), 2)
	if err == nil || !strings.Contains(err.Error(), "2 bytes") || strings.Contains(err.Error(), "draft") {
		t.Fatalf("incoming allowance misreported: %v", err)
	}
}

func FuzzStreamingTextMatchesBufferedReference(f *testing.F) {
	for _, seed := range []string{"<pre>x\n</pre><p>y", "<table><td>A|B", "<a href=\"https://example.com\">text</a>", "<ul><li>one<li>two", "\xff\r\n\x00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source string) {
		if len(source) > 16384 {
			return
		}
		root, err := html.Parse(strings.NewReader(source))
		if err != nil {
			return
		}
		got, gotErr := renderPlainText(context.Background(), root, MaximumDraftBodyBytes)
		want, wantErr := bufferedReferenceDraftText(context.Background(), root, MaximumDraftBodyBytes)
		if got != want || (gotErr != nil) != (wantErr != nil) {
			t.Fatalf("got=%q (%v) want=%q (%v)", got, gotErr, want, wantErr)
		}
	})
}

func BenchmarkPlainTextRendering(b *testing.B) {
	for _, fixture := range []struct{ name, source string }{
		{"ordinary", "<p>Hello <b>world</b>.</p>"},
		{"dense", strings.Repeat("<b>x</b>", 65536)},
		{"links", strings.Repeat("<a href=\"https://example.com\">read</a>", 1024)},
		{"large_text", "<p>" + strings.Repeat("word ", 200000) + "</p>"},
	} {
		root, err := html.Parse(strings.NewReader(fixture.source))
		if err != nil {
			b.Fatal(err)
		}
		for _, buffered := range []bool{true, false} {
			name := fixture.name + "/streaming"
			if buffered {
				name = fixture.name + "/buffered_reference"
			}
			b.Run(name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					var text string
					var err error
					if buffered {
						text, err = bufferedReferenceDraftText(context.Background(), root, MaximumDraftBodyBytes)
					} else {
						text, err = renderPlainText(context.Background(), root, MaximumDraftBodyBytes)
					}
					if err != nil || text == "" {
						b.Fatalf("bytes=%d error=%v", len(text), err)
					}
				}
			})
		}
	}
}

// Frozen token-buffering renderer from 855b707. This reference retains its
// original traversal and staging so differential tests can detect changed
// whitespace, link, table and list semantics. The unchanged output formatter
// is shared; this reference is never used by production code.
type bufferedReferenceRenderer struct {
	ctx            context.Context
	err            error
	maximumBytes   int
	labelBytes     int
	tokens         []plainTextToken
	links          []plainTextLink
	lists          []plainTextList
	tables         []plainTextTable
	preDepth       int
	codeDepth      int
	tableCellDepth int
	linePrefix     bool
}

func bufferedReferenceDraftText(ctx context.Context, root *html.Node, maximumBytes int) (string, error) {
	renderer := bufferedReferenceRenderer{ctx: ctx, maximumBytes: maximumBytes, tokens: make([]plainTextToken, 0, 32)}
	renderer.renderNode(root)
	if renderer.err != nil {
		return "", renderer.err
	}
	return renderer.text()
}

func (renderer *bufferedReferenceRenderer) renderNode(node *html.Node) {
	if renderer.err != nil || node == nil {
		return
	}
	if renderer.err = renderer.ctx.Err(); renderer.err != nil {
		return
	}
	switch node.Type {
	case html.TextNode:
		if strings.TrimSpace(node.Data) == "" && renderer.ignoreStructuralWhitespace(node) {
			return
		}
		renderer.appendText(node.Data)
	case html.ElementNode:
		renderer.renderElement(node)
	default:
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			renderer.renderNode(child)
		}
	}
}

func (renderer *bufferedReferenceRenderer) ignoreStructuralWhitespace(node *html.Node) bool {
	if renderer.preDepth > 0 || renderer.codeDepth > 0 || node.Parent == nil || node.Parent.Type != html.ElementNode {
		return false
	}
	switch strings.ToLower(node.Parent.Data) {
	case "body", "html", "ol", "table", "tbody", "tfoot", "thead", "tr", "ul":
		return true
	default:
		return false
	}
}

func (renderer *bufferedReferenceRenderer) renderElement(node *html.Node) {
	name := strings.ToLower(node.Data)
	if name == "img" {
		if alt := htmlAttributeValue(node.Attr, "alt"); alt != "" {
			renderer.appendRaw(alt)
		}
		return
	}
	if dropsPlainTextSubtree(name) {
		return
	}
	switch name {
	case "a":
		renderer.renderLink(node)
	case "br", "hr":
		renderer.appendBreak()
	case "code":
		renderer.codeDepth++
		renderer.renderChildren(node)
		renderer.codeDepth--
	case "pre":
		renderer.appendBlockBreak()
		renderer.preDepth++
		renderer.renderChildren(node)
		renderer.preDepth--
		renderer.appendBlockBreak()
	case "ul", "ol":
		renderer.renderList(node, name == "ol")
	case "li":
		renderer.renderListItem(node)
	case "table":
		renderer.renderTable(node)
	case "tr":
		renderer.renderTableRow(node)
	case "td", "th":
		renderer.renderTableCell(node)
	default:
		if isPlainTextBlock(name) {
			renderer.appendBlockBreak()
		}
		renderer.renderChildren(node)
		if isPlainTextBlock(name) {
			renderer.appendBlockBreak()
		}
	}
}

func (renderer *bufferedReferenceRenderer) renderChildren(node *html.Node) {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		renderer.renderNode(child)
	}
}

func (renderer *bufferedReferenceRenderer) renderLink(node *html.Node) {
	renderer.links = append(renderer.links, plainTextLink{href: plainLinkTarget(node)})
	renderer.renderChildren(node)
	link := &renderer.links[len(renderer.links)-1]
	href := link.href
	label := normalizeLinkLabel(link.label)
	nestedTargets := link.nestedTargets
	represented := label != "" && href != ""
	if represented && !redundantLinkTarget(label, href) && !containsLinkTarget(nestedTargets, href) {
		renderer.appendRaw(" (" + href + ")")
	}
	renderer.links = renderer.links[:len(renderer.links)-1]
	if represented {
		for index := range renderer.links {
			renderer.links[index].nestedTargets = append(renderer.links[index].nestedTargets, href)
		}
	}
}

func (renderer *bufferedReferenceRenderer) renderList(node *html.Node, ordered bool) {
	renderer.appendBlockBreak()
	renderer.lists = append(renderer.lists, plainTextList{ordered: ordered, next: 1})
	renderer.renderChildren(node)
	renderer.lists = renderer.lists[:len(renderer.lists)-1]
	renderer.appendBlockBreak()
}

func (renderer *bufferedReferenceRenderer) renderListItem(node *html.Node) {
	renderer.appendBlockBreak()
	indent := strings.Repeat("  ", maxInt(len(renderer.lists)-1, 0))
	prefix := "- "
	if len(renderer.lists) > 0 {
		list := &renderer.lists[len(renderer.lists)-1]
		if list.ordered {
			prefix = strconv.Itoa(list.next) + ". "
			list.next++
		}
	}
	renderer.appendRaw(indent + prefix)
	renderer.linePrefix = true
	renderer.renderChildren(node)
	renderer.appendBlockBreak()
}

func (renderer *bufferedReferenceRenderer) renderTable(node *html.Node) {
	renderer.appendBlockBreak()
	renderer.tables = append(renderer.tables, plainTextTable{})
	renderer.renderChildren(node)
	renderer.tables = renderer.tables[:len(renderer.tables)-1]
	renderer.appendBlockBreak()
}

func (renderer *bufferedReferenceRenderer) renderTableRow(node *html.Node) {
	if len(renderer.tables) == 0 {
		renderer.appendBlockBreak()
		renderer.renderChildren(node)
		renderer.appendBlockBreak()
		return
	}
	renderer.appendBlockBreak()
	renderer.tables[len(renderer.tables)-1].inRow = true
	renderer.tables[len(renderer.tables)-1].cellCount = 0
	renderer.appendRaw("| ")
	renderer.linePrefix = true
	renderer.renderChildren(node)
	renderer.appendRaw(" |")
	renderer.tables[len(renderer.tables)-1].inRow = false
	renderer.appendBreak()
}

func (renderer *bufferedReferenceRenderer) renderTableCell(node *html.Node) {
	if len(renderer.tables) == 0 || !renderer.tables[len(renderer.tables)-1].inRow {
		renderer.appendBlockBreak()
		renderer.renderChildren(node)
		renderer.appendBlockBreak()
		return
	}
	row := &renderer.tables[len(renderer.tables)-1]
	if row.cellCount > 0 {
		renderer.appendRaw(" | ")
		renderer.linePrefix = true
	}
	row.cellCount++
	renderer.tableCellDepth++
	renderer.renderChildren(node)
	renderer.tableCellDepth--
}

func (renderer *bufferedReferenceRenderer) appendText(value string) {
	if renderer.err != nil || value == "" {
		return
	}
	if renderer.maximumBytes > 0 && len(renderer.links) > 0 {
		if len(value) > (4*renderer.maximumBytes-renderer.labelBytes)/len(renderer.links) {
			renderer.err = validationError("draft HTML exceeds 16 MiB of link-label text")
			return
		}
		renderer.labelBytes += len(value) * len(renderer.links)
	}
	for index := range renderer.links {
		renderer.links[index].label = append(renderer.links[index].label, value...)
	}
	if strings.TrimSpace(value) != "" {
		renderer.linePrefix = false
	}
	if renderer.tableCellDepth > 0 {
		value = strings.ReplaceAll(value, "|", `\|`)
	}
	renderer.tokens = append(renderer.tokens, plainTextToken{
		text: value, preserve: renderer.preDepth > 0 || renderer.codeDepth > 0,
		block: renderer.preDepth > 0,
	})
}

func (renderer *bufferedReferenceRenderer) appendRaw(value string) {
	if renderer.err != nil || value == "" {
		return
	}
	renderer.tokens = append(renderer.tokens, plainTextToken{text: value, preserve: true, raw: true})
}

func (renderer *bufferedReferenceRenderer) appendBreak() {
	renderer.tokens = append(renderer.tokens, plainTextToken{lineBreak: true})
	renderer.linePrefix = false
}

func (renderer *bufferedReferenceRenderer) appendBlockBreak() {
	if renderer.linePrefix {
		return
	}
	if len(renderer.tokens) > 0 && renderer.tokens[len(renderer.tokens)-1].lineBreak {
		return
	}
	if len(renderer.tokens) > 0 {
		last := renderer.tokens[len(renderer.tokens)-1]
		if last.preserve && last.block && strings.HasSuffix(normalizeLineEndings(last.text), "\n") {
			return
		}
	}
	renderer.appendBreak()
}

func (renderer *bufferedReferenceRenderer) text() (string, error) {
	output := plainTextOutput{ctx: renderer.ctx, maximumBytes: renderer.maximumBytes}
	capacity := len(renderer.tokens) * 8
	if renderer.maximumBytes > 0 {
		capacity = min(capacity, renderer.maximumBytes)
	}
	output.output.Grow(capacity)
	for _, token := range renderer.tokens {
		output.write(token)
		if output.err != nil {
			return "", output.err
		}
	}
	return strings.Trim(output.output.String(), "\n"), renderer.ctx.Err()
}
