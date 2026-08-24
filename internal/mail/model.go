package mail

import (
	"context"
	"time"
)

const (
	DefaultPageLimit = 10
	MaximumPageLimit = 25
)

type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail"`
}

type DiagnosticReport struct {
	Checks []Check `json:"checks"`
}

type Account struct {
	Ref            string   `json:"ref"`
	Name           string   `json:"name"`
	EmailAddresses []string `json:"email_addresses"`
}

type Mailbox struct {
	Ref                    string   `json:"ref"`
	AccountRef             string   `json:"account_ref"`
	Name                   string   `json:"name"`
	Path                   []string `json:"path"`
	UnreadCount            int      `json:"unread_count"`
	MessageCount           int      `json:"message_count"`
	LocalMessagesAvailable bool     `json:"local_messages_available"`
}

type Recipient struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

type Attachment struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	MIMEType   *string `json:"mime_type"`
	Size       int64   `json:"size"`
	SizeKnown  bool    `json:"size_known"`
	Downloaded bool    `json:"downloaded"`
}

type SavedAttachment struct {
	AttachmentID string `json:"attachment_id"`
	Path         string `json:"path"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
}

type SaveAttachmentRequest struct {
	MessageRef   string
	AttachmentID string
	OutputPath   string
}

type DraftKind string

const (
	DraftKindNew     DraftKind = "new"
	DraftKindReply   DraftKind = "reply"
	DraftKindForward DraftKind = "forward"
)

type DraftInput struct {
	From        string      `json:"from,omitempty"`
	To          []Recipient `json:"to,omitempty"`
	CC          []Recipient `json:"cc,omitempty"`
	BCC         []Recipient `json:"bcc,omitempty"`
	Subject     string      `json:"subject,omitempty"`
	Body        string      `json:"body"`
	Attachments []string    `json:"attachments,omitempty"`
}

type DraftAttachment struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Draft struct {
	Ref                  string                   `json:"ref"`
	Kind                 DraftKind                `json:"kind"`
	SourceRef            string                   `json:"source_ref,omitempty"`
	ReplyAll             bool                     `json:"reply_all,omitempty"`
	From                 string                   `json:"from,omitempty"`
	To                   []Recipient              `json:"to"`
	CC                   []Recipient              `json:"cc"`
	BCC                  []Recipient              `json:"bcc"`
	Subject              string                   `json:"subject,omitempty"`
	Body                 string                   `json:"body"`
	Attachments          []DraftAttachment        `json:"attachments"`
	CreatedAt            time.Time                `json:"created_at"`
	UpdatedAt            time.Time                `json:"updated_at"`
	SendAttempt          *SendAttempt             `json:"send_attempt,omitempty"`
	PreparedSendBaseline *SendObservationBaseline `json:"-"`
}

type SavedDraft struct {
	LocalDraftRef string         `json:"local_draft_ref"`
	Message       MessageSummary `json:"message"`
}

type CreateDraftRequest struct {
	Kind      DraftKind
	SourceRef string
	ReplyAll  bool
	Input     DraftInput
}

type UpdateDraftRequest struct {
	Ref   string
	Input DraftInput
}

type SendOutcome string

const (
	SendOutcomeObserved SendOutcome = "sent_store_observed"
	SendOutcomeAccepted SendOutcome = "accepted_by_mail"
	SendOutcomeUnknown  SendOutcome = "outcome_unknown"
)

type SendObservationBaseline struct {
	StoreUUID      string  `json:"store_uuid"`
	MaximumRowID   int64   `json:"maximum_row_id"`
	CapturedUnix   int64   `json:"captured_unix"`
	SentMailboxIDs []int64 `json:"sent_mailbox_ids"`
}

type SendAttempt struct {
	ID                  string                   `json:"id"`
	StartedAt           time.Time                `json:"started_at"`
	UpdatedAt           time.Time                `json:"updated_at"`
	Outcome             SendOutcome              `json:"outcome"`
	InvocationStarted   bool                     `json:"invocation_started"`
	AcceptedByMail      bool                     `json:"accepted_by_mail"`
	SentStoreObserved   bool                     `json:"sent_store_observed"`
	ObservedMessageRef  string                   `json:"observed_message_ref,omitempty"`
	ObservationBaseline *SendObservationBaseline `json:"observation_baseline,omitempty"`
}

type SendEvidence struct {
	InvocationStarted   bool
	AcceptedByMail      bool
	SentStoreObserved   bool
	ObservedMessageRef  string
	ObservationBaseline *SendObservationBaseline
}

type SendResult struct {
	DraftRef           string      `json:"draft_ref"`
	AttemptID          string      `json:"attempt_id"`
	Outcome            SendOutcome `json:"outcome"`
	Accepted           bool        `json:"accepted"`
	InvocationStarted  bool        `json:"invocation_started"`
	AcceptedByMail     bool        `json:"accepted_by_mail"`
	SentStoreObserved  bool        `json:"sent_store_observed"`
	ObservedMessageRef string      `json:"observed_message_ref,omitempty"`
	DraftRetained      bool        `json:"draft_retained"`
	Replayed           bool        `json:"replayed"`
	Reconciled         bool        `json:"reconciled"`
}

type MessageSummary struct {
	Ref             string `json:"ref"`
	MailboxRef      string `json:"mailbox_ref"`
	MessageID       string `json:"message_id"`
	Subject         string `json:"subject"`
	Sender          string `json:"sender"`
	DateReceived    string `json:"date_received,omitempty"`
	DateSent        string `json:"date_sent,omitempty"`
	Read            bool   `json:"read"`
	Flagged         bool   `json:"flagged"`
	Junk            bool   `json:"junk"`
	Deleted         bool   `json:"deleted"`
	Size            int64  `json:"size"`
	AttachmentCount int    `json:"attachment_count"`
}

type MarkMessageRequest struct {
	Ref     string
	Read    *bool
	Flagged *bool
	Junk    *bool
}

type TransferMessageRequest struct {
	Ref                string
	DestinationMailbox string
	Copy               bool
}

type DeleteResult struct {
	MessageRef string `json:"message_ref"`
	Deleted    bool   `json:"deleted"`
}

type SyncResult struct {
	AccountRef string `json:"account_ref,omitempty"`
	Triggered  bool   `json:"triggered"`
}

type Message struct {
	Summary         MessageSummary `json:"summary"`
	ReplyTo         string         `json:"reply_to"`
	To              []Recipient    `json:"to"`
	CC              []Recipient    `json:"cc"`
	BCC             []Recipient    `json:"bcc"`
	Headers         string         `json:"headers"`
	Content         string         `json:"content"`
	ContentSource   string         `json:"content_source"`
	ContentComplete bool           `json:"content_complete"`
	MissingParts    []string       `json:"missing_parts"`
	Attachments     []Attachment   `json:"attachments"`
}

type MessagePage struct {
	Messages   []MessageSummary `json:"messages"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

type ListMailboxesRequest struct {
	AccountRef string
}

type ListMessagesRequest struct {
	MailboxRef string
	Cursor     string
	Limit      int
}

type Gateway interface {
	Probe(ctx context.Context, live bool) DiagnosticReport
	ListAccounts(ctx context.Context) ([]Account, error)
	ListMailboxes(ctx context.Context, request ListMailboxesRequest) ([]Mailbox, error)
	ListMessages(ctx context.Context, request ListMessagesRequest) (MessagePage, error)
	GetMessage(ctx context.Context, ref string) (Message, error)
	OpenDraft(ctx context.Context, ref string) (Message, error)
	GetRawSource(ctx context.Context, ref string) (string, error)
	SaveAttachmentTo(ctx context.Context, messageRef string, attachmentID string, outputPath string) error
	SaveDraft(ctx context.Context, draft Draft) (MessageSummary, error)
	SendDraft(ctx context.Context, draft Draft) (SendEvidence, error)
	MarkMessage(ctx context.Context, request MarkMessageRequest) (MessageSummary, error)
	TransferMessage(ctx context.Context, request TransferMessageRequest) (MessageSummary, error)
	DeleteMessage(ctx context.Context, ref string) error
	Sync(ctx context.Context, accountRef string) error
}
