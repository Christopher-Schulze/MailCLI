package mail

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	composerLineLength          = 76  // base64 lines per RFC 2045
	composerHeaderLength        = 78  // recommended header line limit per RFC 5322
	composerHeaderMaximum       = 998 // hard limit excluding CRLF per RFC 5322
	composerEncodedHeaderLength = 76  // RFC 2047 header fields containing encoded words
	composerCRLF                = "\r\n"
)

// ComposerError is the typed error for RFC 5322 message composition failures.
type ComposerError struct {
	Message string
	Err     error
}

func (e *ComposerError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

func (e *ComposerError) Unwrap() error { return e.Err }

// BuildMessage is the compatibility byte-returning adapter for the verified
// spool composer. It materializes the final message because its legacy API
// returns []byte; production send paths must use ComposeMessageSpool instead.
// messageID is used verbatim as the Message-ID header. BCC recipients are
// deliberately omitted from the output. Reply and forward drafts carry
// In-Reply-To and References threading headers when source threading is
// available. Subject prefixes (Re:/Fwd:) are applied at draft creation and
// are never added here.
func BuildMessage(draft Draft, messageID string) (payload []byte, resultErr error) {
	if messageID == "" {
		return nil, &ComposerError{Message: "message id is required"}
	}
	message, err := composeDraftSpool(context.Background(), draft, messageID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := message.Remove(); err != nil {
			resultErr = errors.Join(resultErr, &ComposerError{Message: "remove message spool", Err: err})
		}
	}()
	return readComposedMessage(message)
}

// ComposedMessage is a private replayable spool for a composed RFC 5322
// message. The encoded attachment bytes stay on disk until all consumers have
// replayed the source.
type ComposedMessage struct {
	path            string
	size            int64
	messageID       string
	file            *os.File
	storage         *draftStorage
	storageName     string
	storageIdentity os.FileInfo
	lifecycleMu     sync.Mutex
	readers         int
	removed         bool
	removeErr       error
}

type composedMessageReader struct {
	section   *io.SectionReader
	remaining int64
	closeFn   func() error
	mu        sync.Mutex
	closed    bool
}

func (r *composedMessageReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, os.ErrClosed
	}
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	limit := len(p)
	if int64(limit) > r.remaining {
		limit = int(r.remaining)
	}
	n, err := r.section.Read(p[:limit])
	r.remaining -= int64(n)
	if err == io.EOF && r.remaining > 0 {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *composedMessageReader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, os.ErrClosed
	}
	position, err := r.section.Seek(offset, whence)
	if err == nil {
		r.remaining = r.section.Size() - position
	}
	return position, err
}

func (r *composedMessageReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return os.ErrClosed
	}
	r.closed = true
	return r.closeFn()
}

func ComposeMessageSpool(draft Draft, messageID string) (*ComposedMessage, error) {
	return ComposeMessageSpoolContext(context.Background(), draft, messageID)
}

func ComposeMessageSpoolContext(ctx context.Context, draft Draft, messageID string) (*ComposedMessage, error) {
	return composeMessageSpoolContext(ctx, draft, messageID, func(path string) (io.ReadCloser, error) {
		file, _, err := openRegularAttachment(path)
		return file, err
	})
}

type attachmentOpener func(string) (io.ReadCloser, error)

func composeMessageSpoolContext(
	ctx context.Context,
	draft Draft,
	messageID string,
	openAttachment attachmentOpener,
) (*ComposedMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if messageID == "" {
		return nil, &ComposerError{Message: "message id is required"}
	}
	alternativeBoundary, err := randomBoundary()
	if err != nil {
		return nil, err
	}
	mixedBoundary := ""
	if len(draft.Attachments) > 0 {
		mixedBoundary, err = randomBoundary()
		if err != nil {
			return nil, err
		}
	}
	contentType := "multipart/alternative; boundary=\"" + alternativeBoundary + "\""
	if len(draft.Attachments) > 0 {
		contentType = "multipart/mixed; boundary=\"" + mixedBoundary + "\""
	}

	header := &bytes.Buffer{}
	if err := writeComposerHeaders(header, draft, messageID, contentType); err != nil {
		return nil, err
	}
	header.WriteString(composerCRLF)
	alternative := &bytes.Buffer{}
	writeAlternativeMultipart(alternative, alternativeBoundary, draft)
	alternative.WriteString(composerCRLF)

	file, err := os.CreateTemp("", "mailcli-message-")
	if err != nil {
		return nil, &ComposerError{Message: "create private message spool", Err: err}
	}
	path := file.Name()
	storageName := path
	storage := &draftStorage{}
	identity, err := file.Stat()
	if err != nil {
		return nil, &ComposerError{Message: "stat message spool", Err: errors.Join(err, file.Close())}
	}
	cleanup := func() {
		_ = file.Close()
		_ = removeDraftStorageFile(storage, storageName, identity, "")
	}
	if err := file.Chmod(0o600); err != nil {
		cleanup()
		return nil, &ComposerError{Message: "protect private message spool", Err: err}
	}
	writer := bufio.NewWriterSize(file, 32*1024)
	write := func(value string) error {
		if _, err := io.WriteString(writer, value); err != nil {
			return err
		}
		return nil
	}
	if _, err := writer.Write(header.Bytes()); err != nil {
		cleanup()
		return nil, &ComposerError{Message: "write message headers", Err: err}
	}
	if len(draft.Attachments) == 0 {
		if _, err := writer.Write(alternative.Bytes()); err != nil {
			cleanup()
			return nil, &ComposerError{Message: "write message body", Err: err}
		}
	} else {
		if err := write("--" + mixedBoundary + composerCRLF +
			"Content-Type: multipart/alternative; boundary=\"" + alternativeBoundary + "\"" + composerCRLF + composerCRLF); err != nil {
			cleanup()
			return nil, &ComposerError{Message: "write multipart headers", Err: err}
		}
		if _, err := writer.Write(alternative.Bytes()); err != nil {
			cleanup()
			return nil, &ComposerError{Message: "write multipart body", Err: err}
		}
		for _, attachment := range draft.Attachments {
			if err := ctx.Err(); err != nil {
				cleanup()
				return nil, err
			}
			if err := write("--" + mixedBoundary + composerCRLF); err != nil {
				cleanup()
				return nil, &ComposerError{Message: "write attachment boundary", Err: err}
			}
			headers, err := composerAttachmentHeaders(attachment.Path)
			if err != nil {
				cleanup()
				return nil, err
			}
			if err := write(headers); err != nil {
				cleanup()
				return nil, &ComposerError{Message: "write attachment headers", Err: err}
			}
			if err := write(composerCRLF); err != nil {
				cleanup()
				return nil, &ComposerError{Message: "write attachment separator", Err: err}
			}
			if err := streamAttachmentBase64Context(ctx, writer, attachment, openAttachment); err != nil {
				cleanup()
				return nil, err
			}
			if err := write(composerCRLF); err != nil {
				cleanup()
				return nil, &ComposerError{Message: "write attachment terminator", Err: err}
			}
		}
		if err := write("--" + mixedBoundary + "--" + composerCRLF); err != nil {
			cleanup()
			return nil, &ComposerError{Message: "write multipart terminator", Err: err}
		}
	}
	if err := writer.Flush(); err != nil {
		cleanup()
		return nil, &ComposerError{Message: "flush message spool", Err: err}
	}
	stat, err := file.Stat()
	if err != nil {
		cleanup()
		return nil, &ComposerError{Message: "stat message spool", Err: err}
	}
	if !stat.Mode().IsRegular() || !os.SameFile(identity, stat) {
		cleanup()
		return nil, &ComposerError{Message: "stat message spool", Err: errors.New("message spool identity changed while composing")}
	}
	return &ComposedMessage{
		path: path, size: stat.Size(), messageID: messageID,
		file: file, storage: storage, storageName: storageName, storageIdentity: identity,
	}, nil
}

func (m *ComposedMessage) Open() (io.ReadSeekCloser, error) {
	if m == nil {
		return nil, os.ErrInvalid
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.removed {
		return nil, os.ErrClosed
	}
	if m.storage == nil || m.storageIdentity == nil {
		return nil, errors.New("composed message spool is not identity-pinned")
	}
	file := m.file
	closeFn := m.releaseReader
	if file == nil {
		var err error
		file, _, err = m.storage.openFile(m.storageName, m.storageIdentity, os.O_RDONLY, 0)
		if err != nil {
			return nil, err
		}
		closeFn = file.Close
	} else {
		m.readers++
	}
	return &composedMessageReader{
		section:   io.NewSectionReader(file, 0, m.size),
		remaining: m.size,
		closeFn:   closeFn,
	}, nil
}

func (m *ComposedMessage) Size() int64 { return m.size }

func (m *ComposedMessage) MessageID() string { return m.messageID }

func (m *ComposedMessage) Remove() error {
	if m == nil {
		return nil
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.removed {
		return m.removeErr
	}
	m.removed = true
	if m.storage == nil || m.storageIdentity == nil {
		m.removeErr = errors.New("composed message spool is not identity-pinned")
	} else {
		m.removeErr = removeDraftStorageFile(m.storage, m.storageName, m.storageIdentity, "")
	}
	if m.readers == 0 {
		_ = m.closeOwnerLocked()
	}
	return m.removeErr
}

func (m *ComposedMessage) releaseReader() error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	m.readers--
	if m.removed && m.readers == 0 {
		return m.closeOwnerLocked()
	}
	return nil
}

func (m *ComposedMessage) closeOwnerLocked() error {
	if m.file == nil {
		return nil
	}
	err := m.file.Close()
	m.file = nil
	m.removeErr = errors.Join(m.removeErr, err)
	return err
}

func composerAttachmentHeaders(path string) (string, error) {
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	var buffer bytes.Buffer
	for _, header := range []struct{ name, value string }{
		{"Content-Type", contentType},
		{"Content-Transfer-Encoding", "base64"},
		{"Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(path)})},
	} {
		if err := writeHeader(&buffer, header.name, header.value); err != nil {
			return "", err
		}
	}
	return buffer.String(), nil
}

func streamAttachmentBase64Context(
	ctx context.Context,
	writer io.Writer,
	attachment DraftAttachment,
	openAttachment attachmentOpener,
) (resultErr error) {
	file, err := openAttachment(attachment.Path)
	if err != nil {
		return &ComposerError{Message: "read draft attachment " + filepath.Base(attachment.Path), Err: err}
	}
	defer func() {
		if err := file.Close(); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	encoder := base64.NewEncoder(base64.StdEncoding, &base64LineWriter{w: writer})
	hash := sha256.New()
	limited := io.LimitReader(file, attachment.Size+1)
	written, copyErr := io.Copy(encoder, io.TeeReader(contextReader{ctx: ctx, reader: limited}, hash))
	closeErr := encoder.Close()
	if copyErr != nil {
		return &ComposerError{Message: "encode draft attachment " + filepath.Base(attachment.Path), Err: copyErr}
	}
	if closeErr != nil {
		return &ComposerError{Message: "encode draft attachment " + filepath.Base(attachment.Path), Err: closeErr}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	actualHash := hex.EncodeToString(hash.Sum(nil))
	if written != attachment.Size || !strings.EqualFold(actualHash, attachment.SHA256) {
		return validationError("draft attachment " + filepath.Base(attachment.Path) + " changed after review; update the draft before sending")
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// writeAlternativeMultipart writes the multipart/alternative section
// directly into buffer, streaming quoted-printable encoding without
// intermediate string copies.
func writeAlternativeMultipart(buffer *bytes.Buffer, boundary string, draft Draft) {
	buffer.WriteString("--")
	buffer.WriteString(boundary)
	buffer.WriteString(composerCRLF)
	buffer.WriteString("Content-Type: text/plain; charset=utf-8")
	buffer.WriteString(composerCRLF)
	buffer.WriteString("Content-Transfer-Encoding: quoted-printable")
	buffer.WriteString(composerCRLF)
	buffer.WriteString(composerCRLF)
	if err := writeQuotedPrintable(buffer, draft.Body); err != nil {
		// quotedprintable.Writer cannot fail on valid UTF-8 input; the error
		// path is unreachable for well-formed drafts but we write nothing
		// extra on failure to keep the buffer consistent.
		return
	}
	buffer.WriteString(composerCRLF)

	if draft.BodyHTML == "" {
		buffer.WriteString("--")
		buffer.WriteString(boundary)
		buffer.WriteString("--")
		return
	}

	buffer.WriteString("--")
	buffer.WriteString(boundary)
	buffer.WriteString(composerCRLF)
	buffer.WriteString("Content-Type: text/html; charset=utf-8")
	buffer.WriteString(composerCRLF)
	buffer.WriteString("Content-Transfer-Encoding: quoted-printable")
	buffer.WriteString(composerCRLF)
	buffer.WriteString(composerCRLF)
	if err := writeQuotedPrintable(buffer, draft.BodyHTML); err != nil {
		return
	}
	buffer.WriteString(composerCRLF)
	buffer.WriteString("--")
	buffer.WriteString(boundary)
	buffer.WriteString("--")
}

// writeQuotedPrintable encodes body as quoted-printable directly into buffer
// without an intermediate bytes.Buffer and string conversion.
func writeQuotedPrintable(buffer *bytes.Buffer, body string) error {
	writer := quotedprintable.NewWriter(buffer)
	if _, err := writer.Write([]byte(body)); err != nil {
		_ = writer.Close()
		return fmt.Errorf("quote-printable encode body: %w", err)
	}
	return writer.Close()
}

// base64LineWriter wraps a writer and inserts CRLF every composerLineLength
// bytes of base64 output, per RFC 2045.
type base64LineWriter struct {
	w io.Writer
	n int
}

func (lw *base64LineWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if lw.n >= composerLineLength {
			if _, err := io.WriteString(lw.w, composerCRLF); err != nil {
				return written, err
			}
			lw.n = 0
		}
		chunk := len(p)
		if remaining := composerLineLength - lw.n; chunk > remaining {
			chunk = remaining
		}
		n, err := lw.w.Write(p[:chunk])
		lw.n += n
		written += n
		p = p[n:]
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func writeComposerHeaders(buffer *bytes.Buffer, draft Draft, messageID, contentType string) error {
	if err := validateComposerHeaderValue("Subject", draft.Subject); err != nil {
		return err
	}
	from, err := formatComposerSender(draft.From)
	if err != nil {
		return err
	}
	to, err := formatAddressList(draft.To)
	if err != nil {
		return err
	}
	cc, err := formatAddressList(draft.CC)
	if err != nil {
		return err
	}
	threadingKind := draft.Kind == DraftKindReply || draft.Kind == DraftKindForward
	threaded := threadingKind && draft.SourceMessageID != ""
	references := ""
	if threadingKind {
		var err error
		references, err = canonicalThreadReferences(draft.SourceReferences, draft.SourceMessageID)
		if err != nil {
			return &ComposerError{Message: "validate thread headers", Err: err}
		}
	}
	headers := []struct{ name, value string }{{"From", from}, {"To", to}}
	if len(draft.CC) > 0 {
		headers = append(headers, struct{ name, value string }{"Cc", cc})
	}
	for _, header := range append(headers, []struct{ name, value string }{
		{"Subject", encodeHeaderValue(draft.Subject)}, {"Date", time.Now().Format(time.RFC1123Z)},
		{"Message-ID", messageID}, {"MIME-Version", "1.0"},
	}...) {
		if err := writeHeader(buffer, header.name, header.value); err != nil {
			return err
		}
	}
	if threaded {
		if err := writeHeader(buffer, "In-Reply-To", draft.SourceMessageID); err != nil {
			return err
		}
		if err := writeHeader(buffer, "References", references); err != nil {
			return err
		}
	}
	return writeHeader(buffer, "Content-Type", contentType)
}

func writeHeader(buffer *bytes.Buffer, name, value string) error {
	if err := validateComposerHeaderValue(name, value); err != nil {
		return err
	}
	preferred, maximum := composerHeaderLength, composerHeaderMaximum
	parts := headerFoldingParts(" " + value)
	if name == "Subject" || name == "From" || name == "To" || name == "Cc" {
		for _, part := range parts {
			word := strings.Trim(part, " \t")
			if strings.HasPrefix(word, "=?") && strings.HasSuffix(word, "?=") {
				if _, err := (&mime.WordDecoder{}).Decode(word); err == nil {
					preferred, maximum = composerEncodedHeaderLength, composerEncodedHeaderLength
					break
				}
			}
		}
	}
	buffer.WriteString(name)
	buffer.WriteByte(':')
	column := len(name) + 1
	if value != "" {
		for _, part := range parts {
			if column+len(part) > preferred && strings.Trim(part, " \t") != "" {
				buffer.WriteString(composerCRLF)
				column = 0
			}
			if column+len(part) > maximum {
				return validationError(fmt.Sprintf("%s header cannot be folded without changing its value; physical line exceeds %d bytes", name, maximum))
			}
			buffer.WriteString(part)
			column += len(part)
		}
	}
	buffer.WriteString(composerCRLF)
	return nil
}

// Fold only between structured tokens, preserving whitespace and quoted pairs.
// Text requiring arbitrary splits is encoded before reaching this writer.
func headerFoldingParts(value string) []string {
	var parts []string
	start, comments := 0, 0
	quoted, escaped, angle := false, false, false
	for index := 0; index < len(value); index++ {
		character := value[index]
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' && (quoted || comments > 0) {
			escaped = true
			continue
		}
		if character == '"' && comments == 0 {
			quoted = !quoted
		}
		if !quoted {
			switch character {
			case '(':
				if !angle {
					comments++
				}
			case ')':
				if comments > 0 {
					comments--
				}
			case '<':
				if comments == 0 {
					angle = true
				}
			case '>':
				if comments == 0 {
					angle = false
				}
			}
		}
		if index > 0 && !quoted && !angle && comments == 0 && (character == ' ' || character == '\t') && value[index-1] != ' ' && value[index-1] != '\t' {
			parts = append(parts, value[start:index])
			start = index
		}
	}
	return append(parts, value[start:])
}

func validateComposerHeaderValue(name, value string) error {
	if !utf8.ValidString(value) {
		return validationError(name + " header contains invalid UTF-8")
	}
	for _, character := range value {
		if character == 0x7f || (character < ' ' && character != '\t') {
			return validationError(name + " header contains control characters")
		}
	}
	return nil
}

func formatAddressList(recipients []Recipient) (string, error) {
	if len(recipients) == 0 {
		return "", nil
	}
	var builder strings.Builder
	builder.Grow(len(recipients) * 32)
	for i, recipient := range recipients {
		if i > 0 {
			builder.WriteString(", ")
		}
		address, err := formatComposerAddress(recipient)
		if err != nil {
			return "", err
		}
		builder.WriteString(address)
	}
	return builder.String(), nil
}

func formatComposerAddress(recipient Recipient) (string, error) {
	if err := validateComposerHeaderValue("recipient name", recipient.Name); err != nil {
		return "", err
	}
	if err := validateComposerHeaderValue("recipient address", recipient.Address); err != nil {
		return "", err
	}
	address, err := mail.ParseAddress(recipient.Address)
	if err != nil {
		return "", validationError("invalid recipient address")
	}
	if err := validateComposerHeaderValue("recipient address display name", address.Name); err != nil {
		return "", err
	}
	if recipient.Name == "" {
		recipient.Name = address.Name
	}
	encoded := encodeHeaderValue(recipient.Name)
	if encoded != recipient.Name {
		return encoded + " " + (&mail.Address{Address: address.Address}).String(), nil
	}
	return (&mail.Address{Name: recipient.Name, Address: address.Address}).String(), nil
}

func formatComposerSender(value string) (string, error) {
	if err := validateComposerHeaderValue("From", value); err != nil {
		return "", err
	}
	if value == "" {
		return "", nil
	}
	address, err := mail.ParseAddress(value)
	if err != nil {
		return "", validationError("invalid From header address")
	}
	if err := validateComposerHeaderValue("From display name", address.Name); err != nil {
		return "", err
	}
	if encodeHeaderValue(address.Name) == address.Name {
		return value, nil
	}
	return formatComposerAddress(Recipient{Name: address.Name, Address: MailboxAddrSpec(address.Address)})
}

// encodeHeaderValue requires validated UTF-8, including decoded sender names.
func encodeHeaderValue(value string) string {
	// Include quoted-pair expansion so ASCII names also fit in mixed fields
	// containing encoded names, which impose the stricter 76-byte limit.
	quotedLength := len(value) + strings.Count(value, "\\") + strings.Count(value, "\"")
	if quotedLength <= composerEncodedHeaderLength-len("Subject: ") && strings.Trim(value, " \t") == value && !strings.Contains(value, "=?") && mime.QEncoding.Encode("UTF-8", value) == value {
		return value
	}
	// The standard WordEncoder intentionally leaves ASCII unchanged. Force
	// encoding when folding it would lose text, using complete UTF-8 chunks.
	const maximumChunkBytes = 45 // 60 base64 bytes + 12 framing bytes <= 75.
	var encoded strings.Builder
	for len(value) > 0 {
		end := min(len(value), maximumChunkBytes)
		for end < len(value) && !utf8.RuneStart(value[end]) {
			end--
		}
		if encoded.Len() > 0 {
			encoded.WriteByte(' ')
		}
		encoded.WriteString("=?UTF-8?b?")
		encoded.WriteString(base64.StdEncoding.EncodeToString([]byte(value[:end])))
		encoded.WriteString("?=")
		value = value[end:]
	}
	return encoded.String()
}

// threadReferences returns the canonical References chain for valid inputs.
func threadReferences(references, sourceMessageID string) string {
	canonical, err := canonicalThreadReferences(references, sourceMessageID)
	if err != nil {
		return ""
	}
	return canonical
}

// randomBoundary returns a cryptographically random boundary unique per message.
func randomBoundary() (string, error) {
	entropy := make([]byte, 16)
	if _, err := rand.Read(entropy); err != nil {
		return "", &ComposerError{Message: "generate multipart boundary", Err: err}
	}
	return "=_" + hex.EncodeToString(entropy), nil
}
