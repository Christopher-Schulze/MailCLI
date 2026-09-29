package mail

import (
	"context"
	"fmt"
	"mime"
	stdmail "net/mail"
	"strings"
	"unicode"

	messageMail "github.com/emersion/go-message/mail"
)

// Limits and states of `messages new`.
const (
	DefaultNewMessagesLimit = 20
	MaximumNewMessagesLimit = 50

	NewMailboxStateChecked           = "checked"
	NewMailboxStateUnresolved        = "unresolved"
	NewMailboxStateUIDValidityChange = "uidvalidity_changed"

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

func decodeHeaderWords(value string) string {
	decoded, err := (&mime.WordDecoder{}).DecodeHeader(value)
	if err != nil {
		return value
	}
	return decoded
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
