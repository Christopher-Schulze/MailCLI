package transport

import (
	"context"
	"io"
	"strings"
)

// SubmitConfig carries one SMTP submission target with credentials.
type SubmitConfig struct {
	Host     string
	Port     int
	Username string
	Password string
}

// ImapConfig carries one IMAP mirror target with credentials.
type ImapConfig struct {
	Host     string
	Port     int
	Username string
	Password string
}

// SubmitEvidence records the deterministic proof of an accepted submission.
type SubmitEvidence struct {
	ServerResponse string // final SMTP response line, e.g. "250 2.0.0 OK"
	MessageID      string // Message-ID of the submitted message
}

// AppendEvidence records the result of mirroring a message into the Sent mailbox.
type AppendEvidence struct {
	Mailbox  string // Sent mailbox that holds (or already held) the message
	Appended bool   // false means the message was already present (provider auto-filed)
}

// MailboxInfo carries the parsed name and special-use flags for an IMAP mailbox.
type MailboxInfo struct {
	Name  string
	Flags []string
}

// PickSentMailbox returns the IMAP mailbox that holds sent mail: the first
// mailbox flagged \Sent, else a well-known fallback name. Empty when none.
func PickSentMailbox(mailboxes []MailboxInfo) string {
	for _, mailbox := range mailboxes {
		for _, flag := range mailbox.Flags {
			if strings.EqualFold(flag, "\\Sent") {
				return mailbox.Name
			}
		}
	}
	for _, want := range []string{"Sent Messages", "Gesendet", "[Gmail]/Sent Mail"} {
		for _, mailbox := range mailboxes {
			if strings.EqualFold(mailbox.Name, want) {
				return mailbox.Name
			}
		}
	}
	return ""
}

// PickTrashMailbox returns the IMAP mailbox used for deletion: the first
// mailbox flagged \\Trash, else a well-known fallback name. Empty when none.
func PickTrashMailbox(mailboxes []MailboxInfo) string {
	for _, mailbox := range mailboxes {
		for _, flag := range mailbox.Flags {
			if strings.EqualFold(flag, "\\Trash") {
				return mailbox.Name
			}
		}
	}
	for _, want := range []string{
		"Trash", "Deleted Messages", "[Gmail]/Trash", "[Gmail]/Papierkorb",
		"Papierkorb", "INBOX.Trash",
	} {
		for _, mailbox := range mailboxes {
			if strings.EqualFold(mailbox.Name, want) {
				return mailbox.Name
			}
		}
	}
	return ""
}

// MutationEvidence records server proof for an IMAP message mutation.
type MutationEvidence struct {
	Command        string // STORE, COPY, MOVE, or DELETE
	ServerResponse string // final server status line
	Mailbox        string // source mailbox
	TargetMailbox  string // destination mailbox (for COPY, MOVE, DELETE)
	UID            uint32 // affected message UID
	UIDValidity    uint32 // SELECT-time UIDVALIDITY of the source mailbox
	// DuplicateMatches counts all UIDs returned by the Message-ID search;
	// zero means the search result was not available.
	DuplicateMatches int
	// ExpungeBranch identifies the MOVE fallback cleanup: uid_expunge,
	// deferred, or plain_expunge. It is empty for native MOVE and other
	// mutations.
	ExpungeBranch string
	// ForeignDeletedCount is populated when expunge is deferred because other
	// deleted UIDs are present in the source mailbox.
	ForeignDeletedCount int
	// ExpectedUIDValidity is the UIDVALIDITY the caller resolved before the
	// mutation; together with UIDValidity it forms the compared pair.
	ExpectedUIDValidity uint32
}

// MailboxStatus records server state from an IMAP STATUS command.
type MailboxStatus struct {
	Mailbox     string
	Messages    int
	UIDNext     uint32
	UIDValidity uint32
	Unseen      int
}

// ImapOperator combines sent mirroring, message mutations, hydration, and status checking.
type ImapOperator interface {
	SentMirror
	ListMailboxes(ctx context.Context, cfg ImapConfig) ([]MailboxInfo, error)
	SearchUID(ctx context.Context, cfg ImapConfig, mailbox string, messageID string) (uid uint32, uidvalidity uint32, matchCount int, err error)
	SetFlags(ctx context.Context, cfg ImapConfig, mailbox string, uid uint32, expectedUIDValidity uint32, addFlags, removeFlags []string) (MutationEvidence, error)
	CopyMessage(ctx context.Context, cfg ImapConfig, srcMailbox string, uid uint32, expectedUIDValidity uint32, dstMailbox string) (MutationEvidence, error)
	MoveMessage(ctx context.Context, cfg ImapConfig, srcMailbox string, uid uint32, expectedUIDValidity uint32, dstMailbox string) (MutationEvidence, error)
	DeleteMessage(ctx context.Context, cfg ImapConfig, srcMailbox string, uid uint32, expectedUIDValidity uint32) (MutationEvidence, error)
	FetchMessage(ctx context.Context, cfg ImapConfig, mailbox string, uid uint32, expectedUIDValidity uint32, maxBytes int64) ([]byte, error)
	CheckStatus(ctx context.Context, cfg ImapConfig, mailbox string) (MailboxStatus, error)
}

// Submitter submits a fully composed RFC 5322 message.
type Submitter interface {
	Submit(ctx context.Context, cfg SubmitConfig, from string, rcpts []string, msg []byte) (SubmitEvidence, error)
}

// StreamingSubmitter is an optional transport extension for bounded-memory
// submission of a replayable RFC 5322 source.
type StreamingSubmitter interface {
	SubmitReader(ctx context.Context, cfg SubmitConfig, from string, rcpts []string, messageID string, msg io.Reader, size int64) (SubmitEvidence, error)
}

// SentMirror mirrors an accepted message into the account's Sent mailbox.
type SentMirror interface {
	AppendToSent(ctx context.Context, cfg ImapConfig, msg []byte, messageID string) (AppendEvidence, error)
}

// StreamingSentMirror is an optional mirror extension for replayable sources.
type StreamingSentMirror interface {
	AppendToSentReader(ctx context.Context, cfg ImapConfig, msg io.Reader, size int64, messageID string) (AppendEvidence, error)
}

// CredentialStore reads and stores app-specific passwords in the macOS keychain.
type CredentialStore interface {
	Load(account string) (password string, err error)
	Store(account string, password string) error
	Delete(account string) error
}

// ProviderHosts resolves the SMTP and IMAP endpoints for a sender address domain.
// It returns host and port separately so callers can build SubmitConfig/ImapConfig directly.
func ProviderHosts(email string) (smtpHost string, smtpPort int, imapHost string, imapPort int, err error) {
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return "", 0, "", 0, &TransportError{Code: CodeInvalidAddress, Message: "sender address has no domain: " + email}
	}
	switch strings.ToLower(email[at+1:]) {
	case "gmail.com", "googlemail.com":
		return "smtp.gmail.com", 587, "imap.gmail.com", 993, nil
	case "icloud.com", "me.com", "mac.com":
		return "smtp.mail.me.com", 587, "imap.mail.me.com", 993, nil
	default:
		return "", 0, "", 0, &TransportError{Code: CodeUnsupportedProvider, Message: "no SMTP/IMAP endpoints known for domain: " + email[at+1:]}
	}
}
