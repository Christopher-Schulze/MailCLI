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
	ReplyTo    []Recipient
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
// to the complete source Reply-To list (preferred) or From address; reply --all
// promotes the source To/CC recipients into CC minus the reply targets and
// final To roles; the thread chain is the source References plus the source
// Message-ID, bounded to maximumThreadReferences, deduplicated, and free of
// control characters.
func DeriveReplyInput(source ThreadSource, kind DraftKind, replyAll bool, input DraftInput) (DraftInput, string, string, error) {
	subject := threadSubject(source.Subject, kind)
	if input.SubjectSet || input.Subject != "" {
		subject = input.Subject
	}
	out := input
	out.Subject = subject

	if kind == DraftKindReply {
		targets, err := replyTargetRecipients(source)
		if err != nil {
			return DraftInput{}, "", "", err
		}
		if !input.ToSet && len(out.To) == 0 {
			out.To = append([]Recipient(nil), targets...)
		}
		if replyAll && !input.CCSet && len(out.CC) == 0 {
			out.CC = promotedReplyAllRecipients(source.To, source.CC, targets, out.To)
		}
		if replyAll {
			out.CC = deduplicateRecipientsAgainst(out.CC, out.To)
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

func promotedReplyAllRecipients(to []Recipient, cc []Recipient, targets []Recipient, existingTo []Recipient) []Recipient {
	seen := make(map[string]struct{}, len(existingTo)+len(targets))
	for _, target := range targets {
		if key, err := recipientAddressKey(target); err == nil {
			seen[key] = struct{}{}
		}
	}
	for _, recipient := range existingTo {
		if key, err := recipientAddressKey(recipient); err == nil {
			seen[key] = struct{}{}
		}
	}
	promoted := make([]Recipient, 0, len(to)+len(cc))
	for _, group := range [][]Recipient{to, cc} {
		for _, recipient := range group {
			key, err := recipientAddressKey(recipient)
			if err != nil {
				promoted = append(promoted, recipient)
				continue
			}
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

func replyTargetRecipients(source ThreadSource) ([]Recipient, error) {
	if len(source.ReplyTo) > 0 {
		return append([]Recipient(nil), source.ReplyTo...), nil
	}
	if source.From != "" {
		recipient, err := recipientFromFormatted(source.From)
		if err != nil {
			return nil, &OperationError{
				Code:    "invalid_message_source",
				Message: fmt.Sprintf("source reply target is not a valid address: %v", err),
			}
		}
		return []Recipient{recipient}, nil
	}
	return nil, &OperationError{
		Code:    "invalid_message_source",
		Message: "source message has no reply target",
	}
}

func deduplicateRecipientsAgainst(recipients []Recipient, existing []Recipient) []Recipient {
	seen := make(map[string]struct{}, len(existing)+len(recipients))
	for _, recipient := range existing {
		if key, err := recipientAddressKey(recipient); err == nil {
			seen[key] = struct{}{}
		}
	}
	result := make([]Recipient, 0, len(recipients))
	for _, recipient := range recipients {
		key, err := recipientAddressKey(recipient)
		if err != nil || key == "" {
			result = append(result, recipient)
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, recipient)
	}
	return result
}

func recipientAddressKey(recipient Recipient) (string, error) {
	parsed, err := stdmail.ParseAddress(recipient.Address)
	if err != nil || parsed.Address == "" {
		if err == nil {
			err = fmt.Errorf("recipient address is empty")
		}
		return "", err
	}
	return strings.ToLower(parsed.Address), nil
}

// threadChain builds the outgoing References value from the source headers.
func threadChain(references string, messageID string) (string, error) {
	return canonicalThreadReferences(references, messageID)
}

// canonicalThreadReferences validates and canonicalizes a References chain.
// Valid entries remain in first-seen order, the direct parent is removed from
// any historical position and appended exactly once, and the final chain is
// capped to the newest maximumThreadReferences entries.
func canonicalThreadReferences(references, messageID string) (string, error) {
	// Control characters must be checked on the RAW header values:
	// strings.Fields treats CR/LF as whitespace and would hide them.
	if strings.ContainsAny(references, "\r\n") || strings.ContainsAny(messageID, "\r\n") {
		return "", &OperationError{
			Code:    "invalid_message_source",
			Message: "source thread headers contain control characters",
		}
	}

	parent := ""
	if messageID != "" {
		if strings.TrimSpace(messageID) != messageID {
			return "", invalidThreadMessageID("source message ID must be one standalone angle-bracket Message-ID")
		}
		if err := validateThreadMessageID(messageID); err != nil {
			return "", err
		}
		parent = messageID
	}

	fields := strings.Fields(references)
	chain := make([]string, 0, len(fields)+1)
	seen := make(map[string]struct{}, len(fields)+1)
	for _, field := range fields {
		if err := validateThreadMessageID(field); err != nil {
			return "", err
		}
		if field == parent {
			continue
		}
		if _, duplicate := seen[field]; duplicate {
			continue
		}
		seen[field] = struct{}{}
		chain = append(chain, field)
	}
	if parent != "" {
		chain = append(chain, parent)
	}
	if len(chain) > maximumThreadReferences {
		chain = chain[len(chain)-maximumThreadReferences:]
	}
	return strings.Join(chain, " "), nil
}

func validateThreadMessageID(value string) error {
	if value == "" || !strings.HasPrefix(value, "<") || !strings.HasSuffix(value, ">") {
		return invalidThreadMessageID("source thread header contains a malformed Message-ID")
	}
	parsed, err := stdmail.ParseAddress(value)
	if err != nil || parsed.Address == "" {
		return invalidThreadMessageID("source thread header contains a malformed Message-ID")
	}
	return nil
}

func invalidThreadMessageID(message string) error {
	return &OperationError{Code: "invalid_message_source", Message: message}
}

func recipientFromFormatted(formatted string) (Recipient, error) {
	parsed, err := stdmail.ParseAddress(formatted)
	if err != nil {
		return Recipient{}, err
	}
	return Recipient{Name: parsed.Name, Address: MailboxAddrSpec(parsed.Address)}, nil
}
