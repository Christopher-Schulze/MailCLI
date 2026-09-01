package cli

import (
	"bytes"
	"testing"
)

func TestTextWidthUsesTerminalDisplayCells(t *testing.T) {
	if width := textWidth("A界e\u0301"); width != 4 {
		t.Fatalf("textWidth() = %d, want 4", width)
	}
}

func TestTextWidthTreatsEmojiSequencesAsOneWideCellPair(t *testing.T) {
	for _, value := range []string{"👩🏽", "👩‍💻", "🇩🇪"} {
		if width := textWidth(value); width != 2 {
			t.Fatalf("textWidth(%q) = %d, want 2", value, width)
		}
	}
}

func TestTruncateTextPreservesCombiningCharacterAndCellBudget(t *testing.T) {
	truncated := truncateText("e\u0301clair", 4)
	if truncated != "e\u0301cl…" || textWidth(truncated) != 4 {
		t.Fatalf("truncateText() = %q (%d cells)", truncated, textWidth(truncated))
	}
}

func TestFitTableWidthsRespectsNarrowTerminal(t *testing.T) {
	widths := []int{24, 18, 30, 40, 16}
	if !fitTableWidths(widths, []string{"REF", "DATE", "FROM", "SUBJECT", "MATCH"}, 20) {
		t.Fatal("fitTableWidths() = false")
	}
	if total := sumWidths(widths) + tableColumnGap*(len(widths)-1); total > 20 {
		t.Fatalf("fitted table width = %d, want at most 20", total)
	}
}

func TestWriteTableRowPadsWideAndCombiningTextByCells(t *testing.T) {
	var output bytes.Buffer
	writeTableRow(&output, []string{"界", "e\u0301"}, []int{4, 4})
	if output.String() != "界    e\u0301\n" {
		t.Fatalf("writeTableRow() = %q", output.String())
	}
}
