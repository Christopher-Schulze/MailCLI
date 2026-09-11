package mail

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestPreparedHTMLMatchesRenderedSemantics(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
	}{
		{name: "adjacent text", source: `one<span> </span>two<!-- ignored --> three<img alt=" four"> five`},
		{name: "table caption", source: `<table><caption><b>Summary</b> of rows</caption><tr><td>A</td><td>B</td></tr></table>`},
		{name: "caption and fostered text", source: `<tABle><CAption>0<tr>0`},
		{name: "table formatting", source: `<a href="https://outer.example"><table><a href="https://inner.example">Inner</a><tr><td>Cell</td></tr></table>Outer</a>`},
		{name: "nested link label growth", source: `<a href="https://example.com"><table>text <td></p><a href="https://example.com"></p>text </p></p>`},
		{name: "foreign content", source: `<svg><a href="https://example.com">Link</a><desc><p>Paragraph</p></desc></svg>`},
		{name: "unwrapped headings", source: `<div>Intro<h2>Title</h2><section><p>Body</p></section>End</div>`},
		{name: "preformatted newlines", source: "<pre>\n\n  first\r\n    second\n</pre><code>  code  </code>"},
		{name: "head and body diagnostics", source: `<html><head><meta name="a" content="b"><style>p{}</style></head><body class="x"><p title="removed">Text</p></body></html>`},
		{name: "nested dropped content", source: `<p>Text<form><img alt="Hidden"><p>Form</p></form><template><b>Hidden</b></template><img alt="Visible"></p>`},
		{name: "empty", source: ""},
		{name: "malformed", source: `<p>A<b><i>B</p>C</b>D</i>`},
	} {
		t.Run(test.name, func(t *testing.T) {
			content, err := prepareDraftContent(DraftBodyHTML, test.source)
			if err != nil {
				t.Fatal(err)
			}
			if want := HTMLToPlainText([]byte(content.HTML)); content.Plain != want {
				t.Fatalf("plain = %q, rendered HTML means %q; HTML = %q", content.Plain, want, content.HTML)
			}
		})
	}
}

func BenchmarkPrepareDraftContent(b *testing.B) {
	for _, test := range []struct {
		name   string
		format DraftBodyFormat
		source string
	}{
		{name: "repeated_tags", format: DraftBodyHTML, source: strings.Repeat(`<p>Hello <b>world</b></p>`, 4096)},
		{name: "rich_html", format: DraftBodyHTML, source: strings.Repeat(`<section><h2>Report</h2><p>A <strong>useful</strong> summary with <a href="https://example.com/report">details</a>.</p><ul><li>One</li><li>Two</li></ul><img alt="Logo" src="https://remote.example/logo"></section>`, 512)},
		{name: "plain", format: DraftBodyPlain, source: strings.Repeat("Readable plain text.\n", 5120)},
		{name: "links", format: DraftBodyHTML, source: strings.Repeat(`<p><a href="https://example.com/report">Read report</a> and <a href="mailto:alice@example.com">Alice</a>.</p>`, 1024)},
		{name: "tables", format: DraftBodyHTML, source: strings.Repeat(`<table><caption>Results</caption><tr><th>Item</th><th>Qty</th></tr><tr><td>Widget</td><td>2</td></tr></table>`, 1024)},
		{name: "markdown", format: DraftBodyMarkdown, source: strings.Repeat("## Report\n\nA **useful** [link](https://example.com).\n\n- One\n- Two\n\n", 1024)},
		{name: "depth", format: DraftBodyHTML, source: strings.Repeat("<blockquote>", 250) + "Text" + strings.Repeat("</blockquote>", 250)},
		{name: "one_wrapper", format: DraftBodyHTML, source: "<span>x" + strings.Repeat("text ", 200000) + "</span>"},
		{name: "many_wrappers", format: DraftBodyHTML, source: strings.Repeat("<span>x", 250) + strings.Repeat("text ", 200000) + strings.Repeat("</span>", 250)},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(test.source)))
			for range b.N {
				content, err := prepareDraftContent(test.format, test.source)
				if err != nil {
					b.Fatal(err)
				}
				if content.Plain == "" {
					b.Fatal("content lost its plaintext")
				}
			}
		})
	}
}

func FuzzPreparedHTMLMatchesRenderedSemantics(f *testing.F) {
	for _, source := range []string{
		`<p>Hello <b>world</b></p>`,
		`<table><caption><b>Summary</b></caption><tr><td>Cell</td></tr></table>`,
		`<tABle><CAption>0<tr>0`,
		`<a href="https://outer.example"><table><a href="https://inner.example">Inner</a><tr><td>Cell</td></tr></table>Outer</a>`,
		`<svg><a href="https://example.com">Link</a><desc><p>Paragraph</p></desc></svg>`,
		`<p>one<span> </span>two<img alt=" three"><script>bad</script></p>`,
		"<pre>\n\n  code\n</pre><ul><li>one<ul><li>two</li></ul></li></ul>",
	} {
		f.Add(source)
	}
	f.Fuzz(func(t *testing.T, source string) {
		if len(source) > 4096 {
			return
		}
		content, err := prepareDraftContent(DraftBodyHTML, source)
		if err != nil {
			return
		}
		if want := HTMLToPlainText([]byte(content.HTML)); content.Plain != want {
			t.Fatalf("plain = %q, rendered HTML means %q; HTML = %q", content.Plain, want, content.HTML)
		}
	})
}

func TestSanitizedTreeReusePreservesOwnedNodes(t *testing.T) {
	document, err := html.Parse(strings.NewReader(`<p>Hello <b>world</b><span><img alt="Logo"></span><script>hidden</script></p>`))
	if err != nil {
		t.Fatal(err)
	}
	body := findHTMLElement(document, "body")
	paragraph := body.FirstChild
	text := paragraph.FirstChild
	bold := text.NextSibling
	span := bold.NextSibling
	image := span.FirstChild
	var diagnostics contentDiagnosticCollector
	if err := sanitizeHTMLChildren(context.Background(), body, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if body.FirstChild != paragraph || paragraph.FirstChild != text || text.NextSibling != bold || bold.NextSibling != image {
		t.Fatal("sanitization copied surviving nodes instead of reusing the owned tree")
	}
	if image.Type != html.TextNode || image.Data != "Logo" || image.Parent != paragraph || image.NextSibling != nil ||
		span.Parent != nil || span.FirstChild != nil || paragraph.LastChild != image {
		t.Fatal("unwrap/image conversion broke tree identity or sibling links")
	}
	serialized, err := renderSanitizedHTML(context.Background(), body)
	if err != nil || serialized != `<p>Hello <b>world</b>Logo</p>` {
		t.Fatalf("sanitized HTML = %q, error = %v", serialized, err)
	}
}

func TestPreparedTreeSelectsRequiredNormalization(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		direct bool
	}{
		{name: "typography", source: `<h2>Title</h2><p><a href="https://example.com"><b>Link</b></a><code> code </code></p>`, direct: true},
		{name: "unwrapped flow", source: `<section><p>Text</p><ul><li>One<ol><li>Nested</li></ol></li></ul></section>`, direct: true},
		{name: "regular table", source: `<table><tr><th>A</th><td><p>B</p><table><tr><td>C</td></tr></table></td></tr></table>`, direct: true},
		{name: "caption removed", source: `<table><caption>Caption</caption><tr><td>Cell</td></tr></table>`},
		{name: "nested anchors", source: `<a href="https://outer.example"><table><a href="https://inner.example">Inner</a><tr><td>Cell</td></tr></table>Outer</a>`},
		{name: "unwrapped heading boundary", source: `<h1><div><h2>Heading</h2></div></h1>`},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, _, err := sanitizeEmailHTML(context.Background(), strings.NewReader(test.source))
			if err != nil {
				t.Fatal(err)
			}
			if direct := stableDraftContentTree(body, false, false); direct != test.direct {
				t.Fatalf("direct tree reuse = %t, want %t", direct, test.direct)
			}
			serialized, err := renderSanitizedHTML(context.Background(), body)
			if err != nil {
				t.Fatal(err)
			}
			plain, err := renderPreparedDraftText(context.Background(), body, serialized)
			if err != nil || plain != HTMLToPlainText([]byte(serialized)) {
				t.Fatalf("normalization changed meaning: plain = %q, error = %v", plain, err)
			}
		})
	}
}

func TestDraftContentBounds(t *testing.T) {
	for _, test := range []struct {
		name      string
		format    DraftBodyFormat
		source    string
		wantError string
	}{
		{name: "plain at limit", format: DraftBodyPlain, source: strings.Repeat("x", MaximumDraftBodyBytes)},
		{name: "source over limit", format: DraftBodyHTML, source: strings.Repeat("x", MaximumDraftBodyBytes+1), wantError: "draft body exceeds"},
		{name: "nodes at limit", format: DraftBodyHTML, source: strings.Repeat("<br>", maximumDraftContentNodes-4)},
		{name: "nodes over limit", format: DraftBodyHTML, source: strings.Repeat("<br>", maximumDraftContentNodes-3), wantError: "65536 nodes"},
		{name: "depth at safe bound", format: DraftBodyHTML, source: strings.Repeat("<blockquote>", 500) + "Text" + strings.Repeat("</blockquote>", 500)},
		{name: "depth exceeds parser bound", format: DraftBodyHTML, source: strings.Repeat("<blockquote>", 600) + "Text", wantError: "invalid HTML"},
		{name: "escaped HTML exceeds output", format: DraftBodyHTML, source: strings.Repeat("&", MaximumDraftBodyBytes/4), wantError: "rendered draft body exceeds"},
		{name: "rendered Markdown exceeds input", format: DraftBodyMarkdown, source: strings.Repeat("&", MaximumDraftBodyBytes/4), wantError: "rendered draft body exceeds"},
		{name: "list indentation exceeds plaintext", format: DraftBodyHTML, source: strings.Repeat("<ul><li>", 150) + "<ul>" + strings.Repeat("<li>x</li>", 15000) + "</ul>" + strings.Repeat("</li></ul>", 150), wantError: "plain-text draft body exceeds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			content, err := prepareDraftContent(test.format, test.source)
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if errorCode(err) != "invalid_argument" || !strings.Contains(err.Error(), test.wantError) ||
				content.Plain != "" || content.HTML != "" || content.Source != "" {
				t.Fatalf("bounded rendering returned content or wrong error: %v", err)
			}
		})
	}
}

func TestDraftContentLinkCaptureIsBounded(t *testing.T) {
	root := &html.Node{Type: html.ElementNode, Data: "body"}
	parent := root
	for range 9 {
		link := &html.Node{Type: html.ElementNode, Data: "a", Attr: []html.Attribute{{Key: "href", Val: "https://example.com"}}}
		parent.AppendChild(link)
		parent = link
	}
	parent.AppendChild(&html.Node{Type: html.TextNode, Data: strings.Repeat("x", 2*1024*1024)})
	plain, err := renderDraftText(context.Background(), root, MaximumDraftBodyBytes)
	if plain != "" || errorCode(err) != "invalid_argument" || !strings.Contains(err.Error(), "link-label") {
		t.Fatalf("unbounded nested label capture: output bytes = %d, error = %v", len(plain), err)
	}
}

func TestDraftContentCancelsDuringHTMLInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := strings.Repeat(`<p>Message</p>`, 20000)
	readBytes := 0
	reader := attachmentFingerprintReader{ctx: ctx, reader: strings.NewReader(source), afterRead: func(count int) {
		readBytes += count
		if readBytes >= 8192 {
			cancel()
		}
	}}
	content, err := canonicalRichContent(ctx, DraftBodyHTML, source, reader)
	if !errors.Is(err, context.Canceled) || readBytes == 0 || readBytes >= len(source) || content.HTML != "" || content.Plain != "" {
		t.Fatalf("input cancellation lost: read bytes = %d, error = %v", readBytes, err)
	}
}

func TestDraftContentCancelsDuringTreeWork(t *testing.T) {
	for _, phase := range []string{"validate", "sanitize", "HTML", "plaintext", "normalize"} {
		t.Run(phase, func(t *testing.T) {
			source := strings.Repeat(`<p>Readable <b>text</b>.</p>`, 1024)
			if phase == "normalize" {
				source = strings.Repeat(`<table><caption>Caption</caption><tr><td>Cell</td></tr></table>`, 1024)
			}
			document, err := html.Parse(strings.NewReader(source))
			if err != nil {
				t.Fatal(err)
			}
			body := findHTMLElement(document, "body")
			serialized := ""
			if phase == "normalize" {
				var diagnostics contentDiagnosticCollector
				if err := sanitizeHTMLChildren(context.Background(), body, &diagnostics); err != nil {
					t.Fatal(err)
				}
				serialized, err = renderSanitizedHTML(context.Background(), body)
				if err != nil {
					t.Fatal(err)
				}
			}
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &draftListBoundaryContext{Context: base, trigger: 100, action: cancel}
			switch phase {
			case "validate":
				err = validateDraftContentTree(ctx, document)
			case "sanitize":
				var diagnostics contentDiagnosticCollector
				err = sanitizeHTMLChildren(ctx, body, &diagnostics)
			case "HTML":
				_, err = renderSanitizedHTML(ctx, body)
			case "plaintext":
				_, err = renderDraftText(ctx, body, MaximumDraftBodyBytes)
			case "normalize":
				_, err = renderPreparedDraftText(ctx, body, serialized)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%s did not stop on cancellation: %v", phase, err)
			}
		})
	}
}

func TestPlainTextOutputRetainsLimitAndCancellationErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		cancel bool
	}{
		{name: "limit"},
		{name: "cancellation", cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := base
			limit := 4095
			if test.cancel {
				ctx = &draftListBoundaryContext{Context: base, trigger: 3, action: cancel}
				limit = MaximumDraftBodyBytes
			}
			output := plainTextOutput{ctx: ctx, maximumBytes: limit}
			output.write(plainTextToken{text: strings.Repeat("x", 8192) + strings.Repeat(" ", 8192)})
			firstError := output.err
			output.write(plainTextToken{text: "later"})
			if output.err == nil || output.err != firstError || output.output.Len() == 0 || output.output.Len() > 4096 {
				t.Fatalf("text output lost its terminal bound: bytes = %d, error = %v", output.output.Len(), output.err)
			}
			if test.cancel && !errors.Is(output.err, context.Canceled) {
				t.Fatalf("cancel error = %v", output.err)
			}
		})
	}
}

func TestCreateRichDraftCancellationPreventsPersistence(t *testing.T) {
	for _, format := range []DraftBodyFormat{DraftBodyHTML, DraftBodyMarkdown} {
		t.Run(string(format), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "drafts")
			service := NewServiceWithDraftRoot(nil, root)
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &draftListBoundaryContext{Context: base, trigger: 30, action: cancel}
			draft, err := service.CreateDraftContext(ctx, CreateDraftRequest{Input: DraftInput{
				To: []Recipient{{Address: "recipient@example.com"}}, BodyFormat: format,
				Body: strings.Repeat("<p>Content</p>\n\n**Content**\n\n", 20000),
			}})
			if errorCode(err) != "draft_operation_canceled" || draft.Ref != "" {
				t.Fatalf("create accepted canceled content: ref = %q, error = %v", draft.Ref, err)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("canceled content created draft state: %v", err)
			}
		})
	}
}

func TestUpdateRichDraftCancellationPreservesReviewedFile(t *testing.T) {
	for _, format := range []DraftBodyFormat{DraftBodyHTML, DraftBodyMarkdown} {
		t.Run(string(format), func(t *testing.T) {
			root := t.TempDir()
			service := NewServiceWithDraftRoot(nil, root)
			draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
				To: []Recipient{{Address: "recipient@example.com"}}, BodyFormat: DraftBodyHTML, Body: "<p>Reviewed</p>",
			}})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, draft.Ref+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &draftListBoundaryContext{Context: base, trigger: 30, action: cancel}
			replacement, err := service.UpdateDraftContext(ctx, UpdateDraftRequest{
				Ref: draft.Ref, ExpectedRevision: draft.Revision,
				Input: DraftInput{To: draft.To, BodyFormat: format, Body: strings.Repeat("<p>Replacement</p>\n\n**Replacement**\n\n", 20000)},
			})
			if errorCode(err) != "draft_operation_canceled" || replacement.Ref != "" {
				t.Fatalf("update accepted canceled content: ref = %q, error = %v", replacement.Ref, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("canceled rendering changed the reviewed draft: %v", err)
			}
		})
	}
}

func TestPreparedRichContentSecurity(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
	}{
		{name: "scripts styles events", source: `<script>hidden</script><style>p{background:red}</style><p onclick="run()" style="background:url(https://remote.example)">Visible</p>`},
		{name: "frames forms controls", source: `<iframe src="https://remote.example">hidden</iframe><form action="https://remote.example"><input value="hidden"><button>hidden</button></form><p>Visible</p>`},
		{name: "remote media and literal alt", source: `<p><img alt="<script>literal text</script>" srcset="https://remote.example"><audio src="https://remote.example"></audio><video poster="https://remote.example"></video></p>`},
		{name: "foreign namespaces", source: `<svg><a xlink:href="javascript:run()"><text>Visible</text></a><foreignObject><p onclick="run()">Body</p></foreignObject></svg><math><mi>hidden</mi></math>`},
		{name: "encoded URLs and quoted titles", source: `<a href="&#x6a;avascript:run()" title="&quot; onclick=&quot;run()">Visible</a><a href="data:text/html,bad">Data</a><a href="//remote.example">Relative</a>`},
		{name: "case duplicates templates", source: `<A HREF=" HTTPS://example.com/path " HREF="javascript:run()" ONCLICK="run()">Visible</A><template><b>hidden</b></template>`},
		{name: "allowed mail and web links", source: `<!-- comment --><p><a href="mailto:alice@example.com?subject=Hi">Mail</a><a href="http://example.com">Web</a></p>`},
	} {
		t.Run(test.name, func(t *testing.T) {
			content, err := prepareDraftContent(DraftBodyHTML, test.source)
			if err != nil {
				t.Fatal(err)
			}
			document, err := html.Parse(strings.NewReader(content.HTML))
			if err != nil {
				t.Fatal(err)
			}
			pending := []*html.Node{document}
			for len(pending) > 0 {
				node := pending[len(pending)-1]
				pending = pending[:len(pending)-1]
				for child := node.FirstChild; child != nil; child = child.NextSibling {
					pending = append(pending, child)
				}
				if node.Type == html.TextNode || node.Type == html.DocumentNode {
					continue
				}
				allowed := " html head body a b blockquote br code del em h1 h2 h3 h4 h5 h6 hr i li ol p pre s strong table tbody td tfoot th thead tr u ul "
				if node.Type != html.ElementNode || node.Namespace != "" || !strings.Contains(allowed, " "+node.Data+" ") {
					t.Fatalf("unsafe node survived: type = %v, name = %q", node.Type, node.Data)
				}
				seen := make(map[string]bool)
				for _, attribute := range node.Attr {
					if node.Data != "a" || attribute.Namespace != "" || seen[attribute.Key] {
						t.Fatalf("unsafe/duplicate attribute survived: %+v", attribute)
					}
					seen[attribute.Key] = true
					switch attribute.Key {
					case "href":
						target, err := url.Parse(attribute.Val)
						if err != nil || !target.IsAbs() || !strings.Contains(" http https mailto ", " "+strings.ToLower(target.Scheme)+" ") {
							t.Fatalf("unsafe link survived: %q", attribute.Val)
						}
					case "rel":
						if attribute.Val != "nofollow noreferrer" {
							t.Fatalf("link protection missing: %q", attribute.Val)
						}
					case "title":
					default:
						t.Fatalf("unexpected attribute survived: %+v", attribute)
					}
				}
				if node.Data == "a" && !seen["rel"] {
					t.Fatal("link omitted its protective rel")
				}
			}
		})
	}
}

// Preserve the pre-optimization clone/render traversal as a differential
// oracle only. Attribute policy is unchanged by this task; tree ownership,
// order, wrapper flattening and diagnostic order are checked independently.
func referenceSanitizedDraftHTML(source string) (string, []ContentDiagnostic, error) {
	document, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return "", nil, err
	}
	var output bytes.Buffer
	var diagnostics contentDiagnosticCollector
	body := findHTMLElement(document, "body")
	if body == nil {
		body = document
	}
	if head := findHTMLElement(document, "head"); head != nil {
		for child := head.FirstChild; child != nil; child = child.NextSibling {
			collectHTMLDiagnostics(child, &diagnostics)
		}
	}
	if body.Type == html.ElementNode && strings.EqualFold(body.Data, "body") {
		recordRemovedAttributes("body", body.Attr, &diagnostics)
	}
	for child := body.FirstChild; child != nil; child = child.NextSibling {
		for _, sanitized := range referenceSanitizedDraftNode(child, &diagnostics) {
			if err := html.Render(&output, sanitized); err != nil {
				return "", nil, err
			}
		}
	}
	return strings.TrimSpace(output.String()), diagnostics.values, nil
}

func referenceSanitizedDraftNode(node *html.Node, diagnostics *contentDiagnosticCollector) []*html.Node {
	switch node.Type {
	case html.TextNode:
		return []*html.Node{{Type: html.TextNode, Data: node.Data}}
	case html.ElementNode:
		name := strings.ToLower(node.Data)
		if dropsHTMLSubtree(name) {
			diagnostics.add(ContentDiagnosticRemovedElement, name, "")
			recordDroppedResourceDiagnostics(name, node.Attr, diagnostics)
			if name == "img" {
				if alt := htmlAttributeValue(node.Attr, "alt"); alt != "" {
					return []*html.Node{{Type: html.TextNode, Data: alt}}
				}
			}
			return nil
		}
		var children []*html.Node
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			children = append(children, referenceSanitizedDraftNode(child, diagnostics)...)
		}
		if !allowsHTMLElement(name) {
			recordRemovedAttributes(name, node.Attr, diagnostics)
			if len(children) == 0 {
				diagnostics.add(ContentDiagnosticRemovedElement, name, "")
			}
			return children
		}
		clean := &html.Node{Type: html.ElementNode, Data: name}
		clean.Attr = sanitizeElementAttributes(name, node.Attr, diagnostics)
		for _, child := range children {
			clean.AppendChild(child)
		}
		return []*html.Node{clean}
	default:
		return nil
	}
}

func FuzzPreparedRichContentFragments(f *testing.F) {
	fragments := []string{
		"text ", "\n", "&amp; &lt;", `<p>`, `</p>`, `<span title="removed">`, `</span>`,
		`<table>`, `</table>`, `<caption>`, `</caption>`, `<tr>`, `</tr>`, `<td>`, `</td>`,
		`<a href="https://example.com">`, `</a>`, `<b>`, `</b>`, `<i>`, `</i>`,
		`<ul>`, `</ul>`, `<li>`, `</li>`, `<pre>`, `</pre>`, `<br>`,
		`<img alt="Logo" src="https://remote.example">`, `<script>hidden</script>`,
		`<svg>`, `</svg>`, `<foreignObject>`, `</foreignObject>`, `<hr>`, "\x00",
		`<h1>`, `</h1>`, `<h2>`, `</h2>`, `<div>`, `</div>`, `<code>`, `</code>`,
	}
	for _, seed := range [][]byte{{3, 15, 0, 16, 4}, {7, 9, 0, 11, 0, 13, 0, 14, 12, 8}, {15, 7, 15, 0, 16, 13, 0, 14, 8, 16}, {30, 15, 0, 16, 32, 3, 0, 4, 33, 31}, []byte("\xbf\xb7X90\xbf0X00")} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, choices []byte) {
		if len(choices) > 128 {
			return
		}
		var source strings.Builder
		for _, choice := range choices {
			source.WriteString(fragments[int(choice)%len(fragments)])
		}
		wantHTML, wantDiagnostics, err := referenceSanitizedDraftHTML(source.String())
		if err != nil {
			t.Fatal(err)
		}
		content, err := prepareDraftContent(DraftBodyHTML, source.String())
		if err != nil {
			t.Fatal(err)
		}
		if content.HTML != wantHTML || !contentDiagnosticsEqual(content.Diagnostics, wantDiagnostics) ||
			content.Plain != HTMLToPlainText([]byte(wantHTML)) {
			t.Fatalf("content differs from reference: source = %q, HTML = %q, want HTML = %q, plain = %q, want plain = %q, diagnostics = %+v, want diagnostics = %+v", source.String(), content.HTML, wantHTML, content.Plain, HTMLToPlainText([]byte(wantHTML)), content.Diagnostics, wantDiagnostics)
		}
	})
}
