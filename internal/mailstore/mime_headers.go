package mailstore

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	stdmail "net/mail"
	"strings"

	"github.com/emersion/go-message"
	messageMail "github.com/emersion/go-message/mail"
	"mailcli/internal/mail"
)

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

func readRawHeaders(reader io.Reader) (string, error) {
	buffered := bufio.NewReaderSize(io.LimitReader(reader, int64(maximumHeaderBytes)+1), mimeHeaderReaderBuffer)
	return readRawHeaderBlock(buffered)
}

func readRawHeaderBlock(buffered *bufio.Reader) (string, error) {
	var output strings.Builder
	output.Grow(mimeHeaderInitialBytes)
	lineLength := 0
	var first byte
	for output.Len() <= maximumHeaderBytes {
		fragment, err := buffered.ReadSlice('\n')
		if len(fragment) > maximumHeaderBytes-output.Len() {
			return "", operationError("invalid_message_source", "RFC message headers exceed the safety limit")
		}
		if lineLength == 0 && len(fragment) > 0 {
			first = fragment[0]
		}
		lineLength += len(fragment)
		// Grow explicitly so long physical lines do not cause repeated
		// small append growth. Copy the borrowed fragment before any refill.
		if lineLength >= mimeHeaderReaderBuffer {
			output.Grow(len(fragment))
		}
		output.Write(fragment)
		if lineLength == 1 && first == '\n' ||
			lineLength == 2 && first == '\r' && len(fragment) > 0 && fragment[len(fragment)-1] == '\n' {
			return output.String(), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", operationError("invalid_message_source", "RFC message has no header boundary")
			}
			return "", fmt.Errorf("read RFC message headers: %w", err)
		}
		lineLength = 0
	}
	return "", operationError("invalid_message_source", "RFC message headers exceed the safety limit")
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
		recipients = append(recipients, mail.Recipient{Name: address.Name, Address: mail.MailboxAddrSpec(address.Address)})
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
