package smtpclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"unicode/utf8"

	"mailcli/internal/transport"
)

const (
	smtpHeaderBudget = 8 << 20
	smtpPartLimit    = 4096
	smtpDepthLimit   = 64
)

func submissionRequiresUTF8(ctx context.Context, from string, recipients []string, source io.ReadSeeker, size int64) (bool, error) {
	required, err := envelopeRequiresUTF8(from)
	if err != nil {
		return false, err
	}
	for _, recipient := range recipients {
		international, err := envelopeRequiresUTF8(recipient)
		if err != nil {
			return false, err
		}
		required = required || international
	}
	if required {
		return true, nil
	}
	position, err := source.Seek(0, io.SeekCurrent)
	if err != nil {
		return false, &transport.TransportError{Code: transport.CodeSMTPRejected, Message: "inspect message source position", Err: err}
	}
	scan := smtpHeaderScan{remaining: smtpHeaderBudget}
	required, scanErr := scan.message(smtpScanReader{ctx: ctx, reader: io.LimitReader(source, size)}, 0)
	rewound, rewindErr := source.Seek(position, io.SeekStart)
	if rewindErr == nil && rewound != position {
		rewindErr = errors.New("message source did not restore its original position")
	}
	if err := errors.Join(scanErr, rewindErr); err != nil {
		if ctx.Err() != nil {
			return false, timeoutError(ctx, "message preflight")
		}
		return false, &transport.TransportError{Code: transport.CodeSMTPRejected, Message: "inspect and rewind message MIME headers", Err: err}
	}
	return required, nil
}

func envelopeRequiresUTF8(value string) (bool, error) {
	invalid := &transport.TransportError{Code: transport.CodeInvalidAddress, Message: "SMTP envelope requires a valid mailbox addr-spec"}
	if !utf8.ValidString(value) {
		return false, invalid
	}
	quoted, escaped, international := false, false, false
	for _, character := range value {
		if character < ' ' || character == 0x7f {
			return false, invalid
		}
		international = international || character > 0x7f
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' && quoted {
			escaped = true
			continue
		}
		if character == '"' {
			quoted = !quoted
		}
		if !quoted && strings.ContainsRune(" ()<>", character) {
			return false, invalid
		}
	}
	if _, err := mail.ParseAddress("<" + value + ">"); err != nil {
		return false, invalid
	}
	return international, nil
}

type smtpHeaderScan struct {
	remaining int
	parts     int
}

// Limit root/embedded header reads before allocation, retaining bufio read-ahead
// for the body. Multipart bounds each child header internally to 10 MiB.
func (s *smtpHeaderScan) message(source io.Reader, depth int) (bool, error) {
	if depth > smtpDepthLimit {
		return false, errors.New("SMTP MIME nesting exceeds limit")
	}
	limited := &io.LimitedReader{R: source, N: int64(s.remaining) + 1}
	reader := bufio.NewReader(limited)
	header, err := textproto.NewReader(reader).ReadMIMEHeader()
	if err != nil {
		return false, fmt.Errorf("read MIME headers: %w", err)
	}
	limited.N = math.MaxInt64
	return s.entity(header, reader, "text/plain", depth)
}

func (s *smtpHeaderScan) entity(header textproto.MIMEHeader, body io.Reader, defaultType string, depth int) (bool, error) {
	s.parts++
	if s.parts > smtpPartLimit || depth > smtpDepthLimit {
		return false, errors.New("SMTP MIME structure exceeds limit")
	}
	if required, err := s.headers(header); required || err != nil {
		return required, err
	}
	mediaType := defaultType
	var parameters map[string]string
	if contentType := header.Get("Content-Type"); contentType != "" {
		var err error
		mediaType, parameters, err = mime.ParseMediaType(contentType)
		if err != nil {
			return false, fmt.Errorf("parse MIME content type: %w", err)
		}
	}
	encoding := strings.ToLower(strings.TrimSpace(header.Get("Content-Transfer-Encoding")))
	if strings.HasPrefix(mediaType, "multipart/") {
		if encoding != "" && encoding != "7bit" && encoding != "8bit" && encoding != "binary" {
			return false, errors.New("invalid multipart transfer encoding")
		}
		return s.multipart(body, mediaType, parameters["boundary"], depth)
	}
	// Encoded embedded messages are opaque on the SMTP wire (RFC 6532 section 3.7).
	if encoding == "base64" || encoding == "quoted-printable" {
		return false, nil
	}
	if mediaType == "message/rfc822" || mediaType == "message/global" {
		return s.message(body, depth+1)
	}
	return false, nil
}

func (s *smtpHeaderScan) headers(header textproto.MIMEHeader) (bool, error) {
	s.remaining -= 400
	required := false
	for name, values := range header {
		s.remaining -= len(name) + 200
		for _, value := range values {
			s.remaining -= len(value) + 16
			if !utf8.ValidString(value) {
				return false, errors.New("MIME header contains invalid UTF-8")
			}
			for index := range len(value) {
				required = required || value[index] > 0x7f
			}
		}
	}
	if s.remaining < 0 {
		return false, errors.New("SMTP MIME header budget exceeded")
	}
	return required, nil
}

func (s *smtpHeaderScan) multipart(body io.Reader, mediaType, boundary string, depth int) (bool, error) {
	if boundary == "" || len(boundary) > 70 {
		return false, errors.New("invalid MIME boundary")
	}
	childType := "text/plain"
	if mediaType == "multipart/digest" {
		childType = "message/rfc822"
	}
	reader := multipart.NewReader(body, boundary)
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read MIME part: %w", err)
		}
		if required, err := s.entity(part.Header, part, childType, depth+1); required || err != nil {
			return required, err
		}
	}
}

type smtpScanReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r smtpScanReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

// net/smtp.Mail adds SMTPUTF8 whenever advertised, even for ASCII messages.
// Use the same protocol connection to request it only for messages needing it.
func sendMailCommand(client *smtp.Client, from string, requiresUTF8, eightBitSupported bool) error {
	command := "MAIL FROM:<%s>"
	if eightBitSupported {
		command += " BODY=8BITMIME"
	}
	if requiresUTF8 {
		command += " SMTPUTF8"
	}
	id, err := client.Text.Cmd(command, from)
	if err != nil {
		return err
	}
	client.Text.StartResponse(id)
	_, _, err = client.Text.ReadResponse(250)
	client.Text.EndResponse(id)
	return err
}
