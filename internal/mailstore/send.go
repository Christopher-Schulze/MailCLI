package mailstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	stdmail "net/mail"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

const (
	mailboxAttributeDrafts      = 1 << 12
	mailboxAttributeSent        = 1 << 15
	sendObservationWindow       = 10 * time.Second
	sendObservationInitialDelay = 200 * time.Millisecond
	sendObservationMaximumDelay = time.Second
)

type sendBaseline struct {
	MaximumRowID   int64
	CapturedUnix   int64
	SentMailboxIDs []int64
}

func (s *Store) exportSendBaseline(value sendBaseline) *mail.SendObservationBaseline {
	return &mail.SendObservationBaseline{
		StoreUUID: s.storeUUID, MaximumRowID: value.MaximumRowID,
		CapturedUnix:   value.CapturedUnix,
		SentMailboxIDs: append([]int64(nil), value.SentMailboxIDs...),
	}
}

func (s *Store) importSendBaseline(value *mail.SendObservationBaseline) (sendBaseline, error) {
	if value == nil || value.StoreUUID != s.storeUUID || value.MaximumRowID < 0 ||
		value.CapturedUnix < 1 || len(value.SentMailboxIDs) == 0 {
		return sendBaseline{}, operationError(
			"send_reconcile_unavailable", "send observation baseline is missing or belongs to a different Mail store",
		)
	}
	identifiers := append([]int64(nil), value.SentMailboxIDs...)
	seen := make(map[int64]struct{}, len(identifiers))
	for _, identifier := range identifiers {
		if identifier < 1 {
			return sendBaseline{}, operationError("send_reconcile_unavailable", "send observation baseline is invalid")
		}
		if _, exists := seen[identifier]; exists {
			return sendBaseline{}, operationError("send_reconcile_unavailable", "send observation baseline is invalid")
		}
		seen[identifier] = struct{}{}
	}
	return sendBaseline{
		MaximumRowID: value.MaximumRowID, CapturedUnix: value.CapturedUnix,
		SentMailboxIDs: identifiers,
	}, nil
}

type recipientAddressSets struct {
	To  map[string]struct{}
	CC  map[string]struct{}
	BCC map[string]struct{}
}

type attachmentFingerprint struct {
	Name   string
	Size   int64
	SHA256 string
}

func (s *Store) captureSendBaseline(ctx context.Context) (sendBaseline, error) {
	return s.captureMailboxBaseline(
		ctx, mailboxAttributeSent, "sent_store_unavailable",
		"no active Sent mailbox is available in the local Mail store",
	)
}

func (s *Store) captureMailboxBaseline(
	ctx context.Context,
	attribute int,
	errorCode string,
	errorMessage string,
) (sendBaseline, error) {
	var baseline sendBaseline
	if err := s.database.QueryRowContext(ctx, "SELECT COALESCE(max(ROWID), 0) FROM messages").Scan(
		&baseline.MaximumRowID,
	); err != nil {
		return sendBaseline{}, fmt.Errorf("capture sent-message baseline: %w", err)
	}
	mailboxIDs, err := s.mailboxIDsWithAttribute(ctx, attribute)
	if err != nil {
		return sendBaseline{}, err
	}
	if len(mailboxIDs) == 0 {
		return sendBaseline{}, operationError(errorCode, errorMessage)
	}
	baseline.CapturedUnix = time.Now().UTC().Unix()
	baseline.SentMailboxIDs = mailboxIDs
	return baseline, nil
}

func (s *Store) mailboxIDsWithAttribute(ctx context.Context, attribute int) ([]int64, error) {
	records, err := s.loadMailboxRecords(ctx)
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]mailboxRecord, len(records))
	accountSchemes := make(map[string]string)
	for _, record := range records {
		byPath[mailboxPathKey(record.Location.AccountID, record.Location.VisiblePath)] = record
		accountSchemes[record.Location.AccountID] = record.Location.Scheme
	}
	unique := make(map[int64]struct{})
	for accountID, scheme := range accountSchemes {
		cached, err := s.loadMailboxCache(ctx, accountID)
		if err != nil {
			continue
		}
		collectMailboxIDs(cached.Mailboxes, nil, accountID, scheme, attribute, byPath, unique)
	}
	identifiers := make([]int64, 0, len(unique))
	for identifier := range unique {
		identifiers = append(identifiers, identifier)
	}
	sort.Slice(identifiers, func(left int, right int) bool {
		return identifiers[left] < identifiers[right]
	})
	return identifiers, nil
}

func collectMailboxIDs(
	nodes map[string]mailboxCacheNode,
	parent []string,
	accountID string,
	scheme string,
	attribute int,
	records map[string]mailboxRecord,
	output map[int64]struct{},
) {
	for key, node := range nodes {
		component := node.PathComponent
		if component == "" {
			component = key
		}
		rawPath := append(append([]string(nil), parent...), component)
		visiblePath := append([]string(nil), rawPath...)
		if len(visiblePath) > 1 && visiblePath[0] == "[Gmail]" {
			visiblePath = visiblePath[1:]
		}
		if node.Attributes&attribute != 0 {
			location := mailboxLocation{
				Scheme: scheme, AccountID: accountID, RawPath: rawPath, VisiblePath: visiblePath,
			}
			if record, found := records[mailboxPathKey(location.AccountID, location.VisiblePath)]; found {
				output[record.RowID] = struct{}{}
			}
		}
		collectMailboxIDs(node.Children, rawPath, accountID, scheme, attribute, records, output)
	}
}

func (s *Store) observeSent(
	ctx context.Context,
	baseline sendBaseline,
	draft mail.Draft,
) (mail.MessageSummary, bool, error) {
	return s.observeMailboxCandidate(ctx, baseline, draft, true)
}

func (s *Store) observeMailboxCandidate(
	ctx context.Context,
	baseline sendBaseline,
	draft mail.Draft,
	requireSentDate bool,
) (mail.MessageSummary, bool, error) {
	return observeWithBackoff(
		ctx, sendObservationInitialDelay, sendObservationMaximumDelay,
		func() (mail.MessageSummary, bool, error) {
			message, found, err := s.findMailboxCandidate(ctx, baseline, draft, requireSentDate)
			return message, found, err
		},
	)
}

func (s *Store) findSentCandidate(
	ctx context.Context,
	baseline sendBaseline,
	draft mail.Draft,
) (mail.MessageSummary, bool, error) {
	return s.findMailboxCandidate(ctx, baseline, draft, true)
}

func (s *Store) findMailboxCandidate(
	ctx context.Context,
	baseline sendBaseline,
	draft mail.Draft,
	requireSentDate bool,
) (result mail.MessageSummary, found bool, resultErr error) {
	query, arguments := mailboxCandidateQuery(baseline, draft, requireSentDate)
	rows, err := s.database.QueryContext(ctx, query, arguments...)
	if err != nil {
		return mail.MessageSummary{}, false, fmt.Errorf("query sent-message observation: %w", err)
	}
	defer joinCloseError(&resultErr, rows, "sent-message observation rows")
	var candidates []messageRecord
	for rows.Next() {
		item, err := scanMessageRecord(rows)
		if err != nil {
			return mail.MessageSummary{}, false, err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		return mail.MessageSummary{}, false, fmt.Errorf("iterate sent-message observation: %w", err)
	}
	var matches []messageRecord
	for _, item := range candidates {
		matchesDraft, err := s.recordMatchesDraft(ctx, item, draft, baseline.SentMailboxIDs)
		if err != nil {
			return mail.MessageSummary{}, false, err
		}
		if matchesDraft {
			matches = append(matches, item)
		}
	}
	if len(matches) != 1 {
		return mail.MessageSummary{}, false, nil
	}
	summary, err := s.searchSummary(matches[0], nil)
	return summary, err == nil, err
}

func mailboxCandidateQuery(
	baseline sendBaseline,
	draft mail.Draft,
	requireSentDate bool,
) (string, []any) {
	mailboxPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(baseline.SentMailboxIDs)), ",")
	arguments := make([]any, 0, len(baseline.SentMailboxIDs)*2+5)
	for _, identifier := range baseline.SentMailboxIDs {
		arguments = append(arguments, identifier)
	}
	for _, identifier := range baseline.SentMailboxIDs {
		arguments = append(arguments, identifier)
	}
	arguments = append(arguments, baseline.MaximumRowID, draft.Subject)
	dateClause := ""
	if requireSentDate {
		dateClause = " AND m.date_sent >= ?"
		arguments = append(arguments, baseline.CapturedUnix-120)
	}
	senderClause := ""
	if sender := parsedAddress(draft.From); sender != "" {
		senderClause = " AND lower(sender.address) = lower(?)"
		arguments = append(arguments, sender)
	}
	arguments = append(arguments, 32)
	query := `
		WITH membership(id) AS (
			SELECT ROWID FROM messages WHERE mailbox IN (` + mailboxPlaceholders + `)
			UNION
			SELECT message_id FROM labels WHERE mailbox_id IN (` + mailboxPlaceholders + `)
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
		WHERE m.ROWID > ? AND m.deleted = 0 AND subject.subject = ?` + dateClause + senderClause + `
		ORDER BY m.ROWID DESC
		LIMIT ?
	`
	return query, arguments
}

func (s *Store) recordMatchesDraft(
	ctx context.Context,
	record messageRecord,
	draft mail.Draft,
	mailboxIDs []int64,
) (bool, error) {
	expectedFrom := parsedAddress(draft.From)
	if expectedFrom == "" || !strings.EqualFold(expectedFrom, parsedAddress(record.SenderAddress)) {
		return false, nil
	}
	recipientsMatch, err := s.recordHasExactRecipients(ctx, record.RowID, draft)
	if err != nil || !recipientsMatch {
		return false, err
	}
	contentMatches, err := s.recordContentMatchesDraft(ctx, record, draft)
	if err != nil || !contentMatches {
		return false, err
	}
	return s.recordHasAnyMailboxMembership(ctx, record.RowID, mailboxIDs)
}

func (s *Store) recordHasAnyMailboxMembership(
	ctx context.Context,
	rowID int64,
	mailboxIDs []int64,
) (bool, error) {
	libraryID := strconv.FormatInt(rowID, 10)
	for _, mailboxID := range mailboxIDs {
		present, err := s.messageHasMembership(ctx, libraryID, mailboxID)
		if err != nil {
			return false, err
		}
		if present {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) recordHasExactRecipients(
	ctx context.Context,
	rowID int64,
	draft mail.Draft,
) (result bool, resultErr error) {
	expected, valid := draftRecipientAddressSets(draft)
	if !valid {
		return false, nil
	}
	rows, err := s.database.QueryContext(ctx, `
		SELECT recipient.type, address.address
		FROM recipients recipient
		JOIN addresses address ON address.ROWID = recipient.address
		WHERE recipient.message = ?
	`, rowID)
	if err != nil {
		return false, fmt.Errorf("query sent-message recipients: %w", err)
	}
	defer joinCloseError(&resultErr, rows, "sent-message recipient rows")
	actual := newRecipientAddressSets()
	for rows.Next() {
		var recipientType int
		var address string
		if err := rows.Scan(&recipientType, &address); err != nil {
			return false, fmt.Errorf("scan sent-message recipient: %w", err)
		}
		set, known := recipientAddressSet(&actual, recipientType)
		normalized := normalizedAddress(address)
		if !known || normalized == "" {
			return false, nil
		}
		if _, duplicate := set[normalized]; duplicate {
			return false, nil
		}
		set[normalized] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate sent-message recipients: %w", err)
	}
	return equalAddressSets(expected, actual), nil
}

func draftRecipientAddressSets(draft mail.Draft) (recipientAddressSets, bool) {
	sets := newRecipientAddressSets()
	all := make(map[string]struct{})
	groups := []struct {
		values []mail.Recipient
		set    map[string]struct{}
	}{
		{values: draft.To, set: sets.To},
		{values: draft.CC, set: sets.CC},
		{values: draft.BCC, set: sets.BCC},
	}
	for _, group := range groups {
		for _, recipient := range group.values {
			address := normalizedAddress(recipient.Address)
			if address == "" {
				return recipientAddressSets{}, false
			}
			if _, duplicate := all[address]; duplicate {
				return recipientAddressSets{}, false
			}
			all[address] = struct{}{}
			group.set[address] = struct{}{}
		}
	}
	return sets, true
}

func newRecipientAddressSets() recipientAddressSets {
	return recipientAddressSets{
		To: make(map[string]struct{}), CC: make(map[string]struct{}), BCC: make(map[string]struct{}),
	}
}

func recipientAddressSet(sets *recipientAddressSets, recipientType int) (map[string]struct{}, bool) {
	switch recipientType {
	case 0:
		return sets.To, true
	case 1:
		return sets.CC, true
	case 2:
		return sets.BCC, true
	default:
		return nil, false
	}
}

func equalAddressSets(left recipientAddressSets, right recipientAddressSets) bool {
	return equalStringSets(left.To, right.To) && equalStringSets(left.CC, right.CC) &&
		equalStringSets(left.BCC, right.BCC)
}

func equalStringSets(left map[string]struct{}, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for value := range left {
		if _, exists := right[value]; !exists {
			return false
		}
	}
	return true
}

func normalizedAddress(value string) string {
	return strings.ToLower(parsedAddress(value))
}

func (s *Store) recordContentMatchesDraft(
	ctx context.Context,
	record messageRecord,
	draft mail.Draft,
) (result bool, resultErr error) {
	location, err := parseMailboxURL(record.PhysicalURL)
	if err != nil {
		return false, operationError("unsupported_mail_store_schema", err.Error())
	}
	resolved := s.resolveCandidate(record, location)
	source, err := s.openResolvedSource(resolved)
	if err != nil {
		return false, candidateSourcePending(err)
	}
	defer joinCloseError(&resultErr, source, "sent-message source")
	if source.partial {
		return false, nil
	}
	stable, err := s.messageStillMatches(ctx, resolved)
	if err != nil || !stable {
		return false, err
	}
	document, err := parseMIMEDocument(source.Reader(), false, true)
	if err != nil || !document.Complete || !bodyHasDraftPrefix(document.Content, draft.Body) {
		return false, candidateSourcePending(err)
	}
	return s.recordAttachmentsMatchDraft(
		ctx, record, location, document.Parts, draft.Attachments, draft.ExpectedAttachmentCount,
	)
}

func (s *Store) resolveCandidate(record messageRecord, location mailboxLocation) resolvedMessage {
	return resolvedMessage{
		Reference: mailref.Message{
			AccountID:   location.AccountID,
			MailboxPath: location.VisiblePath, LibraryID: strconv.FormatInt(record.RowID, 10),
			ExpectedSubject: record.Subject, ExpectedStoreUUID: s.storeUUID,
			ExpectedStoreMailboxID: record.StoreMailboxID,
			ExpectedStoreMessageID: record.StoreMessageID,
			ExpectedStoreGlobalID:  record.StoreGlobalID,
		},
		Record: record, PhysicalLocation: location,
	}
}

func candidateSourcePending(err error) error {
	if err == nil {
		return nil
	}
	var typed *Error
	if errors.As(err, &typed) {
		switch typed.Code {
		case "message_source_missing", "store_changed", "invalid_message_source":
			return nil
		}
	}
	return err
}

func bodyHasDraftPrefix(actual string, expected string) bool {
	expected = canonicalSentText(expected)
	if expected == "" {
		return true
	}
	for _, candidate := range normalizedBodyCandidates(actual) {
		if candidate == expected || strings.HasPrefix(candidate, expected+"\n") {
			return true
		}
	}
	return false
}

func normalizedBodyCandidates(value string) []string {
	normalized := canonicalSentText(value)
	candidates := []string{normalized}
	lines := strings.Split(normalized, "\n")
	for len(lines) > 0 && strings.Trim(lines[0], " \uFFFC") == "" {
		lines = lines[1:]
	}
	if len(lines) == 0 {
		return candidates
	}
	withoutPlaceholders := strings.Join(lines, "\n")
	if withoutPlaceholders != normalized {
		candidates = append(candidates, withoutPlaceholders)
	}
	quoted := make([]string, 0, len(lines))
	for _, line := range lines {
		if line == ">" {
			quoted = append(quoted, "")
			continue
		}
		if !strings.HasPrefix(line, "> ") {
			break
		}
		quoted = append(quoted, strings.TrimPrefix(line, "> "))
	}
	if len(quoted) > 0 {
		candidates = append(candidates, canonicalSentText(strings.Join(quoted, "\n")))
	}
	return candidates
}

func canonicalSentText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.Trim(norm.NFC.String(value), "\n")
}

func (s *Store) recordAttachmentsMatchDraft(
	ctx context.Context,
	record messageRecord,
	location mailboxLocation,
	parts map[string]mimePart,
	expected []mail.DraftAttachment,
	expectedTotal *int,
) (bool, error) {
	records, err := s.attachmentRecords(ctx, record.RowID)
	if err != nil {
		return false, err
	}
	if expectedTotal == nil && len(parts) != len(expected) {
		return false, nil
	}
	if expectedTotal != nil && len(parts) != *expectedTotal {
		return false, nil
	}
	recordsByID := make(map[string]attachmentRecord, len(records))
	for _, attachment := range records {
		if _, duplicate := recordsByID[attachment.ID]; duplicate {
			return false, nil
		}
		recordsByID[attachment.ID] = attachment
	}
	resolved := resolvedMessage{Record: record, PhysicalLocation: location}
	actual := make(map[attachmentFingerprint]int, len(parts))
	for identifier, part := range parts {
		fingerprint, complete := fingerprintMIMEPart(part)
		if !complete {
			attachment, exists := recordsByID[identifier]
			if !exists {
				return false, nil
			}
			var err error
			fingerprint, complete, err = s.observedAttachmentFingerprint(resolved, attachment, parts)
			if err != nil || !complete {
				return false, err
			}
		}
		actual[fingerprint]++
	}
	return attachmentFingerprintsContain(actual, expected, expectedTotal != nil), nil
}

func fingerprintMIMEPart(part mimePart) (attachmentFingerprint, bool) {
	if !part.Complete || part.Name == "" || part.Size < 0 || len(part.SHA256) != sha256.Size*2 {
		return attachmentFingerprint{}, false
	}
	return attachmentFingerprint{
		Name: normalizedFilename(part.Name), Size: part.Size, SHA256: strings.ToLower(part.SHA256),
	}, true
}

func (s *Store) observedAttachmentFingerprint(
	resolved resolvedMessage,
	record attachmentRecord,
	parts map[string]mimePart,
) (attachmentFingerprint, bool, error) {
	part, exists := parts[record.ID]
	if !exists || (record.Name != "" && part.Name != "" && normalizedFilename(record.Name) != normalizedFilename(part.Name)) {
		return attachmentFingerprint{}, false, nil
	}
	name := record.Name
	if name == "" {
		name = part.Name
	}
	external, available, err := s.findExternalAttachment(resolved, record)
	if err != nil {
		return attachmentFingerprint{}, false, err
	}
	if available {
		digest, err := s.hashStoreFile(external)
		if err != nil {
			return attachmentFingerprint{}, false, err
		}
		return attachmentFingerprint{
			Name: normalizedFilename(name), Size: external.Size,
			SHA256: hex.EncodeToString(digest[:]),
		}, true, nil
	}
	if !part.Complete {
		return attachmentFingerprint{}, false, nil
	}
	return attachmentFingerprint{
		Name: normalizedFilename(name), Size: part.Size, SHA256: part.SHA256,
	}, true, nil
}

func attachmentFingerprintsContain(
	actual map[attachmentFingerprint]int,
	expected []mail.DraftAttachment,
	allowExtra bool,
) bool {
	if !allowExtra && len(actual) > len(expected) {
		return false
	}
	wanted := make(map[attachmentFingerprint]int, len(expected))
	for _, attachment := range expected {
		wanted[attachmentFingerprint{
			Name: normalizedFilename(filepath.Base(attachment.Path)), Size: attachment.Size,
			SHA256: strings.ToLower(attachment.SHA256),
		}]++
	}
	if !allowExtra && len(wanted) != len(actual) {
		return false
	}
	for fingerprint, count := range wanted {
		if actual[fingerprint] < count || (!allowExtra && actual[fingerprint] != count) {
			return false
		}
	}
	return true
}

func normalizedFilename(value string) string {
	return norm.NFC.String(value)
}

func parsedAddress(value string) string {
	if value == "" {
		return ""
	}
	address, err := stdmail.ParseAddress(value)
	if err != nil {
		return ""
	}
	return address.Address
}
