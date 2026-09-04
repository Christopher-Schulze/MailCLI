package mailstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	stdmail "net/mail"
	"strconv"
	"strings"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

const listCursorVersion = 1

type messageRecord struct {
	RowID           int64
	StoreMessageID  int64
	StoreGlobalID   int64
	StoreMailboxID  int64
	PhysicalURL     string
	Subject         string
	SenderAddress   string
	SenderName      string
	SummaryText     string
	DateSent        int64
	DateReceived    int64
	Read            bool
	Flagged         bool
	Deleted         bool
	Junk            bool
	Size            int64
	AttachmentCount int
}

type listCursor struct {
	Version      int    `json:"version"`
	StoreUUID    string `json:"store_uuid"`
	MailboxRef   string `json:"mailbox_ref"`
	DateReceived int64  `json:"date_received"`
	RowID        int64  `json:"row_id"`
}

func (s *Store) ListMessages(ctx context.Context, request mail.ListMessagesRequest) (mail.MessagePage, error) {
	if request.Limit < 1 || request.Limit > mail.MaximumPageLimit {
		return mail.MessagePage{}, operationError(
			"invalid_argument", fmt.Sprintf("limit must be between 1 and %d", mail.MaximumPageLimit),
		)
	}
	mailbox, err := mailref.DecodeMailbox(request.MailboxRef)
	if err != nil {
		return mail.MessagePage{}, operationError("invalid_reference", fmt.Sprintf("invalid mailbox ref: %v", err))
	}
	if !s.activeAccountID(mailbox.AccountID) {
		return mail.MessagePage{}, operationError("stale_reference", "mailbox account is not active")
	}
	records, err := s.mailboxRecords(ctx)
	if err != nil {
		return mail.MessagePage{}, err
	}
	mailboxRecord, found := findMailboxRecord(records, mailbox.AccountID, mailbox.Path)
	if !found {
		return s.emptyOrMissingMailbox(ctx, request.MailboxRef, mailbox.AccountID, mailbox.Path)
	}
	cursor, err := decodeListCursor(request.Cursor, request.MailboxRef, s.storeUUID)
	if err != nil {
		return mail.MessagePage{}, err
	}
	items, err := s.queryMailboxMessages(ctx, mailboxRecord.RowID, cursor, request.Limit+1)
	if err != nil {
		return mail.MessagePage{}, err
	}
	return mapMessagePage(items, request.MailboxRef, mailbox, request.Limit, s.storeUUID)
}

func (s *Store) emptyOrMissingMailbox(
	ctx context.Context,
	mailboxRef string,
	accountID string,
	path []string,
) (mail.MessagePage, error) {
	mailboxes, err := s.ListMailboxes(ctx, mail.ListMailboxesRequest{})
	if err != nil {
		return mail.MessagePage{}, err
	}
	for _, candidate := range mailboxes {
		if candidate.Ref == mailboxRef && !candidate.LocalMessagesAvailable {
			return mail.MessagePage{Messages: []mail.MessageSummary{}}, nil
		}
	}
	return mail.MessagePage{}, operationError(
		"not_found", fmt.Sprintf("mailbox %s/%s is not present in the local Mail store", accountID, strings.Join(path, "/")),
	)
}

func (s *Store) queryMailboxMessages(
	ctx context.Context,
	mailboxRowID int64,
	cursor *listCursor,
	limit int,
) (result []messageRecord, resultErr error) {
	query := mailboxMessagesSQL("")
	arguments := []any{mailboxRowID, mailboxRowID}
	if cursor != nil {
		query = mailboxMessagesSQL(`
			AND (m.date_received < ? OR (m.date_received = ? AND m.ROWID < ?))
		`)
		arguments = append(arguments, cursor.DateReceived, cursor.DateReceived, cursor.RowID)
	}
	arguments = append(arguments, limit)
	rows, err := s.database.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("list Envelope Index messages: %w", err)
	}
	defer joinCloseError(&resultErr, rows, "message rows")
	items := make([]messageRecord, 0, limit)
	for rows.Next() {
		item, err := scanMessageRecord(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Envelope Index messages: %w", err)
	}
	return items, nil
}

func mailboxMessagesSQL(cursorClause string) string {
	return `
		WITH membership(id) AS (
			SELECT ROWID FROM messages WHERE mailbox = ?
			UNION
			SELECT message_id FROM labels WHERE mailbox_id = ?
		)
		SELECT
			m.ROWID, COALESCE(m.message_id, 0), COALESCE(m.global_message_id, 0),
			m.mailbox, mb.url,
			subject.subject, sender.address, sender.comment,
			COALESCE(summary.summary, ''), COALESCE(m.date_sent, 0),
			COALESCE(m.date_received, 0), m.read, m.flagged, m.deleted,
			EXISTS(
				SELECT 1 FROM server_messages sm
				WHERE sm.message = m.ROWID AND sm.junk_level > 0
			),
			m.size,
			(SELECT count(*) FROM attachments attachment WHERE attachment.message = m.ROWID)
		FROM membership membership
		JOIN messages m ON m.ROWID = membership.id
		JOIN mailboxes mb ON mb.ROWID = m.mailbox
		JOIN subjects subject ON subject.ROWID = m.subject
		JOIN addresses sender ON sender.ROWID = m.sender
		LEFT JOIN summaries summary ON summary.ROWID = m.summary
		WHERE m.deleted = 0
	` + cursorClause + `
		ORDER BY m.date_received DESC, m.ROWID DESC
		LIMIT ?
	`
}

type rowScanner interface {
	Scan(destinations ...any) error
}

func scanMessageRecord(row rowScanner) (messageRecord, error) {
	var item messageRecord
	if err := row.Scan(
		&item.RowID, &item.StoreMessageID, &item.StoreGlobalID, &item.StoreMailboxID,
		&item.PhysicalURL,
		&item.Subject, &item.SenderAddress, &item.SenderName, &item.SummaryText,
		&item.DateSent, &item.DateReceived, &item.Read, &item.Flagged, &item.Deleted,
		&item.Junk, &item.Size, &item.AttachmentCount,
	); err != nil {
		return messageRecord{}, fmt.Errorf("scan Envelope Index message: %w", err)
	}
	return item, nil
}

func mapMessagePage(
	items []messageRecord,
	mailboxRef string,
	mailbox mailref.Mailbox,
	limit int,
	storeUUID string,
) (mail.MessagePage, error) {
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	messages := make([]mail.MessageSummary, 0, len(items))
	for _, item := range items {
		summary, err := mapMessageSummary(item, mailboxRef, mailbox.AccountID, mailbox.Path, storeUUID)
		if err != nil {
			return mail.MessagePage{}, err
		}
		messages = append(messages, summary)
	}
	page := mail.MessagePage{Messages: messages}
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		cursor, err := encodeListCursor(listCursor{
			StoreUUID: storeUUID, MailboxRef: mailboxRef,
			DateReceived: last.DateReceived, RowID: last.RowID,
		})
		if err != nil {
			return mail.MessagePage{}, err
		}
		page.NextCursor = cursor
	}
	return page, nil
}

func mapMessageSummary(
	item messageRecord,
	mailboxRef string,
	accountID string,
	mailboxPath []string,
	storeUUID string,
) (mail.MessageSummary, error) {
	messageRef, err := mailref.EncodeMessage(mailref.Message{
		AccountID: accountID, MailboxPath: mailboxPath, LibraryID: strconv.FormatInt(item.RowID, 10),
		ExpectedSubject: item.Subject, ExpectedStoreUUID: storeUUID,
		ExpectedStoreMailboxID: item.StoreMailboxID,
		ExpectedStoreMessageID: item.StoreMessageID,
		ExpectedStoreGlobalID:  item.StoreGlobalID,
	})
	if err != nil {
		return mail.MessageSummary{}, err
	}
	return mail.MessageSummary{
		Ref: messageRef, MailboxRef: mailboxRef, Subject: item.Subject,
		Sender:       formatSender(item.SenderName, item.SenderAddress),
		DateReceived: formatUnixTime(item.DateReceived), DateSent: formatUnixTime(item.DateSent),
		Read: item.Read, Flagged: item.Flagged, Junk: item.Junk, Deleted: item.Deleted,
		Size: item.Size, AttachmentCount: item.AttachmentCount,
	}, nil
}

func formatSender(name string, address string) string {
	if name == "" {
		return address
	}
	return (&stdmail.Address{Name: name, Address: address}).String()
}

func formatUnixTime(value int64) string {
	if value == 0 {
		return ""
	}
	return time.Unix(value, 0).UTC().Format(time.RFC3339)
}

func encodeListCursor(cursor listCursor) (string, error) {
	cursor.Version = listCursorVersion
	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode list cursor: %w", err)
	}
	return "lcur_" + base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeListCursor(value string, mailboxRef string, storeUUID string) (*listCursor, error) {
	if value == "" {
		return nil, nil
	}
	if !strings.HasPrefix(value, "lcur_") {
		return nil, operationError("invalid_cursor", "invalid list cursor prefix")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, "lcur_"))
	if err != nil {
		return nil, operationError("invalid_cursor", fmt.Sprintf("decode list cursor: %v", err))
	}
	var cursor listCursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return nil, operationError("invalid_cursor", fmt.Sprintf("parse list cursor: %v", err))
	}
	if cursor.Version != listCursorVersion || cursor.StoreUUID != storeUUID ||
		cursor.MailboxRef != mailboxRef || cursor.RowID < 1 {
		return nil, operationError("invalid_cursor", "list cursor does not match this mailbox")
	}
	return &cursor, nil
}
