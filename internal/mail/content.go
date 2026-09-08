package mail

import (
	"bytes"
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
	Format DraftBodyFormat
	Source string
	Plain  string
	HTML   string
}

func prepareDraftContent(format DraftBodyFormat, source string) (preparedDraftContent, error) {
	return prepareDraftContentWithObserver(format, source, nil)
}

func prepareDraftContentWithObserver(
	format DraftBodyFormat,
	source string,
	observer draftContentObserver,
) (preparedDraftContent, error) {
	if observer != nil {
		observer.ContentRendered()
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
		return renderMarkdownContent(source)
	case DraftBodyHTML:
		return canonicalRichContent(format, source, []byte(source))
	default:
		return preparedDraftContent{}, validationError("draft body format must be plain, markdown, or html")
	}
}

func validateStoredDraftContent(draft Draft) error {
	return validateStoredDraftContentWithObserver(draft, nil)
}

func validateStoredDraftContentWithObserver(draft Draft, observer draftContentObserver) error {
	switch draft.BodyFormat {
	case DraftBodyPlain:
		if draft.BodySource != "" || draft.BodyHTML != "" {
			return validationError("plain draft contains unexpected rich content")
		}
	case DraftBodyMarkdown, DraftBodyHTML:
		if draft.BodySource == "" && (draft.Body != "" || draft.BodyHTML != "") {
			return validationError("rich draft is missing its source body")
		}
		prepared, err := prepareDraftContentWithObserver(draft.BodyFormat, draft.BodySource, observer)
		if err != nil {
			return err
		}
		if prepared.Plain != draft.Body || prepared.HTML != draft.BodyHTML {
			return validationError("stored rich draft does not match its canonical rendering")
		}
	default:
		return validationError("stored draft has an unsupported body format")
	}
	return validateStoredDraftLimits(draft)
}

func renderMarkdownContent(source string) (preparedDraftContent, error) {
	var rendered bytes.Buffer
	if err := markdown.Convert([]byte(source), &rendered); err != nil {
		return preparedDraftContent{}, fmt.Errorf("render Markdown body: %w", err)
	}
	return canonicalRichContent(DraftBodyMarkdown, source, rendered.Bytes())
}

func canonicalRichContent(format DraftBodyFormat, source string, value []byte) (preparedDraftContent, error) {
	sanitized, err := sanitizeEmailHTML(value)
	if err != nil {
		return preparedDraftContent{}, validationError("draft body contains invalid HTML")
	}
	sanitized = bytes.TrimSpace(sanitized)
	if len(sanitized) > MaximumDraftBodyBytes {
		return preparedDraftContent{}, validationError("rendered draft body exceeds 4 MiB")
	}
	plain, err := htmlDraftText(bytes.NewReader(sanitized))
	if err != nil {
		return preparedDraftContent{}, validationError("draft body contains invalid HTML")
	}
	if len(plain) > MaximumDraftBodyBytes {
		return preparedDraftContent{}, validationError("plain-text draft body exceeds 4 MiB")
	}
	return preparedDraftContent{Format: format, Source: source, Plain: plain, HTML: string(sanitized)}, nil
}

func sanitizeEmailHTML(value []byte) ([]byte, error) {
	document, err := html.Parse(bytes.NewReader(value))
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	body := findHTMLElement(document, "body")
	if body == nil {
		body = document
	}
	for child := body.FirstChild; child != nil; child = child.NextSibling {
		for _, sanitized := range sanitizeHTMLNode(child) {
			if err := html.Render(&output, sanitized); err != nil {
				return nil, err
			}
		}
	}
	return output.Bytes(), nil
}

func sanitizeHTMLNode(node *html.Node) []*html.Node {
	switch node.Type {
	case html.TextNode:
		return []*html.Node{{Type: html.TextNode, Data: node.Data}}
	case html.ElementNode:
		if dropsHTMLSubtree(node.Data) {
			return nil
		}
		children := sanitizeHTMLChildren(node)
		if !allowsHTMLElement(node.Data) {
			return children
		}
		clean := &html.Node{Type: html.ElementNode, Data: node.Data}
		if node.Data == "a" {
			clean.Attr = sanitizeLinkAttributes(node.Attr)
		}
		for _, child := range children {
			clean.AppendChild(child)
		}
		return []*html.Node{clean}
	default:
		return nil
	}
}

func sanitizeHTMLChildren(node *html.Node) []*html.Node {
	var children []*html.Node
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		children = append(children, sanitizeHTMLNode(child)...)
	}
	return children
}

func sanitizeLinkAttributes(attributes []html.Attribute) []html.Attribute {
	clean := make([]html.Attribute, 0, 3)
	for _, attribute := range attributes {
		switch attribute.Key {
		case "href":
			parsed, err := url.Parse(attribute.Val)
			if err == nil && parsed.IsAbs() && allowsLinkScheme(parsed.Scheme) {
				clean = append(clean, html.Attribute{Key: "href", Val: attribute.Val})
			}
		case "title":
			clean = append(clean, html.Attribute{Key: "title", Val: attribute.Val})
		}
	}
	clean = append(clean, html.Attribute{Key: "rel", Val: "nofollow noreferrer"})
	return clean
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
	case "audio", "embed", "iframe", "img", "math", "object", "script", "style", "svg", "template", "video":
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
	label         strings.Builder
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
	renderer := plainTextRenderer{tokens: make([]plainTextToken, 0, 32)}
	renderer.renderNode(root)
	return renderer.text(), nil
}

func (renderer *plainTextRenderer) renderNode(node *html.Node) {
	if node == nil {
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
	label := normalizeLinkLabel(link.label.String())
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
	if value == "" {
		return
	}
	for index := range renderer.links {
		renderer.links[index].label.WriteString(value)
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
	if value == "" {
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

func (renderer *plainTextRenderer) text() string {
	output := plainTextOutput{}
	output.output.Grow(len(renderer.tokens) * 8)
	for _, token := range renderer.tokens {
		output.write(token)
	}
	return strings.Trim(output.output.String(), "\n")
}

type plainTextOutput struct {
	output        strings.Builder
	pendingBreaks int
	pendingSpace  bool
}

func (output *plainTextOutput) write(token plainTextToken) {
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
		output.output.WriteByte(' ')
		value = strings.TrimLeft(value, " \t")
	} else if !token.block && output.pendingSpace && output.output.Len() > 0 && output.pendingBreaks == 0 {
		output.output.WriteByte(' ')
	}
	output.flushBreaks()
	output.pendingSpace = false
	output.output.WriteString(value)
}

func (output *plainTextOutput) writeProse(value string) {
	for index := 0; index < len(value); {
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
			output.output.WriteByte(' ')
		}
		output.pendingSpace = false
		output.output.WriteRune(character)
	}
}

func (output *plainTextOutput) flushBreaks() {
	for output.pendingBreaks > 0 {
		output.output.WriteByte('\n')
		output.pendingBreaks--
	}
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

func normalizeLinkLabel(value string) string {
	var output strings.Builder
	pendingSpace := false
	for _, character := range value {
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
	case "audio", "embed", "head", "iframe", "img", "math", "meta", "object", "script", "style", "svg", "template", "video":
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
