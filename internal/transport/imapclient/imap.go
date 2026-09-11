// Package imapclient implements a minimal IMAP4rev1 client for mirroring a
// sent message into the account's Sent mailbox.
//
// Implemented subset:
//   - Implicit TLS connection over port 993.
//   - Initial greeting and capability negotiation for mailbox encoding.
//   - LOGIN with quoted credentials.
//   - LIST "" "*" for mailbox discovery.
//   - SELECT to make a mailbox active for SEARCH and UID SEARCH.
//   - SEARCH HEADER Message-ID "<id>" plus exact UID FETCH verification.
//   - APPEND <mailbox> (\Seen) {length} with a synchronizing literal.
//   - LOGOUT.
//
// Response parsing is intentionally bounded: untagged "* ..." lines,
// continuation "+ ..." lines, and a final tagged "tag OK|NO|BAD ..." line.
// Quoted strings are unescaped. LIST and FETCH literals are reconstructed
// before their values are parsed; malformed responses fail the operation.
package imapclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	mail "net/mail"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"mailcli/internal/transport"
)

const (
	flagSeen                 = "\\Seen"
	imapDialTimeout          = 10 * time.Second
	maxIMAPResponseLineBytes = 1 << 20
	maxListLiteralBytes      = 1 << 20
	maxListResponseBytes     = 8 << 20
	maxListLiteralCount      = 128
	maxFetchLiteralCount     = 128
	maxFetchNestingDepth     = 128
	maxUIDSearchResults      = 100000
	maxMessageIDHeaderBytes  = 1 << 20
	maxIdentitySearchResults = 128
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

// mailbox carries the parsed server identity and hierarchy for a LIST response.
type mailbox struct {
	name        string // historical alias for wireName in package-local tests
	wireName    string
	displayName string
	displayPath []string
	delimiter   string
	encoding    transport.MailboxEncoding
	flags       []string
}

func (c *Client) dial(ctx context.Context, cfg transport.ImapConfig) (net.Conn, error) {
	host := cfg.Host
	port := cfg.Port
	if port == 0 {
		port = 993
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	tlsCfg := c.tlsConfig(host)
	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: imapDialTimeout},
		Config:    tlsCfg,
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, wrapDialError(ctx, err)
	}
	return conn, nil
}

func (c *Client) tlsConfig(host string) *tls.Config {
	if c.TLSConfig != nil {
		cfg := c.TLSConfig.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName = host
		}
		return cfg
	}
	return &tls.Config{ServerName: host}
}

func (c *Client) readGreeting(ctx context.Context, sess *session) error {
	if err := c.setDeadline(ctx, sess); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPConnectFailed, "IMAP greeting deadline")
	}
	line, err := c.readLine(sess)
	if err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPConnectFailed, "IMAP greeting read")
	}
	if !strings.HasPrefix(line, "* ") {
		sess.dirty = true
		return &transport.TransportError{
			Code:    transport.CodeIMAPResponseMalformed,
			Message: "IMAP greeting is malformed",
			Err:     fmt.Errorf("got %q", line),
		}
	}
	sess.mailboxEncoding = transport.MailboxEncodingModifiedUTF7
	sess.utf8Accept = greetingAdvertisesUTF8(line)
	sess.utf8Only = capabilityLineRequiresUTF8(line)
	return nil
}

func (c *Client) enableUTF8(ctx context.Context, sess *session) error {
	if !sess.utf8Accept {
		if err := c.refreshCapabilities(ctx, sess); err != nil {
			return err
		}
	}
	if !sess.utf8Accept {
		return nil
	}
	tag := sess.nextTag()
	if err := c.setDeadline(ctx, sess); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP ENABLE UTF8 deadline")
	}
	if err := c.writeLine(sess, tag+" ENABLE UTF8=ACCEPT"); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPConnectFailed, "IMAP ENABLE UTF8 write")
	}
	enabled := false
	for {
		line, err := c.readLine(sess)
		if err != nil {
			return wrapIOError(ctx, err, transport.CodeIMAPConnectFailed, "IMAP ENABLE UTF8 read")
		}
		if strings.HasPrefix(strings.ToUpper(line), "* ENABLED ") {
			for _, capability := range strings.Fields(line[len("* ENABLED "):]) {
				if strings.EqualFold(capability, "UTF8=ACCEPT") {
					enabled = true
				}
			}
			continue
		}
		if !strings.HasPrefix(line, tag+" ") {
			continue
		}
		if parseStatus(line, tag) == "OK" && enabled {
			sess.mailboxEncoding = transport.MailboxEncodingUTF8
		}
		if sess.utf8Only && sess.mailboxEncoding != transport.MailboxEncodingUTF8 {
			return &transport.TransportError{
				Code:    transport.CodeIMAPResponseMalformed,
				Message: "IMAP UTF8=ONLY capability was not enabled",
				Err:     fmt.Errorf("ENABLE UTF8=ACCEPT returned %s without UTF8=ACCEPT", parseStatus(line, tag)),
			}
		}
		return nil
	}
}

func (c *Client) refreshCapabilities(ctx context.Context, sess *session) error {
	tag := sess.nextTag()
	if err := c.setDeadline(ctx, sess); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP CAPABILITY deadline")
	}
	if err := c.writeLine(sess, tag+" CAPABILITY"); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPConnectFailed, "IMAP CAPABILITY write")
	}
	for {
		line, err := c.readLine(sess)
		if err != nil {
			return wrapIOError(ctx, err, transport.CodeIMAPConnectFailed, "IMAP CAPABILITY read")
		}
		if capabilityLineAdvertisesUTF8(line) {
			sess.utf8Accept = true
			sess.utf8Only = capabilityLineRequiresUTF8(line)
		}
		if !strings.HasPrefix(line, tag+" ") {
			continue
		}
		return nil
	}
}

func (c *Client) doLogin(ctx context.Context, sess *session, tag string, cfg transport.ImapConfig) error {
	if err := c.setDeadline(ctx, sess); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP login deadline")
	}
	username, err := safeQuoteIMAP(cfg.Username)
	if err != nil {
		return err
	}
	password, err := safeQuoteIMAP(cfg.Password)
	if err != nil {
		return err
	}
	cmd := tag + " LOGIN " + username + " " + password
	if err := c.writeLine(sess, cmd); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPAuthFailed, "IMAP LOGIN write")
	}
	status, _, err := c.readFinal(ctx, sess, tag)
	if err != nil {
		return err
	}
	if status == "OK" {
		return nil
	}
	return &transport.TransportError{
		Code:    transport.CodeIMAPAuthFailed,
		Message: "IMAP LOGIN failed",
		Err:     fmt.Errorf("server returned %s", status),
	}
}

func (c *Client) doList(ctx context.Context, sess *session, tag string) ([]mailbox, error) {
	if err := c.setDeadline(ctx, sess); err != nil {
		return nil, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP LIST deadline")
	}
	if err := c.writeLine(sess, tag+` LIST "" "*"`); err != nil {
		return nil, wrapIOError(ctx, err, transport.CodeIMAPSentMailboxNotFound, "IMAP LIST write")
	}

	var mailboxes []mailbox
	for {
		line, literals, err := c.readLineWithLiteral(sess)
		if err != nil {
			var malformed *malformedResponseError
			if errors.As(err, &malformed) {
				sess.dirty = true
				return nil, listResponseMalformed(malformed)
			}
			return nil, wrapIOError(ctx, err, transport.CodeIMAPSentMailboxNotFound, "IMAP LIST read")
		}
		if strings.HasPrefix(line, tag+" ") {
			status := parseStatus(line, tag)
			if status == "OK" {
				return mailboxes, nil
			}
			return nil, &transport.TransportError{
				Code:    transport.CodeIMAPSentMailboxNotFound,
				Message: "IMAP LIST failed: " + status,
			}
		}
		if !strings.HasPrefix(line, "* LIST ") {
			continue
		}
		parsed, perr := parseListMailbox(line, sess.mailboxEncoding, literals...)
		if perr != nil {
			sess.dirty = true
			return nil, listResponseMalformed(perr)
		}
		mailboxes = append(mailboxes, parsed)
	}
}

func (c *Client) doSearch(ctx context.Context, sess *session, tag, messageID string) (int, error) {
	normalizedMessageID, err := normalizeMessageID(messageID)
	if err != nil {
		return 0, err
	}
	if err := c.setDeadline(ctx, sess); err != nil {
		return 0, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP SEARCH deadline")
	}
	quotedMessageID, err := safeQuoteIMAP(normalizedMessageID)
	if err != nil {
		return 0, err
	}
	if err := c.writeLine(sess, tag+" SEARCH HEADER Message-ID "+quotedMessageID); err != nil {
		return 0, wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP SEARCH write")
	}

	matchCount := 0
	for {
		line, err := c.readLine(sess)
		if err != nil {
			return 0, wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP SEARCH read")
		}
		if strings.HasPrefix(line, tag+" ") {
			status := parseStatus(line, tag)
			if status == "OK" {
				return matchCount, nil
			}
			return 0, &transport.TransportError{
				Code:    transport.CodeIMAPAppendFailed,
				Message: "IMAP SEARCH failed: " + status,
			}
		}
		if strings.HasPrefix(line, "* SEARCHING") {
			return 0, &transport.TransportError{
				Code:    transport.CodeIMAPResponseMalformed,
				Message: "IMAP SEARCH returned invalid SEARCHING response",
			}
		}
		if line != "* SEARCH" && !strings.HasPrefix(line, "* SEARCH ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 2 {
			matchCount += len(fields) - 2
		}
	}
}

func (c *Client) doAppend(ctx context.Context, sess *session, tag, mbox string, msg io.Reader, size int64) error {
	if err := c.setDeadline(ctx, sess); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP APPEND deadline")
	}
	quotedMailbox, err := safeQuoteIMAP(mbox)
	if err != nil {
		return err
	}
	cmd := tag + " APPEND " + quotedMailbox + " (" + flagSeen + ") {" + strconv.FormatInt(size, 10) + "}"
	if err := c.writeLine(sess, cmd); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP APPEND write")
	}

	line, err := c.readLine(sess)
	if err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP APPEND continuation read")
	}
	if !strings.HasPrefix(line, "+") {
		return &transport.TransportError{
			Code:    transport.CodeIMAPAppendFailed,
			Message: "IMAP APPEND expected continuation",
			Err:     fmt.Errorf("got: %s", line),
		}
	}

	if err := c.setTransferDeadline(ctx, sess, size); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP APPEND transfer deadline")
	}
	written, err := io.Copy(sess.bw, msg)
	if err != nil {
		sess.dirty = true
		return appendOutcomeUnknown(wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP APPEND literal write"))
	}
	if size >= 0 && written != size {
		sess.dirty = true
		return appendOutcomeUnknown(&transport.TransportError{
			Code:    transport.CodeIMAPAppendFailed,
			Message: fmt.Sprintf("message source ended after %d of %d bytes", written, size),
		})
	}
	if err := sess.bw.Flush(); err != nil {
		sess.dirty = true
		return appendOutcomeUnknown(wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP APPEND literal flush"))
	}
	if err := c.writeLine(sess, ""); err != nil {
		return appendOutcomeUnknown(wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP APPEND literal CRLF"))
	}

	if err := c.setDeadline(ctx, sess); err != nil {
		return appendOutcomeUnknown(wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP APPEND final reply deadline"))
	}
	status, _, err := c.readFinal(ctx, sess, tag)
	if err != nil {
		return appendOutcomeUnknown(err)
	}
	if status == "OK" {
		return nil
	}
	return &transport.TransportError{
		Code:    transport.CodeIMAPAppendFailed,
		Message: "IMAP APPEND failed",
		Err:     fmt.Errorf("server returned %s", status),
	}
}

func appendOutcomeUnknown(err error) error {
	return &transport.TransportError{
		Code:    transport.CodeIMAPAppendOutcomeUnknown,
		Message: "IMAP APPEND outcome is unknown after message data was sent",
		Err:     err,
	}
}

func (c *Client) doLogout(ctx context.Context, sess *session, tag string) error {
	if err := c.setDeadline(ctx, sess); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP LOGOUT deadline")
	}
	return c.writeLine(sess, tag+" LOGOUT")
}

func (c *Client) setDeadline(ctx context.Context, sess *session) error {
	return c.setDeadlineFor(ctx, sess, transport.TransferCommandBudget)
}

func (c *Client) setTransferDeadline(ctx context.Context, sess *session, size int64) error {
	return c.setDeadlineFor(ctx, sess, transport.TransferBudgetForSize(size))
}

func (c *Client) setDeadlineFor(ctx context.Context, sess *session, budget time.Duration) error {
	if err := ctx.Err(); err != nil {
		sess.dirty = true
		return err
	}
	deadline := time.Now().Add(budget)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := sess.conn.SetDeadline(deadline); err != nil {
		sess.dirty = true
		return err
	}
	return nil
}

func (c *Client) writeLine(sess *session, line string) error {
	if strings.ContainsAny(line, "\r\n\x00") {
		sess.dirty = true
		return &transport.TransportError{
			Code:    transport.CodeIMAPInvalidValue,
			Message: "IMAP command contains a forbidden control character",
		}
	}
	if _, err := sess.bw.WriteString(line + "\r\n"); err != nil {
		sess.dirty = true
		return err
	}
	if err := sess.bw.Flush(); err != nil {
		sess.dirty = true
		return err
	}
	return nil
}

func (c *Client) readLine(sess *session) (string, error) {
	var line []byte
	for {
		fragment, err := sess.br.ReadSlice('\n')
		if len(line)+len(fragment) > maxIMAPResponseLineBytes {
			sess.dirty = true
			return "", &malformedResponseError{err: fmt.Errorf(
				"IMAP response line exceeds %d bytes", maxIMAPResponseLineBytes,
			)}
		}
		line = append(line, fragment...)
		if err == nil {
			return strings.TrimRight(string(line), "\r\n"), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		sess.dirty = true
		return "", err
	}
}

const imapLiteralMarker = "\x00"

type malformedResponseError struct {
	err error
}

func (e *malformedResponseError) Error() string { return "malformed IMAP response: " + e.err.Error() }

func (e *malformedResponseError) Unwrap() error { return e.err }

func listResponseMalformed(err error) *transport.TransportError {
	return &transport.TransportError{
		Code:    transport.CodeIMAPResponseMalformed,
		Message: "IMAP LIST response malformed",
		Err:     err,
	}
}

// readLineWithLiteral reconstructs one logical response line from its line
// fragments and server-sent literals. Each literal is represented in the
// returned text by imapLiteralMarker and kept separately so its bytes remain
// raw when the value parser consumes it.
func (c *Client) readLineWithLiteral(sess *session) (string, [][]byte, error) {
	line, literals, err := c.readLogicalLineWithLiterals(
		sess, int64(maxListLiteralBytes), int64(maxListResponseBytes), maxListLiteralCount,
	)
	var limitErr *literalLimitError
	if errors.As(err, &limitErr) {
		return "", nil, &malformedResponseError{err: fmt.Errorf(
			"IMAP LIST literal response exceeds %d bytes or %d bytes per literal",
			maxListResponseBytes, maxListLiteralBytes,
		)}
	}
	var readErr *literalReadError
	if errors.As(err, &readErr) {
		return "", nil, &malformedResponseError{err: readErr}
	}
	return line, literals, err
}

type literalLimitError struct {
	size        int64
	maxLiteral  int64
	maxResponse int64
}

func (e *literalLimitError) Error() string {
	return fmt.Sprintf(
		"IMAP response literal exceeds %d bytes or response literal budget %d bytes: %d",
		e.maxLiteral, e.maxResponse, e.size,
	)
}

type literalReadError struct{ err error }

func (e *literalReadError) Error() string {
	return "IMAP response literal read failed: " + e.err.Error()
}

func (e *literalReadError) Unwrap() error { return e.err }

func (c *Client) readLogicalLineWithLiterals(
	sess *session,
	maxLiteralBytes int64,
	maxResponseBytes int64,
	maxLiteralCount int,
) (string, [][]byte, error) {
	var reconstructed strings.Builder
	var literals [][]byte
	var literalBytes int64
	for {
		line, err := c.readLine(sess)
		if err != nil {
			return "", nil, err
		}
		prefix, size, hasLiteral, err := parseLiteralSuffix(line)
		if err != nil {
			sess.dirty = true
			return "", nil, &malformedResponseError{err: err}
		}
		if !hasLiteral {
			if int64(reconstructed.Len()+len(line)) > maxResponseBytes-literalBytes {
				sess.dirty = true
				return "", nil, &malformedResponseError{err: fmt.Errorf(
					"IMAP logical response exceeds %d bytes", maxResponseBytes,
				)}
			}
			reconstructed.WriteString(line)
			return reconstructed.String(), literals, nil
		}
		if len(literals) >= maxLiteralCount {
			sess.dirty = true
			return "", nil, &malformedResponseError{err: fmt.Errorf(
				"IMAP response literal count exceeds %d", maxLiteralCount,
			)}
		}
		size64 := int64(size)
		if size64 > maxLiteralBytes || size64 > maxResponseBytes-literalBytes {
			sess.dirty = true
			return "", nil, &literalLimitError{
				size: size64, maxLiteral: maxLiteralBytes, maxResponse: maxResponseBytes,
			}
		}
		if int64(reconstructed.Len()+len(prefix)+1) > maxResponseBytes-literalBytes-size64 {
			sess.dirty = true
			return "", nil, &malformedResponseError{err: fmt.Errorf(
				"IMAP logical response exceeds %d bytes", maxResponseBytes,
			)}
		}

		reconstructed.WriteString(prefix)
		reconstructed.WriteString(imapLiteralMarker)
		literal := make([]byte, size)
		if _, err := io.ReadFull(sess.br, literal); err != nil {
			sess.dirty = true
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return "", nil, &literalReadError{err: err}
			}
			return "", nil, err
		}
		literals = append(literals, literal)
		literalBytes += size64
	}
}

func parseLiteralSuffix(line string) (prefix string, size int, hasLiteral bool, err error) {
	if !strings.HasSuffix(line, "}") {
		return line, 0, false, nil
	}
	start := strings.LastIndexByte(line, '{')
	if start == -1 || (start > 0 && !isSpace(line[start-1])) || quotedAt(line, start) {
		return line, 0, false, nil
	}
	digits := line[start+1 : len(line)-1]
	if digits == "" {
		return "", 0, false, fmt.Errorf("empty IMAP literal length")
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return "", 0, false, fmt.Errorf("invalid IMAP literal length %q", digits)
		}
	}
	size, err = strconv.Atoi(digits)
	if err != nil {
		return "", 0, false, fmt.Errorf("invalid IMAP literal length %q: %w", digits, err)
	}
	return line[:start], size, true, nil
}

func quotedAt(s string, offset int) bool {
	quoted := false
	for i := 0; i < offset; i++ {
		switch s[i] {
		case '\\':
			if quoted {
				i++
			}
		case '"':
			quoted = !quoted
		}
	}
	return quoted
}

func (c *Client) readFinal(ctx context.Context, sess *session, tag string) (string, string, error) {
	for {
		line, err := c.readLine(sess)
		if err != nil {
			return "", "", wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP read final response")
		}
		if !strings.HasPrefix(line, tag+" ") {
			continue
		}
		rest := strings.TrimPrefix(line, tag+" ")
		fields := strings.SplitN(rest, " ", 2)
		status := fields[0]
		var text string
		if len(fields) > 1 {
			text = fields[1]
		}
		return status, text, nil
	}
}

// readFinalWithCodes reads a tagged command completion and retains bracketed
// response codes from both untagged and tagged lines. COPYUID is commonly sent
// as an untagged OK response before the tagged completion, so a plain final
// status parser would silently discard the only destination UID evidence.
func (c *Client) readFinalWithCodes(
	ctx context.Context,
	sess *session,
	tag string,
	code string,
	message string,
) (string, string, []string, error) {
	var responseCodes []string
	for {
		line, err := c.readLine(sess)
		if err != nil {
			return "", "", responseCodes, wrapIOError(ctx, err, code, message)
		}
		if responseCode, ok := bracketedResponseCode(line); ok {
			responseCodes = append(responseCodes, responseCode)
		}
		if !strings.HasPrefix(line, tag+" ") {
			continue
		}
		rest := strings.TrimPrefix(line, tag+" ")
		fields := strings.SplitN(rest, " ", 2)
		status := fields[0]
		var text string
		if len(fields) > 1 {
			text = fields[1]
		}
		return status, text, responseCodes, nil
	}
}

func bracketedResponseCode(line string) (string, bool) {
	start := strings.IndexByte(line, '[')
	if start == -1 {
		return "", false
	}
	relativeEnd := strings.IndexByte(line[start+1:], ']')
	if relativeEnd == -1 {
		return "", false
	}
	value := strings.TrimSpace(line[start+1 : start+1+relativeEnd])
	if value == "" {
		return "", false
	}
	return value, true
}

func makeTagPrefix() string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "T"
	}
	for i := range b {
		if i == 0 {
			b[0] = chars[int(b[0])%26]
			continue
		}
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

func quoteIMAP(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' || c == '"' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	b.WriteByte('"')
	return b.String()
}

func safeQuoteIMAP(s string) (string, error) {
	for index := 0; index < len(s); index++ {
		if s[index] < 0x20 || s[index] == 0x7f {
			return "", &transport.TransportError{
				Code:    transport.CodeIMAPInvalidValue,
				Message: fmt.Sprintf("IMAP value contains control character at byte %d", index),
			}
		}
	}
	return quoteIMAP(s), nil
}

func parseListLine(line string, literals ...[]byte) (string, []string, error) {
	parsed, err := parseListMailbox(line, transport.MailboxEncodingModifiedUTF7, literals...)
	if err != nil {
		return "", nil, err
	}
	return parsed.wireName, parsed.flags, nil
}

func parseListMailbox(line string, encoding transport.MailboxEncoding, literals ...[]byte) (mailbox, error) {
	parser := imapValueParser{literals: literals}
	const prefix = "* LIST "
	if !strings.HasPrefix(line, prefix) {
		return mailbox{}, fmt.Errorf("not a LIST response")
	}
	s := line[len(prefix):]
	if !strings.HasPrefix(s, "(") {
		return mailbox{}, fmt.Errorf("no attribute list")
	}

	depth := 0
	end := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' {
			i++
			continue
		}
		if c == '(' {
			depth++
		}
		if c == ')' {
			depth--
			if depth == 0 {
				end = i + 1
				break
			}
		}
	}
	if end == 0 {
		return mailbox{}, fmt.Errorf("unterminated attribute list")
	}

	attrs := s[1 : end-1]
	flags, err := parser.parseValues(attrs)
	if err != nil {
		return mailbox{}, err
	}

	rest := strings.TrimSpace(s[end:])
	delimiter, rest, err := parser.parse(rest)
	if err != nil {
		return mailbox{}, err
	}

	wireName, rest, err := parser.parse(rest)
	if err != nil {
		return mailbox{}, err
	}
	if strings.TrimSpace(rest) != "" {
		return mailbox{}, fmt.Errorf("trailing LIST response data")
	}
	if parser.nextLiteral != len(literals) {
		return mailbox{}, fmt.Errorf("unused IMAP response literal")
	}
	displayName, displayPath, err := decodeMailboxWireName(wireName, delimiter, encoding)
	if err != nil {
		return mailbox{}, err
	}
	return mailbox{
		name: wireName, wireName: wireName, displayName: displayName, displayPath: displayPath,
		delimiter: delimiter, encoding: normalizedMailboxEncoding(encoding), flags: flags,
	}, nil
}

type imapValueParser struct {
	literals    [][]byte
	nextLiteral int
}

func (p *imapValueParser) parseValues(s string) ([]string, error) {
	var values []string
	s = strings.TrimSpace(s)
	for s != "" {
		v, rest, err := p.parse(s)
		if err != nil {
			return nil, err
		}
		values = append(values, v)
		s = strings.TrimSpace(rest)
	}
	return values, nil
}

func (p *imapValueParser) parse(s string) (string, string, error) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return "", "", fmt.Errorf("empty token")
	}
	if strings.HasPrefix(s, imapLiteralMarker) {
		if p.nextLiteral >= len(p.literals) {
			return "", s, fmt.Errorf("missing IMAP response literal")
		}
		value := string(p.literals[p.nextLiteral])
		p.nextLiteral++
		return value, s[len(imapLiteralMarker):], nil
	}
	if s[0] == '"' {
		return parseQuoted(s)
	}
	if len(s) >= 3 && strings.EqualFold(s[:3], "NIL") && (len(s) == 3 || isSpace(s[3])) {
		return "", s[3:], nil
	}

	i := 0
	for i < len(s) && !isSpace(s[i]) {
		if strings.HasPrefix(s[i:], imapLiteralMarker) {
			return "", s, fmt.Errorf("embedded IMAP response literal")
		}
		i++
	}
	return s[:i], s[i:], nil
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t'
}

func parseQuoted(s string) (string, string, error) {
	if s == "" || s[0] != '"' {
		return "", s, fmt.Errorf("not a quoted string")
	}
	var b strings.Builder
	i := 1
	for i < len(s) {
		c := s[i]
		if c == '\\' {
			if i+1 >= len(s) {
				return "", s, fmt.Errorf("unterminated quoted string")
			}
			b.WriteByte(s[i+1])
			i += 2
			continue
		}
		if c == '"' {
			i++
			return b.String(), s[i:], nil
		}
		b.WriteByte(c)
		i++
	}
	return "", s, fmt.Errorf("unterminated quoted string")
}

func pickSent(mailboxes []mailbox) (string, error) {
	infos := make([]transport.MailboxInfo, len(mailboxes))
	for index, candidate := range mailboxes {
		wireName := candidate.wireName
		if wireName == "" {
			wireName = candidate.name
		}
		infos[index] = transport.MailboxInfo{
			Name: wireName, WireName: wireName,
			DisplayName: candidate.displayName, DisplayPath: append([]string(nil), candidate.displayPath...),
			Delimiter: candidate.delimiter, Encoding: candidate.encoding,
			Flags: append([]string(nil), candidate.flags...),
		}
	}
	return transport.ResolveSentMailbox(infos)
}

func parseStatus(line, tag string) string {
	rest := strings.TrimPrefix(line, tag+" ")
	fields := strings.SplitN(rest, " ", 2)
	return fields[0]
}

func wrapDialError(ctx context.Context, err error) error {
	if ctx.Err() == context.Canceled {
		return &transport.TransportError{
			Code:    transport.CodeIMAPTimeout,
			Message: "IMAP connection canceled",
			Err:     err,
		}
	}
	if isTimeout(err) {
		return &transport.TransportError{
			Code:    transport.CodeIMAPTimeout,
			Message: "IMAP connection timed out",
			Err:     err,
		}
	}
	return &transport.TransportError{
		Code:    transport.CodeIMAPConnectFailed,
		Message: "IMAP connection failed",
		Err:     err,
	}
}

func wrapIOError(ctx context.Context, err error, code, message string) error {
	if err == nil {
		return nil
	}
	if ctx.Err() == context.Canceled {
		return &transport.TransportError{
			Code:    transport.CodeIMAPTimeout,
			Message: message,
			Err:     err,
		}
	}
	if isTimeout(err) {
		return &transport.TransportError{
			Code:    transport.CodeIMAPTimeout,
			Message: message,
			Err:     err,
		}
	}
	return &transport.TransportError{
		Code:    code,
		Message: message,
		Err:     err,
	}
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

type session struct {
	conn            net.Conn
	br              *bufio.Reader
	bw              *bufio.Writer
	nextTag         func() string
	mailboxEncoding transport.MailboxEncoding
	utf8Accept      bool
	utf8Only        bool
	// dirty marks an IO-level failure (read, write, deadline, cancel): the
	// connection state is no longer trustworthy and the pooled session must
	// be discarded. Protocol rejections (NO/BAD) do not set it.
	dirty bool
}

// connect dials and logs in, returning a ready session. The session outlives
// ctx: per-command cancellation is handled by the acquire watcher, which
// force-closes the connection.
func (c *Client) connect(ctx context.Context, cfg transport.ImapConfig) (*session, error) {
	if cfg.Host == "" {
		return nil, &transport.TransportError{
			Code:    transport.CodeIMAPConnectFailed,
			Message: "IMAP host is empty",
		}
	}
	conn, err := c.dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
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
		_ = conn.Close()
		return nil, err
	}
	if err := c.doLogin(ctx, sess, sess.nextTag(), cfg); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := c.enableUTF8(ctx, sess); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return sess, nil
}

type selectInfo struct {
	uidvalidity uint32
	exists      int
	permissions flagPermissions
}

func (c *Client) doSelectInfo(ctx context.Context, sess *session, tag, mbox string) (selectInfo, error) {
	var info selectInfo
	if err := c.setDeadline(ctx, sess); err != nil {
		return info, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP SELECT deadline")
	}
	quotedMailbox, err := safeQuoteIMAP(mbox)
	if err != nil {
		return info, err
	}
	if err := c.writeLine(sess, tag+" SELECT "+quotedMailbox); err != nil {
		return info, wrapIOError(ctx, err, transport.CodeIMAPMailboxNotFound, "IMAP SELECT write")
	}
	remaining := int64(maxFlagResponseBytes)
	for range maxFlagResponseCount {
		line, literals, err := c.readLogicalLineWithLiterals(sess, maxIMAPResponseLineBytes, remaining, maxFetchLiteralCount)
		if err != nil {
			return info, wrapIOError(ctx, err, transport.CodeIMAPMailboxNotFound, "IMAP SELECT read")
		}
		remaining -= int64(len(line) + 2)
		for _, literal := range literals {
			remaining -= int64(len(literal))
		}
		if remaining < 0 {
			break
		}
		code, err := flagResponseCode(line, tag)
		if err == nil {
			err = info.observeFlagCode(code)
		}
		if err != nil {
			sess.dirty = true
			return info, &transport.TransportError{Code: transport.CodeIMAPResponseMalformed, Message: "IMAP SELECT metadata malformed", Err: err}
		}
		if strings.HasPrefix(line, tag+" ") {
			status := parseStatus(line, tag)
			if strings.EqualFold(status, "OK") {
				return info, nil
			}
			return info, &transport.TransportError{
				Code:    transport.CodeIMAPMailboxNotFound,
				Message: "IMAP SELECT failed: " + status,
			}
		}
		if strings.HasPrefix(line, "* ") {
			if strings.HasSuffix(line, " EXISTS") {
				fields := strings.Fields(line)
				if len(fields) >= 3 {
					if n, err := strconv.Atoi(fields[1]); err == nil {
						info.exists = n
					}
				}
			}
		}
	}
	sess.dirty = true
	return info, &transport.TransportError{Code: transport.CodeIMAPResponseMalformed, Message: "IMAP SELECT exceeded its response budget"}
}

// ListMailboxes returns all mailboxes on the IMAP server with their flags.
func (c *Client) ListMailboxes(ctx context.Context, cfg transport.ImapConfig) ([]transport.MailboxInfo, error) {
	ps, release, err := c.acquireIndependent(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer release()
	return c.listMailboxes(ctx, ps)
}

func (c *Client) listMailboxes(ctx context.Context, ps *pooledSession) ([]transport.MailboxInfo, error) {
	mboxes, err := c.doList(ctx, ps.sess, ps.sess.nextTag())
	if err != nil {
		return nil, err
	}
	infos := make([]transport.MailboxInfo, len(mboxes))
	for i, m := range mboxes {
		wireName := m.wireName
		if wireName == "" {
			wireName = m.name
		}
		infos[i] = transport.MailboxInfo{
			Name: wireName, WireName: wireName,
			DisplayName: m.displayName, DisplayPath: append([]string(nil), m.displayPath...),
			Delimiter: m.delimiter, Encoding: m.encoding,
			Flags: append([]string(nil), m.flags...),
		}
	}
	return infos, nil
}

// SearchUID treats UID SEARCH as candidate discovery, then verifies every
// candidate's bounded Message-ID header before returning an exact UID and
// count. A valid but different header is discarded as a SEARCH false positive.
func (c *Client) SearchUID(ctx context.Context, cfg transport.ImapConfig, mailbox string, messageID string) (uint32, uint32, int, error) {
	normalizedMessageID, err := normalizeMessageID(messageID)
	if err != nil {
		return 0, 0, 0, err
	}
	ps, release, err := c.acquire(ctx, cfg)
	if err != nil {
		return 0, 0, 0, err
	}
	defer release()

	info, err := c.ensureSelected(ctx, ps, mailbox)
	if err != nil {
		return 0, 0, 0, err
	}

	uids, err := c.doUIDSearch(ctx, ps.sess, ps.sess.nextTag(), normalizedMessageID)
	if err != nil {
		return 0, 0, 0, err
	}

	if len(uids) == 0 {
		return 0, info.uidvalidity, 0, &transport.TransportError{
			Code:    transport.CodeIMAPMessageNotFound,
			Message: fmt.Sprintf("message %s not found in mailbox %s", messageID, mailbox),
		}
	}

	exactUID, exactMatches, err := c.verifySearchCandidates(
		ctx, ps.sess, uids, normalizedMessageID, mailbox,
	)
	if err != nil {
		return 0, info.uidvalidity, 0, err
	}
	return exactUID, info.uidvalidity, exactMatches, nil
}

// ResolveMessageIdentity discovers one message from exact local metadata. The
// server search is deliberately capped, and every candidate is verified from
// header fields before a UID is returned. No message body is fetched.
func (c *Client) ResolveMessageIdentity(
	ctx context.Context,
	cfg transport.ImapConfig,
	mailbox string,
	hint transport.MessageIdentityHint,
) (transport.MessageIdentity, error) {
	var identity transport.MessageIdentity
	subject := strings.TrimSpace(hint.Subject)
	if subject == "" {
		return identity, &transport.TransportError{
			Code:    transport.CodeIMAPMessageUIDUnknown,
			Message: "cannot discover IMAP identity without a non-empty subject; refresh the local message catalog",
		}
	}
	ps, release, err := c.acquire(ctx, cfg)
	if err != nil {
		return identity, err
	}
	defer release()
	info, err := c.ensureSelected(ctx, ps, mailbox)
	if err != nil {
		return identity, err
	}
	if info.uidvalidity == 0 {
		return identity, &transport.TransportError{
			Code:    transport.CodeIMAPUIDValidityUnknown,
			Message: fmt.Sprintf("mailbox %s returned no UIDVALIDITY; refresh mailbox state and retry", mailbox),
		}
	}
	criteria, err := metadataSearchCriteria(subject, hint.SenderAddress)
	if err != nil {
		return identity, err
	}
	uids, err := c.doUIDSearchCriteriaBounded(
		ctx, ps.sess, ps.sess.nextTag(), criteria, maxIdentitySearchResults,
	)
	if err != nil {
		return identity, err
	}
	var matches []transport.MessageIdentity
	for _, uid := range uids {
		messageID, candidateSubject, senderAddress, err := c.readIdentityHeaders(ctx, ps.sess, uid)
		if err != nil {
			if transport.ErrorCode(err) == transport.CodeIMAPMessageNotFound {
				continue
			}
			return identity, err
		}
		if !identityMetadataMatches(candidateSubject, senderAddress, subject, hint.SenderAddress) {
			continue
		}
		matches = append(matches, transport.MessageIdentity{
			UID: uid, UIDValidity: info.uidvalidity, MessageID: messageID,
		})
	}
	if len(matches) == 0 {
		return identity, &transport.TransportError{
			Code: transport.CodeIMAPMessageUIDUnknown,
			Message: fmt.Sprintf(
				"no unique IMAP message matched subject %q in mailbox %s within the bounded %d-candidate search; refresh the local catalog or provide a fresh reference",
				subject, mailbox, maxIdentitySearchResults,
			),
		}
	}
	if len(matches) > 1 {
		return identity, &transport.TransportError{
			Code: transport.CodeIMAPAmbiguousMessageID,
			Message: fmt.Sprintf(
				"metadata matched %d IMAP messages in mailbox %s; refusing to guess a UID; provide a fresh Message-ID-backed reference",
				len(matches), mailbox,
			),
		}
	}
	return matches[0], nil
}

func metadataSearchCriteria(subject, senderAddress string) (string, error) {
	quotedSubject, err := safeQuoteIMAP(subject)
	if err != nil {
		return "", err
	}
	criteria := "HEADER SUBJECT " + quotedSubject
	if strings.TrimSpace(senderAddress) == "" {
		return criteria, nil
	}
	quotedSender, err := safeQuoteIMAP(strings.TrimSpace(senderAddress))
	if err != nil {
		return "", err
	}
	return criteria + " HEADER FROM " + quotedSender, nil
}

func (c *Client) readIdentityHeaders(ctx context.Context, sess *session, uid uint32) (string, string, string, error) {
	if err := validateMessageUID(uid); err != nil {
		return "", "", "", err
	}
	tag := sess.nextTag()
	if err := c.setDeadline(ctx, sess); err != nil {
		return "", "", "", wrapIOError(ctx, err, transport.CodeIMAPFetchFailed, "IMAP identity FETCH deadline")
	}
	cmd := fmt.Sprintf("%s UID FETCH %d (BODY.PEEK[HEADER.FIELDS (MESSAGE-ID SUBJECT FROM)])", tag, uid)
	if err := c.writeLine(sess, cmd); err != nil {
		return "", "", "", wrapIOError(ctx, err, transport.CodeIMAPFetchFailed, "IMAP identity FETCH write")
	}
	raw, err := c.readFetchLiteral(ctx, sess, tag, uid, maxMessageIDHeaderBytes)
	if err != nil {
		return "", "", "", err
	}
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", "", "", messageIDNotFoundError(uid, "metadata candidate")
	}
	messageID := ""
	if values := headerValues(message.Header, "Message-ID"); len(values) > 0 {
		messageID, err = exactHeaderMessageID(message.Header)
		if err != nil {
			return "", "", "", messageIDNotFoundError(uid, "metadata candidate")
		}
	}
	subject, err := exactHeaderSubject(message.Header)
	if err != nil {
		return "", "", "", messageIDNotFoundError(uid, "metadata candidate")
	}
	sender, err := exactHeaderSender(message.Header)
	if err != nil {
		return "", "", "", messageIDNotFoundError(uid, "metadata candidate")
	}
	return messageID, subject, sender, nil
}

func exactHeaderMessageID(header mail.Header) (string, error) {
	values := headerValues(header, "Message-ID")
	if len(values) != 1 {
		return "", errors.New("metadata candidate has no unique Message-ID")
	}
	return normalizeMessageID(strings.TrimSpace(values[0]))
}

func exactHeaderSubject(header mail.Header) (string, error) {
	values := headerValues(header, "Subject")
	if len(values) != 1 {
		return "", errors.New("metadata candidate has no unique Subject")
	}
	decoded, err := (&mime.WordDecoder{}).DecodeHeader(strings.TrimSpace(values[0]))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(decoded), nil
}

func exactHeaderSender(header mail.Header) (string, error) {
	values := headerValues(header, "From")
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 {
		return "", errors.New("metadata candidate has no unique From")
	}
	address, err := mail.ParseAddress(strings.TrimSpace(values[0]))
	if err != nil || address.Address == "" {
		if err == nil {
			err = errors.New("from address is empty")
		}
		return "", err
	}
	return strings.ToLower(address.Address), nil
}

func headerValues(header mail.Header, wanted string) []string {
	var values []string
	for key, candidates := range header {
		if strings.EqualFold(key, wanted) {
			values = append(values, candidates...)
		}
	}
	return values
}

func identityMetadataMatches(actualSubject, actualSender, expectedSubject, expectedSender string) bool {
	if strings.TrimSpace(actualSubject) != strings.TrimSpace(expectedSubject) {
		return false
	}
	if strings.TrimSpace(expectedSender) == "" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(actualSender), strings.TrimSpace(expectedSender))
}

func (c *Client) verifySearchCandidates(
	ctx context.Context,
	sess *session,
	uids []uint32,
	messageID string,
	mailbox string,
) (uint32, int, error) {
	var exactUID uint32
	exactMatches := 0
	for _, candidateUID := range uids {
		matched, err := c.checkMessageIDNormalized(ctx, sess, candidateUID, messageID)
		if err != nil {
			return 0, 0, err
		}
		if matched {
			exactUID = candidateUID
			exactMatches++
		}
	}
	if exactMatches == 0 {
		return 0, 0, &transport.TransportError{
			Code:    transport.CodeIMAPMessageNotFound,
			Message: fmt.Sprintf("message %s did not have an exact Message-ID header in mailbox %s", messageID, mailbox),
		}
	}
	return exactUID, exactMatches, nil
}

// ensureSelected switches the pooled session to mailbox when needed and
// returns the SELECT-time info. Repeated read commands on the same mailbox
// reuse the cached UIDVALIDITY; mutations call ensureSelectedFresh.
func (c *Client) ensureSelected(ctx context.Context, ps *pooledSession, mailbox string) (selectInfo, error) {
	if ps.selected == mailbox {
		return selectInfo{uidvalidity: ps.uidvalidity}, nil
	}
	info, err := c.doSelectInfo(ctx, ps.sess, ps.sess.nextTag(), mailbox)
	if err != nil {
		// A failed SELECT deselects the current mailbox (RFC 3501): the
		// cached state is stale, but the authenticated connection stays.
		ps.selected = ""
		ps.uidvalidity = 0
		return info, err
	}
	ps.selected = mailbox
	ps.uidvalidity = info.uidvalidity
	return info, nil
}

func (c *Client) ensureSelectedFresh(ctx context.Context, ps *pooledSession, mailbox string) (selectInfo, error) {
	ps.selected = ""
	ps.uidvalidity = 0
	return c.ensureSelected(ctx, ps, mailbox)
}

func (c *Client) doUIDSearch(ctx context.Context, sess *session, tag, messageID string) ([]uint32, error) {
	searchID, err := normalizeMessageID(messageID)
	if err != nil {
		return nil, err
	}
	quotedMessageID, err := safeQuoteIMAP(searchID)
	if err != nil {
		return nil, err
	}
	return c.doUIDSearchCriteria(ctx, sess, tag, "HEADER Message-ID "+quotedMessageID)
}

func normalizeMessageID(messageID string) (string, error) {
	if messageID == "" {
		return "", invalidMessageID("Message-ID is empty")
	}
	if strings.TrimSpace(messageID) != messageID {
		return "", invalidMessageID("Message-ID must not have surrounding whitespace")
	}
	hasOpening := strings.HasPrefix(messageID, "<")
	hasClosing := strings.HasSuffix(messageID, ">")
	if hasOpening != hasClosing {
		return "", invalidMessageID("Message-ID must have both angle brackets or neither")
	}

	inner := messageID
	if hasOpening {
		if len(messageID) <= 2 {
			return "", invalidMessageID("Message-ID is empty inside angle brackets")
		}
		inner = messageID[1 : len(messageID)-1]
	}
	quoted := false
	escaped := false
	for _, r := range inner {
		if unicode.IsControl(r) {
			return "", invalidMessageID("Message-ID contains a control character")
		}
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			quoted = !quoted
			continue
		}
		if r == '<' || r == '>' {
			return "", invalidMessageID("Message-ID contains an embedded angle bracket")
		}
		if unicode.IsSpace(r) && !quoted {
			return "", invalidMessageID("Message-ID contains unquoted whitespace")
		}
	}
	if escaped || quoted {
		return "", invalidMessageID("Message-ID has an unterminated escape or quote")
	}
	return "<" + inner + ">", nil
}

func invalidMessageID(message string) error {
	return &transport.TransportError{
		Code:    transport.CodeIMAPInvalidValue,
		Message: message,
	}
}

func (c *Client) doUIDSearchDeleted(ctx context.Context, sess *session, tag string) ([]uint32, error) {
	return c.doUIDSearchCriteria(ctx, sess, tag, "DELETED")
}

func (c *Client) doUIDSearchCriteria(ctx context.Context, sess *session, tag, criteria string) ([]uint32, error) {
	return c.doUIDSearchCriteriaBounded(ctx, sess, tag, criteria, maxUIDSearchResults)
}

func (c *Client) doUIDSearchCriteriaBounded(
	ctx context.Context,
	sess *session,
	tag string,
	criteria string,
	resultLimit int,
) ([]uint32, error) {
	if resultLimit < 1 {
		return nil, &transport.TransportError{
			Code:    transport.CodeIMAPInvalidValue,
			Message: "IMAP UID SEARCH result limit must be positive",
		}
	}
	if err := c.setDeadline(ctx, sess); err != nil {
		return nil, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP UID SEARCH deadline")
	}
	if err := c.writeLine(sess, tag+" UID SEARCH "+criteria); err != nil {
		return nil, wrapIOError(ctx, err, transport.CodeIMAPMutationFailed, "IMAP UID SEARCH write")
	}

	var uids []uint32
	seenSearch := false
	seenUIDs := make(map[uint32]struct{})
	for {
		line, err := c.readLine(sess)
		if err != nil {
			return nil, wrapIOError(ctx, err, transport.CodeIMAPMutationFailed, "IMAP UID SEARCH read")
		}
		if strings.HasPrefix(line, tag+" ") {
			status := parseStatus(line, tag)
			if status == "OK" {
				if !seenSearch {
					return nil, malformedUIDSearchResponse(sess, "IMAP UID SEARCH completed without a SEARCH response")
				}
				return uids, nil
			}
			return nil, &transport.TransportError{
				Code:    transport.CodeIMAPMutationFailed,
				Message: "IMAP UID SEARCH failed: " + status,
			}
		}
		if strings.HasPrefix(line, "* SEARCH") {
			if line != "* SEARCH" && !strings.HasPrefix(line, "* SEARCH ") {
				return nil, malformedUIDSearchResponse(sess, "IMAP UID SEARCH response has an invalid SEARCH prefix")
			}
			if seenSearch {
				return nil, malformedUIDSearchResponse(sess, "IMAP UID SEARCH returned multiple SEARCH responses")
			}
			seenSearch = true
			fields := strings.Fields(line)
			for _, field := range fields[2:] {
				uid, parseErr := strconv.ParseUint(field, 10, 32)
				if parseErr != nil || uid == 0 {
					return nil, malformedUIDSearchResponse(
						sess, fmt.Sprintf("IMAP UID SEARCH returned invalid UID %q", field),
					)
				}
				if len(uids) >= resultLimit {
					sess.dirty = true
					if resultLimit == maxUIDSearchResults {
						return nil, malformedUIDSearchResponse(
							sess, fmt.Sprintf("IMAP UID SEARCH result count exceeds %d", maxUIDSearchResults),
						)
					}
					return nil, &transport.TransportError{
						Code: transport.CodeIMAPMessageUIDUnknown,
						Message: fmt.Sprintf(
							"IMAP metadata search exceeded the bounded %d-candidate limit; refusing to guess a UID",
							resultLimit,
						),
					}
				}
				parsedUID := uint32(uid)
				if _, duplicate := seenUIDs[parsedUID]; duplicate {
					return nil, malformedUIDSearchResponse(
						sess, fmt.Sprintf("IMAP UID SEARCH returned duplicate UID %d", parsedUID),
					)
				}
				seenUIDs[parsedUID] = struct{}{}
				uids = append(uids, parsedUID)
			}
		}
	}
}

func malformedUIDSearchResponse(sess *session, message string) error {
	sess.dirty = true
	return &transport.TransportError{
		Code:    transport.CodeIMAPResponseMalformed,
		Message: message,
	}
}

// SetFlags adds and removes IMAP flags on a message.
func (c *Client) SetFlags(ctx context.Context, cfg transport.ImapConfig, mailbox string, uid uint32, expectedUIDValidity uint32, addFlags, removeFlags []string) (transport.MutationEvidence, error) {
	ev := transport.MutationEvidence{
		OperationID: transport.MutationOperationID("STORE", cfg.Username, mailbox, uid, expectedUIDValidity,
			"+FLAGS "+strings.Join(addFlags, " ")+" -FLAGS "+strings.Join(removeFlags, " ")),
		Outcome: transport.MutationOutcomeNotStarted, SourceAccount: cfg.Username,
		Command: "STORE", Mailbox: mailbox, UID: uid, ExpectedUIDValidity: expectedUIDValidity,
		FlagsState: transport.FlagObservationUnverified,
	}
	if err := validateMessageUID(uid); err != nil {
		return ev, err
	}
	if err := validateFlagChanges(addFlags, removeFlags); err != nil {
		return ev, err
	}
	ps, release, err := c.acquireMutation(ctx, cfg)
	if err != nil {
		return ev, err
	}
	defer release()

	info, err := c.ensureSelectedFresh(ctx, ps, mailbox)
	if err != nil {
		return ev, err
	}
	if err := checkUIDValidity(expectedUIDValidity, info.uidvalidity); err != nil {
		return ev, err
	}

	ev.UIDValidity = info.uidvalidity
	return c.setFlagsAndVerify(ctx, ps.sess, ev, addFlags, removeFlags, info.permissions)
}

// checkUIDValidity rejects an identity-sensitive operation before it runs when
// the mailbox was rebuilt or UIDVALIDITY is unknown: the stored UID may
// address a different message.
func checkUIDValidity(expected, observed uint32) error {
	if expected == 0 || observed == 0 {
		return &transport.TransportError{
			Code: transport.CodeIMAPUIDValidityUnknown,
			Message: fmt.Sprintf(
				"mailbox UIDVALIDITY is unknown (expected %d, observed %d); refusing the operation; refresh mailbox state and rerun the command",
				expected, observed,
			),
		}
	}
	if expected != 0 && observed != 0 && expected != observed {
		return &transport.TransportError{
			Code: "mailbox_uidvalidity_changed",
			Message: fmt.Sprintf(
				"mailbox was rebuilt between resolution and mutation (UIDVALIDITY %d -> %d); message moved or mailbox rebuilt; rerun the command",
				expected, observed,
			),
		}
	}
	return nil
}

func validateMessageUID(uid uint32) error {
	if uid != 0 {
		return nil
	}
	return &transport.TransportError{
		Code:    transport.CodeIMAPMessageUIDUnknown,
		Message: "message UID is unresolved; refusing the IMAP operation",
	}
}

func (c *Client) doCommandResponse(ctx context.Context, sess *session, cmd string) (string, string, error) {
	tag := cmd[:strings.Index(cmd, " ")]
	if err := c.setDeadline(ctx, sess); err != nil {
		return "", "", wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP command deadline")
	}
	if err := c.writeLine(sess, cmd); err != nil {
		return "", "", wrapIOError(ctx, err, transport.CodeIMAPMutationFailed, "IMAP command write")
	}
	status, text, err := c.readFinal(ctx, sess, tag)
	if err != nil {
		return "", "", err
	}
	if status == "OK" {
		return status, text, nil
	}
	return status, text, &transport.TransportError{
		Code:    transport.CodeIMAPMutationFailed,
		Message: "IMAP command failed: " + status + " " + text,
	}
}

func (c *Client) CopyMessage(ctx context.Context, cfg transport.ImapConfig, srcMailbox string, uid uint32, expectedUIDValidity uint32, dstMailbox string) (transport.MutationEvidence, error) {
	ev := transport.MutationEvidence{
		OperationID:         transport.MutationOperationID("COPY", cfg.Username, srcMailbox, uid, expectedUIDValidity, dstMailbox),
		Outcome:             transport.MutationOutcomeNotStarted,
		SourceAccount:       cfg.Username,
		Command:             "COPY",
		Mailbox:             srcMailbox,
		TargetMailbox:       dstMailbox,
		UID:                 uid,
		ExpectedUIDValidity: expectedUIDValidity,
	}
	if err := validateMessageUID(uid); err != nil {
		return ev, err
	}
	ps, release, err := c.acquireMutation(ctx, cfg)
	if err != nil {
		return ev, err
	}
	defer release()
	info, err := c.ensureSelectedFresh(ctx, ps, srcMailbox)
	if err != nil {
		return ev, err
	}
	if err := checkUIDValidity(expectedUIDValidity, info.uidvalidity); err != nil {
		return ev, err
	}
	ev.UIDValidity = info.uidvalidity

	quotedDestination, err := safeQuoteIMAP(dstMailbox)
	if err != nil {
		return ev, err
	}
	cmd := fmt.Sprintf("%s UID COPY %d %s", ps.sess.nextTag(), uid, quotedDestination)
	ev.Outcome = transport.MutationOutcomeAttempted
	status, text, responseCodes, err := c.doCopyCommandResponse(ctx, ps.sess, cmd)
	if err != nil {
		if status != "" {
			ev.ServerResponse = joinIMAPResponse(status, text)
			if strings.EqualFold(status, "NO") || strings.EqualFold(status, "BAD") {
				ev.Outcome = transport.MutationOutcomeRejected
				return ev, err
			}
		}
		ev.Outcome = transport.MutationOutcomeUnknown
		return ev, copyOutcomeUnknown(ev, err)
	}
	ev.ServerResponse = joinIMAPResponse(status, text)
	if err := applyCopyUIDEvidence(&ev, responseCodes, uid); err != nil {
		ev.Outcome = transport.MutationOutcomeUnknown
		return ev, copyOutcomeUnknown(ev, err)
	}
	ev.Outcome = transport.MutationOutcomeCompleted
	return ev, nil
}

func (c *Client) doCopyCommandResponse(ctx context.Context, sess *session, cmd string) (string, string, []string, error) {
	separator := strings.IndexByte(cmd, ' ')
	if separator <= 0 {
		return "", "", nil, &transport.TransportError{
			Code:    transport.CodeIMAPInvalidValue,
			Message: "IMAP COPY command has no tag separator",
		}
	}
	tag := cmd[:separator]
	if err := c.setDeadline(ctx, sess); err != nil {
		return "", "", nil, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP COPY deadline")
	}
	if err := c.writeLine(sess, cmd); err != nil {
		return "", "", nil, wrapIOError(ctx, err, transport.CodeIMAPMutationFailed, "IMAP COPY write")
	}
	status, text, responseCodes, err := c.readFinalWithCodes(
		ctx, sess, tag, transport.CodeIMAPMutationFailed, "IMAP COPY final response read",
	)
	if err != nil {
		return status, text, responseCodes, err
	}
	if strings.EqualFold(status, "OK") {
		return status, text, responseCodes, nil
	}
	return status, text, responseCodes, &transport.TransportError{
		Code:    transport.CodeIMAPMutationFailed,
		Message: "IMAP COPY failed: " + status + " " + text,
	}
}

func parseCopyUIDResponse(responseCode string, sourceUID uint32) (uint32, uint32, error) {
	fields := strings.Fields(responseCode)
	if len(fields) != 4 || !strings.EqualFold(fields[0], "COPYUID") {
		return 0, 0, fmt.Errorf("malformed IMAP COPYUID response code %q", responseCode)
	}
	uidValidity, err := parsePositiveUIDValue(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("malformed IMAP COPYUID UIDVALIDITY: %w", err)
	}
	returnedSourceUID, err := parsePositiveUIDValue(fields[2])
	if err != nil {
		return 0, 0, fmt.Errorf("malformed IMAP COPYUID source UID: %w", err)
	}
	destinationUID, err := parsePositiveUIDValue(fields[3])
	if err != nil {
		return 0, 0, fmt.Errorf("malformed IMAP COPYUID destination UID: %w", err)
	}
	if returnedSourceUID != sourceUID {
		return 0, 0, fmt.Errorf(
			"IMAP COPYUID source UID %d does not match requested UID %d",
			returnedSourceUID, sourceUID,
		)
	}
	return uidValidity, destinationUID, nil
}

func applyCopyUIDEvidence(evidence *transport.MutationEvidence, responseCodes []string, sourceUID uint32) error {
	var responseCode string
	for _, candidate := range responseCodes {
		fields := strings.Fields(candidate)
		if len(fields) == 0 || !strings.EqualFold(fields[0], "COPYUID") {
			continue
		}
		if responseCode != "" {
			return fmt.Errorf("IMAP COPY returned multiple COPYUID response codes")
		}
		responseCode = candidate
	}
	if responseCode == "" {
		return nil
	}
	uidValidity, destinationUID, err := parseCopyUIDResponse(responseCode, sourceUID)
	evidence.CopyUIDResponse = responseCode
	if err != nil {
		return err
	}
	evidence.CopyUIDValidity = uidValidity
	evidence.CopySourceUID = sourceUID
	evidence.CopyDestinationUID = destinationUID
	evidence.DestinationUIDValidity = uidValidity
	evidence.DestinationUID = destinationUID
	return nil
}

func parsePositiveUIDValue(value string) (uint32, error) {
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("UID value %q is not a positive uint32", value)
	}
	return uint32(parsed), nil
}

func joinIMAPResponse(status, text string) string {
	if text == "" {
		return status
	}
	return status + " " + text
}

func copyOutcomeUnknown(ev transport.MutationEvidence, cause error) error {
	return &transport.MutationOutcomeError{
		Code: transport.CodeIMAPCopyOutcomeUnknown,
		Message: fmt.Sprintf(
			"IMAP COPY outcome is unknown for operation %s; reconcile the destination before retrying",
			ev.OperationID,
		),
		Evidence: ev,
		Err:      cause,
	}
}

func moveOutcomeUnknown(ev transport.MutationEvidence, cause error) error {
	return &transport.MutationOutcomeError{
		Code: transport.CodeIMAPMoveOutcomeUnknown,
		Message: fmt.Sprintf(
			"IMAP MOVE outcome is unknown for operation %s; reconcile source and destination before retrying",
			ev.OperationID,
		),
		Evidence: ev,
		Err:      cause,
	}
}

func appendMutationEffect(evidence *transport.MutationEvidence, effect string) {
	for _, existing := range evidence.CompletedEffects {
		if existing == effect {
			return
		}
	}
	evidence.CompletedEffects = append(evidence.CompletedEffects, effect)
}

// MoveMessage moves a message by UID to dstMailbox using native UID MOVE with COPY+EXPUNGE fallback.
func (c *Client) MoveMessage(ctx context.Context, cfg transport.ImapConfig, srcMailbox string, uid uint32, expectedUIDValidity uint32, dstMailbox string) (transport.MutationEvidence, error) {
	evidence := transport.MutationEvidence{
		OperationID:         transport.MutationOperationID("MOVE", cfg.Username, srcMailbox, uid, expectedUIDValidity, dstMailbox),
		Outcome:             transport.MutationOutcomeNotStarted,
		SourceAccount:       cfg.Username,
		Command:             "MOVE",
		Mailbox:             srcMailbox,
		TargetMailbox:       dstMailbox,
		UID:                 uid,
		ExpectedUIDValidity: expectedUIDValidity,
	}
	if err := validateMessageUID(uid); err != nil {
		return evidence, err
	}
	ps, release, err := c.acquireMutation(ctx, cfg)
	if err != nil {
		return evidence, err
	}
	defer release()
	return c.moveMessage(ctx, ps, srcMailbox, uid, expectedUIDValidity, dstMailbox)
}

func (c *Client) moveMessage(ctx context.Context, ps *pooledSession, srcMailbox string, uid uint32, expectedUIDValidity uint32, dstMailbox string) (transport.MutationEvidence, error) {
	ev := transport.MutationEvidence{
		OperationID:         transport.MutationOperationID("MOVE", ps.key.username, srcMailbox, uid, expectedUIDValidity, dstMailbox),
		Outcome:             transport.MutationOutcomeNotStarted,
		SourceAccount:       ps.key.username,
		Command:             "MOVE",
		Mailbox:             srcMailbox,
		TargetMailbox:       dstMailbox,
		UID:                 uid,
		ExpectedUIDValidity: expectedUIDValidity,
	}
	if err := validateMessageUID(uid); err != nil {
		return ev, err
	}
	info, err := c.ensureSelectedFresh(ctx, ps, srcMailbox)
	if err != nil {
		return ev, err
	}
	if err := checkUIDValidity(expectedUIDValidity, info.uidvalidity); err != nil {
		return ev, err
	}
	ev.UIDValidity = info.uidvalidity
	sess := ps.sess
	quotedDestination, err := safeQuoteIMAP(dstMailbox)
	if err != nil {
		return ev, err
	}

	tag := sess.nextTag()
	cmd := fmt.Sprintf("%s UID MOVE %d %s", tag, uid, quotedDestination)
	ev.Outcome = transport.MutationOutcomeAttempted
	if err := c.setDeadline(ctx, sess); err != nil {
		ev.Outcome = transport.MutationOutcomeNotStarted
		return ev, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP MOVE deadline")
	}
	if err := c.writeLine(sess, cmd); err != nil {
		ev.Outcome = transport.MutationOutcomeUnknown
		return ev, moveOutcomeUnknown(ev, wrapIOError(ctx, err, transport.CodeIMAPMutationFailed, "IMAP MOVE write"))
	}
	status, text, err := c.readFinal(ctx, sess, tag)
	if err != nil {
		ev.Outcome = transport.MutationOutcomeUnknown
		return ev, moveOutcomeUnknown(ev, err)
	}
	if strings.EqualFold(status, "OK") {
		ev.ServerResponse = joinIMAPResponse(status, text)
		ev.Outcome = transport.MutationOutcomeCompleted
		ev.CompletedEffects = []string{"move"}
		return ev, nil
	}
	if status != "NO" && status != "BAD" {
		ev.ServerResponse = joinIMAPResponse(status, text)
		ev.Outcome = transport.MutationOutcomeRejected
		return ev, &transport.TransportError{
			Code:    transport.CodeIMAPMutationFailed,
			Message: "IMAP MOVE failed without fallback permission: " + status + " " + text,
		}
	}

	// Fallback for servers without UID MOVE: COPY + STORE \\Deleted, then
	// prefer UID EXPUNGE. If it is unavailable, leave cleanup deferred so an
	// unscoped EXPUNGE cannot remove another client's deleted message.
	copyCmd := fmt.Sprintf("%s UID COPY %d %s", sess.nextTag(), uid, quotedDestination)
	copyStatus, copyText, responseCodes, err := c.doCopyCommandResponse(ctx, sess, copyCmd)
	if err != nil {
		ev.ServerResponse = joinIMAPResponse(copyStatus, copyText)
		if strings.EqualFold(copyStatus, "NO") || strings.EqualFold(copyStatus, "BAD") {
			ev.Outcome = transport.MutationOutcomeRejected
			return ev, err
		}
		ev.Outcome = transport.MutationOutcomeUnknown
		return ev, moveOutcomeUnknown(ev, err)
	}
	ev.ServerResponse = joinIMAPResponse(copyStatus, copyText)
	appendMutationEffect(&ev, "copy")
	if err := applyCopyUIDEvidence(&ev, responseCodes, uid); err != nil {
		ev.Outcome = transport.MutationOutcomeUnknown
		return ev, moveOutcomeUnknown(ev, err)
	}

	copyResponse := ev.ServerResponse
	// Track the flag phase independently; COPY is already a proven effect.
	ev.Outcome, ev.FlagsState, ev.ServerResponse = transport.MutationOutcomeNotStarted, transport.FlagObservationUnverified, ""
	ev, err = c.setFlagsAndVerify(ctx, sess, ev, []string{"\\Deleted"}, nil, info.permissions)
	flagResponse := ev.ServerResponse
	ev.ServerResponse, ev.Outcome = copyResponse, transport.MutationOutcomePartial
	if flagResponse != "" {
		ev.ServerResponse += "; source flag response: " + flagResponse
	}
	if err != nil {
		return ev, moveOutcomeUnknown(ev, err)
	}
	appendMutationEffect(&ev, "source_flag")

	uidExpungeCmd := fmt.Sprintf("%s UID EXPUNGE %d", sess.nextTag(), uid)
	uidExpungeStatus, _, uidExpungeErr := c.doCommandResponse(ctx, sess, uidExpungeCmd)
	if uidExpungeErr == nil {
		ev.ServerResponse += " (fallback UID EXPUNGE)"
		ev.Outcome = transport.MutationOutcomeCompleted
		ev.ExpungeBranch = "uid_expunge"
		appendMutationEffect(&ev, "uid_expunge")
		return ev, nil
	}
	if !strings.EqualFold(uidExpungeStatus, "NO") && !strings.EqualFold(uidExpungeStatus, "BAD") {
		ev.Outcome = transport.MutationOutcomeUnknown
		return ev, moveOutcomeUnknown(ev, uidExpungeErr)
	}

	deletedUIDs, err := c.doUIDSearchDeleted(ctx, sess, sess.nextTag())
	if err != nil {
		ev.Outcome = transport.MutationOutcomeUnknown
		return ev, moveOutcomeUnknown(ev, err)
	}
	foreignDeleted := 0
	for _, deletedUID := range deletedUIDs {
		if deletedUID != uid {
			foreignDeleted++
		}
	}
	ev.ServerResponse = fmt.Sprintf(
		"%s (moved + flagged deleted; expunge deferred because UID EXPUNGE is unsupported; other deleted messages present: %d)",
		ev.ServerResponse, foreignDeleted,
	)
	ev.Outcome = transport.MutationOutcomeCompleted
	ev.ExpungeBranch = "deferred"
	ev.ForeignDeletedCount = foreignDeleted
	appendMutationEffect(&ev, "cleanup_deferred")
	return ev, nil
}

// DeleteMessage moves a message by UID to the Trash mailbox discovered via special-use flags.
func (c *Client) DeleteMessage(ctx context.Context, cfg transport.ImapConfig, srcMailbox string, uid uint32, expectedUIDValidity uint32) (transport.MutationEvidence, error) {
	if err := validateMessageUID(uid); err != nil {
		return transport.MutationEvidence{}, err
	}
	ps, release, err := c.acquireMutation(ctx, cfg)
	if err != nil {
		return transport.MutationEvidence{}, err
	}
	defer release()

	mboxes, err := c.listMailboxes(ctx, ps)
	if err != nil {
		return transport.MutationEvidence{}, err
	}

	trashBox, err := transport.ResolveTrashMailbox(mboxes)
	if err != nil {
		return transport.MutationEvidence{}, err
	}
	if strings.EqualFold(srcMailbox, trashBox) {
		return transport.MutationEvidence{}, &transport.TransportError{
			Code:    transport.CodeMessageAlreadyTrashed,
			Message: "message is already in the Trash mailbox",
		}
	}

	ev, err := c.moveMessage(ctx, ps, srcMailbox, uid, expectedUIDValidity, trashBox)
	if err != nil {
		return ev, err
	}
	ev.Command = "DELETE"
	return ev, nil
}

// FetchMessage fetches the raw RFC 5322 bytes for a message by UID using BODY.PEEK[].
// maxBytes bounds the announced literal.
func (c *Client) FetchMessage(ctx context.Context, cfg transport.ImapConfig, mailbox string, uid uint32, expectedUIDValidity uint32, maxBytes int64) ([]byte, error) {
	if err := validateFetchLimit(maxBytes); err != nil {
		return nil, err
	}
	if err := validateMessageUID(uid); err != nil {
		return nil, err
	}
	ps, release, err := c.acquire(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer release()

	info, err := c.ensureSelectedFresh(ctx, ps, mailbox)
	if err != nil {
		return nil, err
	}
	if err := checkUIDValidity(expectedUIDValidity, info.uidvalidity); err != nil {
		return nil, err
	}

	tag := ps.sess.nextTag()
	cmd := fmt.Sprintf("%s UID FETCH %d (BODY.PEEK[])", tag, uid)
	if err := c.setDeadline(ctx, ps.sess); err != nil {
		return nil, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP FETCH deadline")
	}
	if err := c.writeLine(ps.sess, cmd); err != nil {
		return nil, wrapIOError(ctx, err, transport.CodeIMAPFetchFailed, "IMAP FETCH write")
	}

	payload, err := c.readFetchLiteral(ctx, ps.sess, tag, uid, maxBytes)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

func validateFetchLimit(maxBytes int64) error {
	if maxBytes > 0 {
		return nil
	}
	return &transport.TransportError{
		Code:    transport.CodeIMAPInvalidValue,
		Message: fmt.Sprintf("IMAP FETCH byte limit must be positive, got %d", maxBytes),
	}
}

func (c *Client) readFetchLiteral(ctx context.Context, sess *session, tag string, requestedUID uint32, maxBytes int64) ([]byte, error) {
	if err := validateFetchLimit(maxBytes); err != nil {
		return nil, err
	}
	responseLimit := maxBytes
	if responseLimit <= int64(^uint64(0)>>1)-maxIMAPResponseLineBytes {
		responseLimit += maxIMAPResponseLineBytes
	}
	var payload []byte
	found := false
	bodyReady := false
	var mismatchedUID uint32
	mismatchSeen := false
	for {
		line, literals, err := c.readLogicalLineWithLiterals(
			sess, maxBytes, responseLimit, maxFetchLiteralCount,
		)
		if err != nil {
			var limitErr *literalLimitError
			if errors.As(err, &limitErr) {
				return nil, &transport.TransportError{
					Code: transport.CodeIMAPRawSourceTooLarge,
					Message: fmt.Sprintf(
						"IMAP FETCH announced %d bytes exceeding the %d byte raw-source cap; read the message from the local Mail store instead",
						limitErr.size, maxBytes,
					),
					Err: err,
				}
			}
			var malformed *malformedResponseError
			if errors.As(err, &malformed) {
				return nil, &transport.TransportError{
					Code:    transport.CodeIMAPResponseMalformed,
					Message: "IMAP FETCH response malformed",
					Err:     err,
				}
			}
			var literalErr *literalReadError
			if errors.As(err, &literalErr) {
				return nil, wrapIOError(ctx, literalErr, transport.CodeIMAPFetchFailed, "IMAP FETCH read literal bytes")
			}
			return nil, wrapIOError(ctx, err, transport.CodeIMAPFetchFailed, "IMAP FETCH read")
		}
		if strings.HasPrefix(line, tag+" ") {
			status := parseStatus(line, tag)
			if status == "OK" {
				if !found {
					if mismatchSeen {
						sess.dirty = true
						return nil, &transport.TransportError{
							Code: transport.CodeIMAPMessageUIDMismatch,
							Message: fmt.Sprintf(
								"IMAP FETCH returned UID %d for requested UID %d",
								mismatchedUID, requestedUID,
							),
						}
					}
					return nil, &transport.TransportError{
						Code:    transport.CodeIMAPMessageNotFound,
						Message: "message not returned by IMAP FETCH",
					}
				}
				if !bodyReady {
					return nil, &transport.TransportError{
						Code:    transport.CodeIMAPMessageNotFound,
						Message: "message BODY value not returned by IMAP FETCH",
					}
				}
				return payload, nil
			}
			return nil, &transport.TransportError{
				Code:    transport.CodeIMAPFetchFailed,
				Message: "IMAP FETCH failed: " + status,
			}
		}
		if !isFetchResponseCandidate(line) {
			continue
		}
		parsed, err := parseFetchResponse(line, literals)
		if err != nil {
			sess.dirty = true
			return nil, &transport.TransportError{
				Code:    transport.CodeIMAPResponseMalformed,
				Message: "IMAP FETCH response malformed",
				Err:     err,
			}
		}
		if !parsed.bodyPresent {
			continue
		}
		if !parsed.uidPresent || parsed.uid != requestedUID {
			if !mismatchSeen {
				mismatchedUID = parsed.uid
				mismatchSeen = true
			}
			continue
		}
		if found {
			sess.dirty = true
			return nil, &transport.TransportError{
				Code:    transport.CodeIMAPResponseMalformed,
				Message: "IMAP FETCH returned duplicate BODY values for the requested UID",
			}
		}
		found = true
		if parsed.bodyLiteral {
			payload = parsed.body
			bodyReady = true
		}
	}
}

func parseFetchUID(line string) (uint32, bool) {
	parsed, err := parseFetchResponse(line, nil)
	if err != nil || !parsed.uidPresent {
		return 0, false
	}
	return parsed.uid, true
}

type fetchResponse struct {
	sequence     uint32
	uid          uint32
	uidPresent   bool
	flags        []string
	flagsPresent bool
	body         []byte
	bodyPresent  bool
	bodyLiteral  bool
}

type fetchValueKind uint8

const (
	fetchValueAtom fetchValueKind = iota
	fetchValueQuoted
	fetchValueNil
	fetchValueLiteral
	fetchValueList
)

type fetchValue struct {
	kind    fetchValueKind
	text    string
	literal []byte
}

type fetchResponseParser struct {
	input       string
	literals    [][]byte
	nextLiteral int
	position    int
	sequence    uint32
}

func parseFetchResponse(line string, literals [][]byte) (fetchResponse, error) {
	parser := fetchResponseParser{input: line, literals: literals}
	if err := parser.parsePrefix(); err != nil {
		return fetchResponse{}, err
	}
	response := fetchResponse{sequence: parser.sequence}
	for {
		parser.skipSpace()
		if parser.position >= len(parser.input) {
			return fetchResponse{}, errors.New("unterminated FETCH attribute list")
		}
		if parser.input[parser.position] == ')' {
			parser.position++
			parser.skipSpace()
			if parser.position != len(parser.input) {
				return fetchResponse{}, errors.New("trailing FETCH response data")
			}
			if parser.nextLiteral != len(literals) {
				return fetchResponse{}, errors.New("unused FETCH response literal")
			}
			return response, nil
		}
		key, err := parser.parseAttributeKey()
		if err != nil {
			return fetchResponse{}, err
		}
		if parser.position >= len(parser.input) || !isSpace(parser.input[parser.position]) {
			return fetchResponse{}, fmt.Errorf("FETCH attribute %q has no separating space", key)
		}
		valueStart := parser.position
		value, err := parser.parseValue(0)
		if err != nil {
			return fetchResponse{}, fmt.Errorf("FETCH attribute %q: %w", key, err)
		}
		if parser.position < len(parser.input) && !isSpace(parser.input[parser.position]) && parser.input[parser.position] != ')' {
			return fetchResponse{}, fmt.Errorf("FETCH attribute %q value has no separator", key)
		}
		switch {
		case strings.EqualFold(key, "FLAGS"):
			if response.flagsPresent {
				return fetchResponse{}, errors.New("duplicate FETCH FLAGS attribute")
			}
			flags, err := parseFlagListValue(parser.input[valueStart:parser.position], false)
			if err != nil {
				return fetchResponse{}, err
			}
			response.flags, response.flagsPresent = flags, true
		case strings.EqualFold(key, "UID"):
			if response.uidPresent {
				return fetchResponse{}, errors.New("duplicate FETCH UID attribute")
			}
			uid, err := parseFetchUIDValue(value)
			if err != nil {
				return fetchResponse{}, err
			}
			response.uid = uid
			response.uidPresent = true
		case isFetchBodyAttribute(key):
			if response.bodyPresent {
				return fetchResponse{}, fmt.Errorf("duplicate FETCH BODY attribute %q", key)
			}
			if value.kind != fetchValueLiteral && value.kind != fetchValueQuoted && value.kind != fetchValueNil {
				return fetchResponse{}, fmt.Errorf("FETCH BODY value is not an nstring")
			}
			response.bodyPresent = true
			switch value.kind {
			case fetchValueLiteral:
				response.body = value.literal
				response.bodyLiteral = true
			case fetchValueQuoted:
				response.body = []byte(value.text)
				response.bodyLiteral = true
			}
		}
	}
}

func (p *fetchResponseParser) parsePrefix() error {
	if len(p.input) < 2 || p.input[0] != '*' || !isSpace(p.input[1]) {
		return errors.New("FETCH response does not start with an untagged response")
	}
	p.position = 2
	sequence, err := p.parseAtom()
	if err != nil {
		return fmt.Errorf("FETCH response sequence: %w", err)
	}
	number, err := parsePositiveUIDValue(sequence)
	if err != nil {
		return fmt.Errorf("invalid FETCH response sequence %q", sequence)
	}
	p.sequence = number
	p.skipSpace()
	command, err := p.parseAtom()
	if err != nil || !strings.EqualFold(command, "FETCH") {
		return fmt.Errorf("expected FETCH response command, got %q", command)
	}
	if p.position >= len(p.input) || !isSpace(p.input[p.position]) {
		return errors.New("FETCH response command has no separating space")
	}
	p.skipSpace()
	if p.position >= len(p.input) || p.input[p.position] != '(' {
		return errors.New("FETCH response has no attribute list")
	}
	p.position++
	return nil
}

func (p *fetchResponseParser) parseAttributeKey() (string, error) {
	p.skipSpace()
	start := p.position
	if start >= len(p.input) || p.input[start] == '(' || p.input[start] == ')' || p.input[start] == imapLiteralMarker[0] {
		return "", errors.New("FETCH attribute name is missing")
	}
	for p.position < len(p.input) {
		c := p.input[p.position]
		if isSpace(c) || c == '(' || c == ')' {
			break
		}
		if c == '[' {
			p.position++
			if err := p.consumeBracketSection(); err != nil {
				return "", err
			}
			if p.position < len(p.input) && p.input[p.position] == '<' {
				if err := p.consumeAngleSection(); err != nil {
					return "", err
				}
			}
			break
		}
		p.position++
	}
	if start == p.position {
		return "", errors.New("FETCH attribute name is empty")
	}
	return p.input[start:p.position], nil
}

func (p *fetchResponseParser) consumeBracketSection() error {
	depth := 1
	for p.position < len(p.input) {
		c := p.input[p.position]
		p.position++
		switch c {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return nil
			}
		case '\\':
			if p.position < len(p.input) {
				p.position++
			}
		}
	}
	return errors.New("unterminated FETCH attribute section")
}

func (p *fetchResponseParser) consumeAngleSection() error {
	start := p.position
	p.position++
	for p.position < len(p.input) {
		if p.input[p.position] == '>' {
			if p.position == start+1 {
				return errors.New("empty FETCH partial section")
			}
			p.position++
			return nil
		}
		if isSpace(p.input[p.position]) || p.input[p.position] == '(' || p.input[p.position] == ')' {
			return errors.New("invalid FETCH partial section")
		}
		if p.input[p.position] < '0' || p.input[p.position] > '9' {
			return errors.New("FETCH partial section is not decimal")
		}
		p.position++
	}
	return errors.New("unterminated FETCH partial section")
}

func (p *fetchResponseParser) parseValue(depth int) (fetchValue, error) {
	p.skipSpace()
	if p.position >= len(p.input) {
		return fetchValue{}, errors.New("FETCH attribute value is missing")
	}
	if depth > maxFetchNestingDepth {
		return fetchValue{}, fmt.Errorf("FETCH value nesting exceeds %d", maxFetchNestingDepth)
	}
	switch p.input[p.position] {
	case imapLiteralMarker[0]:
		if p.nextLiteral >= len(p.literals) {
			return fetchValue{}, errors.New("missing FETCH response literal")
		}
		value := fetchValue{kind: fetchValueLiteral, literal: p.literals[p.nextLiteral]}
		p.nextLiteral++
		p.position++
		return value, nil
	case '"':
		value, rest, err := parseQuoted(p.input[p.position:])
		if err != nil {
			return fetchValue{}, err
		}
		consumed := len(p.input[p.position:]) - len(rest)
		p.position += consumed
		return fetchValue{kind: fetchValueQuoted, text: value}, nil
	case '(':
		p.position++
		for {
			p.skipSpace()
			if p.position >= len(p.input) {
				return fetchValue{}, errors.New("unterminated FETCH value list")
			}
			if p.input[p.position] == ')' {
				p.position++
				return fetchValue{kind: fetchValueList}, nil
			}
			if _, err := p.parseValue(depth + 1); err != nil {
				return fetchValue{}, err
			}
			if p.position < len(p.input) && !isSpace(p.input[p.position]) && p.input[p.position] != ')' {
				return fetchValue{}, errors.New("FETCH list value has no separator")
			}
		}
	case ')':
		return fetchValue{}, errors.New("FETCH attribute value starts with closing parenthesis")
	default:
		atom, err := p.parseAtom()
		if err != nil {
			return fetchValue{}, err
		}
		if strings.EqualFold(atom, "NIL") {
			return fetchValue{kind: fetchValueNil}, nil
		}
		return fetchValue{kind: fetchValueAtom, text: atom}, nil
	}
}

func (p *fetchResponseParser) parseAtom() (string, error) {
	start := p.position
	for p.position < len(p.input) && !isSpace(p.input[p.position]) && p.input[p.position] != '(' && p.input[p.position] != ')' {
		if p.input[p.position] == imapLiteralMarker[0] {
			return "", errors.New("embedded FETCH response literal")
		}
		p.position++
	}
	if start == p.position {
		return "", errors.New("FETCH atom is empty")
	}
	return p.input[start:p.position], nil
}

func (p *fetchResponseParser) skipSpace() {
	for p.position < len(p.input) && isSpace(p.input[p.position]) {
		p.position++
	}
}

func parseFetchUIDValue(value fetchValue) (uint32, error) {
	if value.kind != fetchValueAtom {
		return 0, errors.New("FETCH UID value is not an atom")
	}
	uid, err := strconv.ParseUint(value.text, 10, 32)
	if err != nil || uid == 0 {
		if err == nil {
			err = errors.New("value must be positive")
		}
		return 0, fmt.Errorf("invalid FETCH UID value %q: %w", value.text, err)
	}
	return uint32(uid), nil
}

func isFetchBodyAttribute(key string) bool {
	upper := strings.ToUpper(key)
	return strings.HasPrefix(upper, "BODY[") || strings.HasPrefix(upper, "BODY.PEEK[")
}

func isFetchResponseCandidate(line string) bool {
	if len(line) < 3 || line[0] != '*' || !isSpace(line[1]) {
		return false
	}
	position := 2
	for position < len(line) && !isSpace(line[position]) {
		position++
	}
	if position == len(line) {
		return false
	}
	for position < len(line) && isSpace(line[position]) {
		position++
	}
	start := position
	for position < len(line) && !isSpace(line[position]) && line[position] != '(' && line[position] != ')' {
		position++
	}
	return start != position && strings.EqualFold(line[start:position], "FETCH")
}

// CheckStatus queries server message counts, unseen count, and UIDs via IMAP STATUS.
func (c *Client) CheckStatus(ctx context.Context, cfg transport.ImapConfig, mailbox string) (transport.MailboxStatus, error) {
	var status transport.MailboxStatus
	status.Mailbox = mailbox

	ps, release, err := c.acquireIndependent(ctx, cfg)
	if err != nil {
		return status, err
	}
	defer release()

	quotedMailbox, err := safeQuoteIMAP(mailbox)
	if err != nil {
		return status, err
	}
	tag := ps.sess.nextTag()
	cmd := fmt.Sprintf("%s STATUS %s (MESSAGES UNSEEN UIDNEXT UIDVALIDITY)", tag, quotedMailbox)
	if err := c.setDeadline(ctx, ps.sess); err != nil {
		return status, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP STATUS deadline")
	}
	if err := c.writeLine(ps.sess, cmd); err != nil {
		return status, wrapIOError(ctx, err, transport.CodeIMAPMailboxNotFound, "IMAP STATUS write")
	}

	seenStatus := false
	for {
		line, literals, err := c.readLineWithLiteral(ps.sess)
		if err != nil {
			var malformed *malformedResponseError
			if errors.As(err, &malformed) {
				return status, malformedStatusResponse(ps.sess, malformed)
			}
			return status, wrapIOError(ctx, err, transport.CodeIMAPMailboxNotFound, "IMAP STATUS read")
		}
		if strings.HasPrefix(line, tag+" ") {
			st := parseStatus(line, tag)
			if st == "OK" {
				if !seenStatus {
					return status, malformedStatusResponse(ps.sess, errors.New("STATUS completed without a STATUS response"))
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
				return status, malformedStatusResponse(ps.sess, errors.New("multiple STATUS responses"))
			}
			parsed, parseErr := parseStatusLine(line, literals)
			if parseErr != nil {
				return status, malformedStatusResponse(ps.sess, parseErr)
			}
			if parsed.Mailbox != mailbox {
				return status, malformedStatusResponse(
					ps.sess, fmt.Errorf("STATUS mailbox %q does not match requested mailbox %q", parsed.Mailbox, mailbox),
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
