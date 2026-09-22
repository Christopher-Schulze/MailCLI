package mail

import (
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

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
