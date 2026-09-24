package imapclient

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"mailcli/internal/transport"
)

// CheckStatus queries server message counts, unseen count, and UIDs via IMAP STATUS.
func (c *Client) CheckStatus(ctx context.Context, cfg transport.ImapConfig, mailbox string) (transport.MailboxStatus, error) {
	ps, release, err := c.acquireIndependent(ctx, cfg)
	if err != nil {
		return transport.MailboxStatus{Mailbox: mailbox}, err
	}
	defer release()
	return c.doStatus(ctx, ps.sess, ps.sess.nextTag(), mailbox)
}

func (c *Client) doStatus(ctx context.Context, sess *session, tag, mailbox string) (transport.MailboxStatus, error) {
	var status transport.MailboxStatus
	status.Mailbox = mailbox

	quotedMailbox, err := safeQuoteIMAP(mailbox)
	if err != nil {
		return status, err
	}
	cmd := fmt.Sprintf("%s STATUS %s (MESSAGES UNSEEN UIDNEXT UIDVALIDITY)", tag, quotedMailbox)
	if err := c.setDeadline(ctx, sess); err != nil {
		return status, wrapCommandIOError(ctx, err, "IMAP STATUS deadline")
	}
	if err := c.writeLine(sess, cmd); err != nil {
		return status, wrapCommandIOError(ctx, err, "IMAP STATUS write")
	}

	seenStatus := false
	for {
		line, literals, err := c.readLineWithLiteral(sess)
		if err != nil {
			var malformed *malformedResponseError
			if errors.As(err, &malformed) {
				return status, malformedStatusResponse(sess, malformed)
			}
			return status, wrapCommandIOError(ctx, err, "IMAP STATUS read")
		}
		if strings.HasPrefix(line, tag+" ") {
			st, statusErr := parseTaggedCompletionStatus(line, tag)
			if statusErr != nil {
				return status, malformedStatusResponse(sess, statusErr)
			}
			if st == "OK" {
				if !seenStatus {
					return status, malformedStatusResponse(sess, errors.New("STATUS completed without a STATUS response"))
				}
				return status, nil
			}
			return status, &transport.TransportError{
				Code:    transport.CodeIMAPMailboxNotFound,
				Message: "IMAP STATUS failed: " + st,
			}
		}
		if strings.HasPrefix(line, "* STATUS ") {
			if seenStatus {
				return status, malformedStatusResponse(sess, errors.New("multiple STATUS responses"))
			}
			parsed, parseErr := parseStatusLine(line, literals)
			if parseErr != nil {
				return status, malformedStatusResponse(sess, parseErr)
			}
			if parsed.Mailbox != mailbox {
				return status, malformedStatusResponse(
					sess, fmt.Errorf("STATUS mailbox %q does not match requested mailbox %q", parsed.Mailbox, mailbox),
				)
			}
			status = parsed
			seenStatus = true
		}
	}
}

func malformedStatusResponse(sess *session, err error) error {
	sess.dirty = true
	return &transport.TransportError{
		Code:    transport.CodeIMAPResponseMalformed,
		Message: "IMAP STATUS response malformed",
		Err:     err,
	}
}

func parseStatusLine(line string, literals [][]byte) (transport.MailboxStatus, error) {
	const prefix = "* STATUS "
	if !strings.HasPrefix(line, prefix) {
		return transport.MailboxStatus{}, errors.New("missing STATUS prefix")
	}
	parser := imapValueParser{literals: literals}
	mailbox, rest, err := parser.parse(line[len(prefix):])
	if err != nil {
		return transport.MailboxStatus{}, fmt.Errorf("invalid STATUS mailbox: %w", err)
	}
	if mailbox == "" {
		return transport.MailboxStatus{}, errors.New("invalid STATUS mailbox: empty value")
	}
	fields, err := parseStatusFields(strings.TrimLeft(rest, " \t"), &parser)
	if err != nil {
		return transport.MailboxStatus{}, err
	}
	if parser.nextLiteral != len(literals) {
		return transport.MailboxStatus{}, errors.New("unused STATUS response literal")
	}
	return mapStatusFields(mailbox, fields)
}

func parseStatusFields(value string, parser *imapValueParser) ([]string, error) {
	if value == "" || value[0] != '(' {
		return nil, errors.New("STATUS item list is missing")
	}
	value = value[1:]
	var fields []string
	for {
		value = strings.TrimLeft(value, " \t")
		if value == "" {
			return nil, errors.New("unterminated STATUS item list")
		}
		if value[0] == ')' {
			if strings.TrimSpace(value[1:]) != "" {
				return nil, errors.New("trailing STATUS response data")
			}
			return fields, nil
		}
		field, rest, err := parseStatusToken(value, parser)
		if err != nil {
			return nil, fmt.Errorf("invalid STATUS item: %w", err)
		}
		fields = append(fields, field)
		value = rest
	}
}

func parseStatusToken(value string, parser *imapValueParser) (string, string, error) {
	value = strings.TrimLeft(value, " \t")
	if value == "" {
		return "", value, errors.New("empty STATUS token")
	}
	if value[0] == '"' || strings.HasPrefix(value, imapLiteralMarker) {
		return parser.parse(value)
	}
	index := 0
	for index < len(value) && !isSpace(value[index]) && value[index] != ')' {
		index++
	}
	if index == 0 {
		return "", value, errors.New("empty STATUS token")
	}
	return value[:index], value[index:], nil
}

func mapStatusFields(mailbox string, fields []string) (transport.MailboxStatus, error) {
	if len(fields)%2 != 0 {
		return transport.MailboxStatus{}, errors.New("STATUS item list has an unmatched field")
	}
	status := transport.MailboxStatus{Mailbox: mailbox}
	seen := make(map[string]struct{}, len(fields)/2)
	for index := 0; index < len(fields); index += 2 {
		key := strings.ToUpper(fields[index])
		if _, duplicate := seen[key]; duplicate {
			return transport.MailboxStatus{}, fmt.Errorf("duplicate STATUS field %q", key)
		}
		seen[key] = struct{}{}
		if err := assignStatusField(&status, key, fields[index+1]); err != nil {
			return transport.MailboxStatus{}, err
		}
	}
	for _, key := range []string{"MESSAGES", "UNSEEN", "UIDNEXT", "UIDVALIDITY"} {
		if _, present := seen[key]; !present {
			return transport.MailboxStatus{}, fmt.Errorf("missing STATUS field %q", key)
		}
	}
	if status.Unseen > status.Messages {
		return transport.MailboxStatus{}, fmt.Errorf(
			"STATUS UNSEEN %d exceeds MESSAGES %d", status.Unseen, status.Messages,
		)
	}
	return status, nil
}

func assignStatusField(status *transport.MailboxStatus, key, value string) error {
	switch key {
	case "MESSAGES":
		number, err := parseStatusInt(value)
		status.Messages = number
		return err
	case "UNSEEN":
		number, err := parseStatusInt(value)
		status.Unseen = number
		return err
	case "UIDNEXT":
		number, err := parseStatusUint32(value)
		status.UIDNext = number
		return err
	case "UIDVALIDITY":
		number, err := parseStatusUint32(value)
		status.UIDValidity = number
		return err
	default:
		return fmt.Errorf("unknown STATUS field %q", key)
	}
}

func parseStatusInt(value string) (int, error) {
	number, err := parseStatusUint(value, strconv.IntSize)
	if err != nil {
		return 0, fmt.Errorf("invalid STATUS integer %q: %w", value, err)
	}
	return int(number), nil
}

func parseStatusUint32(value string) (uint32, error) {
	number, err := parseStatusUint(value, 32)
	if err != nil || number == 0 {
		if err == nil {
			err = errors.New("value must be positive")
		}
		return 0, fmt.Errorf("invalid STATUS uint32 %q: %w", value, err)
	}
	return uint32(number), nil
}

func parseStatusUint(value string, bitSize int) (uint64, error) {
	if value == "" {
		return 0, errors.New("value is empty")
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return 0, errors.New("value is not decimal")
		}
	}
	return strconv.ParseUint(value, 10, bitSize)
}
