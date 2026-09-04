package mailstore

import (
	"bufio"
	"bytes"
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
	"golang.org/x/net/html"

	"mailcli/internal/mail"
)

const (
	maximumHeaderBytes   = 1024 * 1024
	maximumTextPartBytes = 16 * 1024 * 1024
)

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
	entity, readErr := message.Read(reader)
	if entity == nil || (readErr != nil && !message.IsUnknownCharset(readErr) && !message.IsUnknownEncoding(readErr)) {
		return mimeDocument{}, operationError("invalid_message_source", fmt.Sprintf("parse RFC message: %v", readErr))
	}
	document := mimeDocument{Complete: readErr == nil && !partial, skipNonTextBodies: skipNonTextBodies}
	if readErr != nil {
		document.MissingParts = append(document.MissingParts, "mime-decoding")
	}
	header := messageMail.Header{Header: entity.Header}
	if messageID, err := header.MessageID(); err == nil {
		document.MessageID = messageID
	}
	var replyToComplete bool
	document.ReplyTo, replyToComplete = firstFormattedAddress(&header, "Reply-To")
	if !replyToComplete {
		document.Complete = false
		document.MissingParts = append(document.MissingParts, "header:reply-to")
	}
	document.To = documentRecipients(&document, &header, "To")
	document.CC = documentRecipients(&document, &header, "Cc")
	document.BCC = documentRecipients(&document, &header, "Bcc")
	representation, walkErr := parseMIMEEntity(
		entity, nil, readErr, partial, hashAttachments, &document,
	)
	if walkErr != nil {
		document.Complete = false
		document.MissingParts = append(document.MissingParts, "mime-structure")
	}
	document.Content = representation.Text
	return document, nil
}

// messageIDFromSource reads only the header block of a raw RFC 5322 message
// and resolves the Message-ID header. Full MIME parsing would stream and drain
// every attachment body just to read this one header, so mutation targeting
// uses this instead. A missing Message-ID header yields "" (same semantics as
// the previous full-parse path).
func messageIDFromSource(reader io.Reader) (string, error) {
	headers, err := readRawHeaders(reader)
	if err != nil {
		return "", err
	}
	entity, readErr := message.Read(strings.NewReader(headers))
	if entity == nil || (readErr != nil && !message.IsUnknownCharset(readErr) && !message.IsUnknownEncoding(readErr)) {
		return "", operationError("invalid_message_source", fmt.Sprintf("parse RFC headers: %v", readErr))
	}
	header := messageMail.Header{Header: entity.Header}
	messageID, err := header.MessageID()
	if err != nil {
		return "", nil
	}
	return messageID, nil
}

func parseMIMEEntity(
	entity *message.Entity,
	path []int,
	partErr error,
	partial bool,
	hashAttachments bool,
	document *mimeDocument,
) (mimeTextRepresentation, error) {
	if partErr != nil {
		document.Complete = false
		document.MissingParts = append(document.MissingParts, mimePartID(path))
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
		size, digest, err := consumeMIMEAttachment(entity.Body, hashAttachments)
		complete := err == nil && !missingAppleContent(
			entity.Header.Get("X-Apple-Content-Length"), size, true,
		)
		if document.Parts == nil {
			document.Parts = make(map[string]mimePart)
		}
		document.Parts[partID] = mimePart{
			ID: partID, Name: filename, MIMEType: mediaType, Size: size,
			SHA256: digest, Complete: complete,
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
		_, err := io.Copy(io.Discard, entity.Body)
		if err != nil {
			document.Complete = false
		}
		return mimeTextRepresentation{}, err
	}
	appleLength, hasAppleLength := parseAppleContentLength(entity.Header.Get("X-Apple-Content-Length"))
	body, truncated, err := readBoundedPart(entity.Body, maximumTextPartBytes, appleLength, hasAppleLength)
	if err != nil || truncated {
		document.Complete = false
	}
	if hasAppleLength && partial && appleLength > int64(len(body)) {
		document.Complete = false
		document.MissingParts = append(document.MissingParts, partID)
		return mimeTextRepresentation{}, nil
	}
	rank := mimeTextPlain
	var text string
	if mediaType == "text/html" {
		text = strings.TrimSpace(htmlToText(body))
		rank = mimeTextHTML
	} else {
		text = strings.TrimSpace(string(body))
	}
	if text == "" {
		return mimeTextRepresentation{}, err
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
		child, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if child == nil {
			return combineMIMEText(mediaType, children), err
		}
		if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
			return combineMIMEText(mediaType, children), err
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

func consumeMIMEAttachment(reader io.Reader, withHash bool) (int64, string, error) {
	if !withHash {
		size, err := io.Copy(io.Discard, reader)
		return size, "", err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, reader)
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

func readBoundedPart(reader io.Reader, maximum int64, sizeHint int64, hasSizeHint bool) ([]byte, bool, error) {
	limited := io.LimitReader(reader, maximum+1)
	// Pre-size from the decoded-length hint instead of pre-allocating
	// maximum+1 bytes (which can be 16 MiB per text part). io.Copy uses a
	// 32 KiB buffer internally; the bytes.Buffer grows only past the hint.
	var buf bytes.Buffer
	if hasSizeHint && sizeHint > 0 {
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
	_, drainErr := io.Copy(io.Discard, reader)
	if drainErr != nil {
		return body, truncated, drainErr
	}
	return body, truncated, nil
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

func documentRecipients(document *mimeDocument, header *messageMail.Header, key string) []mail.Recipient {
	recipients, complete := headerRecipients(header, key)
	if !complete {
		document.Complete = false
		document.MissingParts = append(document.MissingParts, "header:"+strings.ToLower(key))
	}
	return recipients
}

func headerRecipients(header *messageMail.Header, key string) ([]mail.Recipient, bool) {
	if strings.TrimSpace(header.Get(key)) == "" {
		return []mail.Recipient{}, true
	}
	addresses, err := header.AddressList(key)
	if err != nil {
		return []mail.Recipient{}, false
	}
	recipients := make([]mail.Recipient, 0, len(addresses))
	for _, address := range addresses {
		recipients = append(recipients, mail.Recipient{Name: address.Name, Address: address.Address})
	}
	return recipients, true
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
	tokenizer := html.NewTokenizer(bytes.NewReader(source))
	var output strings.Builder
	output.Grow(len(source) / 2)
	skipDepth := 0
	breakPending := false
	for {
		switch tokenType := tokenizer.Next(); tokenType {
		case html.ErrorToken:
			return normalizeTextLayout(output.String())
		case html.StartTagToken, html.SelfClosingTagToken:
			name := htmlTagName(tokenizer.Raw())
			skipped := isHTMLTag(name, "script") || isHTMLTag(name, "style")
			if skipped && tokenType == html.StartTagToken {
				skipDepth++
			}
			if skipDepth == 0 && isBlockTag(name) && output.Len() > 0 {
				breakPending = true
			}
		case html.EndTagToken:
			name := htmlTagName(tokenizer.Raw())
			if isHTMLTag(name, "script") || isHTMLTag(name, "style") {
				if skipDepth > 0 {
					skipDepth--
				}
				continue
			}
			if skipDepth == 0 && isBlockTag(name) && output.Len() > 0 {
				breakPending = true
			}
		case html.TextToken:
			if skipDepth == 0 {
				text := tokenizer.Text()
				if breakPending && hasVisibleText(text) {
					output.WriteByte('\n')
					breakPending = false
				}
				if !breakPending {
					output.Write(text)
				}
			}
		}
	}
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

func htmlTagName(raw []byte) []byte {
	if len(raw) < 3 || raw[0] != '<' {
		return nil
	}
	index := 1
	if raw[index] == '/' {
		index++
	}
	for index < len(raw) && isASCIIWhitespace(raw[index]) {
		index++
	}
	start := index
	for index < len(raw) && !isASCIIWhitespace(raw[index]) && raw[index] != '/' && raw[index] != '>' {
		index++
	}
	return raw[start:index]
}

func isBlockTag(name []byte) bool {
	return isHTMLTag(name, "br") || isHTMLTag(name, "div") ||
		isHTMLTag(name, "h1") || isHTMLTag(name, "h2") || isHTMLTag(name, "h3") ||
		isHTMLTag(name, "h4") || isHTMLTag(name, "h5") || isHTMLTag(name, "h6") ||
		isHTMLTag(name, "li") || isHTMLTag(name, "p") || isHTMLTag(name, "tr")
}

func isHTMLTag(name []byte, target string) bool {
	if len(name) != len(target) {
		return false
	}
	for index, character := range name {
		if character >= 'A' && character <= 'Z' {
			character += 'a' - 'A'
		}
		if character != target[index] {
			return false
		}
	}
	return true
}

func isASCIIWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n' || value == '\r' || value == '\f'
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
