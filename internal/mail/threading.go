package mail

import (
	"context"
	"fmt"
	stdmail "net/mail"
	"strings"
)

// ThreadSource carries the reply/forward derivation inputs read from the
// source message's header block.
type ThreadSource struct {
	Subject    string
	From       string
	ReplyTo    string
	To         []Recipient
	CC         []Recipient
	MessageID  string
	References string
}

// ThreadSourceProvider resolves a source message's thread headers from the
// local Mail store. The durable store gateway implements it; the Mail.app
// fallback does not, so reply/forward derivation requires the store.
type ThreadSourceProvider interface {
	MessageThreadSource(ctx context.Context, ref string) (ThreadSource, error)
}

// maximumThreadReferences bounds the References chain written into new drafts.
const maximumThreadReferences = 20

// ThreadSource resolves the source message headers for reply/forward
// derivation. Gateways without store-bound header access fail with a typed
// error instead of guessing.
func (s *Service) ThreadSource(ctx context.Context, ref string) (ThreadSource, error) {
	if ref == "" {
		return ThreadSource{}, validationError("message ref is required")
	}
	provider, ok := s.gateway.(ThreadSourceProvider)
	if !ok {
		return ThreadSource{}, &OperationError{
			Code:    "store_bound_reference_required",
			Message: "reply and forward derivation requires the local Mail store",
		}
	}
	return provider.MessageThreadSource(ctx, ref)
}

// DeriveReplyInput merges source-derived defaults with the caller input.
// Explicit input fields win (documented last-wins). The subject gains exactly
// one Re:/Fwd: prefix after stripping existing ones; reply recipients default
// to the source Reply-To (preferred) or From address; reply --all promotes the
// source To/CC recipients into CC minus the reply target; the thread chain is
// the source References plus the source Message-ID, bounded to
// maximumThreadReferences and free of control characters.
func DeriveReplyInput(source ThreadSource, kind DraftKind, replyAll bool, input DraftInput) (DraftInput, string, string, error) {
	subject := threadSubject(source.Subject, kind)
	if input.Subject != "" {
		subject = input.Subject
	}
	out := input
	out.Subject = subject

	if kind == DraftKindReply {
		target := source.ReplyTo
		if target == "" {
			target = source.From
		}
		if target == "" {
			return DraftInput{}, "", "", &OperationError{
				Code:    "invalid_message_source",
				Message: "source message has no reply target",
			}
		}
		if len(out.To) == 0 {
			recipient, err := recipientFromFormatted(target)
			if err != nil {
				return DraftInput{}, "", "", &OperationError{
					Code:    "invalid_message_source",
					Message: fmt.Sprintf("source reply target is not a valid address: %v", err),
				}
			}
			out.To = []Recipient{recipient}
		}
		if replyAll && len(out.CC) == 0 {
			out.CC = promotedReplyAllRecipients(source.To, source.CC, target)
		}
	}

	chain, err := threadChain(source.References, source.MessageID)
	if err != nil {
		return DraftInput{}, "", "", err
	}
	return out, source.MessageID, chain, nil
}

// threadSubject normalizes the source subject to exactly one prefix.
func threadSubject(subject string, kind DraftKind) string {
	trimmed := strings.TrimSpace(subject)
	for {
		lowered := strings.ToLower(trimmed)
		cut := -1
		for _, prefix := range []string{"re:", "fwd:", "fw:"} {
			if strings.HasPrefix(lowered, prefix) {
				cut = len(prefix)
				break
			}
		}
		if cut < 0 {
			break
		}
		trimmed = strings.TrimSpace(trimmed[cut:])
	}
	prefix := "Re: "
	if kind == DraftKindForward {
		prefix = "Fwd: "
	}
	return prefix + trimmed
}

func promotedReplyAllRecipients(to []Recipient, cc []Recipient, target string) []Recipient {
	seen := map[string]struct{}{strings.ToLower(addressOnly(target)): {}}
	promoted := make([]Recipient, 0, len(to)+len(cc))
	for _, group := range [][]Recipient{to, cc} {
		for _, recipient := range group {
			key := strings.ToLower(recipient.Address)
			if key == "" {
				continue
			}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			promoted = append(promoted, recipient)
		}
	}
	return promoted
}

// threadChain builds the outgoing References value: source References entries
// plus the source Message-ID, newest last, capped to the newest
// maximumThreadReferences entries.
func threadChain(references string, messageID string) (string, error) {
	// Control characters must be checked on the RAW header values:
	// strings.Fields treats CR/LF as whitespace and would hide them.
	if strings.ContainsAny(references, "\r\n") || strings.ContainsAny(messageID, "\r\n") {
		return "", &OperationError{
			Code:    "invalid_message_source",
			Message: "source thread headers contain control characters",
		}
	}
	fields := append(strings.Fields(references), strings.Fields(messageID)...)
	if len(fields) > maximumThreadReferences {
		fields = fields[len(fields)-maximumThreadReferences:]
	}
	return strings.Join(fields, " "), nil
}

func recipientFromFormatted(formatted string) (Recipient, error) {
	parsed, err := stdmail.ParseAddress(formatted)
	if err != nil {
		return Recipient{}, err
	}
	return Recipient{Name: parsed.Name, Address: parsed.Address}, nil
}

func addressOnly(formatted string) string {
	if parsed, err := stdmail.ParseAddress(formatted); err == nil {
		return parsed.Address
	}
	return formatted
}
