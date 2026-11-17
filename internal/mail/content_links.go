package mail

import (
	"bytes"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// LinkMode selects how the URLs of a plain message body are shown.
type LinkMode string

const (
	// LinkModeFull keeps the body unchanged.
	LinkModeFull LinkMode = "full"
	// LinkModeHost replaces every URL of 40 or more characters by <host>.
	LinkModeHost LinkMode = "host"
	// LinkModeNone removes every URL.
	LinkModeNone LinkMode = "none"
)

// shortenedLinkLength is the length from which host mode replaces a URL.
const shortenedLinkLength = 40

// Unicode whitespace consists of separator characters, ASCII whitespace
// controls and NEXT LINE (U+0085).
var linkPattern = regexp.MustCompile(`(?i)https?://[^\x09-\x0d\p{Z}\x{0085}<>"]+`)

var blankLineRun = regexp.MustCompile(`\n{3,}`)

// ParseLinkMode validates a --links value; an empty value means full.
func ParseLinkMode(value string) (LinkMode, error) {
	switch mode := LinkMode(strings.ToLower(strings.TrimSpace(value))); mode {
	case "", LinkModeFull:
		return LinkModeFull, nil
	case LinkModeHost, LinkModeNone:
		return mode, nil
	default:
		return "", validationError(fmt.Sprintf("links must be full, host or none, got %q", value))
	}
}

// ShortenLinks reduces the URLs of a plain body for token-frugal reading and
// collapses runs of blank lines in the reduced modes. Tracking URLs can make up
// most of a marketing mail; the full body stays available in full mode and in
// exports.
func ShortenLinks(text string, mode LinkMode) string {
	if mode != LinkModeHost && mode != LinkModeNone {
		return text
	}
	// One forward pass over a byte buffer: removing a wrapper only shortens the
	// buffer, so the text already produced is never copied again.
	var output []byte
	var matches [][]int
	if strings.Contains(text, "://") {
		matches = linkPattern.FindAllStringIndex(text, -1)
	}
	last := 0
	for _, match := range matches {
		address, tail := splitLinkTail(text[match[0]:match[1]])
		if mode == LinkModeHost && len(address) < shortenedLinkLength {
			continue
		}
		parsed, err := url.Parse(address)
		if err != nil {
			continue
		}
		host := parsed.Hostname()
		if host == "" {
			continue
		}
		if output == nil {
			output = make([]byte, 0, len(text))
		}
		output = append(output, text[last:match[0]]...)
		last = match[1]
		wrappedInBrackets := bytes.HasSuffix(output, []byte("<")) && strings.HasPrefix(text[match[1]:], ">")
		wrappedInParentheses := bytes.HasSuffix(output, []byte(" (")) && strings.HasPrefix(tail, ")")
		switch {
		case mode == LinkModeHost && wrappedInBrackets:
			output = append(output, host...)
		case mode == LinkModeHost:
			output = append(output, "<"+host+">"...)
		case wrappedInBrackets:
			output = output[:len(output)-len("<")]
			last++
		case wrappedInParentheses:
			output = output[:len(output)-len(" (")]
			tail = strings.TrimPrefix(tail, ")")
		}
		output = append(output, tail...)
	}
	if output != nil {
		text = string(append(output, text[last:]...))
	}
	if strings.Contains(text, "\n\n\n") {
		return blankLineRun.ReplaceAllString(text, "\n\n")
	}
	return text
}

// splitLinkTail separates trailing punctuation and unbalanced closing
// brackets from a matched URL.
func splitLinkTail(matched string) (string, string) {
	end := len(matched)
	var balances [3]int
	var counted [3]bool
	for end > 0 {
		switch character := matched[end-1]; character {
		case '.', ',', ';', ':', '!', '?', '\'':
			end--
			continue
		case ')', ']', '}':
			index := strings.IndexByte(")]}", character)
			if !counted[index] {
				balances[index] = strings.Count(matched[:end], string("([{"[index])) - strings.Count(matched[:end], string(character))
				counted[index] = true
			}
			if balances[index] >= 0 {
				return matched[:end], matched[end:]
			}
			balances[index]++
			end--
			continue
		}
		break
	}
	return matched[:end], matched[end:]
}
