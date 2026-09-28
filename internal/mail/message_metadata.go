package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
}

// MessageEnrichmentGateway enriches a page of refs at once, returning one
// summary per ref in order, and never falls back to an unrestricted body read.
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
	if err != nil {
		return err
	}
	if len(enriched) != len(summaries) {
		return fmt.Errorf("message enrichment returned %d summaries for %d refs", len(enriched), len(summaries))
	}
	for index, summary := range summaries {
		applyEnrichment(summary, enriched[index], request)
	}
	return nil
}

func applyEnrichment(summary *MessageSummary, metadata MessageSummary, request MessageEnrichmentRequest) {
	if request.Threading {
		if summary.MessageID == "" {
			summary.MessageID = metadata.MessageID
		}
		summary.InReplyTo, summary.References = metadata.InReplyTo, metadata.References
		summary.From, summary.ThreadingComplete = metadata.From, metadata.ThreadingComplete
	}
	if request.Excerpt {
		summary.Excerpt, summary.ExcerptComplete, summary.ExcerptSource = metadata.Excerpt, metadata.ExcerptComplete, metadata.ExcerptSource
	}
	summary.EnrichmentError = metadata.EnrichmentError
}

// MarshalJSON normalizes only new fields; historical values and omission
// rules remain those of MessageSummary's original JSON tags.
func (summary MessageSummary) MarshalJSON() ([]byte, error) {
	type encodedSummary MessageSummary
	if summary.InReplyTo == nil {
		summary.InReplyTo = []string{}
	}
	if summary.References == nil {
		summary.References = []string{}
	}
	if summary.ExcerptSource == "" {
		summary.ExcerptSource = ExcerptSourceUnavailable
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(encodedSummary(summary)); err != nil {
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
			fields[len(fields)-1].Value += " " + strings.TrimSpace(line)
			continue
		}
		name, value, found := strings.Cut(line, ":")
		if !found || !validHeaderName(name) {
			valid = false
			continue
		}
		fields = append(fields, HeaderField{Name: name, Value: strings.TrimSpace(value)})
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
	summary.ThreadingComplete = complete
	for _, field := range fields {
		switch strings.ToLower(field.Name) {
		case "in-reply-to", "references":
			ids, valid := scanMessageIDs(field.Value)
			if !valid {
				summary.ThreadingComplete = false
			}
			if strings.EqualFold(field.Name, "in-reply-to") {
				summary.InReplyTo = append(summary.InReplyTo, ids...)
			} else {
				summary.References = append(summary.References, ids...)
			}
		case "from":
			if summary.From.Address != "" {
				continue
			}
			addresses, err := messageMail.ParseAddressList(field.Value)
			if err == nil && len(addresses) > 0 {
				address := addresses[0].Address
				if at := strings.LastIndexByte(address, '@'); at >= 0 {
					address = address[:at+1] + strings.ToLower(address[at+1:])
				}
				summary.From = Recipient{Name: addresses[0].Name, Address: MailboxAddrSpec(address)}
			}
		}
	}
}

// scanMessageIDs returns every well-formed msg-id in order and reports whether
// the whole value parsed; a malformed part never hides the valid IDs.
func scanMessageIDs(value string) ([]string, bool) {
	ids := []string{}
	valid := true
	var cleaned strings.Builder
	depth := 0
	escaped := false
	for _, character := range value {
		if escaped {
			escaped = false
			if depth == 0 {
				cleaned.WriteRune(character)
			}
			continue
		}
		if character == '\\' && depth > 0 {
			escaped = true
			continue
		}
		if character == '(' {
			depth++
			continue
		}
		if character == ')' {
			if depth == 0 {
				valid = false
				continue
			}
			depth--
			continue
		}
		if depth == 0 {
			cleaned.WriteRune(character)
		}
	}
	if depth != 0 || escaped {
		valid = false
	}
	remaining := cleaned.String()
	for {
		start := strings.IndexByte(remaining, '<')
		if start < 0 {
			break
		}
		remaining = remaining[start+1:]
		end := strings.IndexByte(remaining, '>')
		if end < 0 {
			return ids, false
		}
		token := remaining[:end]
		at := strings.LastIndexByte(token, '@')
		if at > 0 && at < len(token)-1 && !strings.ContainsAny(token, "<>\r\n\t ,") {
			ids = append(ids, "<"+token+">")
		} else {
			valid = false
		}
		remaining = remaining[end+1:]
	}
	return ids, valid
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
