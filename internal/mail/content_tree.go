package mail

import (
	"context"
	"strings"

	"golang.org/x/net/html"
)

const maximumDraftContentNodes = 65536

// A sanitized tree can require HTML5 repair after wrappers are removed (for
// example, caption text inside a table or nested anchors). Reuse the tree only
// for this conservative grammar; let the existing parser resolve every other
// structure instead of implementing a second HTML5 tree-construction engine.
func renderPreparedDraftText(ctx context.Context, body *html.Node, sanitized string) (string, error) {
	if !stableDraftContentTree(body, false, false) {
		normalized, err := html.Parse(contextReader{ctx: ctx, reader: strings.NewReader(sanitized)})
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return "", contextErr
			}
			return "", validationError("draft body contains invalid HTML")
		}
		if err := validateDraftContentTree(ctx, normalized); err != nil {
			return "", err
		}
		body = normalized
	}
	return renderDraftText(ctx, body, MaximumDraftBodyBytes)
}

func stableDraftContentTree(node *html.Node, phrasing, anchor bool) bool {
	if node.Type == html.TextNode {
		return !strings.ContainsRune(node.Data, '\x00')
	}
	if node.Type != html.ElementNode || node.Namespace != "" {
		return false
	}
	if phrasing && !draftPhrasingElement(node.Data) || anchor && node.Data == "a" {
		return false
	}
	anchor = anchor || node.Data == "a"
	phrasing = phrasing || draftPhrasingElement(node.Data)
	switch node.Data {
	case "p", "h1", "h2", "h3", "h4", "h5", "h6", "pre":
		phrasing = true
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if !stableDraftContentChild(node.Data, child) || !stableDraftContentTree(child, phrasing, anchor) {
			return false
		}
	}
	return true
}

func stableDraftContentChild(parent string, child *html.Node) bool {
	switch parent {
	case "br", "hr":
		return false
	case "ol", "ul", "table", "tbody", "thead", "tfoot", "tr":
		if child.Type == html.TextNode {
			return strings.Trim(child.Data, " \t\r\n\f") == ""
		}
		if child.Type != html.ElementNode {
			return false
		}
		switch parent {
		case "ol", "ul":
			return child.Data == "li"
		case "table":
			return child.Data == "tbody" || child.Data == "thead" || child.Data == "tfoot"
		case "tbody", "thead", "tfoot":
			return child.Data == "tr"
		case "tr":
			return child.Data == "td" || child.Data == "th"
		}
	}
	switch child.Data {
	case "li", "tbody", "thead", "tfoot", "tr", "td", "th":
		return child.Type != html.ElementNode
	}
	return true
}

func draftPhrasingElement(name string) bool {
	switch name {
	case "a", "b", "br", "code", "del", "em", "i", "s", "strong", "u":
		return true
	default:
		return false
	}
}

// validateDraftContentTree bounds all work after parsing. The parser itself
// rejects open-element stacks over 512; source bytes are bounded at entry.
func validateDraftContentTree(ctx context.Context, root *html.Node) error {
	count := 0
	depth := 0
	for node := root; node != nil; {
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > maximumDraftContentNodes || depth > 512 {
			return validationError("draft HTML exceeds 65536 nodes or 512 levels")
		}
		if node.FirstChild != nil {
			node = node.FirstChild
			depth++
			continue
		}
		for node != root && node.NextSibling == nil {
			node = node.Parent
			depth--
		}
		if node == root {
			break
		}
		node = node.NextSibling
	}
	return nil
}

func mergeHTMLTextNodes(ctx context.Context, parent *html.Node) error {
	for node := parent.FirstChild; node != nil; node = node.NextSibling {
		if err := ctx.Err(); err != nil {
			return err
		}
		if node.Type != html.TextNode || node.NextSibling == nil || node.NextSibling.Type != html.TextNode {
			continue
		}
		var joined strings.Builder
		joined.WriteString(node.Data)
		for node.NextSibling != nil && node.NextSibling.Type == html.TextNode {
			if err := ctx.Err(); err != nil {
				return err
			}
			joined.WriteString(node.NextSibling.Data)
			parent.RemoveChild(node.NextSibling)
		}
		node.Data = joined.String()
	}
	return nil
}

func renderSanitizedHTML(ctx context.Context, body *html.Node) (string, error) {
	output := draftHTMLWriter{ctx: ctx}
	for child := body.FirstChild; child != nil; child = child.NextSibling {
		if err := html.Render(&output, child); err != nil {
			return "", err
		}
	}
	return strings.TrimSpace(output.value.String()), ctx.Err()
}

type draftHTMLWriter struct {
	ctx   context.Context
	value strings.Builder
}

func (writer *draftHTMLWriter) Write(value []byte) (int, error) {
	if err := writer.check(len(value)); err != nil {
		return 0, err
	}
	return writer.value.Write(value)
}

func (writer *draftHTMLWriter) WriteString(value string) (int, error) {
	if err := writer.check(len(value)); err != nil {
		return 0, err
	}
	return writer.value.WriteString(value)
}

func (writer *draftHTMLWriter) WriteByte(value byte) error {
	if err := writer.check(1); err != nil {
		return err
	}
	return writer.value.WriteByte(value)
}

func (writer *draftHTMLWriter) check(size int) error {
	if err := writer.ctx.Err(); err != nil {
		return err
	}
	if size > MaximumDraftBodyBytes-writer.value.Len() {
		return validationError("rendered draft body exceeds 4 MiB")
	}
	return nil
}
