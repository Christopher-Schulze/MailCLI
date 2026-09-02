package mail

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"golang.org/x/net/html"
)

// markdown is a package-level goldmark instance to avoid re-allocating the
// parser/renderer on every draft body conversion.
var markdown = goldmark.New()

type preparedDraftContent struct {
	Format DraftBodyFormat
	Source string
	Plain  string
	HTML   string
}

func prepareDraftContent(format DraftBodyFormat, source string) (preparedDraftContent, error) {
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
	switch draft.BodyFormat {
	case DraftBodyPlain:
		if draft.BodySource != "" || draft.BodyHTML != "" {
			return validationError("plain draft contains unexpected rich content")
		}
	case DraftBodyMarkdown, DraftBodyHTML:
		if draft.BodySource == "" && (draft.Body != "" || draft.BodyHTML != "") {
			return validationError("rich draft is missing its source body")
		}
		prepared, err := prepareDraftContent(draft.BodyFormat, draft.BodySource)
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
		"hr", "i", "li", "ol", "p", "pre", "s", "strong", "u", "ul":
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

func htmlDraftText(reader io.Reader) (string, error) {
	root, err := html.Parse(reader)
	if err != nil {
		return "", err
	}
	var output bytes.Buffer
	output.Grow(4096)
	writeHTMLText(&output, root)
	return normalizeDraftText(output.String()), nil
}

func writeHTMLText(output *bytes.Buffer, node *html.Node) {
	if node.Type == html.TextNode {
		if strings.TrimSpace(node.Data) != "" || !isBlockContainer(node.Parent) {
			output.WriteString(node.Data)
		}
	}
	if node.Type == html.ElementNode {
		switch node.Data {
		case "br", "hr":
			output.WriteByte('\n')
		case "li":
			ensureLineBreak(output)
			output.WriteString("- ")
		case "blockquote", "h1", "h2", "h3", "h4", "h5", "h6", "ol", "p", "pre", "ul":
			ensureLineBreak(output)
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		writeHTMLText(output, child)
	}
	if node.Type == html.ElementNode {
		switch node.Data {
		case "blockquote", "h1", "h2", "h3", "h4", "h5", "h6", "li", "ol", "p", "pre", "ul":
			ensureLineBreak(output)
		}
	}
}

func isBlockContainer(node *html.Node) bool {
	if node == nil || node.Type != html.ElementNode {
		return false
	}
	switch node.Data {
	case "body", "html", "ol", "ul":
		return true
	default:
		return false
	}
}

func ensureLineBreak(output *bytes.Buffer) {
	value := output.Bytes()
	if len(value) > 0 && value[len(value)-1] != '\n' {
		output.WriteByte('\n')
	}
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
