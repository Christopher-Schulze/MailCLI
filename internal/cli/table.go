package cli

import (
	"io"
	"strings"
	"text/tabwriter"

	"golang.org/x/sys/unix"
)

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
	fitTableWidths(widths, headers, terminalWidth(writer))
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	writeTableRow(table, headers, widths)
	for _, row := range rows {
		writeTableRow(table, row, widths)
	}
	if err := table.Flush(); err != nil {
		recordWriteError(writer, err)
	}
	return true
}

func writeTableRow(writer io.Writer, row []string, widths []int) {
	for index := range widths {
		if index > 0 {
			writeRaw(writer, "\t")
		}
		value := ""
		if index < len(row) {
			value = oneLine(row[index])
		}
		writeRaw(writer, truncateText(value, widths[index]))
	}
	writeLine(writer)
}

func fitTableWidths(widths []int, headers []string, terminalColumns int) {
	if terminalColumns < 40 {
		terminalColumns = 120
	}
	available := terminalColumns - 2*(len(widths)-1)
	for sumWidths(widths) > available {
		widest := -1
		for index, width := range widths {
			minimum := max(8, textWidth(headers[index]))
			if width > minimum && (widest < 0 || width > widths[widest]) {
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
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width < 2 {
		return string(runes[:width])
	}
	return string(runes[:width-1]) + "…"
}

func textWidth(value string) int {
	return len([]rune(strings.TrimSpace(value)))
}

func sumWidths(values []int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}
