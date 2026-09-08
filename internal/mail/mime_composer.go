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
	path      string
	size      int64
	messageID string
}

func ComposeMessageSpool(draft Draft, messageID string) (*ComposedMessage, error) {
	return ComposeMessageSpoolContext(context.Background(), draft, messageID)
}

func ComposeMessageSpoolContext(ctx context.Context, draft Draft, messageID string) (*ComposedMessage, error) {
	return composeMessageSpoolContext(ctx, draft, messageID, func(path string) (io.ReadCloser, error) {
		return os.Open(path)
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
			if err := ctx.Err(); err != nil {
				cleanup()
				return nil, err
			}
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
	writeHeader(buffer, "From", draft.From)
	writeHeader(buffer, "To", formatAddressList(draft.To))
	if len(draft.CC) > 0 {
		writeHeader(buffer, "Cc", formatAddressList(draft.CC))
	}
	writeHeader(buffer, "Subject", encodeHeaderValue(draft.Subject))
	writeHeader(buffer, "Date", time.Now().Format(time.RFC1123Z))
	writeHeader(buffer, "Message-ID", messageID)
	writeHeader(buffer, "MIME-Version", "1.0")
	if threaded {
		writeHeader(buffer, "In-Reply-To", draft.SourceMessageID)
		writeHeader(buffer, "References", references)
	}
	writeHeader(buffer, "Content-Type", contentType)
	return nil
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
