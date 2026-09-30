package mail

import (
	"context"
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
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

const (
	semanticMIMEFingerprintVersion = "mime-v1"
	maximumMIMEIdentityDepth       = 32
	maximumMIMEIdentityParts       = 512
)

type mimeIdentityAttachment struct {
	mediaType string
	name      string
	size      int64
	sha256    string
}

type mimeIdentity struct {
	plain           string
	html            string
	hasPlain        bool
	hasHTML         bool
	attachments     []mimeIdentityAttachment
	attachmentBytes int64
	partCount       int
	unexpectedPart  bool
}

func envelopeFingerprint(draft Draft, messageID string) string {
	hash := sha256.New()
	parts := []string{messageID, draft.AccountRef, draft.From, draft.Subject}
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

func draftMIMEFingerprint(draft Draft) (string, error) {
	identity := mimeIdentity{plain: normalizeMIMEText(draft.Body), hasPlain: true}
	if draft.BodyHTML != "" {
		identity.html = normalizeMIMEText(draft.BodyHTML)
		identity.hasHTML = true
	}
	identity.attachments = make([]mimeIdentityAttachment, 0, len(draft.Attachments))
	for _, attachment := range draft.Attachments {
		if attachment.Size < 0 || filepath.Base(attachment.Path) == "." || filepath.Base(attachment.Path) == "" {
			return "", &OperationError{Code: "send_identity_unverifiable", Message: "the draft has an invalid attachment identity"}
		}
		digest := strings.ToLower(strings.TrimSpace(attachment.SHA256))
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size {
			return "", &OperationError{Code: "send_identity_unverifiable", Message: "the draft has an invalid attachment fingerprint"}
		}
		identity.attachments = append(identity.attachments, mimeIdentityAttachment{
			mediaType: attachmentMIMEType(attachment.Path),
			name:      filepath.Base(attachment.Path),
			size:      attachment.Size,
			sha256:    digest,
		})
	}
	return hashMIMEIdentity(identity), nil
}

func attachmentMIMEType(path string) string {
	mediaType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if mediaType == "" {
		return "application/octet-stream"
	}
	baseType, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		return strings.ToLower(mediaType)
	}
	return strings.ToLower(baseType)
}

func rawMessageIdentities(source io.Reader) (string, string, error) {
	headers := &io.LimitedReader{R: source, N: MaximumRawSourceBytes}
	message, err := stdmail.ReadMessage(headers)
	if err != nil {
		return "", "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate is not a readable RFC 5322 message: %v", err), Err: err}
	}
	if headers.N == 0 {
		return "", "", &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate headers exceed the reconciliation limit"}
	}
	message.Body = io.MultiReader(message.Body, source)
	from, err := canonicalHeaderAddress(message.Header.Get("From"))
	if err != nil {
		return "", "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate has an invalid From header: %v", err)}
	}
	to, err := canonicalHeaderAddresses(message.Header.Get("To"))
	if err != nil {
		return "", "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate has an invalid To header: %v", err)}
	}
	cc, err := canonicalHeaderAddresses(message.Header.Get("Cc"))
	if err != nil {
		return "", "", &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate has an invalid Cc header: %v", err)}
	}
	identity, err := parseMIMEIdentity(message.Header, message.Body)
	if err != nil {
		return "", "", err
	}
	envelope := hashMessageIdentity([]string{
		strings.TrimSpace(message.Header.Get("Message-ID")),
		from,
		to,
		cc,
		decodeMessageHeader(message.Header.Get("Subject")),
		normalizeMessageBody(identity.plain),
	})
	return envelope, hashMIMEIdentity(identity), nil
}

func verifySentMessageIdentity(source io.Reader, draft Draft, messageID string, expectedMIMEFingerprint ...string) error {
	if len(expectedMIMEFingerprint) == 0 || strings.TrimSpace(expectedMIMEFingerprint[0]) == "" {
		return &OperationError{Code: "send_identity_unverifiable", Message: "the send claim has no versioned MIME fingerprint; the Sent candidate cannot be adopted safely"}
	}
	expectedDraftMIMEFingerprint, err := draftMIMEFingerprint(draft)
	if err != nil {
		return err
	}
	if expectedDraftMIMEFingerprint != expectedMIMEFingerprint[0] {
		return &OperationError{Code: "send_fingerprint_mismatch", Message: "the draft no longer matches the MIME content claimed for this send"}
	}
	actual, actualMIMEFingerprint, err := rawMessageIdentities(source)
	if err != nil {
		return err
	}
	if actual != messageIdentityFingerprint(draft, messageID) {
		return &OperationError{
			Code:    "send_identity_mismatch",
			Message: "the Sent candidate does not match the claimed Message-ID, sender, recipient roles, subject, or plain body",
		}
	}
	if actualMIMEFingerprint != expectedMIMEFingerprint[0] {
		return &OperationError{Code: "send_identity_mismatch", Message: "the Sent candidate MIME alternatives or attachments do not match the claimed content"}
	}
	return nil
}

func verifyFetchedSentIdentity(ctx context.Context, source io.ReadCloser, size int64, draft Draft, attempt SendAttempt) (resultErr error) {
	if source == nil {
		return &OperationError{Code: "send_identity_unreadable", Message: "the Sent FETCH returned no source"}
	}
	defer func() { resultErr = errors.Join(resultErr, source.Close()) }()
	if size < 0 || size > maximumAcceptedMessageSpoolBytes {
		return &OperationError{Code: "send_identity_unreadable", Message: "the Sent FETCH size exceeds the supported composed-message limit"}
	}
	bounded := &io.LimitedReader{R: contextReader{ctx: ctx, reader: source}, N: size + 1}
	if err := verifySentMessageIdentity(bounded, draft, attempt.MessageID, attempt.MIMEFingerprint); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, bounded); err != nil {
		return &OperationError{Code: "send_identity_unreadable", Message: "reading the complete Sent candidate failed", Err: err}
	}
	if bounded.N != 1 {
		return &OperationError{Code: "send_identity_unreadable", Message: "the Sent FETCH source length differs from its declared size"}
	}
	return ctx.Err()
}

func hashMIMEIdentity(identity mimeIdentity) string {
	parts := []string{semanticMIMEFingerprintVersion, "plain", strconv.FormatBool(identity.hasPlain), normalizeMIMEText(identity.plain), "html", strconv.FormatBool(identity.hasHTML), normalizeMIMEText(identity.html), "attachments", strconv.Itoa(len(identity.attachments))}
	for index, attachment := range identity.attachments {
		parts = append(parts, strconv.Itoa(index), attachment.mediaType, attachment.name, strconv.FormatInt(attachment.size, 10), strings.ToLower(attachment.sha256))
	}
	return hashMessageIdentity(parts)
}

func parseMIMEIdentity(header stdmail.Header, body io.Reader) (mimeIdentity, error) {
	identity := mimeIdentity{}
	if err := parseMIMEIdentityPart(header, body, &identity, 0); err != nil {
		return mimeIdentity{}, err
	}
	if !identity.hasPlain {
		return mimeIdentity{}, &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate has no text/plain body"}
	}
	if identity.unexpectedPart {
		return mimeIdentity{}, &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate contains an unsupported substantive MIME part"}
	}
	return identity, nil
}

func parseMIMEIdentityPart(header stdmail.Header, body io.Reader, identity *mimeIdentity, depth int) error {
	if depth > maximumMIMEIdentityDepth {
		return &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate MIME nesting exceeds the safety limit"}
	}
	mediaType, parameters, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		if strings.TrimSpace(header.Get("Content-Type")) == "" {
			mediaType = "text/plain"
			parameters = map[string]string{}
		} else {
			return &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate has invalid MIME metadata: %v", err)}
		}
	}
	mediaType = strings.ToLower(mediaType)
	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := parameters["boundary"]
		if boundary == "" {
			return &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate multipart body has no boundary"}
		}
		reader := multipart.NewReader(body, boundary)
		for {
			part, nextErr := reader.NextRawPart()
			if errors.Is(nextErr, io.EOF) {
				return nil
			}
			if nextErr != nil {
				return &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate multipart body is malformed: %v", nextErr), Err: nextErr}
			}
			if part == nil {
				return &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate multipart body has an empty part"}
			}
			if err := parseMIMEIdentityPart(stdmail.Header(part.Header), part, identity, depth+1); err != nil {
				return err
			}
		}
	}

	identity.partCount++
	if identity.partCount > maximumMIMEIdentityParts {
		return &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate contains too many MIME parts"}
	}
	disposition, dispositionParameters, dispositionErr := mime.ParseMediaType(header.Get("Content-Disposition"))
	if dispositionErr != nil && strings.TrimSpace(header.Get("Content-Disposition")) != "" {
		return &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate has invalid MIME disposition: %v", dispositionErr)}
	}
	name := dispositionParameters["filename"]
	if name == "" {
		name = parameters["name"]
	}
	if strings.EqualFold(disposition, "attachment") || name != "" {
		if len(identity.attachments) >= MaximumDraftAttachments {
			return &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate exceeds the supported attachment count"}
		}
		remaining := MaximumDraftAttachmentBytes - identity.attachmentBytes
		digest := sha256.New()
		size, readErr := io.Copy(digest, io.LimitReader(decodedMIMEIdentityPart(header, body), remaining+1))
		if readErr != nil {
			return &OperationError{Code: "send_identity_unreadable", Message: "reading the Sent candidate attachment failed", Err: readErr}
		}
		if size > remaining {
			return &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate exceeds the supported aggregate attachment bytes"}
		}
		identity.attachmentBytes += size
		identity.attachments = append(identity.attachments, mimeIdentityAttachment{
			mediaType: mediaType, name: name, size: size, sha256: hex.EncodeToString(digest.Sum(nil)),
		})
		return nil
	}
	switch mediaType {
	case "text/plain", "text/html":
		data, readErr := readMIMEIdentityPart(header, body, MaximumComposeBodyBytes)
		if readErr != nil {
			return readErr
		}
		value := normalizeMIMEText(string(data))
		if mediaType == "text/plain" {
			if identity.hasPlain {
				return &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate contains multiple text/plain parts"}
			}
			identity.plain, identity.hasPlain = value, true
		} else {
			if identity.hasHTML {
				return &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate contains multiple text/html parts"}
			}
			identity.html, identity.hasHTML = value, true
		}
		return nil
	default:
		identity.unexpectedPart = true
		return &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate contains a non-text MIME part without an attachment identity"}
	}
}

func decodedMIMEIdentityPart(header stdmail.Header, body io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(header.Get("Content-Transfer-Encoding"))) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		return quotedprintable.NewReader(body)
	}
	return body
}

func readMIMEIdentityPart(header stdmail.Header, body io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(decodedMIMEIdentityPart(header, body), maximum+1))
	if err != nil {
		return nil, &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("reading the Sent candidate MIME part failed: %v", err), Err: err}
	}
	if int64(len(data)) > maximum {
		return nil, &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate MIME part exceeds the reconciliation limit"}
	}
	_, parameters, parseErr := mime.ParseMediaType(header.Get("Content-Type"))
	if parseErr == nil {
		charset := strings.TrimSpace(parameters["charset"])
		if charset != "" && !strings.EqualFold(charset, "utf-8") && !strings.EqualFold(charset, "utf8") {
			data, codecErr := decodeMIMECharset(data, charset)
			if codecErr != nil {
				return nil, &OperationError{Code: "send_identity_unreadable", Message: fmt.Sprintf("the Sent candidate uses unsupported charset %q", charset)}
			}
			return validateMIMEText(data)
		}
	}
	return validateMIMEText(data)
}

func decodeMIMECharset(data []byte, charset string) ([]byte, error) {
	var decoder interface {
		Bytes([]byte) ([]byte, error)
	}
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1":
		decoder = charmap.ISO8859_1.NewDecoder()
	case "windows-1252", "cp1252":
		decoder = charmap.Windows1252.NewDecoder()
	default:
		return nil, fmt.Errorf("unsupported charset")
	}
	return decoder.Bytes(data)
}

func validateMIMEText(data []byte) ([]byte, error) {
	if !utf8.Valid(data) {
		return nil, &OperationError{Code: "send_identity_unreadable", Message: "the Sent candidate text part is not valid UTF-8"}
	}
	return data, nil
}

func normalizeMIMEText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.Trim(value, "\n")
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
