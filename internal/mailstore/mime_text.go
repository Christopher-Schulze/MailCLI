package mailstore

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"mailcli/internal/mail"
)

func htmlToText(source []byte) string {
	return mail.HTMLToPlainText(source)
}

func isASCIIWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n' || value == '\r' || value == '\f'
}

func hasVisibleText(value []byte) bool {
	for i := 0; i < len(value); {
		c := value[i]
		if c < utf8.RuneSelf {
			if !isASCIIWhitespace(c) {
				return true
			}
			i++
			continue
		}
		character, size := utf8.DecodeRune(value[i:])
		if !unicode.IsSpace(character) {
			return true
		}
		i += size
	}
	return false
}

func normalizeTextLayout(value string) string {
	var output strings.Builder
	output.Grow(len(value))
	pendingBreaks := 0
	pendingSpace := false
	for index := 0; index < len(value); {
		c := value[index]
		if c < utf8.RuneSelf {
			// ASCII fast path
			if c == '\r' && index+1 < len(value) && value[index+1] == '\n' {
				pendingSpace = false
				if output.Len() > 0 && pendingBreaks < 2 {
					pendingBreaks++
				}
				index += 2
				continue
			}
			if c == '\n' {
				pendingSpace = false
				if output.Len() > 0 && pendingBreaks < 2 {
					pendingBreaks++
				}
				index++
				continue
			}
			if isASCIIWhitespace(c) {
				if output.Len() > 0 && pendingBreaks == 0 {
					pendingSpace = true
				}
				index++
				continue
			}
			for pendingBreaks > 0 {
				output.WriteByte('\n')
				pendingBreaks--
			}
			if pendingSpace {
				output.WriteByte(' ')
				pendingSpace = false
			}
			output.WriteByte(c)
			index++
			continue
		}
		// Multi-byte UTF-8
		character, size := utf8.DecodeRuneInString(value[index:])
		index += size
		if unicode.IsSpace(character) {
			if output.Len() > 0 && pendingBreaks == 0 {
				pendingSpace = true
			}
			continue
		}
		for pendingBreaks > 0 {
			output.WriteByte('\n')
			pendingBreaks--
		}
		if pendingSpace {
			output.WriteByte(' ')
			pendingSpace = false
		}
		output.WriteRune(character)
	}
	return output.String()
}

func collapseSearchText(value string) string {
	var output strings.Builder
	output.Grow(len(value))
	pendingSpace := false
	for i := 0; i < len(value); {
		c := value[i]
		if c < utf8.RuneSelf {
			if isASCIIWhitespace(c) {
				pendingSpace = output.Len() > 0
				i++
				continue
			}
			if pendingSpace {
				output.WriteByte(' ')
				pendingSpace = false
			}
			output.WriteByte(c)
			i++
			continue
		}
		character, size := utf8.DecodeRuneInString(value[i:])
		if unicode.IsSpace(character) {
			pendingSpace = output.Len() > 0
		} else {
			if pendingSpace {
				output.WriteByte(' ')
				pendingSpace = false
			}
			output.WriteRune(character)
		}
		i += size
	}
	return output.String()
}
