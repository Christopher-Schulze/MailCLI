package cli

import (
	"io"
	"strings"
	"unicode"

	"golang.org/x/sys/unix"
)

const tableColumnGap = 2

func writeTerminalTable(writer io.Writer, headers []string, rows [][]string) bool {
	if !writerIsTerminal(writer) || len(headers) == 0 {
		return false
	}
	widths := make([]int, len(headers))
	for index, header := range headers {
		widths[index] = textWidth(header)
	}
	for _, row := range rows {
		for index := 0; index < len(row) && index < len(widths); index++ {
			widths[index] = max(widths[index], textWidth(oneLine(row[index])))
		}
	}
	if !fitTableWidths(widths, headers, terminalWidth(writer)) {
		return false
	}
	writeTableRow(writer, headers, widths)
	for _, row := range rows {
		writeTableRow(writer, row, widths)
	}
	return true
}

func writeTableRow(writer io.Writer, row []string, widths []int) {
	for index := range widths {
		value := ""
		if index < len(row) {
			value = oneLine(row[index])
		}
		value = truncateText(value, widths[index])
		writeRaw(writer, value)
		if index < len(widths)-1 {
			padding := widths[index] - textWidth(value) + tableColumnGap
			writeRaw(writer, strings.Repeat(" ", max(0, padding)))
		}
	}
	writeLine(writer)
}

func fitTableWidths(widths []int, headers []string, terminalColumns int) bool {
	if terminalColumns <= 0 {
		terminalColumns = 120
	}
	available := terminalColumns - tableColumnGap*(len(widths)-1)
	if available < len(widths) {
		return false
	}
	minimums := make([]int, len(widths))
	for index := range minimums {
		minimums[index] = max(3, textWidth(headers[index]))
	}
	shrinkTableWidths(widths, minimums, available)
	if sumWidths(widths) > available {
		for index := range minimums {
			minimums[index] = 1
		}
		shrinkTableWidths(widths, minimums, available)
	}
	return sumWidths(widths) <= available
}

func shrinkTableWidths(widths []int, minimums []int, available int) {
	for sumWidths(widths) > available {
		widest := -1
		for index, width := range widths {
			if width > minimums[index] && (widest < 0 || width > widths[widest]) {
				widest = index
			}
		}
		if widest < 0 {
			return
		}
		widths[widest]--
	}
}

func terminalWidth(writer io.Writer) int {
	fileDescriptor, ok := writerFileDescriptor(writer)
	if !ok {
		return 0
	}
	window, err := unix.IoctlGetWinsize(int(fileDescriptor), unix.TIOCGWINSZ)
	if err != nil {
		return 0
	}
	return int(window.Col)
}

func truncateText(value string, width int) string {
	if textWidth(value) <= width {
		return value
	}
	if width <= 0 {
		return ""
	}
	targetWidth := width - 1
	currentWidth := 0
	var state displayWidthState
	var truncated strings.Builder
	for _, character := range value {
		characterWidth := state.next(character)
		if currentWidth+characterWidth > targetWidth {
			break
		}
		truncated.WriteRune(character)
		currentWidth += characterWidth
	}
	truncated.WriteRune('…')
	return truncated.String()
}

func textWidth(value string) int {
	total := 0
	var state displayWidthState
	for _, character := range value {
		total += state.next(character)
	}
	return total
}

type displayWidthState struct {
	joinNext          bool
	regionalIndicator bool
}

func (s *displayWidthState) next(character rune) int {
	if character == '\u200d' {
		s.joinNext = true
		return 0
	}
	if character >= 0x1f3fb && character <= 0x1f3ff {
		return 0
	}
	if character >= 0x1f1e6 && character <= 0x1f1ff {
		if s.regionalIndicator {
			s.regionalIndicator = false
			return 0
		}
		s.regionalIndicator = true
		return 2
	}
	s.regionalIndicator = false
	characterWidth := runeDisplayWidth(character)
	if s.joinNext && characterWidth > 0 {
		s.joinNext = false
		return 0
	}
	if characterWidth > 0 {
		s.joinNext = false
	}
	return characterWidth
}

func runeDisplayWidth(character rune) int {
	if unicode.IsControl(character) || unicode.Is(unicode.Mn, character) ||
		unicode.Is(unicode.Me, character) || unicode.Is(unicode.Cf, character) {
		return 0
	}
	if isWideTerminalRune(character) {
		return 2
	}
	return 1
}

func isWideTerminalRune(character rune) bool {
	return character >= 0x1100 && (character <= 0x115f ||
		character == 0x2329 || character == 0x232a ||
		character >= 0x2e80 && character <= 0x303e ||
		character >= 0x3040 && character <= 0xa4cf && character != 0x303f ||
		character >= 0xac00 && character <= 0xd7a3 ||
		character >= 0xf900 && character <= 0xfaff ||
		character >= 0xfe10 && character <= 0xfe19 ||
		character >= 0xfe30 && character <= 0xfe6f ||
		character >= 0xff00 && character <= 0xff60 ||
		character >= 0xffe0 && character <= 0xffe6 ||
		character >= 0x1f000 && character <= 0x1faff ||
		character >= 0x20000 && character <= 0x3fffd)
}

func sumWidths(values []int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}
