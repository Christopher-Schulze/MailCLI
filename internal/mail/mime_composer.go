package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
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
// recipients are deliberately omitted from the output. Reply drafts carry
// In-Reply-To and References threading headers; forwards carry none. Subject
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
	w *bytes.Buffer
	n int
}

func (lw *base64LineWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if lw.n >= composerLineLength {
			lw.w.WriteString(composerCRLF)
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
	if draft.Kind == DraftKindReply && draft.SourceMessageID != "" {
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
