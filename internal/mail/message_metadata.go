package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	messageMail "github.com/emersion/go-message/mail"
)

const (
	DefaultExcerptLength      = 240
	MaximumExcerptLength      = 1000
	MaximumExcerptSourceBytes = int64(256 * 1024)
	// IMAPExcerptTextBytes bounds the body-text prefix an IMAP excerpt fetch
	// reads per message, next to the MIME header fields.
	IMAPExcerptTextBytes = int64(64 * 1024)
)

type ExcerptSource string

const (
	ExcerptSourceLocal       ExcerptSource = "local"
	ExcerptSourceIMAPPartial ExcerptSource = "imap-partial"
	ExcerptSourceUnavailable ExcerptSource = "unavailable"
)

type HeaderField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type MessageEnrichmentRequest struct {
	Threading     bool
	Excerpt       bool
	ExcerptLength int
	// ExcerptSourceBudget bounds the excerpt source bytes one page may read;
	// zero means unlimited. Rows past the budget keep their reply metadata and
	// report EnrichmentBudgetExhausted for the excerpt.
	ExcerptSourceBudget int64
}

// EnrichmentBudgetExhausted is the enrichment_error of a row whose excerpt was
// skipped because the page used its excerpt source budget.
const EnrichmentBudgetExhausted = "enrichment_page_budget_exhausted"

// MessageEnrichmentGateway enriches a page of refs at once, returning one
// summary per ref in order, and never falls back to an unrestricted body read.
// On an expired deadline it may return indexed partial evidence with the exact
// context.DeadlineExceeded sentinel; each partial summary must name its ref.
type MessageEnrichmentGateway interface {
	EnrichMessages(context.Context, []string, MessageEnrichmentRequest) ([]MessageSummary, error)
}

func (s *Service) EnrichMessage(ctx context.Context, summary MessageSummary, request MessageEnrichmentRequest) (MessageSummary, error) {
	err := s.EnrichMessages(ctx, []*MessageSummary{&summary}, request)
	return summary, err
}

// EnrichMessages fills the requested reply metadata of every summary in place.
func (s *Service) EnrichMessages(ctx context.Context, summaries []*MessageSummary, request MessageEnrichmentRequest) error {
	if request.ExcerptLength < 1 || request.ExcerptLength > MaximumExcerptLength {
		return validationError("excerpt length must be between 1 and 1000")
	}
	reader, ok := s.gateway.(MessageEnrichmentGateway)
	if !ok || (!request.Threading && !request.Excerpt) || len(summaries) == 0 {
		return nil
	}
	refs := make([]string, len(summaries))
	for index, summary := range summaries {
		refs[index] = summary.Ref
	}
	enriched, err := reader.EnrichMessages(ctx, refs, request)
	if err != nil && (err != context.DeadlineExceeded || ctx.Err() != context.DeadlineExceeded) {
		return err
	}
	if len(enriched) != len(summaries) {
		return fmt.Errorf("message enrichment returned %d summaries for %d refs", len(enriched), len(summaries))
	}
	for index, metadata := range enriched {
		if (err != nil || metadata.Ref != "") && metadata.Ref != refs[index] {
			return fmt.Errorf("message enrichment returned a mismatched ref at index %d", index)
		}
	}
	for index, summary := range summaries {
		applyEnrichment(summary, enriched[index], request)
	}
	return err
}

func applyEnrichment(summary *MessageSummary, metadata MessageSummary, request MessageEnrichmentRequest) {
	if request.Threading {
		if summary.MessageID == "" {
			summary.MessageID = metadata.MessageID
		}
		summary.InReplyTo, summary.References = metadata.InReplyTo, metadata.References
		summary.From, summary.ThreadingComplete = metadata.From, metadata.ThreadingComplete
		summary.ThreadingRequested = true
	}
	if request.Excerpt {
		summary.Excerpt, summary.ExcerptComplete, summary.ExcerptSource = metadata.Excerpt, metadata.ExcerptComplete, metadata.ExcerptSource
		summary.ExcerptRequested = true
	}
	summary.EnrichmentError = metadata.EnrichmentError
}

// MarshalJSON normalizes only new fields; historical values and omission
// rules remain those of MessageSummary's original JSON tags.
func (summary MessageSummary) MarshalJSON() ([]byte, error) {
	type encodedSummary MessageSummary
	// The outer fields shadow the embedded ones of the same JSON name; a nil
	// pointer omits the key.
	type leanSummary struct {
		encodedSummary
		MessageID         *string        `json:"message_id,omitempty"`
		InReplyTo         *[]string      `json:"in_reply_to,omitempty"`
		References        *[]string      `json:"references,omitempty"`
		From              *Recipient     `json:"from,omitempty"`
		ThreadingComplete *bool          `json:"threading_complete,omitempty"`
		Excerpt           *string        `json:"excerpt,omitempty"`
		ExcerptComplete   *bool          `json:"excerpt_complete,omitempty"`
		ExcerptSource     *ExcerptSource `json:"excerpt_source,omitempty"`
	}
	lean := leanSummary{encodedSummary: encodedSummary(summary)}
	if summary.MessageID != "" {
		lean.MessageID = &summary.MessageID
	}
	if summary.ThreadingRequested {
		if summary.InReplyTo == nil {
			summary.InReplyTo = []string{}
		}
		if summary.References == nil {
			summary.References = []string{}
		}
		lean.InReplyTo, lean.References = &summary.InReplyTo, &summary.References
		lean.From, lean.ThreadingComplete = &summary.From, &summary.ThreadingComplete
	}
	if summary.ExcerptRequested {
		if summary.ExcerptSource == "" {
			summary.ExcerptSource = ExcerptSourceUnavailable
		}
		lean.Excerpt, lean.ExcerptComplete, lean.ExcerptSource = &summary.Excerpt, &summary.ExcerptComplete, &summary.ExcerptSource
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(lean); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(output.Bytes(), []byte{'\n'}), nil
}

// ParseHeaderFields retains duplicates and original field spelling. Invalid
// physical lines make the block incomplete without discarding valid fields.
func ParseHeaderFields(raw string) ([]HeaderField, bool) {
	fields := []HeaderField{}
	complete := false
	valid := true
	var unfolded strings.Builder
	lines := strings.SplitAfter(raw, "\n")
	for _, physical := range lines {
		if physical == "" {
			break
		}
		line := strings.TrimSuffix(physical, "\n")
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			complete = true
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			if len(fields) == 0 {
				valid = false
				continue
			}
			if unfolded.Len() == 0 {
				unfolded.WriteString(fields[len(fields)-1].Value)
			}
			unfolded.WriteByte(' ')
			unfolded.WriteString(strings.TrimSpace(line))
			continue
		}
		name, value, found := strings.Cut(line, ":")
		if !found || !validHeaderName(name) {
			valid = false
			continue
		}
		if unfolded.Len() > 0 {
			fields[len(fields)-1].Value = unfolded.String()
			unfolded.Reset()
		}
		fields = append(fields, HeaderField{Name: name, Value: strings.TrimSpace(value)})
	}
	if unfolded.Len() > 0 {
		fields[len(fields)-1].Value = unfolded.String()
	}
	return fields, complete && valid
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, character := range name {
		if character < 33 || character > 126 || character == ':' {
			return false
		}
	}
	return true
}

func ApplyThreadingHeaders(summary *MessageSummary, raw string) {
	fields, complete := ParseHeaderFields(raw)
	summary.InReplyTo, summary.References = []string{}, []string{}
	var authorComplete bool
	summary.From, authorComplete = ParseHeaderAuthor(fields)
	summary.ThreadingComplete = complete && authorComplete
	summary.ThreadingRequested = true
	for _, field := range fields {
		switch strings.ToLower(field.Name) {
		case "in-reply-to", "references":
			ids, valid := scanMessageIDs(field.Value)
			if !valid || len(ids) == 0 {
				summary.ThreadingComplete = false
			}
			if strings.EqualFold(field.Name, "in-reply-to") {
				summary.InReplyTo = append(summary.InReplyTo, ids...)
			} else {
				summary.References = append(summary.References, ids...)
			}
		}
	}
}

// ParseHeaderAuthor retains the first available author and reports whether
// every From field identifies that same single author. An absent field is
// complete but supplies no author.
func ParseHeaderAuthor(fields []HeaderField) (Recipient, bool) {
	var author Recipient
	complete := true
	for _, field := range fields {
		if !strings.EqualFold(field.Name, "From") {
			continue
		}
		addresses, err := messageMail.ParseAddressList(field.Value)
		if err != nil || len(addresses) != 1 {
			complete = false
		}
		if err != nil || len(addresses) == 0 {
			continue
		}
		address := addresses[0].Address
		if at := strings.LastIndexByte(address, '@'); at >= 0 {
			address = address[:at+1] + strings.ToLower(address[at+1:])
		}
		from := Recipient{Name: addresses[0].Name, Address: MailboxAddrSpec(address)}
		if author.Address == "" {
			author = from
		} else if author != from {
			complete = false
		}
	}
	return author, complete
}

// scanMessageIDs returns every well-formed msg-id in order and reports whether
// the whole value parsed; a malformed part never hides the valid IDs.
func scanMessageIDs(value string) ([]string, bool) {
	ids := []string{}
	valid := utf8.ValidString(value)
	var cleaned strings.Builder
	depth := 0
	escaped := false
	identifier, literal := false, false
	for index := 0; index < len(value); index++ {
		character := value[index]
		if depth == 0 && identifier && character == '[' {
			literal = true
		}
		if literal {
			cleaned.WriteByte(character)
			if character == ']' {
				literal = false
			}
			continue
		}
		if depth == 0 && character == '<' {
			identifier = true
		} else if depth == 0 && character == '>' {
			identifier = false
		}
		if escaped {
			escaped = false
			if depth == 0 {
				cleaned.WriteByte(character)
			}
			continue
		}
		if character == '\\' && depth > 0 {
			escaped = true
			continue
		}
		if character == '(' {
			if depth == 0 {
				cleaned.WriteByte(' ')
			}
			depth++
			continue
		}
		if character == ')' {
			if depth == 0 {
				valid = false
				cleaned.WriteByte(' ')
				continue
			}
			depth--
			continue
		}
		if depth == 0 {
			cleaned.WriteByte(character)
		}
	}
	if depth != 0 || escaped {
		valid = false
	}
	remaining := cleaned.String()
	for {
		start := strings.IndexByte(remaining, '<')
		if start < 0 {
			valid = valid && threadingSeparators(remaining)
			break
		}
		if !threadingSeparators(remaining[:start]) {
			valid = false
		}
		remaining = remaining[start+1:]
		end, nested := messageIDBoundary(remaining)
		if end < 0 {
			return ids, false
		}
		if nested >= 0 {
			valid = false
			remaining = remaining[nested:]
			continue
		}
		token := remaining[:end]
		if validMessageIDToken(token) {
			ids = append(ids, "<"+token+">")
		} else {
			valid = false
		}
		remaining = remaining[end+1:]
	}
	return ids, valid
}

// A no-fold-literal may contain parentheses, @ and angle brackets as dtext.
// Only delimiters outside that literal end or resynchronize a msg-id.
func messageIDBoundary(value string) (int, int) {
	literal := false
	nested := -1
	for index := 0; index < len(value); index++ {
		switch value[index] {
		case '[':
			literal = true
		case ']':
			literal = false
		case '<':
			if !literal {
				nested = index
			}
		case '>':
			if !literal {
				return index, nested
			}
		}
	}
	return -1, -1
}

func validMessageIDToken(token string) bool {
	if !utf8.ValidString(token) {
		return false
	}
	left, right, found := strings.Cut(token, "@")
	if !found || !messageIDDotAtom(left) {
		return false
	}
	if strings.HasPrefix(right, "[") && strings.HasSuffix(right, "]") {
		for _, character := range right[1 : len(right)-1] {
			if character < 33 || character == 127 || strings.ContainsRune("[]\\", character) {
				return false
			}
		}
		return true
	}
	return messageIDDotAtom(right)
}

func messageIDDotAtom(value string) bool {
	if value == "" || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") || strings.Contains(value, "..") {
		return false
	}
	for _, character := range value {
		if character >= utf8.RuneSelf {
			continue
		}
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune(".!#$%&'*+-/=?^_`{|}~", character) {
			continue
		}
		return false
	}
	return true
}

func threadingSeparators(value string) bool {
	for _, character := range value {
		if character != ',' && !unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

func BuildExcerpt(text string, length int) string {
	var kept strings.Builder
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if line == "-- " {
			break
		}
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), ">") {
			continue
		}
		kept.WriteString(line)
		kept.WriteByte(' ')
	}
	text = strings.Join(strings.Fields(kept.String()), " ")
	if utf8.RuneCountInString(text) <= length {
		return text
	}
	for index := range text {
		if length == 0 {
			return text[:index]
		}
		length--
	}
	return text
}
