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

var linkPattern = regexp.MustCompile(`(?i)https?://[^\s<>"]+`)

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
	output := make([]byte, 0, len(text))
	last := 0
	for _, match := range linkPattern.FindAllStringIndex(text, -1) {
		address, tail := splitLinkTail(text[match[0]:match[1]])
		parsed, err := url.Parse(address)
		if err != nil || parsed.Hostname() == "" {
			continue
		}
		if mode == LinkModeHost && len(address) < shortenedLinkLength {
			continue
		}
		output = append(output, text[last:match[0]]...)
		last = match[1]
		wrappedInBrackets := bytes.HasSuffix(output, []byte("<")) && strings.HasPrefix(text[match[1]:], ">")
		wrappedInParentheses := bytes.HasSuffix(output, []byte(" (")) && strings.HasPrefix(tail, ")")
		switch {
		case mode == LinkModeHost && wrappedInBrackets:
			output = append(output, parsed.Hostname()...)
		case mode == LinkModeHost:
			output = append(output, "<"+parsed.Hostname()+">"...)
		case wrappedInBrackets:
			output = output[:len(output)-len("<")]
			last++
		case wrappedInParentheses:
			output = output[:len(output)-len(" (")]
			tail = strings.TrimPrefix(tail, ")")
		}
		output = append(output, tail...)
	}
	output = append(output, text[last:]...)
	return blankLineRun.ReplaceAllString(string(output), "\n\n")
}

// splitLinkTail separates trailing punctuation and unbalanced closing
// brackets from a matched URL.
func splitLinkTail(matched string) (string, string) {
	end := len(matched)
	for end > 0 {
		switch character := matched[end-1]; character {
		case '.', ',', ';', ':', '!', '?', '\'':
			end--
			continue
		case ')', ']', '}':
			opening := map[byte]byte{')': '(', ']': '[', '}': '{'}[character]
			if strings.Count(matched[:end], string(opening)) >= strings.Count(matched[:end], string(character)) {
				return matched[:end], matched[end:]
			}
			end--
			continue
		}
		break
	}
	return matched[:end], matched[end:]
}
