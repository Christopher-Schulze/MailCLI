package mail

import (
	"context"
	"io"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

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
	diagnostics := contentDiagnosticCollector{values: []ContentDiagnostic{}}
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
