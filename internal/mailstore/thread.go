package mailstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

const (
	threadCursorPrefix        = "thrc_"
	threadCursorVersion       = 2
	legacyThreadCursorVersion = 1
)

const (
	threadCursorDateNull uint8 = 1 << iota
	threadCursorOlder
)

type threadCursor struct {
	dateReceived     int64
	dateReceivedNull bool
	rowID            int64
	older            bool
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
			return mail.MessageThread{}, &mail.ValidationError{Code: "invalid_cursor", Message: "ungrouped messages do not have a continuation cursor"}
		}
		summary, err := s.threadSummary(resolved.Record)
		if err != nil {
			return mail.MessageThread{}, err
		}
		thread.Messages = []mail.MessageSummary{summary}
		return thread, nil
	}
	cursor, err := decodeThreadCursor(
		request.Cursor, request.Ref, resolved.Record.ConversationID, s.storeUUID,
	)
	if err != nil {
		return mail.MessageThread{}, err
	}
	records, hasOlder, hasNewer, err := s.threadPageRecords(ctx, resolved.Record, cursor, request.Limit)
	if err != nil {
		return mail.MessageThread{}, err
	}
	thread.Truncated = hasOlder || hasNewer
	for _, record := range records {
		summary, err := s.threadSummary(record)
		if err != nil {
			return mail.MessageThread{}, err
		}
		thread.Messages = append(thread.Messages, summary)
	}
	if hasNewer && len(records) > 0 {
		thread.NextCursor, err = encodeThreadCursor(
			records[len(records)-1], request.Ref, resolved.Record.ConversationID, s.storeUUID, false,
		)
		if err != nil {
			return mail.MessageThread{}, err
		}
	}
	if hasOlder && len(records) > 0 {
		thread.PrevCursor, err = encodeThreadCursor(
			records[0], request.Ref, resolved.Record.ConversationID, s.storeUUID, true,
		)
		if err != nil {
			return mail.MessageThread{}, err
		}
	}
	return thread, nil
}

func (s *Store) threadPageRecords(ctx context.Context, seed messageRecord, cursor *threadCursor, limit int) ([]messageRecord, bool, bool, error) {
	if cursor == nil {
		return s.seedThreadRecords(ctx, seed, limit)
	}
	records, err := s.conversationRecords(ctx, seed.ConversationID, cursor, limit+1)
	if err != nil {
		return nil, false, false, err
	}
	hasMore := len(records) > limit
	if hasMore {
		if cursor.older {
			records = records[1:]
		} else {
			records = records[:limit]
		}
	}
	if len(records) == 0 {
		return records, false, false, nil
	}
	boundary := records[0]
	if cursor.older {
		boundary = records[len(records)-1]
	}
	opposite, err := s.conversationRecords(ctx, seed.ConversationID, threadCursorFor(boundary, !cursor.older), 1)
	if err != nil {
		return nil, false, false, err
	}
	if cursor.older {
		return records, hasMore, len(opposite) > 0, nil
	}
	return records, len(opposite) > 0, hasMore, nil
}

func (s *Store) seedThreadRecords(ctx context.Context, seed messageRecord, limit int) ([]messageRecord, bool, bool, error) {
	older, err := s.conversationRecords(ctx, seed.ConversationID, threadCursorFor(seed, true), limit)
	if err != nil {
		return nil, false, false, err
	}
	newer, err := s.conversationRecords(ctx, seed.ConversationID, threadCursorFor(seed, false), limit)
	if err != nil {
		return nil, false, false, err
	}
	olderCount := min((limit-1)/2, len(older))
	newerCount := min(limit-1-olderCount, len(newer))
	olderCount = min(limit-1-newerCount, len(older))
	records := append(append(older[len(older)-olderCount:], seed), newer[:newerCount]...)
	return records, len(older) > olderCount, len(newer) > newerCount, nil
}

func threadCursorFor(item messageRecord, older bool) *threadCursor {
	return &threadCursor{dateReceived: item.DateReceived, dateReceivedNull: item.DateReceivedNull, rowID: item.RowID, older: older}
}

func (s *Store) threadAccountSQL() (string, []any) {
	keys := make([]string, 0, len(s.activeAccountKeys))
	for key := range s.activeAccountKeys {
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return "0", nil
	}
	sort.Strings(keys)
	arguments := make([]any, len(keys))
	for index, key := range keys {
		arguments[index] = key
	}
	return mailboxAccountRootSQLName + "(mb.url) IN (" + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + ")", arguments
}

func (s *Store) conversationRecords(
	ctx context.Context,
	conversationID int64,
	cursor *threadCursor,
	limit int,
) (result []messageRecord, resultErr error) {
	activeSQL, arguments := s.threadAccountSQL()
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
		if cursor.older && cursor.dateReceivedNull {
			query += ` AND m.date_received IS NULL AND m.ROWID < ?`
			arguments = append(arguments, cursor.rowID)
		} else if cursor.older {
			query += ` AND (m.date_received IS NULL OR m.date_received < ? OR (m.date_received = ? AND m.ROWID < ?))`
			arguments = append(arguments, cursor.dateReceived, cursor.dateReceived, cursor.rowID)
		} else if cursor.dateReceivedNull {
			query += ` AND ((m.date_received IS NULL AND m.ROWID > ?) OR m.date_received IS NOT NULL)`
			arguments = append(arguments, cursor.rowID)
		} else {
			query += ` AND (m.date_received > ? OR (m.date_received = ? AND m.ROWID > ?))`
			arguments = append(arguments, cursor.dateReceived, cursor.dateReceived, cursor.rowID)
		}
	}
	if cursor != nil && cursor.older {
		query += ` ORDER BY m.date_received DESC, m.ROWID DESC LIMIT ?`
	} else {
		query += ` ORDER BY m.date_received ASC, m.ROWID ASC LIMIT ?`
	}
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
	if cursor != nil && cursor.older {
		slices.Reverse(items)
	}
	return items, nil
}

func encodeThreadCursor(
	item messageRecord,
	seedRef string,
	conversationID int64,
	storeUUID string,
	older bool,
) (string, error) {
	var flags uint8
	if item.DateReceivedNull {
		flags = threadCursorDateNull
	}
	if older {
		flags |= threadCursorOlder
	}
	token, err := mailref.EncodeCompactTokenPayload(threadCursorPrefix, &mailref.CompactPayload{
		Fingerprint: threadCursorFingerprint(seedRef, conversationID), StoreUUID: storeUUID,
		DateReceived: item.DateReceived, Flags: flags, RowID: item.RowID,
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
) (*threadCursor, error) {
	if value == "" {
		return nil, nil
	}
	payload, err := mailref.DecodeTokenPayload(threadCursorPrefix, value)
	if err != nil {
		return nil, &mail.ValidationError{Code: "invalid_cursor", Message: err.Error()}
	}
	compact, err := mailref.DecodeCompactPayload(payload, threadCursorVersion)
	maximumFlags := threadCursorDateNull | threadCursorOlder
	if err != nil {
		compact, err = mailref.DecodeCompactPayload(payload, legacyThreadCursorVersion)
		maximumFlags = threadCursorDateNull
	}
	if err != nil {
		return nil, &mail.ValidationError{Code: "invalid_cursor", Message: err.Error()}
	}
	if compact.Flags > maximumFlags || compact.RowID < 1 || compact.StoreUUID != storeUUID ||
		compact.Fingerprint != threadCursorFingerprint(seedRef, conversationID) {
		return nil, &mail.ValidationError{Code: "invalid_cursor", Message: "thread cursor does not match this conversation, Mail store, or keyset"}
	}
	return &threadCursor{
		dateReceived: compact.DateReceived, dateReceivedNull: compact.Flags&threadCursorDateNull != 0,
		rowID: compact.RowID, older: compact.Flags&threadCursorOlder != 0,
	}, nil
}

func threadCursorFingerprint(seedRef string, conversationID int64) string {
	value := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", conversationID, seedRef)))
	return hex.EncodeToString(value[:])
}

// threadSummary projects one member into the list summary shape. The member
// mailbox comes from the record itself because conversations span mailboxes
// (for example Sent copies next to Inbox messages). SQL has already selected
// active account roots; the full URL and membership checks assert that boundary.
func (s *Store) threadSummary(record messageRecord) (mail.MessageSummary, error) {
	location, err := parseMailboxURL(record.PhysicalURL)
	if err != nil {
		return mail.MessageSummary{}, operationError(
			"unsupported_mail_store_schema", fmt.Sprintf("message has an unsafe physical mailbox URL: %v", err),
		)
	}
	if _, active := s.activeAccountKeys[location.rootKey()]; !active {
		return mail.MessageSummary{}, operationError("unsupported_mail_store_schema", "thread SQL returned a member outside the active account roots")
	}
	mailboxRef, err := mailref.EncodeMailbox(location.AccountID, location.VisiblePath)
	if err != nil {
		return mail.MessageSummary{}, err
	}
	summary, err := mapMessageSummary(
		record, mailboxRef, location.AccountID, location.VisiblePath, s.storeUUID,
	)
	return summary, err
}
