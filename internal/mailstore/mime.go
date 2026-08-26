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

func parseMIMEDocument(reader io.Reader, partial bool, hashAttachments bool) (mimeDocument, error) {
	entity, readErr := message.Read(reader)
	if entity == nil || (readErr != nil && !message.IsUnknownCharset(readErr) && !message.IsUnknownEncoding(readErr)) {
		return mimeDocument{}, operationError("invalid_message_source", fmt.Sprintf("parse RFC message: %v", readErr))
	}
	document := mimeDocument{Complete: readErr == nil && !partial, Parts: make(map[string]mimePart)}
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
		size, digest, err := consumeMIMEAttachment(entity.Body, hashAttachments)
		complete := err == nil && !missingAppleContent(
			entity.Header.Get("X-Apple-Content-Length"), size, true,
		)
		document.Parts[partID] = mimePart{
			ID: partID, Name: filename, MIMEType: mediaType, Size: size,
			SHA256: digest, Complete: complete,
		}
		return mimeTextRepresentation{}, nil
	}
	if mediaType != "text/plain" && mediaType != "text/html" {
		_, err := io.Copy(io.Discard, entity.Body)
		if err != nil {
			document.Complete = false
		}
		return mimeTextRepresentation{}, err
	}
	body, truncated, err := readBoundedPart(entity.Body, maximumTextPartBytes)
	if err != nil || truncated {
		document.Complete = false
	}
	if missingAppleContent(entity.Header.Get("X-Apple-Content-Length"), int64(len(body)), partial) {
		document.Complete = false
		document.MissingParts = append(document.MissingParts, partID)
		return mimeTextRepresentation{}, nil
	}
	text := strings.TrimSpace(string(body))
	rank := mimeTextPlain
	if mediaType == "text/html" {
		text = strings.TrimSpace(htmlToText(body))
		rank = mimeTextHTML
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
		childPath := append(append([]int(nil), path...), index)
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
	var parts []string
	rank := mimeTextNone
	for _, child := range children {
		if child.Text != "" {
			parts = append(parts, child.Text)
		}
		if child.Rank > rank {
			rank = child.Rank
		}
	}
	return mimeTextRepresentation{Text: strings.Join(parts, "\n\n"), Rank: rank}
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
	for output.Len() <= maximumHeaderBytes {
		line, err := buffered.ReadString('\n')
		output.WriteString(line)
		if line == "\n" || line == "\r\n" {
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

func readBoundedPart(reader io.Reader, maximum int64) ([]byte, bool, error) {
	limited := io.LimitReader(reader, maximum+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return body, false, err
	}
	truncated := int64(len(body)) > maximum
	if truncated {
		body = body[:maximum]
	}
	_, drainErr := io.Copy(io.Discard, reader)
	if err == nil {
		err = drainErr
	}
	return body, truncated, err
}

func missingAppleContent(value string, available int64, partial bool) bool {
	if !partial || strings.TrimSpace(value) == "" {
		return false
	}
	expected, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return err == nil && expected > available
}

func mimePartID(path []int) string {
	if len(path) == 0 {
		return "1"
	}
	values := make([]string, len(path))
	for index, component := range path {
		values[index] = strconv.Itoa(component + 1)
	}
	return strings.Join(values, ".")
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
	for len(value) > 0 {
		character, size := utf8.DecodeRune(value)
		if !unicode.IsSpace(character) {
			return true
		}
		value = value[size:]
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
		character, size := utf8.DecodeRuneInString(value[index:])
		if character == '\r' && index+size < len(value) && value[index+size] == '\n' {
			character = '\n'
			size++
		}
		index += size
		if character == '\n' {
			pendingSpace = false
			if output.Len() > 0 && pendingBreaks < 2 {
				pendingBreaks++
			}
			continue
		}
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
	for _, character := range value {
		if unicode.IsSpace(character) {
			pendingSpace = output.Len() > 0
			continue
		}
		if pendingSpace {
			output.WriteByte(' ')
			pendingSpace = false
		}
		output.WriteRune(character)
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
