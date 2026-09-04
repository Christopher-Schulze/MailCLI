package mailstore

import (
	"context"
	"fmt"
	"strconv"

	"mailcli/internal/mailref"
)

type transferBaseline struct {
	Source               resolvedMessage
	SourceMailbox        mailboxRecord
	Destination          mailref.Mailbox
	DestinationMailbox   mailboxRecord
	MaximumRowID         int64
	DestinationHadSource bool
}

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

func (s *Store) captureTransferBaseline(
	ctx context.Context,
	messageRef string,
	destinationRef string,
) (transferBaseline, error) {
	source, err := s.resolveMessage(ctx, messageRef)
	if err != nil {
		return transferBaseline{}, err
	}
	sourceMailbox, err := s.mailboxRecordForRef(
		ctx, source.Reference.AccountID, source.Reference.MailboxPath,
	)
	if err != nil {
		return transferBaseline{}, err
	}
	destination, err := mailref.DecodeMailbox(destinationRef)
	if err != nil {
		return transferBaseline{}, operationError(
			"invalid_reference", "invalid destination mailbox ref: "+err.Error(),
		)
	}
	mailbox, err := s.mailboxRecordForRef(ctx, destination.AccountID, destination.Path)
	if err != nil {
		return transferBaseline{}, err
	}
	var maximumRowID int64
	if err := s.database.QueryRowContext(ctx, "SELECT COALESCE(max(ROWID), 0) FROM messages").Scan(
		&maximumRowID,
	); err != nil {
		return transferBaseline{}, fmt.Errorf("capture transfer baseline: %w", err)
	}
	destinationHadSource, err := s.messageHasMembership(ctx, source.Reference.LibraryID, mailbox.RowID)
	if err != nil {
		return transferBaseline{}, err
	}
	return transferBaseline{
		Source: source, SourceMailbox: sourceMailbox, Destination: destination,
		DestinationMailbox: mailbox, MaximumRowID: maximumRowID,
		DestinationHadSource: destinationHadSource,
	}, nil
}

func observedTransferCandidates(
	candidates []messageRecord,
	baseline transferBaseline,
	copyMessage bool,
) []messageRecord {
	observed := make([]messageRecord, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.RowID == baseline.Source.Record.RowID {
			if !copyMessage || !baseline.DestinationHadSource {
				observed = append(observed, candidate)
			}
			continue
		}
		if candidate.RowID > baseline.MaximumRowID {
			observed = append(observed, candidate)
		}
	}
	return observed
}

func (s *Store) transferCandidates(
	ctx context.Context,
	baseline transferBaseline,
) (result []messageRecord, resultErr error) {
	source := baseline.Source.Record
	rows, err := s.database.QueryContext(ctx, `
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
			EXISTS (
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
		WHERE m.deleted = 0 AND subject.subject = ?
		AND sender.address = ? AND m.size = ?
		AND (
			m.ROWID = ? OR
			(? > 0 AND ? > 0 AND m.message_id = ? AND m.global_message_id = ?)
		)
		ORDER BY m.ROWID
		LIMIT 8
	`,
		baseline.DestinationMailbox.RowID, baseline.DestinationMailbox.RowID,
		source.Subject, source.SenderAddress, source.Size,
		source.RowID,
		source.StoreMessageID, source.StoreGlobalID,
		source.StoreMessageID, source.StoreGlobalID,
	)
	if err != nil {
		return nil, fmt.Errorf("query transferred message: %w", err)
	}
	defer joinCloseError(&resultErr, rows, "transfer candidate rows")
	var records []messageRecord
	for rows.Next() {
		record, err := scanMessageRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate transferred message: %w", err)
	}
	return records, nil
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
