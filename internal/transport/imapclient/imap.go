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
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

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

const (
	// MaxListOperationResponseBytes includes physical lines, literal payloads, and tagged completion.
	MaxListOperationResponseBytes int64 = 32 << 20
	// MaxListOperationResponseLines counts untagged logical lines, including ignored lines.
	MaxListOperationResponseLines = 10_000
	// MaxListOperationMailboxes caps parsed results for one LIST command.
	MaxListOperationMailboxes = 10_000
)

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
	password, err := safeQuoteIMAPBytes(cfg.Password)
	if err != nil {
		return err
	}
	defer wipeBytes(password)
	cmd := make([]byte, 0, len(tag)+7+len(username)+1+len(password))
	cmd = append(cmd, tag...)
	cmd = append(cmd, " LOGIN "...)
	cmd = append(cmd, username...)
	cmd = append(cmd, ' ')
	cmd = append(cmd, password...)
	defer wipeBytes(cmd)
	if err := c.writeLineBytes(sess, cmd); err != nil {
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
		return info, wrapCommandIOError(ctx, err, "IMAP SELECT deadline")
	}
	quotedMailbox, err := safeQuoteIMAP(mbox)
	if err != nil {
		return info, err
	}
	if err := c.writeLine(sess, tag+" SELECT "+quotedMailbox); err != nil {
		return info, wrapCommandIOError(ctx, err, "IMAP SELECT write")
	}
	remaining := int64(maxFlagResponseBytes)
	for range maxFlagResponseCount {
		line, literals, err := c.readLogicalLineWithLiterals(sess, maxIMAPResponseLineBytes, remaining, maxFetchLiteralCount)
		if err != nil {
			return info, wrapCommandIOError(ctx, err, "IMAP SELECT read")
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
			status, statusErr := parseTaggedCompletionStatus(line, tag)
			if statusErr != nil {
				return info, malformedTaggedCommandResponse(sess, "SELECT", statusErr)
			}
			if status == "OK" {
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
