package mailstore

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

func (s *Store) listUnifiedInbox(ctx context.Context, request mail.ListMessagesRequest) (mail.MessagePage, error) {
	mailboxes, err := s.selectedListMailboxes(ctx, request)
	if err != nil {
		return mail.MessagePage{}, err
	}
	scope := unifiedInboxScope(request.AccountRef, mailboxes)
	cursor, err := decodeListCursor(request.Cursor, scope, s.storeUUID)
	if err != nil {
		return mail.MessagePage{}, err
	}
	records, err := s.mailboxRecords(ctx)
	if err != nil {
		return mail.MessagePage{}, err
	}
	identifiers, byAccount, err := unifiedInboxIdentities(mailboxes, records)
	if err != nil {
		return mail.MessagePage{}, err
	}
	items, err := s.queryUnifiedInbox(ctx, identifiers, cursor, request.Limit+1)
	if err != nil {
		return mail.MessagePage{}, err
	}
	return mapUnifiedInboxPage(items, byAccount, scope, request.Limit, s.storeUUID)
}

func unifiedInboxScope(accountRef string, mailboxes []mail.Mailbox) string {
	refs := make([]string, len(mailboxes))
	for index, mailbox := range mailboxes {
		refs[index] = mailbox.Ref
	}
	sort.Strings(refs)
	return fmt.Sprintf("unified-inbox:%x", sha256.Sum256([]byte(accountRef+"\x00"+strings.Join(refs, "\x00"))))
}

func unifiedInboxIdentities(mailboxes []mail.Mailbox, records []mailboxRecord) ([]int64, map[string]mail.Mailbox, error) {
	identifiers := make([]int64, 0, len(mailboxes))
	byAccount := make(map[string]mail.Mailbox, len(mailboxes))
	for _, mailbox := range mailboxes {
		identity, err := mailref.DecodeMailbox(mailbox.Ref)
		if err != nil {
			return nil, nil, err
		}
		byAccount[strings.ToUpper(identity.AccountID)] = mailbox
		record, found := findMailboxRecord(records, identity.AccountID, identity.Path)
		if !found {
			if mailbox.LocalMessagesAvailable {
				return nil, nil, operationError("stale_reference", "inbox disappeared from the local mailbox catalog")
			}
			continue
		}
		identifiers = append(identifiers, record.RowID)
	}
	return identifiers, byAccount, nil
}

func unifiedInboxQuery(identifiers []int64, cursor *listCursor, limit int) (string, []any) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(identifiers)), ",")
	arguments := make([]any, 0, len(identifiers)*2+4)
	for range 2 {
		for _, identifier := range identifiers {
			arguments = append(arguments, identifier)
		}
	}
	clause := ""
	if cursor != nil {
		if cursor.DateReceivedNull {
			clause = " AND m.date_received IS NULL AND m.ROWID < ?"
			arguments = append(arguments, cursor.RowID)
		} else {
			clause = " AND ((m.date_received < ? OR (m.date_received = ? AND m.ROWID < ?)) OR m.date_received IS NULL)"
			arguments = append(arguments, cursor.DateReceived, cursor.DateReceived, cursor.RowID)
		}
	}
	query := "WITH membership(id) AS (SELECT ROWID FROM messages WHERE mailbox IN (" + placeholders +
		") UNION SELECT message_id FROM labels WHERE mailbox_id IN (" + placeholders + ")) " +
		mailboxMessagePageSQL("membership membership JOIN messages m ON m.ROWID = membership.id", "", clause)
	return query, append(arguments, limit)
}

func (s *Store) queryUnifiedInbox(ctx context.Context, identifiers []int64, cursor *listCursor, limit int) (items []messageRecord, resultErr error) {
	if len(identifiers) == 0 {
		return []messageRecord{}, nil
	}
	query, arguments := unifiedInboxQuery(identifiers, cursor, limit)
	rows, err := s.database.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("query unified inbox snapshot: %w", err)
	}
	defer joinCloseError(&resultErr, rows, "unified inbox rows")
	items = make([]messageRecord, 0, limit)
	for rows.Next() {
		item, err := scanListMessageRecord(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unified inbox snapshot: %w", err)
	}
	return items, nil
}

func mapUnifiedInboxPage(items []messageRecord, mailboxes map[string]mail.Mailbox, scope string, limit int, storeUUID string) (mail.MessagePage, error) {
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	page := mail.MessagePage{Messages: make([]mail.MessageSummary, 0, len(items))}
	for _, item := range items {
		location, err := parseMailboxURL(item.PhysicalURL)
		if err != nil {
			return mail.MessagePage{}, err
		}
		mailbox, found := mailboxes[strings.ToUpper(location.AccountID)]
		if !found {
			return mail.MessagePage{}, operationError("unsupported_mail_store_schema", "inbox membership crosses account identities")
		}
		summary, err := mapMessageSummary(item, mailbox.Ref, location.AccountID, mailbox.Path, storeUUID)
		if err != nil {
			return mail.MessagePage{}, err
		}
		summary.Account = mailbox.AccountRef
		page.Messages = append(page.Messages, summary)
	}
	if hasMore {
		last := items[len(items)-1]
		cursor, err := encodeListCursor(listCursor{StoreUUID: storeUUID, MailboxRef: scope,
			DateReceived: last.DateReceived, DateReceivedNull: last.DateReceivedNull, RowID: last.RowID})
		if err != nil {
			return mail.MessagePage{}, err
		}
		page.NextCursor = cursor
	}
	return page, nil
}
