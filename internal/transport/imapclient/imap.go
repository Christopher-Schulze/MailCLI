// Package imapclient implements a minimal IMAP4rev1 client for mirroring a
// sent message into the account's Sent mailbox.
//
// Implemented subset:
//   - Implicit TLS connection over port 993.
//   - Initial greeting (ignored).
//   - LOGIN with quoted credentials.
//   - LIST "" "*" for mailbox discovery.
//   - SELECT to make a mailbox active for SEARCH.
//   - SEARCH HEADER Message-ID "<id>".
//   - APPEND <mailbox> (\Seen) {length} with a synchronizing literal.
//   - LOGOUT.
//
// Response parsing is intentionally minimal: untagged "* ..." lines,
// continuation "+ ..." lines, and a final tagged "tag OK|NO|BAD ..." line.
// Quoted strings are unescaped; IMAP literals sent by the server are not
// supported because the providers targeted by this client send quoted names.
package imapclient

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"mailcli/internal/transport"
)

const (
	flagSent = "\\Sent"
	flagSeen = "\\Seen"
)

// Client is a minimal IMAPv4 client that can mirror a message into the Sent
// mailbox. The zero value is usable; TLSConfig may be set to override the
// default TLS configuration (for tests).
type Client struct {
	TLSConfig *tls.Config
}

// New returns a new Client.
func New() *Client {
	return &Client{}
}

// AppendToSent implements transport.SentMirror.
func (c *Client) AppendToSent(ctx context.Context, cfg transport.ImapConfig, msg []byte, messageID string) (transport.AppendEvidence, error) {
	var empty transport.AppendEvidence

	if cfg.Host == "" {
		return empty, &transport.TransportError{
			Code:    transport.CodeIMAPSentMailboxNotFound,
			Message: "IMAP host is empty",
		}
	}

	conn, err := c.dial(ctx, cfg)
	if err != nil {
		return empty, err
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)

	prefix := makeTagPrefix()
	var cmdNum int
	nextTag := func() string {
		cmdNum++
		return fmt.Sprintf("%s%04d", prefix, cmdNum)
	}

	if err := c.doLogin(ctx, conn, br, bw, nextTag(), cfg); err != nil {
		return empty, err
	}

	mailboxes, err := c.doList(ctx, conn, br, bw, nextTag())
	if err != nil {
		return empty, err
	}

	sentBox := pickSent(mailboxes)
	if sentBox == "" {
		return empty, &transport.TransportError{
			Code:    transport.CodeIMAPSentMailboxNotFound,
			Message: "no Sent mailbox found",
		}
	}

	if err := c.doSelect(ctx, conn, br, bw, nextTag(), sentBox); err != nil {
		return empty, err
	}

	found, err := c.doSearch(ctx, conn, br, bw, nextTag(), messageID)
	if err != nil {
		return empty, err
	}

	if found {
		_ = c.doLogout(ctx, conn, br, bw, nextTag())
		return transport.AppendEvidence{Mailbox: sentBox, Appended: false}, nil
	}

	if err := c.doAppend(ctx, conn, br, bw, nextTag(), sentBox, msg); err != nil {
		return empty, err
	}

	_ = c.doLogout(ctx, conn, br, bw, nextTag())
	return transport.AppendEvidence{Mailbox: sentBox, Appended: true}, nil
}

// mailbox carries the parsed name and special-use flags for a LIST response.
type mailbox struct {
	name  string
	flags []string
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
		NetDialer: &net.Dialer{Timeout: 10 * time.Second},
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

func (c *Client) doLogin(ctx context.Context, conn net.Conn, br *bufio.Reader, bw *bufio.Writer, tag string, cfg transport.ImapConfig) error {
	if err := c.setDeadline(ctx, conn); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP login deadline")
	}
	cmd := tag + " LOGIN " + quoteIMAP(cfg.Username) + " " + quoteIMAP(cfg.Password)
	if err := c.writeLine(bw, cmd); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPAuthFailed, "IMAP LOGIN write")
	}
	status, _, err := c.readFinal(ctx, br, tag)
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

func (c *Client) doList(ctx context.Context, conn net.Conn, br *bufio.Reader, bw *bufio.Writer, tag string) ([]mailbox, error) {
	if err := c.setDeadline(ctx, conn); err != nil {
		return nil, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP LIST deadline")
	}
	if err := c.writeLine(bw, tag+` LIST "" "*"`); err != nil {
		return nil, wrapIOError(ctx, err, transport.CodeIMAPSentMailboxNotFound, "IMAP LIST write")
	}

	var mailboxes []mailbox
	for {
		line, err := c.readLine(br)
		if err != nil {
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
		name, flags, perr := parseListLine(line)
		if perr != nil {
			continue
		}
		mailboxes = append(mailboxes, mailbox{name: name, flags: flags})
	}
}

func (c *Client) doSelect(ctx context.Context, conn net.Conn, br *bufio.Reader, bw *bufio.Writer, tag, mbox string) error {
	if err := c.setDeadline(ctx, conn); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP SELECT deadline")
	}
	if err := c.writeLine(bw, tag+" SELECT "+quoteIMAP(mbox)); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPSentMailboxNotFound, "IMAP SELECT write")
	}
	status, _, err := c.readFinal(ctx, br, tag)
	if err != nil {
		return err
	}
	if status == "OK" {
		return nil
	}
	return &transport.TransportError{
		Code:    transport.CodeIMAPSentMailboxNotFound,
		Message: "IMAP SELECT failed: " + status,
	}
}

func (c *Client) doSearch(ctx context.Context, conn net.Conn, br *bufio.Reader, bw *bufio.Writer, tag, messageID string) (bool, error) {
	if err := c.setDeadline(ctx, conn); err != nil {
		return false, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP SEARCH deadline")
	}
	if err := c.writeLine(bw, tag+" SEARCH HEADER Message-ID "+quoteIMAP(messageID)); err != nil {
		return false, wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP SEARCH write")
	}

	found := false
	for {
		line, err := c.readLine(br)
		if err != nil {
			return false, wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP SEARCH read")
		}
		if strings.HasPrefix(line, tag+" ") {
			status := parseStatus(line, tag)
			if status == "OK" {
				return found, nil
			}
			return false, &transport.TransportError{
				Code:    transport.CodeIMAPAppendFailed,
				Message: "IMAP SEARCH failed: " + status,
			}
		}
		if !strings.HasPrefix(line, "* SEARCH") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 2 {
			found = true
		}
	}
}

func (c *Client) doAppend(ctx context.Context, conn net.Conn, br *bufio.Reader, bw *bufio.Writer, tag, mbox string, msg []byte) error {
	if err := c.setDeadline(ctx, conn); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP APPEND deadline")
	}
	cmd := tag + " APPEND " + quoteIMAP(mbox) + " (" + flagSeen + ") {" + strconv.Itoa(len(msg)) + "}"
	if err := c.writeLine(bw, cmd); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP APPEND write")
	}

	line, err := c.readLine(br)
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

	if _, err := bw.Write(msg); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP APPEND literal write")
	}
	if err := c.writeLine(bw, ""); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP APPEND literal CRLF")
	}

	status, _, err := c.readFinal(ctx, br, tag)
	if err != nil {
		return err
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

func (c *Client) doLogout(ctx context.Context, conn net.Conn, br *bufio.Reader, bw *bufio.Writer, tag string) error {
	if err := c.setDeadline(ctx, conn); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP LOGOUT deadline")
	}
	return c.writeLine(bw, tag+" LOGOUT")
}

func (c *Client) setDeadline(ctx context.Context, conn net.Conn) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	return conn.SetDeadline(deadline)
}

func (c *Client) writeLine(bw *bufio.Writer, line string) error {
	if _, err := bw.WriteString(line + "\r\n"); err != nil {
		return err
	}
	return bw.Flush()
}

func (c *Client) readLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *Client) readFinal(ctx context.Context, br *bufio.Reader, tag string) (string, string, error) {
	for {
		line, err := c.readLine(br)
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

func parseListLine(line string) (string, []string, error) {
	const prefix = "* LIST "
	if !strings.HasPrefix(line, prefix) {
		return "", nil, fmt.Errorf("not a LIST response")
	}
	s := line[len(prefix):]
	if !strings.HasPrefix(s, "(") {
		return "", nil, fmt.Errorf("no attribute list")
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
		return "", nil, fmt.Errorf("unterminated attribute list")
	}

	attrs := s[1 : end-1]
	flags, err := parseValues(attrs)
	if err != nil {
		return "", nil, err
	}

	rest := strings.TrimSpace(s[end:])
	_, rest, err = parseIMAPValue(rest)
	if err != nil {
		return "", nil, err
	}

	name, _, err := parseIMAPValue(rest)
	return name, flags, err
}

func parseValues(s string) ([]string, error) {
	var values []string
	s = strings.TrimSpace(s)
	for s != "" {
		v, rest, err := parseIMAPValue(s)
		if err != nil {
			return nil, err
		}
		values = append(values, v)
		s = strings.TrimSpace(rest)
	}
	return values, nil
}

func parseIMAPValue(s string) (string, string, error) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return "", "", fmt.Errorf("empty token")
	}
	if s[0] == '"' {
		return parseQuoted(s)
	}
	if len(s) >= 3 && strings.EqualFold(s[:3], "NIL") && (len(s) == 3 || isSpace(s[3])) {
		return "", s[3:], nil
	}

	i := 0
	for i < len(s) && !isSpace(s[i]) {
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

var fallbackSentMailboxes = []string{
	"Sent Messages",
	"Gesendet",
	"[Gmail]/Sent Mail",
}

func pickSent(mailboxes []mailbox) string {
	for _, m := range mailboxes {
		for _, f := range m.flags {
			if strings.EqualFold(f, flagSent) {
				return m.name
			}
		}
	}
	for _, want := range fallbackSentMailboxes {
		for _, m := range mailboxes {
			if strings.EqualFold(m.name, want) {
				return m.name
			}
		}
	}
	return ""
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
