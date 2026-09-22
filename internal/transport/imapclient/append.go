package imapclient

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	mail "net/mail"
	"strings"
	"sync"

	"mailcli/internal/transport"
)

// AppendToSent implements transport.SentMirror. It runs on its own dedicated
// connection: the LOGIN/LIST/SELECT/SEARCH/APPEND/LOGOUT sequence is
// self-contained and is not retained in the reusable session set. Its
// reservation still counts against the account connection limit and excludes
// concurrent reads and mutations for that identity.
func (c *Client) AppendToSent(ctx context.Context, cfg transport.ImapConfig, msg []byte, messageID string) (transport.AppendEvidence, error) {
	return c.AppendToSentReader(ctx, cfg, bytes.NewReader(msg), int64(len(msg)), messageID)
}

// AppendToSentReader mirrors a replayable message source without retaining
// the complete RFC 5322 payload in memory.
func (c *Client) AppendToSentReader(ctx context.Context, cfg transport.ImapConfig, msg io.Reader, size int64, messageID string) (transport.AppendEvidence, error) {
	var empty transport.AppendEvidence
	if size < 0 {
		return empty, &transport.TransportError{
			Code:    transport.CodeIMAPInvalidValue,
			Message: "message size cannot be negative",
		}
	}
	normalizedMessageID, err := normalizeMessageID(messageID)
	if err != nil {
		return empty, err
	}
	messageID = normalizedMessageID

	if cfg.Host == "" {
		return empty, &transport.TransportError{
			Code:    transport.CodeIMAPSentMailboxNotFound,
			Message: "IMAP host is empty",
		}
	}
	releaseConnection, err := c.acquireDedicated(ctx, cfg)
	if err != nil {
		return empty, err
	}
	defer releaseConnection()

	conn, err := c.dial(ctx, cfg)
	if err != nil {
		return empty, err
	}
	ctx, cancel := context.WithCancel(ctx)
	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(func() { _ = conn.Close() }) }
	defer closeConn()
	go func() {
		<-ctx.Done()
		closeConn()
	}()
	defer cancel()

	sess := &session{
		conn: conn,
		br:   bufio.NewReader(conn),
		bw:   bufio.NewWriter(conn),
	}
	prefix := makeTagPrefix()
	var cmdNum int
	sess.nextTag = func() string {
		cmdNum++
		return fmt.Sprintf("%s%04d", prefix, cmdNum)
	}

	if err := c.readGreeting(ctx, sess); err != nil {
		return empty, err
	}

	if err := c.doLogin(ctx, sess, sess.nextTag(), cfg); err != nil {
		return empty, err
	}
	if err := c.enableUTF8(ctx, sess); err != nil {
		return empty, err
	}

	mailboxes, err := c.doList(ctx, sess, sess.nextTag())
	if err != nil {
		return empty, err
	}

	sentBox, err := pickSent(mailboxes)
	if err != nil {
		return empty, err
	}

	selected, err := c.doSelectInfo(ctx, sess, sess.nextTag(), sentBox)
	if err != nil {
		return empty, err
	}

	matchCount, err := c.doSearch(ctx, sess, sess.nextTag(), messageID)
	if err != nil {
		return empty, err
	}

	if matchCount > 1 {
		return empty, ambiguousMessageIDError(sentBox, messageID, matchCount)
	}
	if matchCount == 1 {
		uid, err := c.verifySingleSentMatch(ctx, sess, sentBox, messageID)
		if err != nil {
			return empty, err
		}
		_ = c.doLogout(ctx, sess, sess.nextTag())
		return transport.AppendEvidence{
			Mailbox: sentBox, Appended: false, MatchCount: matchCount,
			UIDValidity: selected.uidvalidity, UID: uid,
		}, nil
	}

	if err := c.doAppend(ctx, sess, sess.nextTag(), sentBox, msg, size); err != nil {
		return empty, err
	}
	matchCount, err = c.doSearch(ctx, sess, sess.nextTag(), messageID)
	if err != nil {
		return empty, appendOutcomeUnknown(err)
	}
	if matchCount == 0 {
		return empty, appendOutcomeUnknown(
			fmt.Errorf("Message-ID %s was not visible after APPEND", messageID),
		)
	}
	if matchCount > 1 {
		return empty, ambiguousMessageIDError(sentBox, messageID, matchCount)
	}
	uid, err := c.verifySingleSentMatch(ctx, sess, sentBox, messageID)
	if err != nil {
		if transport.ErrorCode(err) == transport.CodeIMAPAmbiguousMessageID {
			return empty, err
		}
		return empty, appendOutcomeUnknown(err)
	}

	_ = c.doLogout(ctx, sess, sess.nextTag())
	return transport.AppendEvidence{
		Mailbox: sentBox, Appended: true, MatchCount: 1,
		UIDValidity: selected.uidvalidity, UID: uid,
	}, nil
}

func (c *Client) verifySingleSentMatch(ctx context.Context, sess *session, mailbox, messageID string) (uint32, error) {
	uids, err := c.doUIDSearch(ctx, sess, sess.nextTag(), messageID)
	if err != nil {
		return 0, err
	}
	if len(uids) != 1 {
		if len(uids) > 1 {
			return 0, ambiguousMessageIDError(mailbox, messageID, len(uids))
		}
		return 0, messageIDNotFoundError(0, messageID)
	}
	if err := c.verifyMessageID(ctx, sess, uids[0], messageID); err != nil {
		return 0, err
	}
	return uids[0], nil
}

func ambiguousMessageIDError(mailbox, messageID string, count int) error {
	return &transport.TransportError{
		Code: transport.CodeIMAPAmbiguousMessageID,
		Message: fmt.Sprintf(
			"Sent mailbox %q contains %d messages with Message-ID %s",
			mailbox, count, messageID,
		),
	}
}

func (c *Client) verifyMessageID(ctx context.Context, sess *session, uid uint32, messageID string) error {
	normalizedMessageID, err := normalizeMessageID(messageID)
	if err != nil {
		return err
	}
	matched, err := c.checkMessageIDNormalized(ctx, sess, uid, normalizedMessageID)
	if err != nil {
		return err
	}
	if !matched {
		return messageIDNotFoundError(uid, normalizedMessageID)
	}
	return nil
}

// checkMessageIDNormalized fetches one bounded header block without setting
// Seen and reports whether the candidate contains exactly the requested
// Message-ID. A valid but different header is a normal SEARCH false positive;
// malformed, missing, or duplicate headers remain explicit identity errors.
func (c *Client) checkMessageIDNormalized(
	ctx context.Context,
	sess *session,
	uid uint32,
	normalizedMessageID string,
) (bool, error) {
	if err := validateMessageUID(uid); err != nil {
		return false, err
	}
	tag := sess.nextTag()
	if err := c.setDeadline(ctx, sess); err != nil {
		return false, wrapIOError(ctx, err, transport.CodeIMAPFetchFailed, "IMAP Message-ID FETCH deadline")
	}
	cmd := fmt.Sprintf("%s UID FETCH %d (BODY.PEEK[HEADER.FIELDS (MESSAGE-ID)])", tag, uid)
	if err := c.writeLine(sess, cmd); err != nil {
		return false, wrapIOError(ctx, err, transport.CodeIMAPFetchFailed, "IMAP Message-ID FETCH write")
	}
	raw, err := c.readFetchLiteral(ctx, sess, tag, uid, maxMessageIDHeaderBytes)
	if err != nil {
		return false, err
	}
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return false, messageIDNotFoundError(uid, normalizedMessageID)
	}
	var values []string
	for key, headers := range message.Header {
		if strings.EqualFold(key, "Message-ID") {
			values = append(values, headers...)
		}
	}
	if len(values) != 1 {
		return false, messageIDNotFoundError(uid, normalizedMessageID)
	}
	actual, err := normalizeMessageID(strings.TrimSpace(values[0]))
	if err != nil {
		return false, messageIDNotFoundError(uid, normalizedMessageID)
	}
	return actual == normalizedMessageID, nil
}

func messageIDNotFoundError(uid uint32, messageID string) error {
	return &transport.TransportError{
		Code: transport.CodeIMAPMessageNotFound,
		Message: fmt.Sprintf(
			"IMAP candidate UID %d did not contain the exact Message-ID %s",
			uid, messageID,
		),
	}
}
