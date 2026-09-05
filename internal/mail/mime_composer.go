package mail

import (
	"bufio"
	"bytes"
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
)

const (
	composerLineLength   = 76 // base64 lines per RFC 2045
	composerHeaderLength = 78 // recommended header line limit per RFC 5322
	composerCRLF         = "\r\n"
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

// BuildMessage renders a draft into exact RFC 5322 bytes ready for SMTP
// submission. messageID is used verbatim as the Message-ID header. BCC
// recipients are deliberately omitted from the output. Reply and forward
// drafts carry In-Reply-To and References threading headers when source
// threading is available. Subject
// prefixes (Re:/Fwd:) are applied at draft creation and are never added here.
func BuildMessage(draft Draft, messageID string) ([]byte, error) {
	if messageID == "" {
		return nil, &ComposerError{Message: "message id is required"}
	}
	attachments, err := loadComposerAttachments(draft.Attachments)
	if err != nil {
		return nil, err
	}
	return buildMessageWithAttachments(draft, messageID, attachments)
}

// ComposedMessage is a private replayable spool for a composed RFC 5322
// message. The encoded attachment bytes stay on disk until all consumers have
// replayed the source.
type ComposedMessage struct {
	path      string
	size      int64
	messageID string
}

func ComposeMessageSpool(draft Draft, messageID string) (*ComposedMessage, error) {
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
	writeComposerHeaders(header, draft, messageID, contentType)
	header.WriteString(composerCRLF)
	alternative := &bytes.Buffer{}
	writeAlternativeMultipart(alternative, alternativeBoundary, draft)
	alternative.WriteString(composerCRLF)

	file, err := os.CreateTemp("", "mailcli-message-")
	if err != nil {
		return nil, &ComposerError{Message: "create private message spool", Err: err}
	}
	path := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(path)
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
			if err := write("--" + mixedBoundary + composerCRLF); err != nil {
				cleanup()
				return nil, &ComposerError{Message: "write attachment boundary", Err: err}
			}
			for _, line := range composerAttachmentHeaders(attachment.Path) {
				if err := write(line + composerCRLF); err != nil {
					cleanup()
					return nil, &ComposerError{Message: "write attachment headers", Err: err}
				}
			}
			if err := write(composerCRLF); err != nil {
				cleanup()
				return nil, &ComposerError{Message: "write attachment separator", Err: err}
			}
			if err := streamAttachmentBase64(writer, attachment); err != nil {
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
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, &ComposerError{Message: "close message spool", Err: err}
	}
	stat, err := os.Stat(path)
	if err != nil {
		_ = os.Remove(path)
		return nil, &ComposerError{Message: "stat message spool", Err: err}
	}
	return &ComposedMessage{path: path, size: stat.Size(), messageID: messageID}, nil
}

func (m *ComposedMessage) Open() (io.ReadCloser, error) {
	return os.Open(m.path)
}

func (m *ComposedMessage) Size() int64 { return m.size }

func (m *ComposedMessage) MessageID() string { return m.messageID }

func (m *ComposedMessage) Remove() error { return os.Remove(m.path) }

func composerAttachmentHeaders(path string) []string {
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return []string{
		"Content-Type: " + contentType,
		"Content-Transfer-Encoding: base64",
		"Content-Disposition: " + mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(path)}),
	}
}

func streamAttachmentBase64(writer io.Writer, attachment DraftAttachment) (resultErr error) {
	file, err := os.Open(attachment.Path)
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
	written, copyErr := io.Copy(encoder, io.TeeReader(limited, hash))
	closeErr := encoder.Close()
	if copyErr != nil {
		return &ComposerError{Message: "encode draft attachment " + filepath.Base(attachment.Path), Err: copyErr}
	}
	if closeErr != nil {
		return &ComposerError{Message: "encode draft attachment " + filepath.Base(attachment.Path), Err: closeErr}
	}
	actualHash := hex.EncodeToString(hash.Sum(nil))
	if written != attachment.Size || !strings.EqualFold(actualHash, attachment.SHA256) {
		return validationError("draft attachment " + filepath.Base(attachment.Path) + " changed after review; update the draft before sending")
	}
	return nil
}

func buildMessageWithAttachments(draft Draft, messageID string, attachments []composerAttachment) ([]byte, error) {
	alternativeBoundary, err := randomBoundary()
	if err != nil {
		return nil, err
	}
	mixedBoundary := ""
	if len(attachments) > 0 {
		mixedBoundary, err = randomBoundary()
		if err != nil {
			return nil, err
		}
	}

	contentType := "multipart/alternative; boundary=\"" + alternativeBoundary + "\""
	if len(attachments) > 0 {
		contentType = "multipart/mixed; boundary=\"" + mixedBoundary + "\""
	}

	estimatedSize := estimateMessageSize(draft, attachments)
	buffer := &bytes.Buffer{}
	buffer.Grow(estimatedSize)
	writeComposerHeaders(buffer, draft, messageID, contentType)
	buffer.WriteString(composerCRLF)

	if len(attachments) == 0 {
		writeAlternativeMultipart(buffer, alternativeBoundary, draft)
		buffer.WriteString(composerCRLF)
		return buffer.Bytes(), nil
	}

	// Mixed multipart: write the alternative section as the first part,
	// then each attachment, all directly into the buffer without intermediate
	// string copies.
	buffer.WriteString("--")
	buffer.WriteString(mixedBoundary)
	buffer.WriteString(composerCRLF)
	buffer.WriteString("Content-Type: multipart/alternative; boundary=\"")
	buffer.WriteString(alternativeBoundary)
	buffer.WriteString("\"")
	buffer.WriteString(composerCRLF)
	buffer.WriteString(composerCRLF)
	writeAlternativeMultipart(buffer, alternativeBoundary, draft)
	buffer.WriteString(composerCRLF)

	for _, attachment := range attachments {
		buffer.WriteString("--")
		buffer.WriteString(mixedBoundary)
		buffer.WriteString(composerCRLF)
		for _, header := range attachment.headers() {
			buffer.WriteString(header)
			buffer.WriteString(composerCRLF)
		}
		buffer.WriteString(composerCRLF)
		writeBase64Body(buffer, attachment.data)
		buffer.WriteString(composerCRLF)
	}

	buffer.WriteString("--")
	buffer.WriteString(mixedBoundary)
	buffer.WriteString("--")
	buffer.WriteString(composerCRLF)
	return buffer.Bytes(), nil
}

// estimateMessageSize pre-allocates the buffer to avoid repeated growth.
func estimateMessageSize(draft Draft, attachments []composerAttachment) int {
	size := 1024                // headers
	size += len(draft.Body) * 2 // quoted-printable worst case
	size += len(draft.BodyHTML) * 2
	for _, a := range attachments {
		size += len(a.data)*4/3 + len(a.data)/57*2 + 256
	}
	return size
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

// writeBase64Body streams base64-encoded data into buffer with RFC 2045
// line wrapping, avoiding the intermediate EncodeToString + string copy
// that the previous base64Body() method required.
func writeBase64Body(buffer *bytes.Buffer, data []byte) {
	encoder := base64.NewEncoder(base64.StdEncoding, &base64LineWriter{w: buffer})
	_, _ = encoder.Write(data)
	_ = encoder.Close()
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

func writeComposerHeaders(buffer *bytes.Buffer, draft Draft, messageID, contentType string) {
	writeHeader(buffer, "From", draft.From)
	writeHeader(buffer, "To", formatAddressList(draft.To))
	if len(draft.CC) > 0 {
		writeHeader(buffer, "Cc", formatAddressList(draft.CC))
	}
	writeHeader(buffer, "Subject", encodeHeaderValue(draft.Subject))
	writeHeader(buffer, "Date", time.Now().Format(time.RFC1123Z))
	writeHeader(buffer, "Message-ID", messageID)
	writeHeader(buffer, "MIME-Version", "1.0")
	if (draft.Kind == DraftKindReply || draft.Kind == DraftKindForward) && draft.SourceMessageID != "" {
		writeHeader(buffer, "In-Reply-To", draft.SourceMessageID)
		writeHeader(buffer, "References", threadReferences(draft.SourceReferences, draft.SourceMessageID))
	}
	writeHeader(buffer, "Content-Type", contentType)
}

func writeHeader(buffer *bytes.Buffer, name, value string) {
	if value == "" {
		buffer.WriteString(name)
		buffer.WriteString(":")
		buffer.WriteString(composerCRLF)
		return
	}
	if len(name)+2+len(value) <= composerHeaderLength {
		buffer.WriteString(name)
		buffer.WriteString(": ")
		buffer.WriteString(value)
		buffer.WriteString(composerCRLF)
		return
	}
	limit := composerHeaderLength - len(name) - 2
	buffer.WriteString(name)
	buffer.WriteString(": ")
	buffer.WriteString(strings.Join(foldAt(value, limit), composerCRLF+" "))
	buffer.WriteString(composerCRLF)
}

// foldAt splits value into lines of at most limit bytes, breaking at spaces so
// continuation lines can carry single-space folding whitespace.
func foldAt(value string, limit int) []string {
	if limit < 1 {
		limit = 1
	}
	var lines []string
	for len(value) > limit {
		cut := strings.LastIndex(value[:limit+1], " ")
		if cut < 0 {
			cut = strings.Index(value, " ")
			if cut < 0 {
				break
			}
		}
		lines = append(lines, value[:cut])
		value = value[cut+1:]
	}
	if len(value) > 0 || len(lines) == 0 {
		lines = append(lines, value)
	}
	return lines
}

func formatAddressList(recipients []Recipient) string {
	if len(recipients) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.Grow(len(recipients) * 32)
	for i, recipient := range recipients {
		if i > 0 {
			builder.WriteString(", ")
		}
		builder.WriteString((&mail.Address{Name: recipient.Name, Address: recipient.Address}).String())
	}
	return builder.String()
}

func encodeHeaderValue(value string) string {
	return mime.QEncoding.Encode("UTF-8", value)
}

// threadReferences appends the replied-to message to the prior References chain.
func threadReferences(references, sourceMessageID string) string {
	prior := strings.TrimSpace(references)
	if prior == "" {
		return sourceMessageID
	}
	return prior + " " + sourceMessageID
}

type composerAttachment struct {
	filename    string
	contentType string
	data        []byte
}

func loadComposerAttachments(attachments []DraftAttachment) ([]composerAttachment, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	if len(attachments) == 1 {
		return loadSingleAttachment(attachments[0])
	}
	return loadAttachmentsParallel(attachments)
}

func loadSingleAttachment(attachment DraftAttachment) ([]composerAttachment, error) {
	loaded, err := readAttachmentData(attachment)
	if err != nil {
		return nil, err
	}
	return []composerAttachment{loaded}, nil
}

// loadAttachmentsParallel reads multiple attachment files concurrently.
// For the typical 1-2 attachment case the goroutine overhead is negligible
// compared to file I/O; for larger attachment sets it provides real speedup.
// All attachment errors are aggregated so the caller sees every failure,
// not just the first one encountered.
func loadAttachmentsParallel(attachments []DraftAttachment) ([]composerAttachment, error) {
	type result struct {
		loaded composerAttachment
		err    error
	}
	results := make([]result, len(attachments))
	var wg sync.WaitGroup
	wg.Add(len(attachments))
	for i, attachment := range attachments {
		go func(idx int, att DraftAttachment) {
			defer wg.Done()
			loaded, err := readAttachmentData(att)
			results[idx] = result{loaded: loaded, err: err}
		}(i, attachment)
	}
	wg.Wait()
	loaded := make([]composerAttachment, len(attachments))
	var errs []error
	for i, r := range results {
		if r.err != nil {
			errs = append(errs, r.err)
			continue
		}
		loaded[i] = r.loaded
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return loaded, nil
}

func readAttachmentData(attachment DraftAttachment) (composerAttachment, error) {
	data, err := os.ReadFile(attachment.Path)
	if err != nil {
		return composerAttachment{}, &ComposerError{
			Message: "read draft attachment " + filepath.Base(attachment.Path),
			Err:     err,
		}
	}
	return composerAttachmentFromData(attachment.Path, data), nil
}

func composerAttachmentFromData(path string, data []byte) composerAttachment {
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return composerAttachment{
		filename:    filepath.Base(path),
		contentType: contentType,
		data:        data,
	}
}

func (a composerAttachment) headers() []string {
	return []string{
		"Content-Type: " + a.contentType,
		"Content-Transfer-Encoding: base64",
		"Content-Disposition: " + mime.FormatMediaType("attachment", map[string]string{"filename": a.filename}),
	}
}

// randomBoundary returns a cryptographically random boundary unique per message.
func randomBoundary() (string, error) {
	entropy := make([]byte, 16)
	if _, err := rand.Read(entropy); err != nil {
		return "", &ComposerError{Message: "generate multipart boundary", Err: err}
	}
	return "=_" + hex.EncodeToString(entropy), nil
}
