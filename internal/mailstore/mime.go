package mailstore

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	stdmail "net/mail"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/emersion/go-message"
	messageMail "github.com/emersion/go-message/mail"
	"mailcli/internal/mail"
)

const (
	maximumHeaderBytes       = 1024 * 1024
	maximumTextPartBytes     = 16 * 1024 * 1024
	maximumMIMETextBytes     = int64(32 * 1024 * 1024)
	maximumMIMEParts         = int64(4096)
	maximumMIMEDepth         = int64(64)
	maximumMIMEMetadata      = int64(8 * 1024 * 1024)
	maximumMIMERawBytes      = maximumRFCSourceBytes
	maximumInt64             = int64(1<<63 - 1)
	mimePartMetadataOverhead = int64(96)
)

type mimeBudgetResource string

const (
	mimeBudgetTextBytes      mimeBudgetResource = "text_bytes"
	mimeBudgetParts          mimeBudgetResource = "parts"
	mimeBudgetDepth          mimeBudgetResource = "depth"
	mimeBudgetMetadata       mimeBudgetResource = "metadata_bytes"
	mimeBudgetRawBytes       mimeBudgetResource = "raw_bytes"
	mimeBudgetDiagnosticID                      = "mime:budget:"
	mimeCanceledDiagnosticID                    = "mime:canceled"
)

type mimeParseBudgetLimits struct {
	textBytes int64
	parts     int64
	depth     int64
	metadata  int64
	rawBytes  int64
}

type mimeResourceLimitError struct {
	resource mimeBudgetResource
	used     int64
	limit    int64
}

func (e *mimeResourceLimitError) Error() string {
	return fmt.Sprintf("MIME %s budget exceeded (%d/%d bytes or items)", e.resource, e.used, e.limit)
}

func (e *mimeResourceLimitError) ErrorCode() string {
	return "mime_resource_limit"
}

type mimeParseBudget struct {
	limits    mimeParseBudgetLimits
	textBytes int64
	parts     int64
	metadata  int64
	rawBytes  int64
	exhausted *mimeResourceLimitError
}

func defaultMIMEParseBudgetLimits() mimeParseBudgetLimits {
	return mimeParseBudgetLimits{
		textBytes: maximumMIMETextBytes,
		parts:     maximumMIMEParts,
		depth:     maximumMIMEDepth,
		metadata:  maximumMIMEMetadata,
		rawBytes:  maximumMIMERawBytes,
	}
}

func newMIMEParseBudget(limits mimeParseBudgetLimits) *mimeParseBudget {
	if limits.textBytes < 0 {
		limits.textBytes = 0
	}
	if limits.parts < 0 {
		limits.parts = 0
	}
	if limits.depth < 0 {
		limits.depth = 0
	}
	if limits.metadata < 0 {
		limits.metadata = 0
	}
	if limits.rawBytes < 0 {
		limits.rawBytes = 0
	}
	return &mimeParseBudget{limits: limits}
}

func (b *mimeParseBudget) error() *mimeResourceLimitError {
	return b.exhausted
}

func (b *mimeParseBudget) exhaust(resource mimeBudgetResource, used, limit int64) *mimeResourceLimitError {
	if b.exhausted != nil {
		return b.exhausted
	}
	if used < 0 {
		used = maximumInt64
	}
	if limit < 0 {
		limit = 0
	}
	b.exhausted = &mimeResourceLimitError{resource: resource, used: used, limit: limit}
	return b.exhausted
}

func (b *mimeParseBudget) reserve(resource mimeBudgetResource, used *int64, amount, limit int64) bool {
	if b.exhausted != nil {
		return false
	}
	if amount < 0 || *used > limit || amount > limit-*used {
		candidate := *used
		if amount > 0 && candidate <= maximumInt64-amount {
			candidate += amount
		} else {
			candidate = maximumInt64
		}
		return b.exhaust(resource, candidate, limit) == nil
	}
	*used += amount
	return true
}

func (b *mimeParseBudget) visit(path []int) *mimeResourceLimitError {
	depth := int64(len(path))
	if depth >= b.limits.depth {
		return b.exhaust(mimeBudgetDepth, depth+1, b.limits.depth)
	}
	if !b.reserve(mimeBudgetParts, &b.parts, 1, b.limits.parts) {
		return b.error()
	}
	return nil
}

func (b *mimeParseBudget) checkDepth(depth int64) *mimeResourceLimitError {
	if depth < 0 || depth >= b.limits.depth {
		return b.exhaust(mimeBudgetDepth, depth+1, b.limits.depth)
	}
	return nil
}

func (b *mimeParseBudget) remainingTextBytes() int64 {
	if b.exhausted != nil || b.textBytes >= b.limits.textBytes {
		return 0
	}
	return b.limits.textBytes - b.textBytes
}

func (b *mimeParseBudget) consumeText(amount int64) bool {
	return b.reserve(mimeBudgetTextBytes, &b.textBytes, amount, b.limits.textBytes)
}

func (b *mimeParseBudget) reserveMetadata(amount int64) bool {
	return b.reserve(mimeBudgetMetadata, &b.metadata, amount, b.limits.metadata)
}

type mimeBudgetReader struct {
	ctx    context.Context
	reader io.Reader
	budget *mimeParseBudget
}

func (r *mimeBudgetReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if err := r.budget.error(); err != nil {
		return 0, err
	}
	remaining := r.budget.limits.rawBytes - r.budget.rawBytes
	if remaining <= 0 {
		return 0, r.budget.exhaust(mimeBudgetRawBytes, r.budget.rawBytes+1, r.budget.limits.rawBytes)
	}
	if int64(len(buffer)) > remaining {
		buffer = buffer[:remaining]
	}
	read, err := r.reader.Read(buffer)
	if read < 0 || read > len(buffer) {
		return 0, fmt.Errorf("MIME source reader returned invalid byte count %d", read)
	}
	if read > 0 && !r.budget.reserve(mimeBudgetRawBytes, &r.budget.rawBytes, int64(read), r.budget.limits.rawBytes) {
		return read, r.budget.error()
	}
	if contextErr := r.ctx.Err(); contextErr != nil {
		return read, contextErr
	}
	return read, err
}

type mimeContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r mimeContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	read, err := r.reader.Read(buffer)
	if contextErr := r.ctx.Err(); contextErr != nil {
		return read, contextErr
	}
	return read, err
}

func closeMIMEReaderOnCancel(ctx context.Context, reader io.Reader) func() {
	if ctx == nil || ctx.Done() == nil {
		return func() {}
	}
	closer, ok := reader.(io.Closer)
	if !ok {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = closer.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

type mimePart struct {
	ID       string
	Name     string
	MIMEType string
	Size     int64
	SHA256   string
	Complete bool
}

type mimeDocument struct {
	MessageID    string
	ReplyTo      string
	To           []mail.Recipient
	CC           []mail.Recipient
	BCC          []mail.Recipient
	Content      string
	Complete     bool
	MissingParts []string
	Parts        map[string]mimePart
	// skipNonTextBodies avoids decoding non-text part bodies (no base64/QP
	// decode, no drain through the decoded stream). The walker still reads
	// and discards the raw bytes, so I/O is unchanged. Search-only: names
	// and counts stay exact, sizes stay unknown.
	skipNonTextBodies bool
	ctx               context.Context
	budget            *mimeParseBudget
}

type mimeTextRepresentation struct {
	Text string
	Rank int
}

const (
	mimeTextNone = iota
	mimeTextHTML
	mimeTextPlain
)

func parseMIMEDocument(reader io.Reader, partial bool, hashAttachments bool, skipNonTextBodies bool) (mimeDocument, error) {
	return parseMIMEDocumentWithContext(
		context.Background(), reader, partial, hashAttachments, skipNonTextBodies,
	)
}

func parseMIMEDocumentWithContext(
	ctx context.Context,
	reader io.Reader,
	partial bool,
	hashAttachments bool,
	skipNonTextBodies bool,
) (mimeDocument, error) {
	return parseMIMEDocumentWithLimits(
		ctx, reader, partial, hashAttachments, skipNonTextBodies,
		defaultMIMEParseBudgetLimits(),
	)
}

func parseMIMEDocumentWithLimits(
	ctx context.Context,
	reader io.Reader,
	partial bool,
	hashAttachments bool,
	skipNonTextBodies bool,
	limits mimeParseBudgetLimits,
) (mimeDocument, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	budget := newMIMEParseBudget(limits)
	document := mimeDocument{
		Complete:          false,
		skipNonTextBodies: skipNonTextBodies,
		ctx:               ctx,
		budget:            budget,
	}
	if err := ctx.Err(); err != nil {
		markMIMECanceled(&document)
		return document, err
	}
	trackedReader := &mimeBudgetReader{ctx: ctx, reader: reader, budget: budget}
	stopCloseWatcher := closeMIMEReaderOnCancel(ctx, reader)
	defer stopCloseWatcher()
	entity, readErr := message.Read(trackedReader)
	if entity == nil {
		if err := ctx.Err(); err != nil {
			markMIMECanceled(&document)
			return document, err
		}
		if budgetErr := budget.error(); budgetErr != nil {
			markMIMEBudgetExceeded(&document, budgetErr)
			return document, nil
		}
	}
	if entity == nil || (readErr != nil && !message.IsUnknownCharset(readErr) && !message.IsUnknownEncoding(readErr)) {
		return mimeDocument{}, operationError("invalid_message_source", fmt.Sprintf("parse RFC message: %v", readErr))
	}
	document.Complete = readErr == nil && !partial
	if readErr != nil {
		markMissingPart(&document, "mime-decoding")
	}
	header := messageMail.Header{Header: entity.Header}
	if messageID, err := header.MessageID(); err == nil {
		if document.retainMetadata(int64(len(messageID))) {
			document.MessageID = messageID
		}
	}
	var replyToComplete bool
	document.ReplyTo, replyToComplete = firstFormattedAddress(&header, "Reply-To")
	if document.ReplyTo != "" && !document.retainMetadata(int64(len(document.ReplyTo))) {
		document.ReplyTo = ""
	}
	if !replyToComplete {
		markMissingPart(&document, "header:reply-to")
	}
	document.To = documentRecipients(&document, &header, "To")
	document.CC = documentRecipients(&document, &header, "Cc")
	document.BCC = documentRecipients(&document, &header, "Bcc")
	if budgetErr := budget.error(); budgetErr != nil {
		markMIMEBudgetExceeded(&document, budgetErr)
		return document, nil
	}
	representation, walkErr := parseMIMEEntity(
		entity, nil, readErr, partial, hashAttachments, &document,
	)
	document.Content = representation.Text
	if walkErr != nil {
		if err := ctx.Err(); err != nil {
			markMIMECanceled(&document)
			return document, err
		}
		if budgetErr := mimeBudgetError(&document, walkErr); budgetErr != nil {
			markMIMEBudgetExceeded(&document, budgetErr)
			return document, nil
		}
		markMissingPart(&document, "mime-structure")
	}
	if err := ctx.Err(); err != nil {
		markMIMECanceled(&document)
		return document, err
	}
	return document, nil
}

func (document *mimeDocument) contextErr() error {
	if document.ctx == nil {
		return nil
	}
	return document.ctx.Err()
}

func (document *mimeDocument) retainMetadata(amount int64) bool {
	if document.budget == nil || document.budget.reserveMetadata(amount) {
		return true
	}
	markMIMEBudgetExceeded(document, document.budget.error())
	return false
}

func mimeBudgetError(document *mimeDocument, err error) *mimeResourceLimitError {
	if document != nil && document.budget != nil {
		if budgetErr := document.budget.error(); budgetErr != nil {
			return budgetErr
		}
	}
	var budgetErr *mimeResourceLimitError
	if errors.As(err, &budgetErr) {
		return budgetErr
	}
	return nil
}

func appendMissingPart(document *mimeDocument, identifier string) {
	for _, existing := range document.MissingParts {
		if existing == identifier {
			return
		}
	}
	document.MissingParts = append(document.MissingParts, identifier)
}

func markMIMEBudgetExceeded(document *mimeDocument, budgetErr *mimeResourceLimitError) {
	if budgetErr == nil {
		return
	}
	document.Complete = false
	appendMissingPart(document, mimeBudgetDiagnosticID+string(budgetErr.resource))
}

func markMIMECanceled(document *mimeDocument) {
	document.Complete = false
	appendMissingPart(document, mimeCanceledDiagnosticID)
}

// sourceHeaders carries the header-block values reply/forward derivation and
// mutation targeting need, without parsing the MIME body.
type sourceHeaders struct {
	MessageID    string
	References   string
	Subject      string
	From         string
	ReplyTo      []mail.Recipient
	ReplyToError error
	To           []mail.Recipient
	CC           []mail.Recipient
}

// sourceHeadersFromReader reads only the header block of a raw RFC 5322
// message. Full MIME parsing would stream and drain every attachment body to
// read these headers, so derivation and mutation targeting use this instead.
func sourceHeadersFromReader(reader io.Reader) (sourceHeaders, error) {
	headers, err := readRawHeaders(reader)
	if err != nil {
		return sourceHeaders{}, err
	}
	entity, readErr := message.Read(strings.NewReader(headers))
	if entity == nil || (readErr != nil && !message.IsUnknownCharset(readErr) && !message.IsUnknownEncoding(readErr)) {
		return sourceHeaders{}, operationError("invalid_message_source", fmt.Sprintf("parse RFC headers: %v", readErr))
	}
	header := messageMail.Header{Header: entity.Header}
	var out sourceHeaders
	if id, err := header.MessageID(); err == nil {
		out.MessageID = id
	}
	if subject, err := header.Subject(); err == nil {
		out.Subject = subject
	}
	out.From, _ = firstFormattedAddress(&header, "From")
	out.ReplyTo, _, out.ReplyToError = headerRecipients(&header, "Reply-To")
	out.To, _, _ = headerRecipients(&header, "To")
	out.CC, _, _ = headerRecipients(&header, "Cc")
	out.References = strings.TrimSpace(header.Get("References"))
	return out, nil
}

// messageIDFromSource resolves the Message-ID header from the header block
// only. A missing Message-ID header yields "" (same semantics as the previous
// full-parse path).
func messageIDFromSource(reader io.Reader) (string, error) {
	headers, err := sourceHeadersFromReader(reader)
	if err != nil {
		return "", err
	}
	return headers.MessageID, nil
}

func parseMIMEEntity(
	entity *message.Entity,
	path []int,
	partErr error,
	partial bool,
	hashAttachments bool,
	document *mimeDocument,
) (mimeTextRepresentation, error) {
	if err := document.contextErr(); err != nil {
		return mimeTextRepresentation{}, err
	}
	if budgetErr := document.budget.visit(path); budgetErr != nil {
		markMIMEBudgetExceeded(document, budgetErr)
		return mimeTextRepresentation{}, budgetErr
	}
	if partErr != nil {
		markMissingPart(document, mimePartID(path))
		if budgetErr := document.budget.error(); budgetErr != nil {
			markMIMEBudgetExceeded(document, budgetErr)
			return mimeTextRepresentation{}, budgetErr
		}
	}
	mediaType, parameters, contentTypeErr := entity.Header.ContentType()
	if contentTypeErr != nil {
		mediaType = "application/octet-stream"
		document.Complete = false
	}
	mediaType = strings.ToLower(mediaType)
	if strings.HasPrefix(mediaType, "multipart/") {
		return parseMIMEMultipart(entity, path, mediaType, partial, hashAttachments, document)
	}
	disposition, dispositionParameters, dispositionErr := entity.Header.ContentDisposition()
	if dispositionErr != nil && entity.Header.Get("Content-Disposition") != "" {
		document.Complete = false
	}
	filename := dispositionParameters["filename"]
	if filename == "" {
		filename = parameters["name"]
	}
	partID := mimePartID(path)
	if strings.EqualFold(disposition, "attachment") || filename != "" {
		if !document.retainMetadata(mimePartMetadataBytes(partID, filename, mediaType, hashAttachments)) {
			return mimeTextRepresentation{}, document.budget.error()
		}
		if document.skipNonTextBodies {
			// Search path: skip the decode, not the I/O. The walker still
			// reads and discards raw bytes. Names and counts stay exact.
			if document.Parts == nil {
				document.Parts = make(map[string]mimePart)
			}
			document.Parts[partID] = mimePart{
				ID: partID, Name: filename, MIMEType: mediaType, Size: -1,
				Complete: !partial,
			}
			if partial {
				document.Complete = false
			}
			return mimeTextRepresentation{}, nil
		}
		size, digest, err := consumeMIMEAttachmentContext(document.ctx, entity.Body, hashAttachments)
		complete := partErr == nil && err == nil && !missingAppleContent(
			entity.Header.Get("X-Apple-Content-Length"), size, true,
		)
		if !complete {
			digest = ""
		}
		if document.Parts == nil {
			document.Parts = make(map[string]mimePart)
		}
		document.Parts[partID] = mimePart{
			ID: partID, Name: filename, MIMEType: mediaType, Size: size,
			SHA256: digest, Complete: complete,
		}
		if !complete {
			markMissingPart(document, partID)
		}
		if budgetErr := mimeBudgetError(document, err); budgetErr != nil {
			markMIMEBudgetExceeded(document, budgetErr)
			return mimeTextRepresentation{}, budgetErr
		}
		if contextErr := document.contextErr(); contextErr != nil {
			return mimeTextRepresentation{}, contextErr
		}
		return mimeTextRepresentation{}, nil
	}
	if mediaType != "text/plain" && mediaType != "text/html" {
		if document.skipNonTextBodies {
			if partial {
				document.Complete = false
			}
			return mimeTextRepresentation{}, nil
		}
		_, err := io.Copy(io.Discard, mimeContextReader{ctx: document.ctx, reader: entity.Body})
		if err != nil {
			document.Complete = false
			if budgetErr := mimeBudgetError(document, err); budgetErr != nil {
				markMIMEBudgetExceeded(document, budgetErr)
				return mimeTextRepresentation{}, budgetErr
			}
			if contextErr := document.contextErr(); contextErr != nil {
				return mimeTextRepresentation{}, contextErr
			}
		}
		return mimeTextRepresentation{}, err
	}
	appleLength, hasAppleLength := parseAppleContentLength(entity.Header.Get("X-Apple-Content-Length"))
	remainingText := document.budget.remainingTextBytes()
	if remainingText <= 0 {
		budgetErr := document.budget.exhaust(mimeBudgetTextBytes, document.budget.textBytes+1, document.budget.limits.textBytes)
		markMIMEBudgetExceeded(document, budgetErr)
		return mimeTextRepresentation{}, budgetErr
	}
	partMaximum := int64(maximumTextPartBytes)
	aggregateLimited := remainingText <= partMaximum
	if remainingText < partMaximum {
		partMaximum = remainingText
	}
	body, truncated, err := readBoundedPartContext(
		document.ctx, entity.Body, partMaximum, appleLength, hasAppleLength, !aggregateLimited,
	)
	if len(body) > 0 && !document.budget.consumeText(int64(len(body))) {
		budgetErr := document.budget.error()
		markMIMEBudgetExceeded(document, budgetErr)
		return mimeTextRepresentation{}, budgetErr
	}
	if err != nil || truncated {
		document.Complete = false
	}
	if truncated && aggregateLimited {
		budgetErr := document.budget.exhaust(
			mimeBudgetTextBytes, document.budget.textBytes+1, document.budget.limits.textBytes,
		)
		markMIMEBudgetExceeded(document, budgetErr)
		err = budgetErr
	}
	if hasAppleLength && partial && appleLength > int64(len(body)) {
		markMissingPart(document, partID)
		if budgetErr := mimeBudgetError(document, err); budgetErr != nil {
			return mimeTextRepresentation{}, budgetErr
		}
		if contextErr := document.contextErr(); contextErr != nil {
			return mimeTextRepresentation{}, contextErr
		}
		return mimeTextRepresentation{}, nil
	}
	rank := mimeTextPlain
	var text string
	if mediaType == "text/html" {
		text = htmlToText(body)
		rank = mimeTextHTML
	} else {
		text = strings.TrimSpace(string(body))
	}
	if text == "" {
		return mimeTextRepresentation{}, err
	}
	if budgetErr := mimeBudgetError(document, err); budgetErr != nil {
		markMIMEBudgetExceeded(document, budgetErr)
		return mimeTextRepresentation{Text: text, Rank: rank}, budgetErr
	}
	if contextErr := document.contextErr(); contextErr != nil {
		return mimeTextRepresentation{Text: text, Rank: rank}, contextErr
	}
	return mimeTextRepresentation{Text: text, Rank: rank}, err
}

func parseMIMEMultipart(
	entity *message.Entity,
	path []int,
	mediaType string,
	partial bool,
	hashAttachments bool,
	document *mimeDocument,
) (result mimeTextRepresentation, resultErr error) {
	reader := entity.MultipartReader()
	if reader == nil {
		return mimeTextRepresentation{}, fmt.Errorf("multipart entity has no reader")
	}
	defer joinCloseError(&resultErr, reader, "MIME multipart reader")
	var children []mimeTextRepresentation
	for index := 0; ; index++ {
		if contextErr := document.contextErr(); contextErr != nil {
			return combineMIMEText(mediaType, children), contextErr
		}
		if budgetErr := document.budget.error(); budgetErr != nil {
			return combineMIMEText(mediaType, children), budgetErr
		}
		child, err := reader.NextPart()
		if contextErr := document.contextErr(); contextErr != nil {
			return combineMIMEText(mediaType, children), contextErr
		}
		if budgetErr := document.budget.error(); budgetErr != nil {
			return combineMIMEText(mediaType, children), budgetErr
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if child == nil {
			return combineMIMEText(mediaType, children), err
		}
		if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
			return combineMIMEText(mediaType, children), err
		}
		childDepth := int64(len(path)) + 1
		if budgetErr := document.budget.checkDepth(childDepth); budgetErr != nil {
			markMIMEBudgetExceeded(document, budgetErr)
			return combineMIMEText(mediaType, children), budgetErr
		}
		childPath := make([]int, len(path)+1)
		copy(childPath, path)
		childPath[len(path)] = index
		representation, childErr := parseMIMEEntity(
			child, childPath, err, partial, hashAttachments, document,
		)
		children = append(children, representation)
		if childErr != nil {
			return combineMIMEText(mediaType, children), childErr
		}
	}
	return combineMIMEText(mediaType, children), nil
}

func combineMIMEText(mediaType string, children []mimeTextRepresentation) mimeTextRepresentation {
	if mediaType == "multipart/alternative" {
		var selected mimeTextRepresentation
		for _, child := range children {
			if child.Text != "" && child.Rank >= selected.Rank {
				selected = child
			}
		}
		return selected
	}
	var builder strings.Builder
	rank := mimeTextNone
	first := true
	for _, child := range children {
		if child.Text != "" {
			if !first {
				builder.WriteString("\n\n")
			}
			builder.WriteString(child.Text)
			first = false
		}
		if child.Rank > rank {
			rank = child.Rank
		}
	}
	return mimeTextRepresentation{Text: builder.String(), Rank: rank}
}

func consumeMIMEAttachmentContext(ctx context.Context, reader io.Reader, withHash bool) (int64, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	contextReader := mimeContextReader{ctx: ctx, reader: reader}
	if !withHash {
		size, err := io.Copy(io.Discard, contextReader)
		return size, "", err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, contextReader)
	return size, hex.EncodeToString(hash.Sum(nil)), err
}

func readRawHeaders(reader io.Reader) (string, error) {
	buffered := bufio.NewReaderSize(io.LimitReader(reader, int64(maximumHeaderBytes)+1), 64*1024)
	var output strings.Builder
	output.Grow(maximumHeaderBytes)
	for output.Len() <= maximumHeaderBytes {
		line, err := buffered.ReadBytes('\n')
		output.Write(line)
		if len(line) == 1 && line[0] == '\n' || len(line) == 2 && line[0] == '\r' && line[1] == '\n' {
			return output.String(), nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", operationError("invalid_message_source", "RFC message has no header boundary")
			}
			return "", fmt.Errorf("read RFC message headers: %w", err)
		}
	}
	return "", operationError("invalid_message_source", "RFC message headers exceed the safety limit")
}

func readBoundedPartContext(
	ctx context.Context,
	reader io.Reader,
	maximum int64,
	sizeHint int64,
	hasSizeHint bool,
	drain bool,
) ([]byte, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if maximum < 0 {
		return nil, false, fmt.Errorf("MIME part limit must not be negative")
	}
	limit := maximum
	if limit < maximumInt64 {
		limit++
	}
	limited := io.LimitReader(mimeContextReader{ctx: ctx, reader: reader}, limit)
	// Pre-size from the decoded-length hint instead of pre-allocating
	// maximum+1 bytes (which can be 16 MiB per text part). io.Copy uses a
	// 32 KiB buffer internally; the bytes.Buffer grows only past the hint.
	var buf bytes.Buffer
	if hasSizeHint && sizeHint > 0 && maximum > 0 {
		buf.Grow(int(min(sizeHint, maximum)))
	}
	if _, err := io.Copy(&buf, limited); err != nil {
		return buf.Bytes(), false, err
	}
	body := buf.Bytes()
	truncated := int64(len(body)) > maximum
	if truncated {
		body = body[:maximum]
	}
	if !drain {
		return body, truncated, nil
	}
	_, drainErr := io.Copy(io.Discard, mimeContextReader{ctx: ctx, reader: reader})
	if drainErr != nil {
		return body, truncated, drainErr
	}
	return body, truncated, nil
}

func mimePartMetadataBytes(partID, name, mediaType string, withHash bool) int64 {
	amount := mimePartMetadataOverhead
	for _, value := range []string{partID, name, mediaType} {
		length := int64(len(value))
		if length > maximumInt64-amount {
			return maximumInt64
		}
		amount += length
	}
	if withHash {
		if 64 > maximumInt64-amount {
			return maximumInt64
		}
		amount += 64
	}
	return amount
}

func mimeStringBytes(values ...string) int64 {
	var amount int64
	for _, value := range values {
		length := int64(len(value))
		if length > maximumInt64-amount {
			return maximumInt64
		}
		amount += length
	}
	return amount
}

// parseAppleContentLength reads the decoded-length hint from Mail's
// X-Apple-Content-Length header for buffer pre-sizing.
func parseAppleContentLength(value string) (int64, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, false
	}
	length, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || length < 0 {
		return 0, false
	}
	return length, true
}

func missingAppleContent(value string, available int64, partial bool) bool {
	if !partial {
		return false
	}
	expected, ok := parseAppleContentLength(value)
	return ok && expected > available
}

func mimePartID(path []int) string {
	if len(path) == 0 {
		return "1"
	}
	var buf []byte
	for index, component := range path {
		if index > 0 {
			buf = append(buf, '.')
		}
		buf = strconv.AppendInt(buf, int64(component+1), 10)
	}
	return string(buf)
}

func markMissingPart(document *mimeDocument, identifier string) {
	document.Complete = false
	for _, existing := range document.MissingParts {
		if existing == identifier {
			return
		}
	}
	if document.budget != nil && !document.budget.reserveMetadata(int64(len(identifier))) {
		markMIMEBudgetExceeded(document, document.budget.error())
		return
	}
	appendMissingPart(document, identifier)
}

func documentRecipients(document *mimeDocument, header *messageMail.Header, key string) []mail.Recipient {
	recipients, complete, _ := headerRecipients(header, key)
	if !complete {
		markMissingPart(document, "header:"+strings.ToLower(key))
	}
	retained := make([]mail.Recipient, 0, len(recipients))
	for _, recipient := range recipients {
		if !document.retainMetadata(mimeStringBytes(recipient.Name, recipient.Address)) {
			break
		}
		retained = append(retained, recipient)
	}
	return retained
}

func headerRecipients(header *messageMail.Header, key string) ([]mail.Recipient, bool, error) {
	if strings.TrimSpace(header.Get(key)) == "" {
		return []mail.Recipient{}, true, nil
	}
	addresses, err := header.AddressList(key)
	if err != nil {
		return []mail.Recipient{}, false, err
	}
	recipients := make([]mail.Recipient, 0, len(addresses))
	for _, address := range addresses {
		recipients = append(recipients, mail.Recipient{Name: address.Name, Address: address.Address})
	}
	return recipients, true, nil
}

func firstFormattedAddress(header *messageMail.Header, key string) (string, bool) {
	if strings.TrimSpace(header.Get(key)) == "" {
		return "", true
	}
	addresses, err := header.AddressList(key)
	if err != nil || len(addresses) == 0 {
		return "", false
	}
	return (&stdmail.Address{Name: addresses[0].Name, Address: addresses[0].Address}).String(), true
}

func htmlToText(source []byte) string {
	return mail.HTMLToPlainText(source)
}

func isASCIIWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n' || value == '\r' || value == '\f'
}

func hasVisibleText(value []byte) bool {
	for i := 0; i < len(value); {
		c := value[i]
		if c < utf8.RuneSelf {
			if !isASCIIWhitespace(c) {
				return true
			}
			i++
			continue
		}
		character, size := utf8.DecodeRune(value[i:])
		if !unicode.IsSpace(character) {
			return true
		}
		i += size
	}
	return false
}

func normalizeTextLayout(value string) string {
	var output strings.Builder
	output.Grow(len(value))
	pendingBreaks := 0
	pendingSpace := false
	for index := 0; index < len(value); {
		c := value[index]
		if c < utf8.RuneSelf {
			// ASCII fast path
			if c == '\r' && index+1 < len(value) && value[index+1] == '\n' {
				pendingSpace = false
				if output.Len() > 0 && pendingBreaks < 2 {
					pendingBreaks++
				}
				index += 2
				continue
			}
			if c == '\n' {
				pendingSpace = false
				if output.Len() > 0 && pendingBreaks < 2 {
					pendingBreaks++
				}
				index++
				continue
			}
			if isASCIIWhitespace(c) {
				if output.Len() > 0 && pendingBreaks == 0 {
					pendingSpace = true
				}
				index++
				continue
			}
			for pendingBreaks > 0 {
				output.WriteByte('\n')
				pendingBreaks--
			}
			if pendingSpace {
				output.WriteByte(' ')
				pendingSpace = false
			}
			output.WriteByte(c)
			index++
			continue
		}
		// Multi-byte UTF-8
		character, size := utf8.DecodeRuneInString(value[index:])
		index += size
		if unicode.IsSpace(character) {
			if output.Len() > 0 && pendingBreaks == 0 {
				pendingSpace = true
			}
			continue
		}
		for pendingBreaks > 0 {
			output.WriteByte('\n')
			pendingBreaks--
		}
		if pendingSpace {
			output.WriteByte(' ')
			pendingSpace = false
		}
		output.WriteRune(character)
	}
	return output.String()
}

func collapseSearchText(value string) string {
	var output strings.Builder
	output.Grow(len(value))
	pendingSpace := false
	for i := 0; i < len(value); {
		c := value[i]
		if c < utf8.RuneSelf {
			if isASCIIWhitespace(c) {
				pendingSpace = output.Len() > 0
				i++
				continue
			}
			if pendingSpace {
				output.WriteByte(' ')
				pendingSpace = false
			}
			output.WriteByte(c)
			i++
			continue
		}
		character, size := utf8.DecodeRuneInString(value[i:])
		if unicode.IsSpace(character) {
			pendingSpace = output.Len() > 0
		} else {
			if pendingSpace {
				output.WriteByte(' ')
				pendingSpace = false
			}
			output.WriteRune(character)
		}
		i += size
	}
	return output.String()
}

func guessedMIMEType(name string) *string {
	mediaType := mime.TypeByExtension(strings.ToLower(filepathExtension(name)))
	if mediaType == "" {
		return nil
	}
	if baseType, _, err := mime.ParseMediaType(mediaType); err == nil {
		mediaType = baseType
	}
	return &mediaType
}

func filepathExtension(name string) string {
	index := strings.LastIndexByte(name, '.')
	if index < 0 {
		return ""
	}
	return name[index:]
}
