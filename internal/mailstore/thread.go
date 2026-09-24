package mailstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

const (
	threadCursorPrefix  = "thrc_"
	threadCursorVersion = 1
)

const threadCursorDateNull uint8 = 1

type threadCursor struct {
	dateReceived     int64
	dateReceivedNull bool
	rowID            int64
}

// MessageThread is a local-store read; the Apple Events fallback cannot
// provide conversation membership, so it fails closed like other
// store-only reads.
func (c *Client) MessageThread(
	ctx context.Context,
	request mail.MessageThreadRequest,
) (mail.MessageThread, error) {
	if c.store == nil {
		return mail.MessageThread{}, c.readUnavailableError()
	}
	return c.store.MessageThread(ctx, request)
}

// MessageThread returns the bounded chronological member list of the
// Envelope Index conversation containing the referenced message. The seed
// reference resolves through the same identity and staleness checks as
// message reads; members are located by the store's conversation_id
// grouping key rather than RFC headers, so the result mirrors Mail.app's
// own conversation grouping. Members in deactivated accounts are omitted
// because their references could not resolve again. The read is local
// only: no IMAP traffic and no Apple Events.
func (s *Store) MessageThread(
	ctx context.Context,
	request mail.MessageThreadRequest,
) (result mail.MessageThread, resultErr error) {
	if request.Limit < 1 || request.Limit > mail.MaximumPageLimit {
		return mail.MessageThread{}, operationError(
			"invalid_argument", fmt.Sprintf("limit must be between 1 and %d", mail.MaximumPageLimit),
		)
	}
	resolved, err := s.resolveMessage(ctx, request.Ref)
	if err != nil {
		return mail.MessageThread{}, err
	}
	thread := mail.MessageThread{
		Ref:            request.Ref,
		ConversationID: resolved.Record.ConversationID,
	}
	if resolved.Record.ConversationID <= 0 {
		if request.Cursor != "" {
			return mail.MessageThread{}, operationError("invalid_cursor", "ungrouped messages do not have a continuation cursor")
		}
		summary, _, err := s.threadSummary(resolved.Record)
		if err != nil {
			return mail.MessageThread{}, err
		}
		thread.Messages = []mail.MessageSummary{summary}
		return thread, nil
	}
	indexRevision, err := s.searchIndexRevision(ctx)
	if err != nil {
		return mail.MessageThread{}, err
	}
	cursor, err := decodeThreadCursor(
		request.Cursor, request.Ref, resolved.Record.ConversationID, s.storeUUID, indexRevision,
	)
	if err != nil {
		return mail.MessageThread{}, err
	}
	records, err := s.conversationRecords(ctx, resolved.Record.ConversationID, cursor, request.Limit+1)
	if err != nil {
		return mail.MessageThread{}, err
	}
	hasMore := len(records) > request.Limit
	if hasMore {
		thread.Truncated = true
		records = records[:request.Limit]
	}
	for _, record := range records {
		summary, visible, err := s.threadSummary(record)
		if err != nil {
			return mail.MessageThread{}, err
		}
		if !visible {
			continue
		}
		thread.Messages = append(thread.Messages, summary)
	}
	if hasMore && len(records) > 0 {
		thread.NextCursor, err = encodeThreadCursor(
			records[len(records)-1], request.Ref, resolved.Record.ConversationID, s.storeUUID, indexRevision,
		)
		if err != nil {
			return mail.MessageThread{}, err
		}
	}
	return thread, nil
}

func (s *Store) conversationRecords(
	ctx context.Context,
	conversationID int64,
	cursor *threadCursor,
	limit int,
) (result []messageRecord, resultErr error) {
	activeSQL, arguments := s.activeAccountSQL()
	query := `
		SELECT
			m.ROWID, COALESCE(m.message_id, 0), COALESCE(m.global_message_id, 0),
			COALESCE(m.remote_id, 0), COALESCE(m.remote_mailbox, 0),
			m.mailbox, mb.url,
			subject.subject, sender.address, sender.comment,
			COALESCE(summary.summary, ''), COALESCE(m.date_sent, 0), m.date_sent IS NULL,
			COALESCE(m.date_received, 0), m.date_received IS NULL, m.read, m.flagged, m.deleted,
			EXISTS(
				SELECT 1 FROM server_messages sm
				WHERE sm.message = m.ROWID AND sm.junk_level > 0
			),
			m.size,
			(SELECT count(*) FROM attachments attachment WHERE attachment.message = m.ROWID),
			COALESCE(m.conversation_id, 0)
		FROM messages m
		JOIN mailboxes mb ON mb.ROWID = m.mailbox
		JOIN subjects subject ON subject.ROWID = m.subject
		JOIN addresses sender ON sender.ROWID = m.sender
		LEFT JOIN summaries summary ON summary.ROWID = m.summary
		WHERE m.conversation_id = ? AND m.deleted = 0 AND ` + activeSQL
	arguments = append([]any{conversationID}, arguments...)
	if cursor != nil {
		if cursor.dateReceivedNull {
			query += ` AND ((m.date_received IS NULL AND m.ROWID > ?) OR m.date_received IS NOT NULL)`
			arguments = append(arguments, cursor.rowID)
		} else {
			query += ` AND (m.date_received > ? OR (m.date_received = ? AND m.ROWID > ?))`
			arguments = append(arguments, cursor.dateReceived, cursor.dateReceived, cursor.rowID)
		}
	}
	query += ` ORDER BY m.date_received ASC, m.ROWID ASC LIMIT ?`
	arguments = append(arguments, limit)
	rows, err := s.database.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("list Envelope Index conversation members: %w", err)
	}
	defer joinCloseError(&resultErr, rows, "conversation member rows")
	items := make([]messageRecord, 0, limit)
	for rows.Next() {
		item, err := scanMessageRecord(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Envelope Index conversation members: %w", err)
	}
	return items, nil
}

func encodeThreadCursor(
	item messageRecord,
	seedRef string,
	conversationID int64,
	storeUUID string,
	indexRevision string,
) (string, error) {
	var flags uint8
	if item.DateReceivedNull {
		flags = threadCursorDateNull
	}
	token, err := mailref.EncodeCompactTokenPayload(threadCursorPrefix, &mailref.CompactPayload{
		Fingerprint: threadCursorFingerprint(seedRef, conversationID), StoreUUID: storeUUID,
		IndexRevision: indexRevision, DateReceived: item.DateReceived, Flags: flags, RowID: item.RowID,
	}, threadCursorVersion)
	if err != nil {
		return "", fmt.Errorf("encode thread cursor: %w", err)
	}
	return token, nil
}

func decodeThreadCursor(
	value string,
	seedRef string,
	conversationID int64,
	storeUUID string,
	indexRevision string,
) (*threadCursor, error) {
	if value == "" {
		return nil, nil
	}
	payload, err := mailref.DecodeTokenPayload(threadCursorPrefix, value)
	if err != nil {
		return nil, operationError("invalid_cursor", err.Error())
	}
	compact, err := mailref.DecodeCompactPayload(payload, threadCursorVersion)
	if err != nil {
		return nil, operationError("invalid_cursor", err.Error())
	}
	if compact.Flags > threadCursorDateNull || compact.RowID < 1 || compact.StoreUUID != storeUUID ||
		compact.IndexRevision != indexRevision ||
		compact.Fingerprint != threadCursorFingerprint(seedRef, conversationID) {
		return nil, operationError("invalid_cursor", "thread cursor does not match this conversation, Mail store, or index revision")
	}
	return &threadCursor{
		dateReceived: compact.DateReceived, dateReceivedNull: compact.Flags&threadCursorDateNull != 0,
		rowID: compact.RowID,
	}, nil
}

func threadCursorFingerprint(seedRef string, conversationID int64) string {
	value := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", conversationID, seedRef)))
	return hex.EncodeToString(value[:])
}

// threadSummary projects one member into the list summary shape. The member
// mailbox comes from the record itself because conversations span mailboxes
// (for example Sent copies next to Inbox messages). Members whose account is
// no longer active report visible=false because their references could not
// resolve again.
func (s *Store) threadSummary(record messageRecord) (mail.MessageSummary, bool, error) {
	location, err := parseMailboxURL(record.PhysicalURL)
	if err != nil {
		return mail.MessageSummary{}, false, operationError(
			"unsupported_mail_store_schema", fmt.Sprintf("message has an unsafe physical mailbox URL: %v", err),
		)
	}
	if _, active := s.activeAccountKeys[location.rootKey()]; !active {
		return mail.MessageSummary{}, false, nil
	}
	mailboxRef, err := mailref.EncodeMailbox(location.AccountID, location.VisiblePath)
	if err != nil {
		return mail.MessageSummary{}, false, err
	}
	summary, err := mapMessageSummary(
		record, mailboxRef, location.AccountID, location.VisiblePath, s.storeUUID,
	)
	return summary, err == nil, err
}
