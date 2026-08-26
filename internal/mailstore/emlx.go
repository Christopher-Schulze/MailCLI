package mailstore

import (
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"mailcli/internal/mailref"
)

const (
	emlxPrefixBytes       = 11
	maximumRFCSourceBytes = int64(128 * 1024 * 1024)
	maximumPlistBytes     = int64(16 * 1024 * 1024)
)

type resolvedMessage struct {
	Reference        mailref.Message
	Record           messageRecord
	PhysicalLocation mailboxLocation
}

type emlxSource struct {
	file    *os.File
	length  int64
	partial bool
	path    string
}

func (s *emlxSource) Close() error {
	return s.file.Close()
}

func (s *emlxSource) Reader() io.Reader {
	return io.NewSectionReader(s.file, emlxPrefixBytes, s.length)
}

func (s *Store) openMessageSource(ctx context.Context, ref string) (resolvedMessage, *emlxSource, error) {
	for attempt := 0; attempt < 2; attempt++ {
		resolved, err := s.resolveMessage(ctx, ref)
		if err != nil {
			return resolvedMessage{}, nil, err
		}
		source, err := s.openResolvedSource(resolved)
		if err != nil {
			return resolvedMessage{}, nil, err
		}
		stable, err := s.messageStillMatches(ctx, resolved)
		if err != nil {
			resultErr := err
			joinCloseError(&resultErr, source, "message source")
			return resolvedMessage{}, nil, resultErr
		}
		if stable {
			return resolved, source, nil
		}
		if err := source.Close(); err != nil {
			return resolvedMessage{}, nil, fmt.Errorf("close changed message source: %w", err)
		}
	}
	return resolvedMessage{}, nil, operationError(
		"store_changed", "message moved or changed while its local source was opened",
	)
}

func (s *Store) resolveMessage(ctx context.Context, value string) (resolvedMessage, error) {
	ref, err := mailref.DecodeMessage(value)
	if err != nil {
		return resolvedMessage{}, operationError("invalid_reference", fmt.Sprintf("invalid message ref: %v", err))
	}
	if !ref.IsStoreBound() {
		return resolvedMessage{}, operationError(
			"store_bound_reference_required",
			"message ref is not bound to this Mail store; list or search the message again",
		)
	}
	if ref.ExpectedStoreUUID != s.storeUUID {
		return resolvedMessage{}, operationError("stale_reference", "message Mail store identity changed")
	}
	row := s.database.QueryRowContext(ctx, `
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
		FROM messages m
		JOIN mailboxes mb ON mb.ROWID = m.mailbox
		JOIN subjects subject ON subject.ROWID = m.subject
		JOIN addresses sender ON sender.ROWID = m.sender
		LEFT JOIN summaries summary ON summary.ROWID = m.summary
		WHERE m.ROWID = ? AND m.deleted = 0
	`, ref.LibraryID)
	record, err := scanMessageRecord(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return resolvedMessage{}, operationError("not_found", "message is not present in the local Mail store")
		}
		return resolvedMessage{}, err
	}
	location, err := parseMailboxURL(record.PhysicalURL)
	if err != nil {
		return resolvedMessage{}, operationError(
			"unsupported_mail_store_schema", fmt.Sprintf("message has an unsafe physical mailbox URL: %v", err),
		)
	}
	if _, active := s.activeAccountKeys[location.rootKey()]; !active ||
		!strings.EqualFold(location.AccountID, ref.AccountID) {
		return resolvedMessage{}, operationError("stale_reference", "message account identity changed")
	}
	if ref.ExpectedSubject != "" && record.Subject != ref.ExpectedSubject {
		return resolvedMessage{}, operationError("stale_reference", "message subject identity changed")
	}
	if record.StoreMailboxID != ref.ExpectedStoreMailboxID ||
		record.StoreMessageID != ref.ExpectedStoreMessageID ||
		record.StoreGlobalID != ref.ExpectedStoreGlobalID {
		return resolvedMessage{}, operationError("stale_reference", "message store identity changed")
	}
	if err := s.validateReferenceMembership(ctx, ref); err != nil {
		return resolvedMessage{}, err
	}
	return resolvedMessage{Reference: ref, Record: record, PhysicalLocation: location}, nil
}

func (s *Store) validateReferenceMembership(ctx context.Context, ref mailref.Message) error {
	present, err := s.referenceMembershipMatches(ctx, ref)
	if err != nil {
		return err
	}
	if !present {
		return operationError("stale_reference", "message is no longer present in the referenced mailbox")
	}
	return nil
}

func (s *Store) referenceMembershipMatches(ctx context.Context, ref mailref.Message) (bool, error) {
	records, err := s.loadMailboxRecords(ctx)
	if err != nil {
		return false, err
	}
	mailbox, found := findMailboxRecord(records, ref.AccountID, ref.MailboxPath)
	if !found {
		return false, nil
	}
	return s.messageHasMembership(ctx, ref.LibraryID, mailbox.RowID)
}

func (s *Store) messageStillMatches(ctx context.Context, resolved resolvedMessage) (bool, error) {
	var messageID int64
	var globalID int64
	var mailboxID int64
	var mailboxURL string
	var subject string
	err := s.database.QueryRowContext(ctx, `
		SELECT COALESCE(m.message_id, 0), COALESCE(m.global_message_id, 0),
			m.mailbox, mb.url, subject.subject
		FROM messages m
		JOIN mailboxes mb ON mb.ROWID = m.mailbox
		JOIN subjects subject ON subject.ROWID = m.subject
		WHERE m.ROWID = ? AND m.deleted = 0
	`, resolved.Record.RowID).Scan(&messageID, &globalID, &mailboxID, &mailboxURL, &subject)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("revalidate Envelope Index message: %w", err)
	}
	physicalMatch := messageID == resolved.Record.StoreMessageID &&
		globalID == resolved.Record.StoreGlobalID &&
		mailboxID == resolved.Record.StoreMailboxID &&
		mailboxURL == resolved.Record.PhysicalURL && subject == resolved.Record.Subject
	if !physicalMatch {
		return false, nil
	}
	return s.referenceMembershipMatches(ctx, resolved.Reference)
}

func (s *Store) openResolvedSource(resolved resolvedMessage) (*emlxSource, error) {
	base, err := s.messageBasePath(resolved.PhysicalLocation, resolved.Record.RowID)
	if err != nil {
		return nil, err
	}
	fullPath := base + ".emlx"
	partialPath := base + ".partial.emlx"
	fullFile, fullInfo, fullErr := openRegularPath(s.versionDirectory, s.versionRoot, fullPath)
	partialFile, partialInfo, partialErr := openRegularPath(s.versionDirectory, s.versionRoot, partialPath)
	if fullErr != nil && !os.IsNotExist(fullErr) {
		if partialFile != nil {
			joinCloseError(&fullErr, partialFile, "partial EMLX source")
		}
		return nil, fullErr
	}
	if partialErr != nil && !os.IsNotExist(partialErr) {
		if fullFile != nil {
			joinCloseError(&partialErr, fullFile, "full EMLX source")
		}
		return nil, partialErr
	}
	if fullErr == nil && partialErr == nil {
		resultErr := error(operationError("ambiguous_message_source", "both full and partial EMLX sources exist"))
		joinCloseError(&resultErr, fullFile, "full EMLX source")
		joinCloseError(&resultErr, partialFile, "partial EMLX source")
		return nil, resultErr
	}
	path := fullPath
	file := fullFile
	info := fullInfo
	partial := false
	if fullErr != nil {
		if partialErr != nil {
			return nil, operationError("message_source_missing", "message source is not downloaded locally")
		}
		path = partialPath
		file = partialFile
		info = partialInfo
		partial = true
	}
	length, err := validateEMLXFrame(file, info.Size())
	if err != nil {
		resultErr := err
		joinCloseError(&resultErr, file, "EMLX source")
		return nil, resultErr
	}
	return &emlxSource{file: file, length: length, partial: partial, path: path}, nil
}

func (s *Store) messageBasePath(location mailboxLocation, rowID int64) (string, error) {
	if rowID < 1 || !validUUID(s.storeUUID) {
		return "", operationError("unsafe_message_source", "message source identity is invalid")
	}
	parts := []string{s.versionRoot, location.AccountID}
	for _, segment := range location.RawPath {
		if err := validatePathSegment(segment); err != nil {
			return "", operationError("unsafe_message_source", err.Error())
		}
		parts = append(parts, segment+".mbox")
	}
	parts = append(parts, s.storeUUID, "Data")
	parts = append(parts, messageBucket(rowID)...)
	parts = append(parts, "Messages", strconv.FormatInt(rowID, 10))
	path := filepath.Join(parts...)
	relative, err := filepath.Rel(s.versionRoot, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", operationError("unsafe_message_source", "message source escapes the Mail store")
	}
	return path, nil
}

func messageBucket(rowID int64) []string {
	group := strconv.FormatInt(rowID/1000, 10)
	if group == "0" {
		return nil
	}
	parts := make([]string, 0, len(group))
	for index := len(group) - 1; index >= 0; index-- {
		parts = append(parts, group[index:index+1])
	}
	return parts
}

func validatePathWithoutSymlinks(root string, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return operationError("unsafe_message_source", "message source escapes the Mail store")
	}
	current := root
	components := strings.Split(relative, string(filepath.Separator))
	for _, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect message source component: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return operationError("unsafe_message_source", "message source path contains a symlink")
		}
	}
	return nil
}

func validateEMLXFrame(file *os.File, fileSize int64) (int64, error) {
	if fileSize < emlxPrefixBytes {
		return 0, operationError("invalid_emlx", "EMLX source is shorter than its framing prefix")
	}
	var prefix [emlxPrefixBytes]byte
	if _, err := file.ReadAt(prefix[:], 0); err != nil {
		return 0, fmt.Errorf("read EMLX framing prefix: %w", err)
	}
	if prefix[emlxPrefixBytes-1] != '\n' {
		return 0, operationError("invalid_emlx", "EMLX source length prefix has no newline")
	}
	lengthText := strings.TrimSpace(string(prefix[:emlxPrefixBytes-1]))
	if lengthText == "" || strings.IndexFunc(lengthText, func(character rune) bool {
		return character < '0' || character > '9'
	}) >= 0 {
		return 0, operationError("invalid_emlx", "EMLX source length prefix is not decimal")
	}
	length, err := strconv.ParseInt(lengthText, 10, 64)
	if err != nil || length < 1 || length > maximumRFCSourceBytes {
		return 0, operationError("invalid_emlx", "EMLX declared source length is invalid")
	}
	trailerOffset := int64(emlxPrefixBytes) + length
	if trailerOffset > fileSize {
		return 0, operationError("invalid_emlx", "EMLX source is shorter than its declared length")
	}
	trailerSize := fileSize - trailerOffset
	if trailerSize < 1 || trailerSize > maximumPlistBytes {
		return 0, operationError("invalid_emlx", "EMLX plist trailer size is invalid")
	}
	if err := validateXMLPlist(io.NewSectionReader(file, trailerOffset, trailerSize)); err != nil {
		return 0, err
	}
	return length, nil
}

func validateXMLPlist(reader io.Reader) error {
	decoder := xml.NewDecoder(reader)
	foundPlist := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return operationError("invalid_emlx", fmt.Sprintf("parse EMLX plist trailer: %v", err))
		}
		start, ok := token.(xml.StartElement)
		if ok && start.Name.Local == "plist" {
			foundPlist = true
		}
	}
	if !foundPlist {
		return operationError("invalid_emlx", "EMLX trailer is not an XML plist")
	}
	return nil
}
