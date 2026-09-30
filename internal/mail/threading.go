package mail

import (
	"context"
	"fmt"
	stdmail "net/mail"
	"strings"

	"mailcli/internal/mailref"
)

// ThreadSource carries the reply/forward derivation inputs read from the
// source message's header block.
type ThreadSource struct {
	Subject             string
	From                string
	ReplyTo             []Recipient
	To                  []Recipient
	CC                  []Recipient
	MessageID           string
	References          string
	RecipientParseError error
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

// InferDerivedSender chooses the sender of a reply or forward draft from the
// account that holds the source message: the address of that account found in
// the source's To or CC, otherwise its only address. It returns empty values
// when the choice is not unique or the account is unusable, so the draft stays
// unsendable until the caller names a sender. The third return value carries
// the usable source account's own identities even when its sender is ambiguous.
func (s *Service) InferDerivedSender(ctx context.Context, messageRef string, source ThreadSource) (string, string, []string) {
	message, err := mailref.DecodeMessage(messageRef)
	if err != nil {
		return "", "", nil
	}
	accounts, err := s.ListAccounts(ctx)
	if err != nil {
		return "", "", nil
	}
	for _, account := range accounts {
		decoded, err := mailref.DecodeAccount(account.Ref)
		if err != nil || !strings.EqualFold(decoded.AccountID, message.AccountID) {
			continue
		}
		if account.State == "disabled" || account.State == "degraded" {
			return "", "", nil
		}
		identities := derivedSenderIdentities(account)
		if from := derivedSenderAddress(identities, source); from != "" {
			return from, account.Ref, identities
		}
		return "", "", identities
	}
	return "", "", nil
}

func derivedSenderIdentities(account Account) []string {
	var addresses []string
	seen := map[string]bool{}
	for _, group := range [][]string{account.EmailAddresses, account.ConfiguredSenderAliases, account.DiscoveredSenderIdentities} {
		for _, candidate := range group {
			key, err := recipientAddressKey(Recipient{Address: candidate})
			if err == nil && !seen[key] {
				seen[key] = true
				addresses = append(addresses, key)
			}
		}
	}
	return addresses
}

func derivedSenderAddress(addresses []string, source ThreadSource) string {
	if source.RecipientParseError != nil {
		return ""
	}
	matched := ""
	for _, group := range [][]Recipient{source.To, source.CC} {
		for _, recipient := range group {
			key, err := recipientAddressKey(recipient)
			if err != nil {
				return ""
			}
			for _, address := range addresses {
				if key == address {
					if matched != "" && matched != address {
						return ""
					}
					matched = address
				}
			}
		}
	}
	if matched != "" {
		return matched
	}
	if len(addresses) == 1 {
		return addresses[0]
	}
	return ""
}

// DeriveReplyInput merges source-derived defaults with the caller input.
// Explicit input fields win (documented last-wins). The subject gains exactly
// one Re:/Fwd: prefix after stripping existing ones; reply recipients default
// to the complete source Reply-To list (preferred) or From address; reply --all
// promotes the source To/CC recipients into CC minus the reply targets and
// final To roles and verified own identities; the thread chain is the source
// References plus the source Message-ID, bounded to maximumThreadReferences,
// deduplicated, and free of control characters.
func DeriveReplyInput(source ThreadSource, kind DraftKind, replyAll bool, input DraftInput, ownIdentities []string) (DraftInput, string, string, error) {
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
			if source.RecipientParseError != nil {
				return DraftInput{}, "", "", &OperationError{
					Code: "invalid_message_source", Message: "source To or CC header is malformed; automatic reply-all requires complete recipients",
				}
			}
			out.CC = promotedReplyAllRecipients(source.To, source.CC, targets, out.To, ownIdentities)
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
		match := subjectPrefix.FindStringSubmatchIndex(trimmed)
		if match == nil {
			break
		}
		trimmed = strings.TrimSpace(trimmed[match[2]:match[3]] + trimmed[match[1]:])
	}
	prefix := "Re: "
	if kind == DraftKindForward {
		prefix = "Fwd: "
	}
	return prefix + trimmed
}

func promotedReplyAllRecipients(to []Recipient, cc []Recipient, targets []Recipient, existingTo []Recipient, ownIdentities []string) []Recipient {
	seen := make(map[string]struct{}, len(existingTo)+len(targets)+len(ownIdentities))
	for _, identity := range ownIdentities {
		if key, err := recipientAddressKey(Recipient{Address: identity}); err == nil {
			seen[key] = struct{}{}
		}
	}
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
	at := strings.LastIndexByte(parsed.Address, '@')
	if at <= 0 || at == len(parsed.Address)-1 {
		return "", fmt.Errorf("recipient address has no mailbox domain")
	}
	return MailboxAddrSpec(parsed.Address[:at+1] + strings.ToLower(parsed.Address[at+1:])), nil
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

	fields, complete := scanMessageIDs(references)
	if !complete || references != "" && len(fields) == 0 {
		return "", invalidThreadMessageID("source thread header contains a malformed Message-ID")
	}
	chain := make([]string, 0, len(fields)+1)
	seen := make(map[string]struct{}, len(fields)+1)
	for _, field := range fields {
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
	ids, complete := scanMessageIDs(value)
	if !complete || len(ids) != 1 || ids[0] != value {
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
