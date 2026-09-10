package mail

import (
	"context"
	"os"
	"time"
)

const (
	DefaultPageLimit               = 10
	MaximumPageLimit               = 25
	DefaultSenderIdentityScanLimit = 2000
	MaximumSenderIdentityScanLimit = 10000
	MaximumDraftSubjectBytes       = 64 * 1024
	MaximumDraftBodyBytes          = 4 * 1024 * 1024
	MaximumDraftRecipients         = 200
	MaximumDraftAttachments        = 100
	MaximumDraftAttachmentBytes    = int64(512 * 1024 * 1024)
	MaximumComposeBodyBytes        = 16 * 1024 * 1024
	MaximumRawSourceBytes          = int64(64 * 1024 * 1024)
	SendReceiptRetention           = 30 * 24 * time.Hour
)

type SenderIdentityCoverageSource string

type SenderIdentityCoverageState string

const (
	SenderIdentityCoverageSourceSentHistory    SenderIdentityCoverageSource = "sent_history"
	SenderIdentityCoverageSourceMailApp        SenderIdentityCoverageSource = "mail_app"
	SenderIdentityCoverageSourceAccountBinding SenderIdentityCoverageSource = "account_binding"
	SenderIdentityCoverageSourceConfigured     SenderIdentityCoverageSource = SenderIdentityCoverageSourceAccountBinding
	SenderIdentityCoverageSourceUnknown        SenderIdentityCoverageSource = "unknown"
	SenderIdentityCoverageSourceNotApplicable  SenderIdentityCoverageSource = "not_applicable"

	SenderIdentityCoverageStateComplete      SenderIdentityCoverageState = "complete"
	SenderIdentityCoverageStateBounded       SenderIdentityCoverageState = "bounded"
	SenderIdentityCoverageStateNotObserved   SenderIdentityCoverageState = "not_observed"
	SenderIdentityCoverageStateNoValidSender SenderIdentityCoverageState = "no_valid_sender"
	SenderIdentityCoverageStateNoSentMailbox SenderIdentityCoverageState = "no_sent_mailbox"
	SenderIdentityCoverageStateConfigured    SenderIdentityCoverageState = "configured"
	SenderIdentityCoverageStateUnavailable   SenderIdentityCoverageState = "unavailable"
	SenderIdentityCoverageStateNotApplicable SenderIdentityCoverageState = "not_applicable"
)

type SenderIdentityCoverage struct {
	Source           SenderIdentityCoverageSource `json:"source"`
	State            SenderIdentityCoverageState  `json:"state"`
	ObservedMessages int                          `json:"observed_messages"`
	Limit            int                          `json:"limit"`
	MoreAvailable    bool                         `json:"more_available"`
}

type AccountType string

const (
	AccountTypeIMAP    AccountType = "imap"
	AccountTypeLocal   AccountType = "local"
	AccountTypeUnknown AccountType = "unknown"
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
	Ref                        string                 `json:"ref"`
	Name                       string                 `json:"name"`
	Type                       AccountType            `json:"type"`
	DisplayName                string                 `json:"display_name"`
	EmailAddresses             []string               `json:"email_addresses"`
	DiscoveredSenderIdentities []string               `json:"discovered_sender_identities"`
	ConfiguredSenderAliases    []string               `json:"configured_sender_aliases"`
	IdentityCoverage           SenderIdentityCoverage `json:"identity_coverage"`
	// State is "ok" or "degraded"; degraded accounts carry a reason and
	// keep empty identities rather than breaking the whole listing. The
	// remediation tells callers how to restore a usable account state.
	State               string `json:"state"`
	DegradedReason      string `json:"degraded_reason,omitempty"`
	DegradedRemediation string `json:"degraded_remediation,omitempty"`
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

// AttachmentEvidence is the immutable byte proof returned by an attachment
// writer. Path and Identity bind the proof to the output it owns; callers
// must still recheck that path identity before reusing the proof.
type AttachmentEvidence struct {
	Path     string      `json:"-"`
	Size     int64       `json:"-"`
	SHA256   string      `json:"-"`
	Identity os.FileInfo `json:"-"`
}

// AttachmentEvidenceGateway is an optional extension of Gateway used by
// attachment writers that can return proof from the authoritative copy pass.
// Gateways that do not implement it retain the safe inspect-after-publish
// fallback.
type AttachmentEvidenceGateway interface {
	SaveAttachmentToWithEvidence(
		ctx context.Context,
		messageRef string,
		attachmentID string,
		outputPath string,
	) (AttachmentEvidence, error)
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

const (
	ContentDiagnosticRemovedElement   = "removed_element"
	ContentDiagnosticRemovedAttribute = "removed_attribute"
	ContentDiagnosticUnsafeAttribute  = "unsafe_attribute_removed"
	ContentDiagnosticUnsafeURL        = "unsafe_url_removed"
	ContentDiagnosticUnsafeStyle      = "unsafe_style_removed"
	ContentDiagnosticRemoteResource   = "remote_resource_removed"
)

// ContentDiagnostic records a deterministic, value-free transformation made
// while preparing a rich draft. It excludes source values so diagnostics are
// safe to expose in structured output and logs.
type ContentDiagnostic struct {
	Code      string `json:"code"`
	Element   string `json:"element,omitempty"`
	Attribute string `json:"attribute,omitempty"`
}

type DraftInput struct {
	AccountRef  string          `json:"account_ref,omitempty"`
	From        string          `json:"from,omitempty"`
	To          []Recipient     `json:"to,omitempty"`
	CC          []Recipient     `json:"cc,omitempty"`
	BCC         []Recipient     `json:"bcc,omitempty"`
	Subject     string          `json:"subject,omitempty"`
	Body        string          `json:"body"`
	BodyFormat  DraftBodyFormat `json:"body_format,omitempty"`
	Attachments []string        `json:"attachments,omitempty"`
	// SubjectSet, ToSet, and CCSet preserve whether a caller supplied a
	// derivable field. They are intentionally excluded from JSON because the
	// field's presence is carried by the corresponding JSON key itself.
	SubjectSet bool `json:"-"`
	ToSet      bool `json:"-"`
	CCSet      bool `json:"-"`
}

type DraftAttachment struct {
	Path         string `json:"path"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	ModTimeNanos int64  `json:"mod_time_nanos,omitempty"`
}

type Draft struct {
	Ref                           string                   `json:"ref"`
	Kind                          DraftKind                `json:"kind"`
	AccountRef                    string                   `json:"account_ref,omitempty"`
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
	ContentDiagnostics            []ContentDiagnostic      `json:"content_diagnostics,omitempty"`
	Attachments                   []DraftAttachment        `json:"attachments"`
	CreatedAt                     time.Time                `json:"created_at"`
	UpdatedAt                     time.Time                `json:"updated_at"`
	SendAttempt                   *SendAttempt             `json:"send_attempt,omitempty"`
	SaveAttempt                   *DraftSaveAttempt        `json:"save_attempt,omitempty"`
	HandoffAttempt                *HandoffAttempt          `json:"handoff_attempt,omitempty"`
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

// DraftSummary is the list-view of a draft: envelope metadata plus
// metadata-only send/save attempt state, never body, HTML, raw MIME, or
// attachment content. Lists must stay cheap: building a summary never
// re-renders Markdown/HTML bodies.
type DraftSummary struct {
	Ref             string                      `json:"ref"`
	Kind            DraftKind                   `json:"kind"`
	AccountRef      string                      `json:"account_ref,omitempty"`
	Subject         string                      `json:"subject,omitempty"`
	From            string                      `json:"from,omitempty"`
	To              []Recipient                 `json:"to"`
	CC              []Recipient                 `json:"cc"`
	CreatedAt       time.Time                   `json:"created_at"`
	UpdatedAt       time.Time                   `json:"updated_at"`
	BodyFormat      DraftBodyFormat             `json:"body_format"`
	AttachmentCount int                         `json:"attachment_count"`
	EverSent        bool                        `json:"ever_sent"`
	SendAttempt     *DraftSendAttemptSummary    `json:"send_attempt,omitempty"`
	SaveAttempt     *DraftSaveAttemptSummary    `json:"save_attempt,omitempty"`
	HandoffAttempt  *DraftHandoffAttemptSummary `json:"handoff_attempt,omitempty"`
	StateError      string                      `json:"state_error,omitempty"`
}

// DraftSendAttemptSummary is the metadata-only list projection of SendAttempt.
// Materialized content is intentionally excluded. AcceptedByMail and
// SentStoreObserved remain compatibility aliases; use SubmissionAccepted and
// SentCopyObserved for precise send evidence.
type DraftSendAttemptSummary struct {
	ID                  string                   `json:"id"`
	StartedAt           time.Time                `json:"started_at"`
	UpdatedAt           time.Time                `json:"updated_at"`
	MessageID           string                   `json:"message_id,omitempty"`
	EnvelopeFingerprint string                   `json:"envelope_fingerprint,omitempty"`
	MIMEFingerprint     string                   `json:"mime_fingerprint,omitempty"`
	Outcome             SendOutcome              `json:"outcome"`
	InvocationStarted   bool                     `json:"invocation_started"`
	AcceptedByMail      bool                     `json:"accepted_by_mail"`
	SubmissionAccepted  bool                     `json:"submission_accepted"`
	SentStoreObserved   bool                     `json:"sent_store_observed"`
	SentCopyObserved    bool                     `json:"sent_copy_observed"`
	ObservedMessageRef  string                   `json:"observed_message_ref,omitempty"`
	ObservationBaseline *SendObservationBaseline `json:"observation_baseline,omitempty"`
	Transport           *TransportEvidence       `json:"transport,omitempty"`
}

// DraftSaveAttemptSummary is the metadata-only list projection of
// DraftSaveAttempt. Materialized content is intentionally excluded.
type DraftSaveAttemptSummary struct {
	ID                  string                   `json:"id"`
	StartedAt           time.Time                `json:"started_at"`
	UpdatedAt           time.Time                `json:"updated_at"`
	InvocationStarted   bool                     `json:"invocation_started"`
	AcceptedByMail      bool                     `json:"accepted_by_mail"`
	ObservedMessageRef  string                   `json:"observed_message_ref,omitempty"`
	ObservationBaseline *SendObservationBaseline `json:"observation_baseline"`
}

// HandoffOutcome describes the durable lifecycle of a visible compose
// request. A confirmed completion means only that NSSharingService reported
// success; it never proves that Mail saved, sent, or closed a window.
type HandoffOutcome string

const (
	HandoffOutcomePrepared        HandoffOutcome = "prepared"
	HandoffOutcomeDispatched      HandoffOutcome = "dispatched"
	HandoffOutcomeConfirmedOpened HandoffOutcome = "confirmed_opened"
	HandoffOutcomeConfirmedFailed HandoffOutcome = "confirmed_failed"
	HandoffOutcomeCanceled        HandoffOutcome = "canceled_before_dispatch"
	HandoffOutcomeUnknown         HandoffOutcome = "outcome_unknown"
)

type HandoffResolution string

const (
	HandoffResolutionOpened HandoffResolution = "opened"
	HandoffResolutionFailed HandoffResolution = "failed"
)

type HandoffSnapshot struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type HandoffAttempt struct {
	ID                string            `json:"id"`
	DraftRef          string            `json:"draft_ref"`
	StartedAt         time.Time         `json:"started_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
	Outcome           HandoffOutcome    `json:"outcome"`
	DispatchStarted   bool              `json:"dispatch_started"`
	SnapshotsRetained bool              `json:"snapshots_retained"`
	Snapshots         []HandoffSnapshot `json:"snapshots"`
}

// DraftHandoffAttemptSummary is the metadata-only list projection of a
// retained visible-compose attempt. It contains no attachment paths or
// content, only enough evidence to select the required reconciliation.
type DraftHandoffAttemptSummary struct {
	ID                string         `json:"id"`
	StartedAt         time.Time      `json:"started_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
	Outcome           HandoffOutcome `json:"outcome"`
	DispatchStarted   bool           `json:"dispatch_started"`
	SnapshotsRetained bool           `json:"snapshots_retained"`
	SnapshotCount     int            `json:"snapshot_count"`
	SnapshotBytes     int64          `json:"snapshot_bytes"`
}

type HandoffReconcileResult struct {
	DraftRef          string         `json:"draft_ref"`
	AttemptID         string         `json:"attempt_id"`
	Outcome           HandoffOutcome `json:"outcome"`
	SnapshotsRetained bool           `json:"snapshots_retained"`
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

// AcceptedMessageSpool identifies the immutable MIME bytes retained across the
// SMTP submission boundary while Sent mirroring remains unresolved. The spool
// path is derived from the draft reference and is never serialized.
type AcceptedMessageSpool struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type SendAttempt struct {
	ID                  string                   `json:"id"`
	StartedAt           time.Time                `json:"started_at"`
	UpdatedAt           time.Time                `json:"updated_at"`
	MessageID           string                   `json:"message_id,omitempty"`
	EnvelopeFingerprint string                   `json:"envelope_fingerprint,omitempty"`
	MIMEFingerprint     string                   `json:"mime_fingerprint,omitempty"`
	RecoverySpool       *AcceptedMessageSpool    `json:"recovery_spool,omitempty"`
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
// response line, SubmissionAccepted records that final 2yz response after
// DATA, MessageID the submitted Message-ID, MirrorMailbox the Sent
// mailbox holding the message, MirrorAttemptID identifies the latest durable
// mirror boundary, MirrorAttempted records that boundary, and
// MirrorOutcomeUnknown forbids replay after an ambiguous APPEND.
type TransportEvidence struct {
	ServerResponse       string `json:"server_response,omitempty"`
	MessageID            string `json:"message_id,omitempty"`
	SubmissionStage      string `json:"submission_stage,omitempty"`
	SubmissionAccepted   bool   `json:"submission_accepted,omitempty"`
	MirrorMailbox        string `json:"mirror_mailbox,omitempty"`
	MirrorUIDValidity    uint32 `json:"mirror_uidvalidity,omitempty"`
	MirrorUID            uint32 `json:"mirror_uid,omitempty"`
	MirrorAttemptID      string `json:"mirror_attempt_id,omitempty"`
	MirrorAppended       bool   `json:"mirror_appended,omitempty"`
	MirrorAttempted      bool   `json:"mirror_attempted,omitempty"`
	MirrorOutcomeUnknown bool   `json:"mirror_outcome_unknown,omitempty"`
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

// SendReceipt is the compact terminal proof retained after a successfully
// observed send has removed the local draft and transient send claim. It never
// contains message bodies or attachment bytes. Accepted remains a compatibility
// alias; the canonical fields separate SMTP submission from Sent persistence.
type SendReceipt struct {
	DraftRef           string      `json:"draft_ref"`
	AttemptID          string      `json:"attempt_id"`
	StartedAt          time.Time   `json:"started_at"`
	CompletedAt        time.Time   `json:"completed_at"`
	ExpiresAt          time.Time   `json:"expires_at"`
	Outcome            SendOutcome `json:"outcome"`
	Accepted           bool        `json:"accepted"`
	SubmissionAccepted bool        `json:"submission_accepted"`
	SentCopyObserved   bool        `json:"sent_copy_observed"`
	ObservedMessageRef string      `json:"observed_message_ref,omitempty"`
	MessageID          string      `json:"message_id,omitempty"`
	ServerResponse     string      `json:"server_response,omitempty"`
	SentMailbox        string      `json:"sent_mailbox,omitempty"`
	UIDValidity        uint32      `json:"uidvalidity,omitempty"`
	UID                uint32      `json:"uid,omitempty"`
	SentAppended       bool        `json:"sent_appended,omitempty"`
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

// SendResult keeps the legacy Accepted/AcceptedByMail aliases while exposing
// the separate SMTP submission and Sent-copy evidence boundaries.
type SendResult struct {
	DraftRef           string       `json:"draft_ref"`
	AttemptID          string       `json:"attempt_id"`
	Outcome            SendOutcome  `json:"outcome"`
	Accepted           bool         `json:"accepted"`
	SubmissionAccepted bool         `json:"submission_accepted"`
	InvocationStarted  bool         `json:"invocation_started"`
	AcceptedByMail     bool         `json:"accepted_by_mail"`
	SentStoreObserved  bool         `json:"sent_store_observed"`
	SentCopyObserved   bool         `json:"sent_copy_observed"`
	ObservedMessageRef string       `json:"observed_message_ref,omitempty"`
	DraftRetained      bool         `json:"draft_retained"`
	Replayed           bool         `json:"replayed"`
	Reconciled         bool         `json:"reconciled"`
	Receipt            *SendReceipt `json:"receipt,omitempty"`
}

type ServerMutationEvidence struct {
	OperationID    string `json:"operation_id,omitempty"`
	Outcome        string `json:"outcome,omitempty"`
	SourceAccount  string `json:"source_account,omitempty"`
	Command        string `json:"command"`
	ServerResponse string `json:"server_response"`
	Mailbox        string `json:"mailbox"`
	TargetMailbox  string `json:"target_mailbox,omitempty"`
	UID            uint32 `json:"uid"`
	// ExpectedUIDValidity is the UIDVALIDITY resolved before the mutation;
	// with UIDValidity it forms the compared pair (048: fail closed on rebuild).
	ExpectedUIDValidity    uint32   `json:"expected_uidvalidity,omitempty"`
	UIDValidity            uint32   `json:"uidvalidity,omitempty"`
	DuplicateMatches       int      `json:"duplicate_matches,omitempty"`
	ExpungeBranch          string   `json:"expunge_branch,omitempty"`
	ForeignDeletedCount    int      `json:"foreign_deleted_count,omitempty"`
	DestinationUIDValidity uint32   `json:"destination_uidvalidity,omitempty"`
	DestinationUID         uint32   `json:"destination_uid,omitempty"`
	CopyUIDResponse        string   `json:"copyuid_response,omitempty"`
	CopyUIDValidity        uint32   `json:"copyuid_validity,omitempty"`
	CopySourceUID          uint32   `json:"copy_source_uid,omitempty"`
	CopyDestinationUID     uint32   `json:"copy_destination_uid,omitempty"`
	CompletedEffects       []string `json:"completed_effects,omitempty"`
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

// MailboxDeltaState describes how a mailbox identity was covered by a sync
// check. Counts are comparable only for matched entries with both catalogs
// available; the other states retain the evidence that prevented comparison.
type MailboxDeltaState string

const (
	MailboxDeltaStateMatched      MailboxDeltaState = "matched"
	MailboxDeltaStateLocalOnly    MailboxDeltaState = "local_only"
	MailboxDeltaStateServerOnly   MailboxDeltaState = "server_only"
	MailboxDeltaStateInaccessible MailboxDeltaState = "inaccessible"
	MailboxDeltaStateUnresolved   MailboxDeltaState = "unresolved"
)

type MailboxDelta struct {
	MailboxRef              string            `json:"mailbox_ref"`
	AccountRef              string            `json:"account_ref"`
	State                   MailboxDeltaState `json:"state"`
	Name                    string            `json:"name"`
	Path                    []string          `json:"path"`
	ServerName              string            `json:"server_name,omitempty"`
	LocalMessagesAvailable  bool              `json:"local_messages_available"`
	ServerMessagesAvailable bool              `json:"server_messages_available"`
	LocalMessages           int               `json:"local_messages"`
	ServerMessages          int               `json:"server_messages"`
	Delta                   int               `json:"delta"`
	Unseen                  int               `json:"unseen"`
}

type SyncCheckResult struct {
	AccountRef string             `json:"account_ref,omitempty"`
	Mailboxes  []MailboxDelta     `json:"mailboxes"`
	Failures   []SyncCheckFailure `json:"failures,omitempty"`
	// Complete is true only when every mailbox of every targeted account
	// was checked. False means partial coverage: see Failures.
	Complete bool `json:"complete"`
}

// SyncCheckFailure records one skipped account or mailbox: the check ran,
// this entry explains what was not verified.
type SyncCheckFailure struct {
	Account string `json:"account"`
	Mailbox string `json:"mailbox,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type HydrationState string

const (
	HydrationStateFailed   HydrationState = "failed"
	HydrationStateCanceled HydrationState = "canceled"
)

type HydrationCause struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

type HydrationDiagnostic struct {
	State           HydrationState  `json:"state"`
	AttemptedSource string          `json:"attempted_source"`
	Local           *HydrationCause `json:"local,omitempty"`
	Remote          *HydrationCause `json:"remote,omitempty"`
	Remediation     string          `json:"remediation"`
}

type Message struct {
	Summary         MessageSummary       `json:"summary"`
	ReplyTo         string               `json:"reply_to"`
	To              []Recipient          `json:"to"`
	CC              []Recipient          `json:"cc"`
	BCC             []Recipient          `json:"bcc"`
	Headers         string               `json:"headers"`
	Content         string               `json:"content"`
	ContentSource   string               `json:"content_source"`
	ContentComplete bool                 `json:"content_complete"`
	MissingParts    []string             `json:"missing_parts"`
	Hydration       *HydrationDiagnostic `json:"hydration,omitempty"`
	Attachments     []Attachment         `json:"attachments"`
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
	DeleteMessage(ctx context.Context, request DeleteMessageRequest) (DeleteResult, error)
	Sync(ctx context.Context, accountRef string) error
}

type FallbackGateway interface {
	Probe(ctx context.Context, live bool) DiagnosticReport
	ListAccounts(ctx context.Context) ([]Account, error)
	ListMessages(ctx context.Context, request ListMessagesRequest) (MessagePage, error)
	Sync(ctx context.Context, accountRef string) error
}
