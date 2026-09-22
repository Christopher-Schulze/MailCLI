package mailstore

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"mailcli/internal/mail"
)

func normalizedSearchTerms(value string) []string {
	value = foldSearchText(value)
	var terms []string
	var current []byte
	flush := func() {
		if len(current) > 0 {
			term := string(current)
			current = current[:0]
			duplicate := false
			for _, existing := range terms {
				if term == existing {
					duplicate = true
					break
				}
			}
			if !duplicate {
				terms = append(terms, term)
			}
		}
	}
	for _, r := range value {
		if unicode.IsSpace(r) {
			flush()
		} else {
			current = utf8.AppendRune(current, r)
		}
	}
	flush()
	return terms
}

// foldSearchText defines the local search policy: canonical NFC normalization
// followed by Unicode simple lowercasing. Simple lowercasing keeps one rune
// per normalized input rune, so a boundary map is needed only when NFC changes
// the number of runes; full case-fold expansions such as ß -> ss would require
// a separate offset map.
func foldSearchText(value string) string {
	// ASCII is already NFC. ToLower returns unchanged input without allocating.
	for index := range len(value) {
		if value[index] >= utf8.RuneSelf {
			return foldUnicodeSearchText(value)
		}
	}
	return strings.ToLower(value)
}

func foldUnicodeSearchText(value string) string {
	value = norm.NFC.String(value)
	var folded strings.Builder
	folded.Grow(len(value))
	for _, r := range value {
		folded.WriteRune(unicode.ToLower(r))
	}
	return folded.String()
}

type searchTextRepresentations struct {
	original string
	folded   string
	// A nil map means folded and original text share rune boundaries. The map
	// is populated lazily when normalization changes those boundaries.
	foldedRuneBoundaries []int
}

func buildSearchTextRepresentations(item messageRecord, document mimeDocument) searchTextRepresentations {
	original := buildSearchText(item, document)
	return newSearchTextRepresentations(original)
}

func newSearchTextRepresentations(original string) searchTextRepresentations {
	folded := foldSearchText(original)
	return searchTextRepresentations{
		original: original,
		folded:   folded,
	}
}

func foldedRuneBoundaries(original, folded string) []int {
	if original == folded {
		return nil
	}
	originalRunes := utf8.RuneCountInString(original)
	foldedRunes := utf8.RuneCountInString(folded)
	// Simple lowercasing preserves one rune per normalized input rune. A
	// different byte representation with equal rune counts therefore still has
	// an implicit identity mapping.
	if originalRunes == foldedRunes {
		return nil
	}
	boundaries := make([]int, 1, foldedRunes+1)
	boundaries[0] = 0
	var iterator norm.Iter
	iterator.InitString(norm.NFC, original)
	originalRune := 0
	for !iterator.Done() {
		start := iterator.Pos()
		segment := iterator.Next()
		if len(segment) == 0 {
			break
		}
		originalRune += utf8.RuneCountInString(original[start:iterator.Pos()])
		for range utf8.RuneCount(segment) {
			boundaries = append(boundaries, originalRune)
		}
	}
	if len(boundaries) != foldedRunes+1 {
		boundaries = make([]int, foldedRunes+1)
		for index := range boundaries {
			boundaries[index] = min(index, originalRunes)
		}
	}
	return boundaries
}

func containsAllFoldedSearchTerms(folded string, terms []string) (bool, string) {
	first := ""
	for _, term := range terms {
		if !strings.Contains(folded, term) {
			return false, ""
		}
		if first == "" {
			first = term
		}
	}
	return len(terms) > 0, first
}

func snippetFor(value string, term string) string {
	value = collapseSearchText(value)
	representations := newSearchTextRepresentations(value)
	return snippetForSearchText(&representations, term)
}

func snippetForSearchText(representations *searchTextRepresentations, term string) string {
	value := representations.original
	if value == "" {
		return ""
	}
	runeCount := utf8.RuneCountInString(value)
	foldedRuneCount := utf8.RuneCountInString(representations.folded)
	start := 0
	if term != "" {
		byteIndex := strings.Index(representations.folded, foldSearchText(term))
		if byteIndex > 0 {
			start = utf8.RuneCountInString(representations.folded[:byteIndex]) - maximumSnippetRunes/3
			if start < 0 {
				start = 0
			}
		}
	}
	end := min(start+maximumSnippetRunes, foldedRuneCount)
	prefix := ""
	suffix := ""
	boundaries := representations.foldedRuneBoundaries
	if boundaries == nil {
		boundaries = foldedRuneBoundaries(representations.original, representations.folded)
		representations.foldedRuneBoundaries = boundaries
	}
	originalStart := originalRuneBoundary(boundaries, start)
	originalEnd := originalRuneBoundary(boundaries, end)
	if originalStart > 0 {
		prefix = "…"
	}
	if originalEnd < runeCount {
		suffix = "…"
	}
	startByte, endByte := runeByteRangeFast(value, originalStart, originalEnd)
	return prefix + value[startByte:endByte] + suffix
}

func originalRuneBoundary(boundaries []int, foldedBoundary int) int {
	if foldedBoundary < 0 {
		return 0
	}
	if len(boundaries) == 0 {
		return foldedBoundary
	}
	if foldedBoundary >= len(boundaries) {
		return boundaries[len(boundaries)-1]
	}
	return boundaries[foldedBoundary]
}

// runeByteRangeFast converts rune indices to byte indices in value.
func runeByteRangeFast(value string, startRune int, endRune int) (int, int) {
	index := 0
	startByte := 0
	for count := 0; count < endRune && index < len(value); count++ {
		if count == startRune {
			startByte = index
		}
		_, size := utf8.DecodeRuneInString(value[index:])
		index += size
	}
	if startRune == endRune {
		startByte = index
	}
	return startByte, index
}

func buildSearchText(item messageRecord, document mimeDocument) string {
	var builder collapsedSearchTextBuilder
	builder.Grow(searchTextCapacity(item, document))
	for _, value := range []string{
		item.Subject, item.SenderName, item.SenderAddress, item.SummaryText,
	} {
		builder.Add(value)
	}
	for _, recipients := range [][]mail.Recipient{document.To, document.CC, document.BCC} {
		for _, recipient := range recipients {
			builder.Add(recipient.Name)
			builder.Add(recipient.Address)
		}
	}
	names := make([]string, 0, len(document.Parts))
	for _, part := range document.Parts {
		names = append(names, part.Name)
	}
	sort.Strings(names)
	for _, name := range names {
		builder.Add(name)
	}
	builder.Add(document.Content)
	return builder.String()
}

// buildLoweredSearchText builds the folded form from the exact original
// representation used for snippets, including attachment-name ordering.
func buildLoweredSearchText(item messageRecord, document mimeDocument) string {
	return buildSearchTextRepresentations(item, document).folded
}

func searchTextCapacity(item messageRecord, document mimeDocument) int {
	size := len(item.Subject) + len(item.SenderName) + len(item.SenderAddress) +
		len(item.SummaryText) + len(document.Content) + 5 + len(document.Parts)
	for _, recipients := range [][]mail.Recipient{document.To, document.CC, document.BCC} {
		size += 2 * len(recipients)
		for _, recipient := range recipients {
			size += len(recipient.Name) + len(recipient.Address)
		}
	}
	for _, part := range document.Parts {
		size += len(part.Name)
	}
	return size
}

type collapsedSearchTextBuilder struct {
	output       strings.Builder
	pendingSpace bool
}

func (b *collapsedSearchTextBuilder) Grow(size int) {
	b.output.Grow(size)
}

func (b *collapsedSearchTextBuilder) Add(value string) {
	b.add(norm.NFC.String(value))
}

func (b *collapsedSearchTextBuilder) add(value string) {
	if value != "" && b.output.Len() > 0 {
		b.pendingSpace = true
	}
	for _, r := range value {
		if unicode.IsSpace(r) {
			b.pendingSpace = b.output.Len() > 0
			continue
		}
		if b.pendingSpace {
			b.output.WriteByte(' ')
			b.pendingSpace = false
		}
		b.output.WriteRune(r)
	}
}

// AddLowered adds value with the shared Unicode search policy and collapsed
// whitespace.
func (b *collapsedSearchTextBuilder) AddLowered(value string) {
	b.add(foldSearchText(value))
}

func (b *collapsedSearchTextBuilder) String() string {
	return b.output.String()
}

func containsLike(value string) string {
	return "%" + escapeLike(value) + "%"
}

func escapeLike(value string) string {
	var builder strings.Builder
	builder.Grow(len(value) + strings.Count(value, "\\") + strings.Count(value, "%") + strings.Count(value, "_"))
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\':
			builder.WriteString("\\\\")
		case '%':
			builder.WriteString("\\%")
		case '_':
			builder.WriteString("\\_")
		default:
			builder.WriteByte(value[i])
		}
	}
	return builder.String()
}
