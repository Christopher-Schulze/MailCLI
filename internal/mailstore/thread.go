package mailstore

import (
	"context"
	"fmt"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

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
		summary, _, err := s.threadSummary(resolved.Record)
		if err != nil {
			return mail.MessageThread{}, err
		}
		thread.Messages = []mail.MessageSummary{summary}
		return thread, nil
	}
	records, err := s.conversationRecords(ctx, resolved.Record.ConversationID, request.Limit+1)
	if err != nil {
		return mail.MessageThread{}, err
	}
	if len(records) > request.Limit {
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
	return thread, nil
}

func (s *Store) conversationRecords(
	ctx context.Context,
	conversationID int64,
	limit int,
) (result []messageRecord, resultErr error) {
	rows, err := s.database.QueryContext(ctx, `
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
		WHERE m.conversation_id = ? AND m.deleted = 0
		ORDER BY m.date_received ASC, m.ROWID ASC
		LIMIT ?
	`, conversationID, limit)
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
