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
// Quoted strings are unescaped. LIST literals are reconstructed before their
// mailbox values are parsed; malformed LIST responses fail the operation.
package imapclient

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"mailcli/internal/transport"
)

const (
	flagSeen = "\\Seen"
)

// Client is a minimal IMAPv4 client that can mirror a message into the Sent
// mailbox and perform message mutations. Authenticated connections are pooled
// per host/port/username: repeated operations within one process reuse a
// single TLS connection instead of dialing per command. IO-level failures
// mark the session dirty and the pool discards it, so the next operation
// reconnects; protocol rejections (NO/BAD, message not found) keep the
// session. The zero value is usable; TLSConfig may be set to override the
// default TLS configuration (for tests). Close logs out of every pooled
// session.
type Client struct {
	TLSConfig *tls.Config

	mu       sync.Mutex
	sessions map[string]*pooledSession
}

// New returns a new Client.
func New() *Client {
	return &Client{}
}

// pooledSession wraps one authenticated connection with its selected mailbox
// state, so follow-up commands on the same mailbox skip the SELECT round
// trip.
type pooledSession struct {
	sess        *session
	key         string
	selected    string
	uidvalidity uint32
	cancelWatch context.CancelFunc
}

func sessionKey(cfg transport.ImapConfig) string {
	return fmt.Sprintf("%s:%d/%s", cfg.Host, cfg.Port, cfg.Username)
}

// acquire returns the pooled session for cfg with the client mutex held.
// Operations on one client are therefore serialized (CLI invocations are
// sequential; library callers get safe sharing). The caller MUST call the
// returned release exactly once; it discards the session when the command
// context expired (the watcher force-closed the connection) or the session is
// dirty, and always unlocks.
func (c *Client) acquire(ctx context.Context, cfg transport.ImapConfig) (*pooledSession, func(), error) {
	c.mu.Lock()
	key := sessionKey(cfg)
	ps, ok := c.sessions[key]
	if !ok {
		sess, err := c.connect(ctx, cfg)
		if err != nil {
			c.mu.Unlock()
			return nil, nil, err
		}
		ps = &pooledSession{sess: sess, key: key}
		if c.sessions == nil {
			c.sessions = make(map[string]*pooledSession)
		}
		c.sessions[key] = ps
	}
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	ps.cancelWatch = cancelWatch
	conn := ps.sess.conn
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-watchCtx.Done():
		}
	}()
	release := func() {
		cancelWatch()
		if ctx.Err() != nil || ps.sess.dirty {
			delete(c.sessions, ps.key)
			_ = ps.sess.conn.Close()
		}
		c.mu.Unlock()
	}
	return ps, release, nil
}

// Close logs out of and closes every pooled session. Safe to call repeatedly.
func (c *Client) Close() error {
	c.mu.Lock()
	pooled := make([]*pooledSession, 0, len(c.sessions))
	for _, ps := range c.sessions {
		pooled = append(pooled, ps)
	}
	c.sessions = nil
	c.mu.Unlock()
	var joined []error
	for _, ps := range pooled {
		logoutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.doLogout(logoutCtx, ps.sess, ps.sess.nextTag())
		cancel()
		if err := ps.sess.conn.Close(); err != nil {
			joined = append(joined, err)
		}
	}
	return errors.Join(joined...)
}

// AppendToSent implements transport.SentMirror. It runs on its own dedicated
// connection: the LOGIN/LIST/SELECT/SEARCH/APPEND/LOGOUT sequence is
// self-contained and does not participate in the session pool.
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

	if err := c.doLogin(ctx, sess, sess.nextTag(), cfg); err != nil {
		return empty, err
	}

	mailboxes, err := c.doList(ctx, sess, sess.nextTag())
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

	if err := c.doSelect(ctx, sess, sess.nextTag(), sentBox); err != nil {
		return empty, err
	}

	found, err := c.doSearch(ctx, sess, sess.nextTag(), messageID)
	if err != nil {
		return empty, err
	}

	if found {
		_ = c.doLogout(ctx, sess, sess.nextTag())
		return transport.AppendEvidence{Mailbox: sentBox, Appended: false}, nil
	}

	if err := c.doAppend(ctx, sess, sess.nextTag(), sentBox, msg); err != nil {
		return empty, err
	}

	_ = c.doLogout(ctx, sess, sess.nextTag())
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

func (c *Client) doLogin(ctx context.Context, sess *session, tag string, cfg transport.ImapConfig) error {
	if err := c.setDeadline(ctx, sess); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP login deadline")
	}
	cmd := tag + " LOGIN " + quoteIMAP(cfg.Username) + " " + quoteIMAP(cfg.Password)
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
		name, flags, perr := parseListLine(line, literals...)
		if perr != nil {
			sess.dirty = true
			return nil, listResponseMalformed(perr)
		}
		mailboxes = append(mailboxes, mailbox{name: name, flags: flags})
	}
}

func (c *Client) doSelect(ctx context.Context, sess *session, tag, mbox string) error {
	if err := c.setDeadline(ctx, sess); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP SELECT deadline")
	}
	if err := c.writeLine(sess, tag+" SELECT "+quoteIMAP(mbox)); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPSentMailboxNotFound, "IMAP SELECT write")
	}
	status, _, err := c.readFinal(ctx, sess, tag)
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

func (c *Client) doSearch(ctx context.Context, sess *session, tag, messageID string) (bool, error) {
	if err := c.setDeadline(ctx, sess); err != nil {
		return false, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP SEARCH deadline")
	}
	if err := c.writeLine(sess, tag+" SEARCH HEADER Message-ID "+quoteIMAP(messageID)); err != nil {
		return false, wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP SEARCH write")
	}

	found := false
	for {
		line, err := c.readLine(sess)
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

func (c *Client) doAppend(ctx context.Context, sess *session, tag, mbox string, msg []byte) error {
	if err := c.setDeadline(ctx, sess); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP APPEND deadline")
	}
	cmd := tag + " APPEND " + quoteIMAP(mbox) + " (" + flagSeen + ") {" + strconv.Itoa(len(msg)) + "}"
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

	if _, err := sess.bw.Write(msg); err != nil {
		sess.dirty = true
		return wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP APPEND literal write")
	}
	if err := c.writeLine(sess, ""); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP APPEND literal CRLF")
	}

	status, _, err := c.readFinal(ctx, sess, tag)
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

func (c *Client) doLogout(ctx context.Context, sess *session, tag string) error {
	if err := c.setDeadline(ctx, sess); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP LOGOUT deadline")
	}
	return c.writeLine(sess, tag+" LOGOUT")
}

func (c *Client) setDeadline(ctx context.Context, sess *session) error {
	if err := ctx.Err(); err != nil {
		sess.dirty = true
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
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
	line, err := sess.br.ReadString('\n')
	if err != nil {
		sess.dirty = true
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
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
	var reconstructed strings.Builder
	var literals [][]byte
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
			reconstructed.WriteString(line)
			return reconstructed.String(), literals, nil
		}

		reconstructed.WriteString(prefix)
		reconstructed.WriteString(imapLiteralMarker)
		literal := make([]byte, size)
		if _, err := io.ReadFull(sess.br, literal); err != nil {
			sess.dirty = true
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return "", nil, &malformedResponseError{err: err}
			}
			return "", nil, err
		}
		literals = append(literals, literal)
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

func parseListLine(line string, literals ...[]byte) (string, []string, error) {
	parser := imapValueParser{literals: literals}
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
	flags, err := parser.parseValues(attrs)
	if err != nil {
		return "", nil, err
	}

	rest := strings.TrimSpace(s[end:])
	_, rest, err = parser.parse(rest)
	if err != nil {
		return "", nil, err
	}

	name, rest, err := parser.parse(rest)
	if err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(rest) != "" {
		return "", nil, fmt.Errorf("trailing LIST response data")
	}
	if parser.nextLiteral != len(literals) {
		return "", nil, fmt.Errorf("unused IMAP response literal")
	}
	return name, flags, nil
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

func parseValues(s string) ([]string, error) {
	var parser imapValueParser
	return parser.parseValues(s)
}

func parseIMAPValue(s string) (string, string, error) {
	var parser imapValueParser
	return parser.parse(s)
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

func pickSent(mailboxes []mailbox) string {
	infos := make([]transport.MailboxInfo, len(mailboxes))
	for index, candidate := range mailboxes {
		infos[index] = transport.MailboxInfo{Name: candidate.name, Flags: candidate.flags}
	}
	return transport.PickSentMailbox(infos)
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
	conn    net.Conn
	br      *bufio.Reader
	bw      *bufio.Writer
	nextTag func() string
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
	if err := c.doLogin(ctx, sess, sess.nextTag(), cfg); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return sess, nil
}

type selectInfo struct {
	uidvalidity uint32
	exists      int
}

func (c *Client) doSelectInfo(ctx context.Context, sess *session, tag, mbox string) (selectInfo, error) {
	var info selectInfo
	if err := c.setDeadline(ctx, sess); err != nil {
		return info, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP SELECT deadline")
	}
	if err := c.writeLine(sess, tag+" SELECT "+quoteIMAP(mbox)); err != nil {
		return info, wrapIOError(ctx, err, transport.CodeIMAPMailboxNotFound, "IMAP SELECT write")
	}
	for {
		line, err := c.readLine(sess)
		if err != nil {
			return info, wrapIOError(ctx, err, transport.CodeIMAPMailboxNotFound, "IMAP SELECT read")
		}
		if strings.HasPrefix(line, tag+" ") {
			status := parseStatus(line, tag)
			if status == "OK" {
				return info, nil
			}
			return info, &transport.TransportError{
				Code:    transport.CodeIMAPMailboxNotFound,
				Message: "IMAP SELECT failed: " + status,
			}
		}
		if strings.HasPrefix(line, "* ") {
			if strings.Contains(line, "UIDVALIDITY ") {
				info.uidvalidity = parseUIDValidity(line)
			}
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
}

func parseUIDValidity(line string) uint32 {
	idx := strings.Index(line, "UIDVALIDITY ")
	if idx == -1 {
		return 0
	}
	rest := line[idx+len("UIDVALIDITY "):]
	end := strings.IndexAny(rest, " ]")
	if end != -1 {
		rest = rest[:end]
	}
	v, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 32)
	if err != nil {
		return 0
	}
	return uint32(v)
}

// ListMailboxes returns all mailboxes on the IMAP server with their flags.
func (c *Client) ListMailboxes(ctx context.Context, cfg transport.ImapConfig) ([]transport.MailboxInfo, error) {
	ps, release, err := c.acquire(ctx, cfg)
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
		infos[i] = transport.MailboxInfo{Name: m.name, Flags: m.flags}
	}
	return infos, nil
}

// SearchUID resolves a Message-ID to its last IMAP UID in the specified
// mailbox and returns the total number of matching UIDs.
func (c *Client) SearchUID(ctx context.Context, cfg transport.ImapConfig, mailbox string, messageID string) (uint32, uint32, int, error) {
	ps, release, err := c.acquire(ctx, cfg)
	if err != nil {
		return 0, 0, 0, err
	}
	defer release()

	info, err := c.ensureSelected(ctx, ps, mailbox)
	if err != nil {
		return 0, 0, 0, err
	}

	uids, err := c.doUIDSearch(ctx, ps.sess, ps.sess.nextTag(), messageID)
	if err != nil {
		return 0, 0, 0, err
	}

	if len(uids) == 0 {
		return 0, info.uidvalidity, 0, &transport.TransportError{
			Code:    transport.CodeIMAPMessageNotFound,
			Message: fmt.Sprintf("message %s not found in mailbox %s", messageID, mailbox),
		}
	}
	return uids[len(uids)-1], info.uidvalidity, len(uids), nil
}

// ensureSelected switches the pooled session to mailbox when needed and
// returns the SELECT-time info. Repeated commands on the same mailbox reuse
// the cached uidvalidity instead of issuing another SELECT.
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

func (c *Client) doUIDSearch(ctx context.Context, sess *session, tag, messageID string) ([]uint32, error) {
	searchID := messageID
	if !strings.HasPrefix(searchID, "<") && !strings.HasSuffix(searchID, ">") {
		searchID = "<" + searchID + ">"
	}
	return c.doUIDSearchCriteria(ctx, sess, tag, "HEADER Message-ID "+quoteIMAP(searchID))
}

func (c *Client) doUIDSearchDeleted(ctx context.Context, sess *session, tag string) ([]uint32, error) {
	return c.doUIDSearchCriteria(ctx, sess, tag, "DELETED")
}

func (c *Client) doUIDSearchCriteria(ctx context.Context, sess *session, tag, criteria string) ([]uint32, error) {
	if err := c.setDeadline(ctx, sess); err != nil {
		return nil, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP UID SEARCH deadline")
	}
	if err := c.writeLine(sess, tag+" UID SEARCH "+criteria); err != nil {
		return nil, wrapIOError(ctx, err, transport.CodeIMAPMutationFailed, "IMAP UID SEARCH write")
	}

	var uids []uint32
	for {
		line, err := c.readLine(sess)
		if err != nil {
			return nil, wrapIOError(ctx, err, transport.CodeIMAPMutationFailed, "IMAP UID SEARCH read")
		}
		if strings.HasPrefix(line, tag+" ") {
			status := parseStatus(line, tag)
			if status == "OK" {
				return uids, nil
			}
			return nil, &transport.TransportError{
				Code:    transport.CodeIMAPMutationFailed,
				Message: "IMAP UID SEARCH failed: " + status,
			}
		}
		if strings.HasPrefix(line, "* SEARCH") {
			fields := strings.Fields(line)
			for _, field := range fields[2:] {
				if uid, err := strconv.ParseUint(field, 10, 32); err == nil {
					uids = append(uids, uint32(uid))
				}
			}
		}
	}
}

// SetFlags adds and removes IMAP flags on a message.
func (c *Client) SetFlags(ctx context.Context, cfg transport.ImapConfig, mailbox string, uid uint32, expectedUIDValidity uint32, addFlags, removeFlags []string) (transport.MutationEvidence, error) {
	var ev transport.MutationEvidence
	ps, release, err := c.acquire(ctx, cfg)
	if err != nil {
		return ev, err
	}
	defer release()

	info, err := c.ensureSelected(ctx, ps, mailbox)
	if err != nil {
		return ev, err
	}
	if err := checkUIDValidity(expectedUIDValidity, info.uidvalidity); err != nil {
		return ev, err
	}

	var lastStatus string
	if len(addFlags) > 0 {
		cmd := fmt.Sprintf("%s UID STORE %d +FLAGS (%s)", ps.sess.nextTag(), uid, strings.Join(addFlags, " "))
		status, err := c.doCommand(ctx, ps.sess, cmd)
		if err != nil {
			return ev, err
		}
		lastStatus = status
	}

	if len(removeFlags) > 0 {
		cmd := fmt.Sprintf("%s UID STORE %d -FLAGS (%s)", ps.sess.nextTag(), uid, strings.Join(removeFlags, " "))
		status, err := c.doCommand(ctx, ps.sess, cmd)
		if err != nil {
			return ev, err
		}
		lastStatus = status
	}

	return transport.MutationEvidence{
		Command:             "STORE",
		ServerResponse:      lastStatus,
		Mailbox:             mailbox,
		UID:                 uid,
		UIDValidity:         info.uidvalidity,
		ExpectedUIDValidity: expectedUIDValidity,
	}, nil
}

// checkUIDValidity rejects a mutation before it runs when the mailbox was
// rebuilt between the SEARCH that resolved the UID and this SELECT: the
// stored UID would address a different message.
func checkUIDValidity(expected, observed uint32) error {
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

func (c *Client) doCommand(ctx context.Context, sess *session, cmd string) (string, error) {
	status, text, err := c.doCommandResponse(ctx, sess, cmd)
	if err != nil {
		return "", err
	}
	if text != "" {
		return status + " " + text, nil
	}
	return status, nil
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
	var ev transport.MutationEvidence
	ps, release, err := c.acquire(ctx, cfg)
	if err != nil {
		return ev, err
	}
	defer release()
	info, err := c.ensureSelected(ctx, ps, srcMailbox)
	if err != nil {
		return ev, err
	}
	if err := checkUIDValidity(expectedUIDValidity, info.uidvalidity); err != nil {
		return ev, err
	}

	cmd := fmt.Sprintf("%s UID COPY %d %s", ps.sess.nextTag(), uid, quoteIMAP(dstMailbox))
	status, err := c.doCommand(ctx, ps.sess, cmd)
	if err != nil {
		return ev, err
	}

	return transport.MutationEvidence{
		Command:             "COPY",
		ServerResponse:      status,
		Mailbox:             srcMailbox,
		TargetMailbox:       dstMailbox,
		UID:                 uid,
		UIDValidity:         info.uidvalidity,
		ExpectedUIDValidity: expectedUIDValidity,
	}, nil
}

// MoveMessage moves a message by UID to dstMailbox using native UID MOVE with COPY+EXPUNGE fallback.
func (c *Client) MoveMessage(ctx context.Context, cfg transport.ImapConfig, srcMailbox string, uid uint32, expectedUIDValidity uint32, dstMailbox string) (transport.MutationEvidence, error) {
	ps, release, err := c.acquire(ctx, cfg)
	if err != nil {
		return transport.MutationEvidence{}, err
	}
	defer release()
	return c.moveMessage(ctx, ps, srcMailbox, uid, expectedUIDValidity, dstMailbox)
}

func (c *Client) moveMessage(ctx context.Context, ps *pooledSession, srcMailbox string, uid uint32, expectedUIDValidity uint32, dstMailbox string) (transport.MutationEvidence, error) {
	var ev transport.MutationEvidence
	info, err := c.ensureSelected(ctx, ps, srcMailbox)
	if err != nil {
		return ev, err
	}
	if err := checkUIDValidity(expectedUIDValidity, info.uidvalidity); err != nil {
		return ev, err
	}
	sess := ps.sess

	tag := sess.nextTag()
	cmd := fmt.Sprintf("%s UID MOVE %d %s", tag, uid, quoteIMAP(dstMailbox))
	if err := c.setDeadline(ctx, sess); err != nil {
		return ev, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP MOVE deadline")
	}
	if err := c.writeLine(sess, cmd); err != nil {
		return ev, wrapIOError(ctx, err, transport.CodeIMAPMutationFailed, "IMAP MOVE write")
	}
	status, text, err := c.readFinal(ctx, sess, tag)
	if err == nil && status == "OK" {
		resp := status
		if text != "" {
			resp += " " + text
		}
		return transport.MutationEvidence{
			Command:             "MOVE",
			ServerResponse:      resp,
			Mailbox:             srcMailbox,
			TargetMailbox:       dstMailbox,
			UID:                 uid,
			UIDValidity:         info.uidvalidity,
			ExpectedUIDValidity: expectedUIDValidity,
		}, nil
	}

	// Fallback for servers without UID MOVE: COPY + STORE \\Deleted, then
	// prefer UID EXPUNGE so unrelated deleted messages cannot be removed.
	copyCmd := fmt.Sprintf("%s UID COPY %d %s", sess.nextTag(), uid, quoteIMAP(dstMailbox))
	copyStatus, err := c.doCommand(ctx, sess, copyCmd)
	if err != nil {
		return ev, err
	}

	storeCmd := fmt.Sprintf("%s UID STORE %d +FLAGS (\\Deleted)", sess.nextTag(), uid)
	if _, err := c.doCommand(ctx, sess, storeCmd); err != nil {
		return ev, err
	}

	uidExpungeCmd := fmt.Sprintf("%s UID EXPUNGE %d", sess.nextTag(), uid)
	uidExpungeStatus, _, uidExpungeErr := c.doCommandResponse(ctx, sess, uidExpungeCmd)
	if uidExpungeErr == nil {
		return transport.MutationEvidence{
			Command:             "MOVE",
			ServerResponse:      copyStatus + " (fallback UID EXPUNGE)",
			Mailbox:             srcMailbox,
			TargetMailbox:       dstMailbox,
			UID:                 uid,
			UIDValidity:         info.uidvalidity,
			ExpectedUIDValidity: expectedUIDValidity,
			ExpungeBranch:       "uid_expunge",
		}, nil
	}
	if !strings.EqualFold(uidExpungeStatus, "NO") && !strings.EqualFold(uidExpungeStatus, "BAD") {
		return ev, uidExpungeErr
	}

	deletedUIDs, err := c.doUIDSearchDeleted(ctx, sess, sess.nextTag())
	if err != nil {
		return ev, err
	}
	foreignDeleted := 0
	for _, deletedUID := range deletedUIDs {
		if deletedUID != uid {
			foreignDeleted++
		}
	}
	if foreignDeleted > 0 {
		return transport.MutationEvidence{
			Command:             "MOVE",
			ServerResponse:      fmt.Sprintf("%s (moved + flagged deleted; expunge deferred (other deleted messages present: %d))", copyStatus, foreignDeleted),
			Mailbox:             srcMailbox,
			TargetMailbox:       dstMailbox,
			UID:                 uid,
			UIDValidity:         info.uidvalidity,
			ExpectedUIDValidity: expectedUIDValidity,
			ExpungeBranch:       "deferred",
			ForeignDeletedCount: foreignDeleted,
		}, nil
	}

	expungeCmd := fmt.Sprintf("%s EXPUNGE", sess.nextTag())
	if _, err := c.doCommand(ctx, sess, expungeCmd); err != nil {
		return ev, err
	}

	return transport.MutationEvidence{
		Command:             "MOVE",
		ServerResponse:      copyStatus + " (fallback plain EXPUNGE)",
		Mailbox:             srcMailbox,
		TargetMailbox:       dstMailbox,
		UID:                 uid,
		UIDValidity:         info.uidvalidity,
		ExpectedUIDValidity: expectedUIDValidity,
		ExpungeBranch:       "plain_expunge",
	}, nil
}

// DeleteMessage moves a message by UID to the Trash mailbox discovered via special-use flags.
func (c *Client) DeleteMessage(ctx context.Context, cfg transport.ImapConfig, srcMailbox string, uid uint32, expectedUIDValidity uint32) (transport.MutationEvidence, error) {
	ps, release, err := c.acquire(ctx, cfg)
	if err != nil {
		return transport.MutationEvidence{}, err
	}
	defer release()

	mboxes, err := c.listMailboxes(ctx, ps)
	if err != nil {
		return transport.MutationEvidence{}, err
	}

	trashBox := transport.PickTrashMailbox(mboxes)
	if trashBox == "" {
		return transport.MutationEvidence{}, &transport.TransportError{
			Code:    transport.CodeIMAPMailboxNotFound,
			Message: "no Trash mailbox found on IMAP server",
		}
	}

	ev, err := c.moveMessage(ctx, ps, srcMailbox, uid, expectedUIDValidity, trashBox)
	if err != nil {
		return transport.MutationEvidence{}, err
	}
	ev.Command = "DELETE"
	return ev, nil
}

// FetchMessage fetches the raw RFC 5322 bytes for a message by UID using BODY.PEEK[].
// maxBytes bounds the announced literal (parity with the local raw-source
// cap); maxBytes <= 0 leaves the fetch uncapped for content hydration.
func (c *Client) FetchMessage(ctx context.Context, cfg transport.ImapConfig, mailbox string, uid uint32, maxBytes int64) ([]byte, error) {
	ps, release, err := c.acquire(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer release()

	if _, err := c.ensureSelected(ctx, ps, mailbox); err != nil {
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

	payload, err := c.readFetchLiteral(ctx, ps.sess, tag, maxBytes)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

func (c *Client) readFetchLiteral(ctx context.Context, sess *session, tag string, maxBytes int64) ([]byte, error) {
	var payload []byte
	found := false
	for {
		line, err := c.readLine(sess)
		if err != nil {
			return nil, wrapIOError(ctx, err, transport.CodeIMAPFetchFailed, "IMAP FETCH read")
		}
		if strings.HasPrefix(line, tag+" ") {
			status := parseStatus(line, tag)
			if status == "OK" {
				if !found {
					return nil, &transport.TransportError{
						Code:    transport.CodeIMAPMessageNotFound,
						Message: "message not returned by IMAP FETCH",
					}
				}
				return payload, nil
			}
			return nil, &transport.TransportError{
				Code:    transport.CodeIMAPFetchFailed,
				Message: "IMAP FETCH failed: " + status,
			}
		}
		if strings.HasPrefix(line, "* ") && strings.Contains(line, "FETCH ") {
			idx := strings.LastIndex(line, "{")
			if idx != -1 && strings.HasSuffix(line, "}") {
				lenStr := line[idx+1 : len(line)-1]
				length, perr := strconv.Atoi(lenStr)
				if perr == nil && length >= 0 {
					if maxBytes > 0 && int64(length) > maxBytes {
						// Refuse before buffering: the session is dirty
						// (release discards it) and the unread literal is
						// never parsed. Do not attempt to skip it.
						sess.dirty = true
						return nil, &transport.TransportError{
							Code: transport.CodeIMAPRawSourceTooLarge,
							Message: fmt.Sprintf(
								"IMAP FETCH announced %d bytes exceeding the %d byte raw-source cap; read the message from the local Mail store instead",
								length, maxBytes,
							),
						}
					}
					buf := make([]byte, length)
					if _, rerr := io.ReadFull(sess.br, buf); rerr != nil {
						sess.dirty = true
						return nil, wrapIOError(ctx, rerr, transport.CodeIMAPFetchFailed, "IMAP FETCH read literal bytes")
					}
					payload = buf
					found = true
				}
			}
		}
	}
}

// CheckStatus queries server message counts, unseen count, and UIDs via IMAP STATUS.
func (c *Client) CheckStatus(ctx context.Context, cfg transport.ImapConfig, mailbox string) (transport.MailboxStatus, error) {
	var status transport.MailboxStatus
	status.Mailbox = mailbox

	ps, release, err := c.acquire(ctx, cfg)
	if err != nil {
		return status, err
	}
	defer release()

	tag := ps.sess.nextTag()
	cmd := fmt.Sprintf("%s STATUS %s (MESSAGES UNSEEN UIDNEXT UIDVALIDITY)", tag, quoteIMAP(mailbox))
	if err := c.setDeadline(ctx, ps.sess); err != nil {
		return status, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP STATUS deadline")
	}
	if err := c.writeLine(ps.sess, cmd); err != nil {
		return status, wrapIOError(ctx, err, transport.CodeIMAPMailboxNotFound, "IMAP STATUS write")
	}

	for {
		line, err := c.readLine(ps.sess)
		if err != nil {
			return status, wrapIOError(ctx, err, transport.CodeIMAPMailboxNotFound, "IMAP STATUS read")
		}
		if strings.HasPrefix(line, tag+" ") {
			st := parseStatus(line, tag)
			if st == "OK" {
				return status, nil
			}
			return status, &transport.TransportError{
				Code:    transport.CodeIMAPMailboxNotFound,
				Message: "IMAP STATUS failed: " + st,
			}
		}
		if strings.HasPrefix(line, "* STATUS ") {
			parseStatusLine(line, &status)
		}
	}
}

func parseStatusLine(line string, st *transport.MailboxStatus) {
	idx := strings.Index(line, "(")
	end := strings.LastIndex(line, ")")
	if idx == -1 || end == -1 || end <= idx {
		return
	}
	fields := strings.Fields(line[idx+1 : end])
	for i := 0; i+1 < len(fields); i += 2 {
		key := strings.ToUpper(fields[i])
		val := fields[i+1]
		switch key {
		case "MESSAGES":
			if n, err := strconv.Atoi(val); err == nil {
				st.Messages = n
			}
		case "UNSEEN":
			if n, err := strconv.Atoi(val); err == nil {
				st.Unseen = n
			}
		case "UIDNEXT":
			if n, err := strconv.ParseUint(val, 10, 32); err == nil {
				st.UIDNext = uint32(n)
			}
		case "UIDVALIDITY":
			if n, err := strconv.ParseUint(val, 10, 32); err == nil {
				st.UIDValidity = uint32(n)
			}
		}
	}
}
