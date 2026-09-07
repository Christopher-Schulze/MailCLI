package mail

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	stdmail "net/mail"
	"strings"
)

func envelopeFingerprint(draft Draft, messageID string) string {
	hash := sha256.New()
	parts := []string{messageID, draft.From, draft.Subject}
	for _, recipient := range draft.To {
		parts = append(parts, recipient.Address)
	}
	for _, recipient := range draft.CC {
		parts = append(parts, recipient.Address)
	}
	for _, recipient := range draft.BCC {
		parts = append(parts, recipient.Address)
	}
	parts = append(parts, strings.ReplaceAll(draft.Body, "\r\n", "\n"))
	for _, part := range parts {
		hash.Write([]byte(part))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
func messageIdentityFingerprint(draft Draft, messageID string) string {
	parts := []string{
		strings.TrimSpace(messageID),
		canonicalDraftAddress(draft.From),
		canonicalDraftAddresses(draft.To),
		canonicalDraftAddresses(draft.CC),
		strings.TrimSpace(draft.Subject),
		normalizeMessageBody(draft.Body),
	}
	return hashMessageIdentity(parts)
}

func rawMessageIdentityFingerprint(raw []byte) (string, error) {
	message, err := stdmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate is not a readable RFC 5322 message: %v", err)}
	}
	from, err := canonicalHeaderAddress(message.Header.Get("From"))
	if err != nil {
		return "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate has an invalid From header: %v", err)}
	}
	to, err := canonicalHeaderAddresses(message.Header.Get("To"))
	if err != nil {
		return "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate has an invalid To header: %v", err)}
	}
	cc, err := canonicalHeaderAddresses(message.Header.Get("Cc"))
	if err != nil {
		return "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate has an invalid Cc header: %v", err)}
	}
	body, err := readPlainMessageBody(message.Header, message.Body)
	if err != nil {
		return "", err
	}
	return hashMessageIdentity([]string{
		strings.TrimSpace(message.Header.Get("Message-ID")),
		from,
		to,
		cc,
		decodeMessageHeader(message.Header.Get("Subject")),
		normalizeMessageBody(body),
	}), nil
}

func verifySentMessageIdentity(raw []byte, draft Draft, messageID string) error {
	actual, err := rawMessageIdentityFingerprint(raw)
	if err != nil {
		return err
	}
	if actual != messageIdentityFingerprint(draft, messageID) {
		return &OperationError{
			Code:    "send_identity_mismatch",
			Message: "the Sent candidate has the claimed Message-ID but does not match the draft envelope and body",
		}
	}
	return nil
}

func canonicalDraftAddress(value string) string {
	parsed, err := stdmail.ParseAddress(strings.TrimSpace(value))
	if err != nil {
		return strings.ToLower(strings.TrimSpace(value))
	}
	return strings.ToLower(parsed.Address)
}

func canonicalDraftAddresses(values []Recipient) string {
	addresses := make([]string, 0, len(values))
	for _, value := range values {
		addresses = append(addresses, canonicalDraftAddress(value.Address))
	}
	return strings.Join(addresses, "\x00")
}

func canonicalHeaderAddress(value string) (string, error) {
	parsed, err := stdmail.ParseAddress(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	return strings.ToLower(parsed.Address), nil
}

func canonicalHeaderAddresses(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	parsed, err := stdmail.ParseAddressList(value)
	if err != nil {
		return "", err
	}
	addresses := make([]string, 0, len(parsed))
	for _, address := range parsed {
		addresses = append(addresses, strings.ToLower(address.Address))
	}
	return strings.Join(addresses, "\x00"), nil
}

func hashMessageIdentity(parts []string) string {
	hash := sha256.New()
	for _, part := range parts {
		hash.Write([]byte(part))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func decodeMessageHeader(value string) string {
	decoded, err := (&mime.WordDecoder{}).DecodeHeader(value)
	if err != nil {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(decoded)
}

func normalizeMessageBody(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
}

func readPlainMessageBody(header stdmail.Header, body io.Reader) (string, error) {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		return readDecodedMessagePart(header, body)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return "", &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate multipart body has no boundary"}
	}
	reader := multipart.NewReader(body, boundary)
	for {
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate multipart body is malformed: %v", err)}
		}
		partHeader := stdmail.Header(part.Header)
		partType, partParams, parseErr := mime.ParseMediaType(partHeader.Get("Content-Type"))
		if parseErr == nil && strings.HasPrefix(strings.ToLower(partType), "multipart/") {
			nestedBoundary := partParams["boundary"]
			if nestedBoundary == "" {
				continue
			}
			nested, nestedErr := readPlainMessageBody(partHeader, part)
			if nestedErr == nil {
				return nested, nil
			}
			continue
		}
		if parseErr == nil && strings.EqualFold(partType, "text/plain") {
			return readDecodedMessagePart(partHeader, part)
		}
	}
	return "", &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate has no text/plain body"}
}

func readDecodedMessagePart(header stdmail.Header, body io.Reader) (string, error) {
	limited := io.LimitReader(body, maximumDraftStateBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("reading the Sent candidate body failed: %v", err)}
	}
	if int64(len(data)) > maximumDraftStateBytes {
		return "", &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate body exceeds the reconciliation limit"}
	}
	encoding := strings.ToLower(strings.TrimSpace(header.Get("Content-Transfer-Encoding")))
	switch encoding {
	case "base64":
		decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(data)))
		if err != nil {
			return "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate body has invalid base64: %v", err)}
		}
		return string(decoded), nil
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(data)))
		if err != nil {
			return "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate body has invalid quoted-printable data: %v", err)}
		}
		return string(decoded), nil
	default:
		return string(data), nil
	}
}
