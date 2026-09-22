package mailstore

import (
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

const stalenessExplanation = "changes applied to IMAP server; local read store updates on next Mail.app sync"

const (
	accountReferenceInvalidCode            = "account_reference_invalid"
	accountReferenceCorruptCode            = "account_reference_corrupt"
	accountReferenceVersionUnsupportedCode = "account_reference_version_unsupported"
	accountDisabledCode                    = "account_disabled"
	accountIdentityMissingCode             = "account_identity_missing"
	accountBindingStaleCode                = "account_binding_stale"
)

type imapTarget struct {
	cfg              transport.ImapConfig
	imapMailbox      string
	trashMailbox     string
	uid              uint32
	uidvalidity      uint32
	duplicateMatches int
	messageID        string
	accountID        string
	summary          mail.MessageSummary
}

const mailboxCacheTTL = 5 * time.Minute

const (
	syncCheckMissingLocalMailboxCode      = "sync_check_missing_local_mailbox"
	syncCheckServerCatalogIncompleteCode  = "sync_check_server_catalog_incomplete"
	syncCheckLocalMessagesUnavailableCode = "sync_check_local_messages_unavailable"
)

type syncStatusJobKind uint8

const (
	syncStatusLocal syncStatusJobKind = iota
	syncStatusServerOnly
)

type syncStatusJob struct {
	kind       syncStatusJobKind
	mailbox    string
	local      mail.Mailbox
	server     transport.MailboxInfo
	serverName string
}

type syncStatusResult struct {
	status transport.MailboxStatus
	err    error
}

type syncStatusPlanItem struct {
	statusJobIndex int
	delta          mail.MailboxDelta
	failures       []mail.SyncCheckFailure
}

type mailboxCacheEntry struct {
	boxes     []transport.MailboxInfo
	expiresAt time.Time
}
