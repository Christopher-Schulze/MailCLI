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

func parseMIMEDocument(reader io.Reader, partial bool, hashAttachments bool) (mimeDocument, error) {
	entity, readErr := message.Read(reader)
	if entity == nil || (readErr != nil && !message.IsUnknownCharset(readErr) && !message.IsUnknownEncoding(readErr)) {
		return mimeDocument{}, operationError("invalid_message_source", fmt.Sprintf("parse RFC message: %v", readErr))
	}
	document := mimeDocument{Complete: readErr == nil && !partial, Parts: make(map[string]mimePart)}
	header := messageMail.Header{Header: entity.Header}
	document.MessageID, _ = header.MessageID()
	document.ReplyTo = firstFormattedAddress(&header, "Reply-To")
	document.To = headerRecipients(&header, "To")
	document.CC = headerRecipients(&header, "Cc")
	document.BCC = headerRecipients(&header, "Bcc")
	var plainParts []string
	var htmlParts []string
	walkErr := entity.Walk(func(path []int, part *message.Entity, partErr error) error {
		if partErr != nil {
			document.Complete = false
		}
		mediaType, parameters, contentTypeErr := part.Header.ContentType()
		if contentTypeErr != nil {
			mediaType = "application/octet-stream"
			document.Complete = false
		}
		mediaType = strings.ToLower(mediaType)
		if strings.HasPrefix(mediaType, "multipart/") {
			return nil
		}
		disposition, dispositionParameters, dispositionErr := part.Header.ContentDisposition()
		if dispositionErr != nil && part.Header.Get("Content-Disposition") != "" {
			document.Complete = false
		}
		filename := dispositionParameters["filename"]
		if filename == "" {
			filename = parameters["name"]
		}
		partID := mimePartID(path)
		attachment := strings.EqualFold(disposition, "attachment") || filename != ""
		if attachment {
			size, digest, err := consumeMIMEAttachment(part.Body, hashAttachments)
			complete := err == nil && !missingAppleContent(
				part.Header.Get("X-Apple-Content-Length"), size, true,
			)
			document.Parts[partID] = mimePart{
				ID: partID, Name: filename, MIMEType: mediaType, Size: size,
				SHA256: digest, Complete: complete,
			}
			return nil
		}
		if mediaType != "text/plain" && mediaType != "text/html" {
			if _, err := io.Copy(io.Discard, part.Body); err != nil {
				document.Complete = false
			}
			return nil
		}
		body, truncated, err := readBoundedPart(part.Body, maximumTextPartBytes)
		if err != nil || truncated {
			document.Complete = false
		}
		if missingAppleContent(part.Header.Get("X-Apple-Content-Length"), int64(len(body)), partial) {
			document.Complete = false
			document.MissingParts = append(document.MissingParts, partID)
			return nil
		}
		if mediaType == "text/html" {
			htmlParts = appendNonEmptyText(htmlParts, htmlToText(body))
		} else {
			plainParts = appendNonEmptyText(plainParts, string(body))
		}
		return nil
	})
	if walkErr != nil {
		document.Complete = false
	}
	if len(plainParts) > 0 {
		document.Content = strings.Join(plainParts, "\n\n")
	} else {
		document.Content = strings.Join(htmlParts, "\n\n")
	}
	return document, nil
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

func headerRecipients(header *messageMail.Header, key string) []mail.Recipient {
	addresses, err := header.AddressList(key)
	if err != nil {
		return []mail.Recipient{}
	}
	recipients := make([]mail.Recipient, 0, len(addresses))
	for _, address := range addresses {
		recipients = append(recipients, mail.Recipient{Name: address.Name, Address: address.Address})
	}
	return recipients
}

func firstFormattedAddress(header *messageMail.Header, key string) string {
	addresses, err := header.AddressList(key)
	if err != nil || len(addresses) == 0 {
		return ""
	}
	return (&stdmail.Address{Name: addresses[0].Name, Address: addresses[0].Address}).String()
}

func appendNonEmptyText(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	return append(values, value)
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
