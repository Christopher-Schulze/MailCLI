package mail

import (
	"context"
	"time"
)

const (
	DefaultPageLimit            = 10
	MaximumPageLimit            = 25
	MaximumDraftSubjectBytes    = 64 * 1024
	MaximumDraftBodyBytes       = 4 * 1024 * 1024
	MaximumDraftRecipients      = 200
	MaximumDraftAttachments     = 100
	MaximumDraftAttachmentBytes = int64(512 * 1024 * 1024)
	MaximumComposeBodyBytes     = 16 * 1024 * 1024
	MaximumRawSourceBytes       = int64(64 * 1024 * 1024)
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

type DiagnosticTiming struct {
	Phase        string  `json:"phase"`
	Milliseconds float64 `json:"milliseconds"`
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

type DraftBodyFormat string

const (
	DraftKindNew     DraftKind = "new"
	DraftKindReply   DraftKind = "reply"
	DraftKindForward DraftKind = "forward"

	DraftBodyPlain    DraftBodyFormat = "plain"
	DraftBodyMarkdown DraftBodyFormat = "markdown"
	DraftBodyHTML     DraftBodyFormat = "html"
)

type DraftInput struct {
	From        string          `json:"from,omitempty"`
	To          []Recipient     `json:"to,omitempty"`
	CC          []Recipient     `json:"cc,omitempty"`
	BCC         []Recipient     `json:"bcc,omitempty"`
	Subject     string          `json:"subject,omitempty"`
	Body        string          `json:"body"`
	BodyFormat  DraftBodyFormat `json:"body_format,omitempty"`
	Attachments []string        `json:"attachments,omitempty"`
}

type DraftAttachment struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Draft struct {
	Ref                           string                   `json:"ref"`
	Kind                          DraftKind                `json:"kind"`
	SourceRef                     string                   `json:"source_ref,omitempty"`
	ReplyAll                      bool                     `json:"reply_all,omitempty"`
	SourceMessageID               string                   `json:"source_message_id,omitempty"`
	SourceReferences              string                   `json:"source_references,omitempty"`
	From                          string                   `json:"from,omitempty"`
	To                            []Recipient              `json:"to"`
	CC                            []Recipient              `json:"cc"`
	BCC                           []Recipient              `json:"bcc"`
	Subject                       string                   `json:"subject,omitempty"`
	Body                          string                   `json:"body"`
	BodyFormat                    DraftBodyFormat          `json:"body_format"`
	BodySource                    string                   `json:"body_source,omitempty"`
	BodyHTML                      string                   `json:"body_html,omitempty"`
	Attachments                   []DraftAttachment        `json:"attachments"`
	CreatedAt                     time.Time                `json:"created_at"`
	UpdatedAt                     time.Time                `json:"updated_at"`
	SendAttempt                   *SendAttempt             `json:"send_attempt,omitempty"`
	SaveAttempt                   *DraftSaveAttempt        `json:"save_attempt,omitempty"`
	PreparedSendBaseline          *SendObservationBaseline `json:"-"`
	PreparedSaveBaseline          *SendObservationBaseline `json:"-"`
	ExpectedNativeAttachmentCount int                      `json:"-"`
	ExpectedAttachmentCount       *int                     `json:"-"`
	ExpectedBody                  *string                  `json:"-"`
}

type SavedDraft struct {
	LocalDraftRef string         `json:"local_draft_ref"`
	Message       MessageSummary `json:"message"`
}

type CreateDraftRequest struct {
	Kind             DraftKind
	SourceRef        string
	ReplyAll         bool
	SourceMessageID  string
	SourceReferences string
	Input            DraftInput
}

type UpdateDraftRequest struct {
	Ref   string
	Input DraftInput
}

type SendOutcome string

const (
	SendOutcomeObserved      SendOutcome = "sent_store_observed"
	SendOutcomeAccepted      SendOutcome = "accepted_by_mail"
	SendOutcomeSent          SendOutcome = "sent"
	SendOutcomeMirrorPending SendOutcome = "sent_mirror_pending"
	SendOutcomeUnknown       SendOutcome = "outcome_unknown"
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
	Materialized        *SendMaterialization     `json:"materialized,omitempty"`
	Transport           *TransportEvidence       `json:"transport,omitempty"`
}

// TransportEvidence records the deterministic proof of a direct SMTP
// submission and its Sent-mailbox mirror. ServerResponse is the final SMTP
// response line, MessageID the submitted Message-ID, MirrorMailbox the Sent
// mailbox holding the message, and MirrorAppended false when the provider had
// already filed the message itself.
type TransportEvidence struct {
	ServerResponse string `json:"server_response,omitempty"`
	MessageID      string `json:"message_id,omitempty"`
	MirrorMailbox  string `json:"mirror_mailbox,omitempty"`
	MirrorAppended bool   `json:"mirror_appended,omitempty"`
}

type SendMaterialization struct {
	From            string      `json:"from"`
	To              []Recipient `json:"to"`
	CC              []Recipient `json:"cc"`
	BCC             []Recipient `json:"bcc"`
	Subject         string      `json:"subject"`
	Body            *string     `json:"body,omitempty"`
	AttachmentCount int         `json:"attachment_count"`
}

type SendEvidence struct {
	InvocationStarted   bool
	AcceptedByMail      bool
	SentStoreObserved   bool
	ObservedMessageRef  string
	ObservationBaseline *SendObservationBaseline
	Materialized        *SendMaterialization
}

type DraftSaveAttempt struct {
	ID                  string                   `json:"id"`
	StartedAt           time.Time                `json:"started_at"`
	UpdatedAt           time.Time                `json:"updated_at"`
	InvocationStarted   bool                     `json:"invocation_started"`
	AcceptedByMail      bool                     `json:"accepted_by_mail"`
	ObservedMessageRef  string                   `json:"observed_message_ref,omitempty"`
	ObservationBaseline *SendObservationBaseline `json:"observation_baseline"`
	Materialized        *SendMaterialization     `json:"materialized,omitempty"`
}

type DraftSaveEvidence struct {
	InvocationStarted   bool
	AcceptedByMail      bool
	ObservedMessage     MessageSummary
	ObservationBaseline *SendObservationBaseline
	Materialized        *SendMaterialization
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

type ServerMutationEvidence struct {
	Command        string `json:"command"`
	ServerResponse string `json:"server_response"`
	Mailbox        string `json:"mailbox"`
	TargetMailbox  string `json:"target_mailbox,omitempty"`
	UID            uint32 `json:"uid"`
}

type MessageSummary struct {
	Ref             string                  `json:"ref"`
	MailboxRef      string                  `json:"mailbox_ref"`
	MessageID       string                  `json:"message_id"`
	Subject         string                  `json:"subject"`
	Sender          string                  `json:"sender"`
	DateReceived    string                  `json:"date_received,omitempty"`
	DateSent        string                  `json:"date_sent,omitempty"`
	Read            bool                    `json:"read"`
	Flagged         bool                    `json:"flagged"`
	Junk            bool                    `json:"junk"`
	Deleted         bool                    `json:"deleted"`
	Size            int64                   `json:"size"`
	AttachmentCount int                     `json:"attachment_count"`
	ServerTruth     *ServerMutationEvidence `json:"server_truth,omitempty"`
	StalenessNote   string                  `json:"staleness_note,omitempty"`
}
type MarkMessageRequest struct {
	Ref                string
	Read               *bool
	Flagged            *bool
	Junk               *bool
	AllowDraftMutation bool
}

type TransferMessageRequest struct {
	Ref                string
	DestinationMailbox string
	Copy               bool
	AllowDraftMutation bool
}

type DeleteMessageRequest struct {
	Ref                string
	AllowDraftMutation bool
}

type DeleteResult struct {
	MessageRef  string                  `json:"message_ref"`
	Deleted     bool                    `json:"deleted"`
	ServerTruth *ServerMutationEvidence `json:"server_truth,omitempty"`
}

type SyncResult struct {
	AccountRef string `json:"account_ref,omitempty"`
	Triggered  bool   `json:"triggered"`
}

type MailboxDelta struct {
	MailboxRef     string   `json:"mailbox_ref"`
	AccountRef     string   `json:"account_ref"`
	Name           string   `json:"name"`
	Path           []string `json:"path"`
	LocalMessages  int      `json:"local_messages"`
	ServerMessages int      `json:"server_messages"`
	Delta          int      `json:"delta"`
	Unseen         int      `json:"unseen"`
}

type SyncCheckResult struct {
	AccountRef string         `json:"account_ref,omitempty"`
	Mailboxes  []MailboxDelta `json:"mailboxes"`
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
	MarkMessage(ctx context.Context, request MarkMessageRequest) (MessageSummary, error)
	TransferMessage(ctx context.Context, request TransferMessageRequest) (MessageSummary, error)
	DeleteMessage(ctx context.Context, request DeleteMessageRequest) error
	Sync(ctx context.Context, accountRef string) error
}
