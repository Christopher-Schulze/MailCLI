package mailstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/emersion/go-message"
	"golang.org/x/text/unicode/norm"

	"mailcli/internal/mail"
)

type attachmentRecord struct {
	ID   string
	Name string
}

type externalAttachment struct {
	Path string
	Size int64
}

func (s *Store) messageAttachments(
	ctx context.Context,
	resolved resolvedMessage,
	source *emlxSource,
	parts map[string]mimePart,
) ([]mail.Attachment, error) {
	records, err := s.attachmentRecords(ctx, resolved.Record.RowID)
	if err != nil {
		return nil, err
	}
	records, err = mergeAttachmentRecords(records, parts)
	if err != nil {
		return nil, err
	}
	attachments := make([]mail.Attachment, 0, len(records))
	for _, record := range records {
		external, available, err := s.findExternalAttachment(resolved, record)
		if err != nil {
			return nil, err
		}
		part, hasPart := parts[record.ID]
		name := record.Name
		if name == "" && hasPart {
			name = part.Name
		}
		attachment := mail.Attachment{
			ID: record.ID, Name: name, MIMEType: guessedMIMEType(name),
			Downloaded: available || (hasPart && !source.partial && part.Complete),
		}
		if hasPart && part.MIMEType != "" {
			mediaType := part.MIMEType
			attachment.MIMEType = &mediaType
		}
		if available {
			attachment.Size = external.Size
			attachment.SizeKnown = true
		} else if hasPart && !source.partial && part.Complete {
			attachment.Size = part.Size
			attachment.SizeKnown = true
		}
		attachments = append(attachments, attachment)
	}
	return attachments, nil
}

func (s *Store) SaveAttachmentTo(
	ctx context.Context,
	messageRef string,
	attachmentID string,
	outputPath string,
) (resultErr error) {
	resolved, source, err := s.openMessageSource(ctx, messageRef)
	if err != nil {
		return err
	}
	defer joinCloseError(&resultErr, source, "message source")
	records, err := s.attachmentRecords(ctx, resolved.Record.RowID)
	if err != nil {
		return err
	}
	var selected attachmentRecord
	foundInStore := false
	for _, record := range records {
		if record.ID == attachmentID {
			selected = record
			foundInStore = true
			break
		}
	}
	if foundInStore {
		external, available, err := s.findExternalAttachment(resolved, selected)
		if err != nil {
			return err
		}
		if available {
			return copyExternalAttachment(external.Path, outputPath)
		}
	}
	document, err := parseMIMEDocument(source.Reader(), source.partial, false)
	if err != nil {
		return err
	}
	part, foundInMIME := document.Parts[attachmentID]
	if !foundInMIME {
		if !foundInStore {
			return operationError("not_found", "attachment is not present on this message")
		}
		return operationError(
			"attachment_not_downloaded",
			"attachment bytes are not downloaded; a targeted Mail.app fallback is required",
		)
	}
	if !part.Complete {
		return operationError(
			"attachment_not_downloaded",
			"attachment bytes are not downloaded; a targeted Mail.app fallback is required",
		)
	}
	return extractMIMEAttachment(source.Reader(), attachmentID, outputPath)
}

func mergeAttachmentRecords(
	records []attachmentRecord,
	parts map[string]mimePart,
) ([]attachmentRecord, error) {
	byID := make(map[string]attachmentRecord, len(records)+len(parts))
	for _, record := range records {
		if _, duplicate := byID[record.ID]; duplicate {
			return nil, operationError("ambiguous_attachment", "attachment identifier is duplicated")
		}
		byID[record.ID] = record
	}
	for identifier, part := range parts {
		if record, exists := byID[identifier]; exists {
			if part.Name != "" {
				record.Name = part.Name
				byID[identifier] = record
			}
			continue
		}
		byID[identifier] = attachmentRecord{ID: identifier, Name: part.Name}
	}
	merged := make([]attachmentRecord, 0, len(byID))
	for _, record := range byID {
		merged = append(merged, record)
	}
	sort.Slice(merged, func(left int, right int) bool { return merged[left].ID < merged[right].ID })
	return merged, nil
}

func (s *Store) attachmentRecords(
	ctx context.Context,
	rowID int64,
) (result []attachmentRecord, resultErr error) {
	rows, err := s.database.QueryContext(ctx, `
		SELECT COALESCE(attachment_id, ''), COALESCE(name, '')
		FROM attachments
		WHERE message = ?
		ORDER BY attachment_id, ROWID
	`, rowID)
	if err != nil {
		return nil, fmt.Errorf("list Envelope Index attachments: %w", err)
	}
	defer joinCloseError(&resultErr, rows, "attachment rows")
	var records []attachmentRecord
	for rows.Next() {
		var record attachmentRecord
		if err := rows.Scan(&record.ID, &record.Name); err != nil {
			return nil, fmt.Errorf("scan Envelope Index attachment: %w", err)
		}
		if !validAttachmentID(record.ID) {
			return nil, operationError("unsupported_mail_store_schema", "attachment has an unsafe MIME part identifier")
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Envelope Index attachments: %w", err)
	}
	return records, nil
}

func (s *Store) findExternalAttachment(
	resolved resolvedMessage,
	record attachmentRecord,
) (externalAttachment, bool, error) {
	directory, err := s.attachmentDirectory(resolved, record.ID)
	if err != nil {
		return externalAttachment{}, false, err
	}
	info, err := os.Lstat(directory)
	if os.IsNotExist(err) {
		return externalAttachment{}, false, nil
	}
	if err != nil {
		return externalAttachment{}, false, fmt.Errorf("inspect external attachment directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return externalAttachment{}, false, operationError(
			"ambiguous_attachment", "external attachment location is not a regular directory",
		)
	}
	if err := validatePathWithoutSymlinks(s.versionRoot, directory); err != nil {
		return externalAttachment{}, false, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return externalAttachment{}, false, fmt.Errorf("list external attachment files: %w", err)
	}
	files := make([]externalAttachment, 0, len(entries))
	nameMatches := make([]externalAttachment, 0, 1)
	wantedName := norm.NFC.String(record.Name)
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		fileInfo, err := os.Lstat(path)
		if err != nil {
			return externalAttachment{}, false, fmt.Errorf("inspect external attachment file: %w", err)
		}
		if !fileInfo.Mode().IsRegular() || fileInfo.Mode()&os.ModeSymlink != 0 {
			return externalAttachment{}, false, operationError(
				"ambiguous_attachment", "external attachment directory contains a non-regular file",
			)
		}
		candidate := externalAttachment{Path: path, Size: fileInfo.Size()}
		files = append(files, candidate)
		if wantedName != "" && norm.NFC.String(entry.Name()) == wantedName {
			nameMatches = append(nameMatches, candidate)
		}
	}
	if len(nameMatches) == 1 {
		return nameMatches[0], true, nil
	}
	if len(nameMatches) > 1 {
		return selectIdenticalAttachment(nameMatches)
	}
	if len(files) == 0 {
		return externalAttachment{}, false, nil
	}
	if len(files) == 1 {
		return files[0], true, nil
	}
	return selectIdenticalAttachment(files)
}

func (s *Store) attachmentDirectory(resolved resolvedMessage, attachmentID string) (string, error) {
	if !validAttachmentID(attachmentID) {
		return "", operationError("invalid_argument", "attachment id is not a MIME part path")
	}
	messageBase, err := s.messageBasePath(resolved.PhysicalLocation, resolved.Record.RowID)
	if err != nil {
		return "", err
	}
	bucketRoot := filepath.Dir(filepath.Dir(messageBase))
	return filepath.Join(
		bucketRoot, "Attachments", strconv.FormatInt(resolved.Record.RowID, 10), attachmentID,
	), nil
}

func validAttachmentID(value string) bool {
	if value == "" {
		return false
	}
	for _, component := range strings.Split(value, ".") {
		part, err := strconv.ParseInt(component, 10, 32)
		if err != nil || part < 1 || strconv.FormatInt(part, 10) != component {
			return false
		}
	}
	return true
}

func selectIdenticalAttachment(files []externalAttachment) (externalAttachment, bool, error) {
	sort.Slice(files, func(left int, right int) bool { return files[left].Path < files[right].Path })
	var expected [sha256.Size]byte
	for index, file := range files {
		digest, err := hashRegularFile(file.Path)
		if err != nil {
			return externalAttachment{}, false, err
		}
		if index == 0 {
			expected = digest
			continue
		}
		if digest != expected {
			return externalAttachment{}, false, operationError(
				"ambiguous_attachment", "multiple external attachment files have different bytes",
			)
		}
	}
	return files[0], true, nil
}

func hashRegularFile(path string) (result [sha256.Size]byte, resultErr error) {
	info, err := os.Lstat(path)
	if err != nil {
		return result, fmt.Errorf("inspect external attachment for hashing: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return result, operationError("ambiguous_attachment", "external attachment is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return result, fmt.Errorf("open external attachment for hashing: %w", err)
	}
	defer joinCloseError(&resultErr, file, "external attachment")
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return result, operationError("store_changed", "external attachment changed while opening")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return result, fmt.Errorf("hash external attachment: %w", err)
	}
	final, err := file.Stat()
	if err != nil || !os.SameFile(opened, final) || opened.Size() != final.Size() ||
		!opened.ModTime().Equal(final.ModTime()) {
		return result, operationError("store_changed", "external attachment changed while hashing")
	}
	copy(result[:], hash.Sum(nil))
	return result, nil
}

func copyExternalAttachment(sourcePath string, outputPath string) (resultErr error) {
	info, err := os.Lstat(sourcePath)
	if err != nil {
		return fmt.Errorf("inspect external attachment: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return operationError("ambiguous_attachment", "external attachment is not a regular file")
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open external attachment: %w", err)
	}
	defer joinCloseError(&resultErr, source, "external attachment")
	openedInfo, err := source.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return operationError("store_changed", "external attachment changed while opening")
	}
	return writeExclusiveFile(outputPath, source)
}

func extractMIMEAttachment(reader io.Reader, attachmentID string, outputPath string) error {
	errAttachmentExtracted := errors.New("attachment extracted")
	entity, readErr := message.Read(reader)
	if entity == nil || (readErr != nil && !message.IsUnknownCharset(readErr) && !message.IsUnknownEncoding(readErr)) {
		return operationError("invalid_message_source", fmt.Sprintf("parse RFC message: %v", readErr))
	}
	found := false
	walkErr := entity.Walk(func(path []int, part *message.Entity, partErr error) error {
		if mimePartID(path) != attachmentID {
			return nil
		}
		if partErr != nil && !message.IsUnknownCharset(partErr) && !message.IsUnknownEncoding(partErr) {
			return partErr
		}
		if err := writeExclusiveFile(outputPath, part.Body); err != nil {
			return err
		}
		found = true
		return errAttachmentExtracted
	})
	if errors.Is(walkErr, errAttachmentExtracted) && found {
		return nil
	}
	if walkErr != nil {
		return fmt.Errorf("extract MIME attachment: %w", walkErr)
	}
	return operationError("not_found", "MIME attachment part is not present")
}

func writeExclusiveFile(path string, reader io.Reader) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create attachment output: %w", err)
	}
	_, copyErr := io.Copy(file, reader)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		removeErr := os.Remove(path)
		if copyErr != nil {
			return errors.Join(fmt.Errorf("write attachment output: %w", copyErr), removeErr)
		}
		return errors.Join(fmt.Errorf("close attachment output: %w", closeErr), removeErr)
	}
	return nil
}
