package mail

import (
	"context"
	"fmt"
	"mime"
	stdmail "net/mail"
	"regexp"
	"strings"
	"unicode"

	"github.com/emersion/go-message/charset"
	messageMail "github.com/emersion/go-message/mail"
	"golang.org/x/text/unicode/norm"
)

// Limits and states of `messages new`.
const (
	DefaultNewMessagesLimit = 20
	MaximumNewMessagesLimit = 50

	NewMailboxStateChecked           = "checked"
	NewMailboxStateUnresolved        = "unresolved"
	NewMailboxStateUIDValidityChange = "uidvalidity_changed"

	// NewMatchedByUID compares server UIDs with the server UIDs of the local
	// rows; NewMatchedByHeaders compares sender, subject and sent time, for a
	// mailbox the local store keeps as labels of other rows (Gmail).
	NewMatchedByUID     = "uid"
	NewMatchedByHeaders = "headers"

	newMessageTextRunes = 200
)

// NewMessagesRequest selects what to compare with the server. An empty
// MailboxRef means the inbox of every account.
type NewMessagesRequest struct {
	AccountRef string
	MailboxRef string
	Limit      int
}

// NewMessage is server evidence for a message the local store does not have
// yet. ServerRef is readable over IMAP only.
type NewMessage struct {
	ServerRef string `json:"server_ref"`
	Subject   string `json:"subject"`
	Sender    string `json:"sender"`
	DateSent  string `json:"date_sent,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Unseen    bool   `json:"unseen"`
}

// NewMailbox reports one compared mailbox. NewCount counts the server
// messages of the newest window that the local store lacks; Truncated says
// more than Limit of them exist.
type NewMailbox struct {
	AccountRef     string       `json:"account_ref"`
	MailboxRef     string       `json:"mailbox_ref"`
	Name           string       `json:"name"`
	State          string       `json:"state"`
	MatchedBy      string       `json:"matched_by"`
	Reason         string       `json:"reason,omitempty"`
	ServerMessages int          `json:"server_messages"`
	NewCount       int          `json:"new_count"`
	Truncated      bool         `json:"truncated"`
	Messages       []NewMessage `json:"messages"`
}

// NewMessagesFailure explains one account or mailbox that could not be compared.
type NewMessagesFailure struct {
	Account string `json:"account"`
	Mailbox string `json:"mailbox,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// NewMessagesResult is the read-only server comparison. Complete is false when
// any account or mailbox could not be compared.
type NewMessagesResult struct {
	Mailboxes []NewMailbox         `json:"mailboxes"`
	Failures  []NewMessagesFailure `json:"failures"`
	Skipped   []SyncCheckSkip      `json:"skipped"`
	Complete  bool                 `json:"complete"`
	NewCount  int                  `json:"new_count"`
}

// NewMessagesReader is implemented by gateways that can compare the server
// with the local store.
type NewMessagesReader interface {
	NewMessages(ctx context.Context, request NewMessagesRequest) (NewMessagesResult, error)
}

func (s *Service) NewMessages(ctx context.Context, request NewMessagesRequest) (NewMessagesResult, error) {
	if request.Limit == 0 {
		request.Limit = DefaultNewMessagesLimit
	}
	if request.Limit < 1 || request.Limit > MaximumNewMessagesLimit {
		return NewMessagesResult{}, validationError(fmt.Sprintf("limit must be between 1 and %d", MaximumNewMessagesLimit))
	}
	reader, ok := s.gateway.(NewMessagesReader)
	if !ok {
		return NewMessagesResult{}, fmt.Errorf("messages new is not supported by the active mail gateway")
	}
	return reader.NewMessages(ctx, request)
}

// NewMessageFromHeader turns the header block of a server message into a
// triage row: decoded, single-line, control-free and bounded text.
func NewMessageFromHeader(serverRef string, header []byte, seen bool) NewMessage {
	message := NewMessage{ServerRef: serverRef, Unseen: !seen}
	fields, _ := ParseHeaderFields(string(header))
	for _, field := range fields {
		switch strings.ToLower(field.Name) {
		case "subject":
			if message.Subject == "" {
				message.Subject = cleanHeaderText(decodeHeaderWords(field.Value))
			}
		case "from":
			if message.Sender == "" {
				message.Sender = senderText(field.Value)
			}
		case "date":
			if message.DateSent == "" {
				if parsed, err := stdmail.ParseDate(strings.TrimSpace(field.Value)); err == nil {
					message.DateSent = parsed.UTC().Format("2006-01-02T15:04:05Z")
				}
			}
		case "message-id":
			if message.MessageID == "" {
				message.MessageID = cleanHeaderText(strings.TrimSpace(field.Value))
			}
		}
	}
	return message
}

func senderText(value string) string {
	addresses, err := messageMail.ParseAddressList(value)
	if err != nil || len(addresses) == 0 {
		return cleanHeaderText(decodeHeaderWords(value))
	}
	address := addresses[0]
	if address.Name == "" {
		return cleanHeaderText(address.Address)
	}
	return cleanHeaderText(address.Name + " <" + address.Address + ">")
}

// headerWordDecoder reads RFC 2047 words in every charset MailCLI can read,
// not only UTF-8, ISO-8859-1 and US-ASCII.
var headerWordDecoder = &mime.WordDecoder{CharsetReader: charset.Reader}

func decodeHeaderWords(value string) string {
	decoded, err := headerWordDecoder.DecodeHeader(value)
	if err != nil {
		return value
	}
	return decoded
}

// HeaderIdentity is what a local Envelope Index row and a server header both
// state about a message: lower-case sender address, sent time and a
// whitespace-normalized lower-case subject.
type HeaderIdentity struct {
	Address  string
	SentUnix int64
	Subject  string
}

// ParseHeaderIdentity derives the identity from a header block; a header
// without a parseable sender address or Date has none.
func ParseHeaderIdentity(header []byte) (HeaderIdentity, bool) {
	var identity HeaderIdentity
	haveAddress, haveDate := false, false
	fields, _ := ParseHeaderFields(string(header))
	for _, field := range fields {
		switch strings.ToLower(field.Name) {
		case "from":
			if addresses, err := messageMail.ParseAddressList(field.Value); err == nil && len(addresses) > 0 && !haveAddress {
				identity.Address, haveAddress = strings.ToLower(strings.TrimSpace(addresses[0].Address)), true
			}
		case "date":
			if parsed, err := stdmail.ParseDate(strings.TrimSpace(field.Value)); err == nil && !haveDate {
				identity.SentUnix, haveDate = parsed.Unix(), true
			}
		case "subject":
			if identity.Subject == "" {
				identity.Subject = NormalizeIdentitySubject(decodeHeaderWords(field.Value))
			}
		}
	}
	return identity, haveAddress && haveDate
}

// subjectPrefix matches the reply and forward prefixes of common mail clients
// (Re, Aw, Fw, Fwd, Wg, Sv, Vs, Tr, Odp, Res, Rv, optionally numbered such as
// Re[2]); the Envelope Index keeps the subject without them.
// A list tag such as "[Reddit Support]" may precede the prefix; Mail drops the
// prefix behind it as well.
var subjectPrefix = regexp.MustCompile(`^((?:\[[^\]]*\]\s*)*)(?:(?:re|aw|fwd?|wg|sv|vs|tr|odp|res|rv)(?:\[\d+\])?\s*:\s*)+`)

// NormalizeIdentitySubject folds a subject for identity comparison: NFC,
// lower case, single spaces, without reply and forward prefixes (also behind
// leading list tags such as "[Reddit Support]").
func NormalizeIdentitySubject(subject string) string {
	folded := strings.ToLower(strings.Join(strings.Fields(norm.NFC.String(subject)), " "))
	return subjectPrefix.ReplaceAllString(folded, "$1")
}

// cleanHeaderText collapses whitespace and control characters to single
// spaces and bounds the result, so a hostile header cannot inject lines.
func cleanHeaderText(value string) string {
	var builder strings.Builder
	space := false
	for _, character := range value {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			space = builder.Len() > 0
			continue
		}
		if space {
			builder.WriteByte(' ')
			space = false
		}
		builder.WriteRune(character)
	}
	runes := []rune(builder.String())
	if len(runes) > newMessageTextRunes {
		return string(runes[:newMessageTextRunes-1]) + "…"
	}
	return string(runes)
}
