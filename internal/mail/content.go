package mail

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"golang.org/x/net/html"
)

// markdown is a package-level goldmark instance to avoid re-allocating the
// parser/renderer on every draft body conversion.
var markdown = goldmark.New()

type draftContentObserver interface {
	ContentRendered()
}

type preparedDraftContent struct {
	Format      DraftBodyFormat
	Source      string
	Plain       string
	HTML        string
	Diagnostics []ContentDiagnostic
}

func prepareDraftContent(format DraftBodyFormat, source string) (preparedDraftContent, error) {
	return prepareDraftContentWithObserver(context.Background(), format, source, nil)
}

func prepareDraftContentWithObserver(
	ctx context.Context,
	format DraftBodyFormat,
	source string,
	observer draftContentObserver,
) (preparedDraftContent, error) {
	if err := ctx.Err(); err != nil {
		return preparedDraftContent{}, err
	}
	if observer != nil {
		observer.ContentRendered()
	}
	if err := ctx.Err(); err != nil {
		return preparedDraftContent{}, err
	}
	if format == "" {
		format = DraftBodyPlain
	}
	if len(source) > MaximumDraftBodyBytes {
		return preparedDraftContent{}, validationError("draft body exceeds 4 MiB")
	}
	switch format {
	case DraftBodyPlain:
		return preparedDraftContent{Format: format, Plain: source}, nil
	case DraftBodyMarkdown:
		return renderMarkdownContent(ctx, source)
	case DraftBodyHTML:
		return canonicalRichContent(ctx, format, source, strings.NewReader(source))
	default:
		return preparedDraftContent{}, validationError("draft body format must be plain, markdown, or html")
	}
}

func validateStoredDraftContent(draft Draft) error {
	return validateStoredDraftContentWithObserver(&draft, nil)
}

func validateStoredDraftContentWithObserver(draft *Draft, observer draftContentObserver) error {
	if draft == nil {
		return validationError("stored draft is missing")
	}
	switch draft.BodyFormat {
	case DraftBodyPlain:
		if draft.BodySource != "" || draft.BodyHTML != "" || len(draft.ContentDiagnostics) > 0 {
			return validationError("plain draft contains unexpected rich content")
		}
	case DraftBodyMarkdown, DraftBodyHTML:
		if draft.BodySource == "" && (draft.Body != "" || draft.BodyHTML != "") {
			return validationError("rich draft is missing its source body")
		}
		prepared, err := prepareDraftContentWithObserver(context.Background(), draft.BodyFormat, draft.BodySource, observer)
		if err != nil {
			return err
		}
		if prepared.Plain != draft.Body || prepared.HTML != draft.BodyHTML {
			return validationError("stored rich draft does not match its canonical rendering")
		}
		// Drafts written before diagnostics existed remain readable. New writes
		// always carry the computed values, and a present value is integrity
		// checked against the canonical transformation.
		if draft.ContentDiagnostics != nil &&
			!contentDiagnosticsEqual(prepared.Diagnostics, draft.ContentDiagnostics) {
			return validationError("stored rich draft diagnostics do not match its canonical rendering")
		}
		draft.ContentDiagnostics = prepared.Diagnostics
	default:
		return validationError("stored draft has an unsupported body format")
	}
	return validateStoredDraftLimits(*draft)
}

// validateStoredDraftContentStructuralWithObserver checks stored shape,
// format, and limits without re-rendering rich bodies. A draft whose stored
// diagnostics are absent (legacy layout) still runs the canonical pass so
// display receives computed diagnostics; a present diagnostics value is
// trusted on read paths and re-verified canonically on mutation gates.
func validateStoredDraftContentStructuralWithObserver(draft *Draft, observer draftContentObserver) error {
	if draft == nil {
		return validationError("stored draft is missing")
	}
	switch draft.BodyFormat {
	case DraftBodyPlain:
		if draft.BodySource != "" || draft.BodyHTML != "" || len(draft.ContentDiagnostics) > 0 {
			return validationError("plain draft contains unexpected rich content")
		}
	case DraftBodyMarkdown, DraftBodyHTML:
		if draft.BodySource == "" && (draft.Body != "" || draft.BodyHTML != "") {
			return validationError("rich draft is missing its source body")
		}
		if len(draft.BodySource) > MaximumDraftBodyBytes {
			return validationError("draft body exceeds 4 MiB")
		}
		if draft.ContentDiagnostics == nil {
			prepared, err := prepareDraftContentWithObserver(
				context.Background(), draft.BodyFormat, draft.BodySource, observer,
			)
			if err != nil {
				return err
			}
			if prepared.Plain != draft.Body || prepared.HTML != draft.BodyHTML {
				return validationError("stored rich draft does not match its canonical rendering")
			}
			draft.ContentDiagnostics = prepared.Diagnostics
		}
	default:
		return validationError("stored draft has an unsupported body format")
	}
	return validateStoredDraftLimits(*draft)
}

func renderMarkdownContent(ctx context.Context, source string) (preparedDraftContent, error) {
	rendered := draftHTMLWriter{ctx: ctx}
	if err := markdown.Convert([]byte(source), &rendered); err != nil {
		return preparedDraftContent{}, fmt.Errorf("render Markdown body: %w", err)
	}
	return canonicalRichContent(ctx, DraftBodyMarkdown, source, strings.NewReader(rendered.value.String()))
}

func canonicalRichContent(ctx context.Context, format DraftBodyFormat, source string, input io.Reader) (preparedDraftContent, error) {
	body, diagnostics, err := sanitizeEmailHTML(ctx, input)
	if err != nil {
		return preparedDraftContent{}, err
	}
	sanitized, err := renderSanitizedHTML(ctx, body)
	if err != nil {
		return preparedDraftContent{}, err
	}
	plain, err := renderPreparedDraftText(ctx, body, sanitized)
	if err != nil {
		return preparedDraftContent{}, err
	}
	if len(plain) > MaximumDraftBodyBytes {
		return preparedDraftContent{}, validationError("plain-text draft body exceeds 4 MiB")
	}
	return preparedDraftContent{
		Format: format, Source: source, Plain: plain, HTML: sanitized, Diagnostics: diagnostics,
	}, nil
}

type contentDiagnosticCollector struct {
	values []ContentDiagnostic
	seen   map[string]struct{}
}

func (collector *contentDiagnosticCollector) add(code, element, attribute string) {
	if collector == nil || code == "" {
		return
	}
	if collector.seen == nil {
		collector.seen = make(map[string]struct{})
	}
	key := code + "\x00" + element + "\x00" + attribute
	if _, exists := collector.seen[key]; exists {
		return
	}
	collector.seen[key] = struct{}{}
	collector.values = append(collector.values, ContentDiagnostic{
		Code: code, Element: element, Attribute: attribute,
	})
}

func contentDiagnosticsEqual(left, right []ContentDiagnostic) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sanitizeEmailHTML(ctx context.Context, input io.Reader) (*html.Node, []ContentDiagnostic, error) {
	document, err := html.Parse(contextReader{ctx: ctx, reader: input})
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, nil, contextErr
		}
		return nil, nil, validationError("draft body contains invalid HTML")
	}
	if err := validateDraftContentTree(ctx, document); err != nil {
		return nil, nil, err
	}
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
	body.Attr = nil
	if err := sanitizeHTMLChildren(ctx, body, &diagnostics); err != nil {
		return nil, nil, err
	}
	return body, diagnostics.values, nil
}

func collectHTMLDiagnostics(node *html.Node, diagnostics *contentDiagnosticCollector) {
	if node == nil {
		return
	}
	if node.Type != html.ElementNode {
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			collectHTMLDiagnostics(child, diagnostics)
		}
		return
	}
	name := strings.ToLower(node.Data)
	if dropsHTMLSubtree(name) {
		diagnostics.add(ContentDiagnosticRemovedElement, name, "")
		recordDroppedResourceDiagnostics(name, node.Attr, diagnostics)
		return
	}
	recordRemovedAttributes(name, node.Attr, diagnostics)
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		collectHTMLDiagnostics(child, diagnostics)
	}
}

func sanitizeHTMLNode(ctx context.Context, node, parent, before *html.Node, diagnostics *contentDiagnosticCollector) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	switch node.Type {
	case html.TextNode:
	case html.ElementNode:
		name := strings.ToLower(node.Data)
		if dropsHTMLSubtree(name) {
			diagnostics.add(ContentDiagnosticRemovedElement, name, "")
			recordDroppedResourceDiagnostics(name, node.Attr, diagnostics)
			alt := ""
			if name == "img" {
				alt = htmlAttributeValue(node.Attr, "alt")
			}
			if alt == "" {
				node.Parent.RemoveChild(node)
				return false, nil
			}
			for node.FirstChild != nil {
				node.RemoveChild(node.FirstChild)
			}
			node.Type, node.Data, node.DataAtom, node.Namespace, node.Attr = html.TextNode, alt, 0, "", nil
			break
		}
		if !allowsHTMLElement(name) {
			return unwrapSanitizedHTMLNode(ctx, node, parent, before, diagnostics)
		}
		if err := sanitizeHTMLChildren(ctx, node, diagnostics); err != nil {
			return false, err
		}
		node.Data, node.DataAtom, node.Namespace = name, 0, ""
		node.Attr = sanitizeElementAttributes(name, node.Attr, diagnostics)
	default:
		node.Parent.RemoveChild(node)
		return false, nil
	}
	if node.Parent != parent {
		node.Parent.RemoveChild(node)
		parent.InsertBefore(node, before)
	}
	return true, nil
}

// Descendants skip removed wrappers and move directly to their retained
// parent. Each surviving node is reparented at most once, even through deep
// wrapper chains, and text is joined only at that final parent.
func unwrapSanitizedHTMLNode(ctx context.Context, node, parent, before *html.Node, diagnostics *contentDiagnosticCollector) (bool, error) {
	retained := false
	for child := node.FirstChild; child != nil; {
		next := child.NextSibling
		keep, err := sanitizeHTMLNode(ctx, child, parent, before, diagnostics)
		if err != nil {
			return false, err
		}
		retained = retained || keep
		child = next
	}
	name := strings.ToLower(node.Data)
	recordRemovedAttributes(name, node.Attr, diagnostics)
	if !retained {
		diagnostics.add(ContentDiagnosticRemovedElement, name, "")
	}
	node.Parent.RemoveChild(node)
	return retained, nil
}

func sanitizeHTMLChildren(ctx context.Context, node *html.Node, diagnostics *contentDiagnosticCollector) error {
	for child := node.FirstChild; child != nil; {
		next := child.NextSibling
		if _, err := sanitizeHTMLNode(ctx, child, node, child, diagnostics); err != nil {
			return err
		}
		child = next
	}
	return mergeHTMLTextNodes(ctx, node)
}

func sanitizeElementAttributes(name string, attributes []html.Attribute, diagnostics *contentDiagnosticCollector) []html.Attribute {
	if name != "a" {
		recordRemovedAttributes(name, attributes, diagnostics)
		return nil
	}
	clean := make([]html.Attribute, 0, 3)
	hrefSeen := false
	titleSeen := false
	for _, attribute := range attributes {
		key := strings.ToLower(strings.TrimSpace(attribute.Key))
		switch key {
		case "href":
			if hrefSeen {
				diagnostics.add(ContentDiagnosticRemovedAttribute, name, key)
				continue
			}
			parsed, err := url.Parse(strings.TrimSpace(attribute.Val))
			if err == nil && parsed.IsAbs() && allowsLinkScheme(parsed.Scheme) {
				clean = append(clean, html.Attribute{Key: key, Val: strings.TrimSpace(attribute.Val)})
				hrefSeen = true
			} else {
				diagnostics.add(ContentDiagnosticUnsafeURL, name, key)
			}
		case "title":
			if titleSeen {
				diagnostics.add(ContentDiagnosticRemovedAttribute, name, key)
				continue
			}
			clean = append(clean, html.Attribute{Key: key, Val: attribute.Val})
			titleSeen = true
		default:
			recordRemovedAttribute(name, key, attribute.Val, diagnostics)
		}
	}
	clean = append(clean, html.Attribute{Key: "rel", Val: "nofollow noreferrer"})
	return clean
}

func recordRemovedAttributes(element string, attributes []html.Attribute, diagnostics *contentDiagnosticCollector) {
	for _, attribute := range attributes {
		recordRemovedAttribute(element, strings.ToLower(strings.TrimSpace(attribute.Key)), attribute.Val, diagnostics)
	}
}

func recordRemovedAttribute(element, attribute, value string, diagnostics *contentDiagnosticCollector) {
	if attribute == "" {
		return
	}
	switch {
	case strings.HasPrefix(attribute, "on"):
		diagnostics.add(ContentDiagnosticUnsafeAttribute, element, attribute)
	case attribute == "style":
		diagnostics.add(ContentDiagnosticUnsafeStyle, element, attribute)
	case isRemoteResourceAttribute(attribute) && isRemoteURL(value):
		diagnostics.add(ContentDiagnosticRemoteResource, element, attribute)
	default:
		diagnostics.add(ContentDiagnosticRemovedAttribute, element, attribute)
	}
}

func recordDroppedResourceDiagnostics(element string, attributes []html.Attribute, diagnostics *contentDiagnosticCollector) {
	for _, attribute := range attributes {
		key := strings.ToLower(strings.TrimSpace(attribute.Key))
		if !isRemoteResourceAttribute(key) {
			continue
		}
		if isRemoteURL(attribute.Val) {
			diagnostics.add(ContentDiagnosticRemoteResource, element, key)
			continue
		}
		if key == "src" || key == "srcset" || key == "poster" {
			parsed, err := url.Parse(strings.TrimSpace(attribute.Val))
			if err == nil && parsed.Scheme != "" && !allowsLinkScheme(parsed.Scheme) {
				diagnostics.add(ContentDiagnosticUnsafeURL, element, key)
			}
		}
	}
}

func isRemoteResourceAttribute(attribute string) bool {
	switch attribute {
	case "action", "background", "cite", "data", "formaction", "href", "poster", "src", "srcset":
		return true
	default:
		return false
	}
}

func isRemoteURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || !parsed.IsAbs() {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return true
	default:
		return false
	}
}

func htmlAttributeValue(attributes []html.Attribute, key string) string {
	for _, attribute := range attributes {
		if strings.EqualFold(attribute.Key, key) {
			return attribute.Val
		}
	}
	return ""
}

func allowsLinkScheme(value string) bool {
	switch strings.ToLower(value) {
	case "http", "https", "mailto":
		return true
	default:
		return false
	}
}

func allowsHTMLElement(value string) bool {
	switch value {
	case "a", "b", "blockquote", "br", "code", "del", "em", "h1", "h2", "h3", "h4", "h5", "h6",
		"hr", "i", "li", "ol", "p", "pre", "s", "strong", "table", "tbody", "td", "tfoot", "th", "thead", "tr", "u", "ul":
		return true
	default:
		return false
	}
}

func dropsHTMLSubtree(value string) bool {
	switch value {
	case "applet", "audio", "base", "button", "canvas", "embed", "form", "frame", "frameset", "iframe", "img", "input", "link", "math", "noscript", "object", "option", "script", "select", "source", "style", "template", "textarea", "track", "video":
		return true
	default:
		return false
	}
}

func findHTMLElement(node *html.Node, name string) *html.Node {
	if node.Type == html.ElementNode && node.Data == name {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := findHTMLElement(child, name); found != nil {
			return found
		}
	}
	return nil
}

// HTMLToPlainText converts safe or incoming HTML to deterministic readable
// text. It preserves link destinations, code whitespace, list nesting and
// table cell boundaries while ignoring active or remotely loaded content.
func HTMLToPlainText(source []byte) string {
	text, err := htmlDraftText(bytes.NewReader(source))
	if err != nil {
		return ""
	}
	return text
}

type plainTextToken struct {
	text      string
	preserve  bool
	block     bool
	raw       bool
	lineBreak bool
}

type plainTextLink struct {
	href          string
	label         []byte
	nestedTargets []string
}

type plainTextList struct {
	ordered bool
	next    int
}

type plainTextTable struct {
	inRow     bool
	cellCount int
}

type plainTextRenderer struct {
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

func htmlDraftText(reader io.Reader) (string, error) {
	root, err := html.Parse(reader)
	if err != nil {
		return "", err
	}
	return renderDraftText(context.Background(), root, 0)
}

func renderDraftText(ctx context.Context, root *html.Node, maximumBytes int) (string, error) {
	renderer := plainTextRenderer{ctx: ctx, maximumBytes: maximumBytes, tokens: make([]plainTextToken, 0, 32)}
	renderer.renderNode(root)
	if renderer.err != nil {
		return "", renderer.err
	}
	return renderer.text()
}

func (renderer *plainTextRenderer) renderNode(node *html.Node) {
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

func (renderer *plainTextRenderer) ignoreStructuralWhitespace(node *html.Node) bool {
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

func (renderer *plainTextRenderer) renderElement(node *html.Node) {
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

func (renderer *plainTextRenderer) renderChildren(node *html.Node) {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		renderer.renderNode(child)
	}
}

func (renderer *plainTextRenderer) renderLink(node *html.Node) {
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

func (renderer *plainTextRenderer) renderList(node *html.Node, ordered bool) {
	renderer.appendBlockBreak()
	renderer.lists = append(renderer.lists, plainTextList{ordered: ordered, next: 1})
	renderer.renderChildren(node)
	renderer.lists = renderer.lists[:len(renderer.lists)-1]
	renderer.appendBlockBreak()
}

func (renderer *plainTextRenderer) renderListItem(node *html.Node) {
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

func (renderer *plainTextRenderer) renderTable(node *html.Node) {
	renderer.appendBlockBreak()
	renderer.tables = append(renderer.tables, plainTextTable{})
	renderer.renderChildren(node)
	renderer.tables = renderer.tables[:len(renderer.tables)-1]
	renderer.appendBlockBreak()
}

func (renderer *plainTextRenderer) renderTableRow(node *html.Node) {
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

func (renderer *plainTextRenderer) renderTableCell(node *html.Node) {
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

func (renderer *plainTextRenderer) appendText(value string) {
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

func (renderer *plainTextRenderer) appendRaw(value string) {
	if renderer.err != nil || value == "" {
		return
	}
	renderer.tokens = append(renderer.tokens, plainTextToken{text: value, preserve: true, raw: true})
}

func (renderer *plainTextRenderer) appendBreak() {
	renderer.tokens = append(renderer.tokens, plainTextToken{lineBreak: true})
	renderer.linePrefix = false
}

func (renderer *plainTextRenderer) appendBlockBreak() {
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

func (renderer *plainTextRenderer) text() (string, error) {
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

type plainTextOutput struct {
	ctx           context.Context
	err           error
	maximumBytes  int
	output        strings.Builder
	pendingBreaks int
	pendingSpace  bool
}

func (output *plainTextOutput) write(token plainTextToken) {
	if output.err != nil {
		return
	}
	if output.err = output.ctx.Err(); output.err != nil {
		return
	}
	if token.lineBreak {
		output.pendingSpace = false
		if output.output.Len() > 0 && output.pendingBreaks < 2 {
			output.pendingBreaks++
		}
		return
	}
	if token.text == "" {
		return
	}
	if token.preserve {
		output.writePreserved(token)
		return
	}
	output.writeProse(token.text)
}

func (output *plainTextOutput) writePreserved(token plainTextToken) {
	value := normalizeLineEndings(token.text)
	if token.raw && output.pendingSpace && output.output.Len() > 0 && output.pendingBreaks == 0 {
		output.writeString(" ")
		value = strings.TrimLeft(value, " \t")
	} else if !token.block && output.pendingSpace && output.output.Len() > 0 && output.pendingBreaks == 0 {
		output.writeString(" ")
	}
	output.flushBreaks()
	output.pendingSpace = false
	output.writeString(value)
}

func (output *plainTextOutput) writeProse(value string) {
	nextContextCheck := 0
	for index := 0; index < len(value); {
		if output.err != nil {
			return
		}
		if index >= nextContextCheck {
			if output.err = output.ctx.Err(); output.err != nil {
				return
			}
			nextContextCheck = index + 4096
		}
		character, size := utf8.DecodeRuneInString(value[index:])
		index += size
		if unicode.IsSpace(character) {
			if output.output.Len() > 0 && output.pendingBreaks == 0 {
				output.pendingSpace = true
			}
			continue
		}
		output.flushBreaks()
		if output.pendingSpace && output.output.Len() > 0 {
			output.writeString(" ")
		}
		output.pendingSpace = false
		if output.canAppend(utf8.RuneLen(character)) {
			if _, err := output.output.WriteRune(character); err != nil {
				output.err = err
			}
		}
	}
}

func (output *plainTextOutput) flushBreaks() {
	for output.pendingBreaks > 0 {
		output.writeString("\n")
		output.pendingBreaks--
	}
}

func (output *plainTextOutput) writeString(value string) {
	if output.canAppend(len(value)) {
		if _, err := output.output.WriteString(value); err != nil {
			output.err = err
		}
	}
}

func (output *plainTextOutput) canAppend(size int) bool {
	if output.err != nil {
		return false
	}
	if output.maximumBytes > 0 && size > output.maximumBytes-output.output.Len() {
		output.err = validationError("plain-text draft body exceeds 4 MiB")
		return false
	}
	return true
}

func normalizeLineEndings(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	return strings.ReplaceAll(value, "\r", "\n")
}

func plainLinkTarget(node *html.Node) string {
	for _, attribute := range node.Attr {
		if !strings.EqualFold(attribute.Key, "href") {
			continue
		}
		parsed, err := url.Parse(strings.TrimSpace(attribute.Val))
		if err == nil && parsed.IsAbs() && allowsLinkScheme(parsed.Scheme) {
			return strings.TrimSpace(attribute.Val)
		}
	}
	return ""
}

func normalizeLinkLabel(value []byte) string {
	var output strings.Builder
	pendingSpace := false
	for len(value) > 0 {
		character, size := utf8.DecodeRune(value)
		value = value[size:]
		if unicode.IsSpace(character) {
			pendingSpace = true
			continue
		}
		if pendingSpace && output.Len() > 0 {
			output.WriteByte(' ')
		}
		pendingSpace = false
		output.WriteRune(character)
	}
	return strings.TrimSpace(output.String())
}

func redundantLinkTarget(label, href string) bool {
	if strings.EqualFold(label, strings.TrimSpace(href)) {
		return true
	}
	parsed, err := url.Parse(href)
	if err != nil || !strings.EqualFold(parsed.Scheme, "mailto") {
		return false
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	address := strings.TrimSpace(parsed.Opaque)
	if address == "" {
		address = strings.TrimSpace(parsed.Path)
	}
	return strings.EqualFold(label, address)
}

func containsLinkTarget(targets []string, target string) bool {
	for _, value := range targets {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

func dropsPlainTextSubtree(name string) bool {
	switch name {
	case "applet", "audio", "base", "button", "canvas", "embed", "form", "frame", "frameset", "head", "iframe", "input", "link", "math", "meta", "noscript", "object", "option", "script", "select", "source", "style", "svg", "template", "textarea", "track", "video":
		return true
	default:
		return false
	}
}

func isPlainTextBlock(name string) bool {
	switch name {
	case "address", "article", "aside", "blockquote", "caption", "dd", "details", "div", "dl", "dt", "figcaption", "figure", "footer", "form", "h1", "h2", "h3", "h4", "h5", "h6", "header", "main", "nav", "p", "section", "summary", "title":
		return true
	default:
		return false
	}
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func normalizeDraftText(value string) string {
	normalized := value
	if strings.Contains(value, "\r\n") {
		normalized = strings.ReplaceAll(value, "\r\n", "\n")
	}
	lines := strings.Split(normalized, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimRightFunc(collapseHorizontalSpace(line), unicode.IsSpace)
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

func collapseHorizontalSpace(value string) string {
	var output strings.Builder
	output.Grow(len(value))
	space := false
	for i := 0; i < len(value); {
		c := value[i]
		if c < utf8.RuneSelf {
			// ASCII fast path
			if isASCIIWhitespace(c) {
				space = true
				i++
				continue
			}
			if space && output.Len() > 0 {
				output.WriteByte(' ')
			}
			space = false
			output.WriteByte(c)
			i++
			continue
		}
		// Multi-byte UTF-8
		r, size := utf8.DecodeRuneInString(value[i:])
		if unicode.IsSpace(r) {
			space = true
		} else {
			if space && output.Len() > 0 {
				output.WriteByte(' ')
			}
			space = false
			output.WriteRune(r)
		}
		i += size
	}
	return output.String()
}

// isASCIIWhitespace returns true for ASCII whitespace bytes (0x00-0x20).
func isASCIIWhitespace(c byte) bool {
	return c <= ' '
}
