package mail

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

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
	output         plainTextOutput
	previous       plainTextToken
	hasPrevious    bool
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
	text, err := renderPlainText(ctx, root, maximumBytes)
	if limit, ok := err.(*plainTextBudgetError); ok {
		if limit.resource == "link-label text" {
			return "", validationError("draft HTML exceeds 16 MiB of link-label text")
		}
		return "", validationError("plain-text draft body exceeds 4 MiB")
	}
	return text, err
}

func renderPlainText(ctx context.Context, root *html.Node, maximumBytes int) (string, error) {
	renderer := plainTextRenderer{ctx: ctx, maximumBytes: maximumBytes,
		output: plainTextOutput{ctx: ctx, maximumBytes: maximumBytes}}
	capacity := 256
	if maximumBytes > 0 {
		capacity = min(capacity, maximumBytes)
	}
	renderer.output.output.Grow(capacity)
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
		if renderer.err != nil {
			return
		}
		renderer.renderNode(child)
	}
}

func (renderer *plainTextRenderer) renderLink(node *html.Node) {
	renderer.links = append(renderer.links, plainTextLink{href: plainLinkTarget(node)})
	renderer.renderChildren(node)
	if renderer.err != nil {
		renderer.links = renderer.links[:len(renderer.links)-1]
		return
	}
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
			renderer.err = &plainTextBudgetError{resource: "link-label text", limit: 4 * renderer.maximumBytes}
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
	renderer.appendToken(plainTextToken{
		text: value, preserve: renderer.preDepth > 0 || renderer.codeDepth > 0,
		block: renderer.preDepth > 0,
	})
}

func (renderer *plainTextRenderer) appendRaw(value string) {
	if renderer.err != nil || value == "" {
		return
	}
	renderer.appendToken(plainTextToken{text: value, preserve: true, raw: true})
}

func (renderer *plainTextRenderer) appendBreak() {
	renderer.appendToken(plainTextToken{lineBreak: true})
	renderer.linePrefix = false
}

func (renderer *plainTextRenderer) appendToken(token plainTextToken) {
	if renderer.err != nil {
		return
	}
	renderer.output.write(token)
	renderer.err = renderer.output.err
	renderer.previous, renderer.hasPrevious = token, true
}

func (renderer *plainTextRenderer) appendBlockBreak() {
	if renderer.linePrefix {
		return
	}
	if renderer.hasPrevious && renderer.previous.lineBreak {
		return
	}
	if renderer.hasPrevious {
		last := renderer.previous
		if last.preserve && last.block && strings.HasSuffix(normalizeLineEndings(last.text), "\n") {
			return
		}
	}
	renderer.appendBreak()
}

func (renderer *plainTextRenderer) text() (string, error) {
	text := strings.Trim(renderer.output.output.String(), "\n")
	// A tiny trimmed result must not retain a large builder backing array.
	if renderer.output.output.Cap() >= 4096 && len(text) < renderer.output.output.Cap()/4 {
		text = strings.Clone(text)
	}
	return text, renderer.ctx.Err()
}

type plainTextBudgetError struct {
	resource string
	limit    int
}

func (err *plainTextBudgetError) Error() string {
	return fmt.Sprintf("plain-text %s exceeds %d bytes", err.resource, err.limit)
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
		output.err = &plainTextBudgetError{resource: "output", limit: output.maximumBytes}
		return false
	}
	return true
}
