package mailstore

import (
	"context"
	"fmt"
	"strconv"
)

func (s *Store) messageInSpecialMailbox(ctx context.Context, messageRef string, attribute int) (bool, error) {
	resolved, err := s.resolveMessage(ctx, messageRef)
	if err != nil {
		return false, err
	}
	var flaggedDraft bool
	if err := s.database.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM server_messages WHERE message = ? AND draft != 0
		)
	`, resolved.Record.RowID).Scan(&flaggedDraft); err != nil {
		return false, fmt.Errorf("inspect message draft state: %w", err)
	}
	if flaggedDraft {
		return true, nil
	}
	records, err := s.mailboxRecords(ctx)
	if err != nil {
		return false, err
	}
	byPath := make(map[string]mailboxRecord, len(records))
	for _, record := range records {
		byPath[mailboxPathKey(record.Location.AccountID, record.Location.VisiblePath)] = record
	}
	cached, err := s.loadMailboxCache(ctx, resolved.Reference.AccountID)
	if err != nil {
		return false, fmt.Errorf("inspect Drafts mailbox identity: %w", err)
	}
	identifiers := make(map[int64]struct{})
	if err := collectMailboxIDs(
		cached.Mailboxes, nil, resolved.Reference.AccountID, resolved.PhysicalLocation.Scheme,
		attribute, byPath, identifiers, 1,
	); err != nil {
		return false, err
	}
	_, protected := identifiers[resolved.Record.StoreMailboxID]
	if protected {
		return true, nil
	}
	sourceMailbox, err := s.mailboxRecordForRef(
		ctx, resolved.Reference.AccountID, resolved.Reference.MailboxPath,
	)
	if err != nil {
		return false, err
	}
	_, protected = identifiers[sourceMailbox.RowID]
	return protected, nil
}

func (s *Store) mailboxRecordForRef(
	ctx context.Context,
	accountID string,
	path []string,
) (mailboxRecord, error) {
	records, err := s.mailboxRecords(ctx)
	if err != nil {
		return mailboxRecord{}, err
	}
	record, found := findMailboxRecord(records, accountID, path)
	if !found {
		return mailboxRecord{}, operationError("not_found", "mailbox is not present in the local Mail store")
	}
	return record, nil
}

func (s *Store) messageHasMembership(
	ctx context.Context,
	libraryID string,
	mailboxRowID int64,
) (bool, error) {
	rowID, err := strconv.ParseInt(libraryID, 10, 64)
	if err != nil || rowID < 1 {
		return false, operationError("invalid_reference", "message ref has an invalid ROWID")
	}
	var present bool
	err = s.database.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM messages m
			WHERE m.ROWID = ? AND m.deleted = 0
			AND (
				m.mailbox = ? OR EXISTS (
					SELECT 1 FROM labels label
					WHERE label.message_id = m.ROWID AND label.mailbox_id = ?
				)
			)
		)
	`, rowID, mailboxRowID, mailboxRowID).Scan(&present)
	if err != nil {
		return false, fmt.Errorf("observe message mailbox membership: %w", err)
	}
	return present, nil
}
