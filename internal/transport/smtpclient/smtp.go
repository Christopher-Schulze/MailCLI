// Package smtpclient implements the SMTP submission side of the send
// transport: dial, STARTTLS, AUTH PLAIN, envelope, DATA.
package smtpclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"mailcli/internal/transport"
)

const (
	dialTimeout = 10 * time.Second
	// commandBudget caps every short SMTP command (greeting, EHLO,
	// STARTTLS, AUTH, MAIL, RCPT, DATA start, final reply).
	commandBudget = transport.TransferCommandBudget
	// transferCap bounds the DATA phase even for the largest messages:
	// 512 MiB at the floor needs ~542 s, inside the 15 min cap.
	transferCap = transport.TransferBudgetCap
)

// Client submits fully composed RFC 5322 messages to an SMTP submission
// endpoint. It satisfies transport.Submitter.
type Client struct {
	tlsConfig *tls.Config
}

var _ transport.Submitter = (*Client)(nil)

// Option is a functional option for New.
type Option func(*Client)

// WithTLSConfig overrides the TLS configuration used for the STARTTLS
// handshake. Test-only hook so self-signed fake servers work; production
// callers keep the default verified configuration.
func WithTLSConfig(cfg *tls.Config) Option {
	return func(c *Client) { c.tlsConfig = cfg }
}

// New creates a Client.
func New(opts ...Option) *Client {
	c := &Client{}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Submit sends msg from from to rcpts (deduplicated, order-stable) over
// SMTP with STARTTLS and AUTH PLAIN.
func (c *Client) Submit(ctx context.Context, cfg transport.SubmitConfig, from string, rcpts []string, msg []byte) (transport.SubmitEvidence, error) {
	return c.SubmitReader(ctx, cfg, from, rcpts, messageID(msg), bytes.NewReader(msg), int64(len(msg)))
}

// SubmitReader streams a replayable RFC 5322 source through SMTP without
// retaining the complete message in memory.
func (c *Client) SubmitReader(ctx context.Context, cfg transport.SubmitConfig, from string, rcpts []string, messageID string, msg io.ReadSeeker, size int64) (transport.SubmitEvidence, error) {
	var evidence transport.SubmitEvidence
	if size < 0 {
		return evidence, &transport.TransportError{Code: transport.CodeSMTPRejected, Message: "message size cannot be negative"}
	}
	if len(rcpts) == 0 {
		return evidence, &transport.TransportError{Code: transport.CodeSMTPRejected, Message: "no recipients"}
	}
	requiresUTF8, err := submissionRequiresUTF8(ctx, from, rcpts, msg, size)
	if err != nil {
		return evidence, err
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	conn, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return evidence, dialError(ctx, addr, err)
	}

	// Abort blocked I/O promptly once ctx is done. Use sync.Once to avoid
	// double-close races between the context goroutine and client.Close().
	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(func() { _ = conn.Close() }) }
	defer closeConn()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			closeConn()
		case <-done:
		}
	}()

	if err := bumpDeadline(conn, ctx); err != nil {
		return evidence, sessionError(ctx, "greeting", err)
	}
	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		closeConn()
		return evidence, sessionError(ctx, "greeting", err)
	}
	defer func() { _ = client.Close() }()

	tlsCfg := c.tlsConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{ServerName: cfg.Host} // default: verified TLS
	}

	if err := bumpDeadline(conn, ctx); err != nil {
		return evidence, sessionError(ctx, "EHLO", err)
	}
	if err := client.Hello("localhost"); err != nil {
		return evidence, sessionError(ctx, "EHLO", err)
	}
	if ok, _ := client.Extension("STARTTLS"); !ok {
		return evidence, &transport.TransportError{Code: transport.CodeSMTPTLSFailed, Message: "server does not advertise STARTTLS"}
	}
	if err := bumpDeadline(conn, ctx); err != nil {
		return evidence, sessionError(ctx, "STARTTLS", err)
	}
	if err := client.StartTLS(tlsCfg); err != nil {
		return evidence, startTLSError(ctx, err)
	}
	utf8Supported, _ := client.Extension("SMTPUTF8")
	eightBitSupported, _ := client.Extension("8BITMIME")
	if requiresUTF8 && (!utf8Supported || !eightBitSupported) {
		return evidence, &transport.TransportError{Code: transport.CodeSMTPUTF8Unsupported, Message: "internationalized envelope or MIME headers require SMTPUTF8 and 8BITMIME after STARTTLS"}
	}

	if err := bumpDeadline(conn, ctx); err != nil {
		return evidence, sessionError(ctx, "AUTH", err)
	}
	auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	if err := client.Auth(auth); err != nil {
		if ctx.Err() != nil {
			return evidence, timeoutError(ctx, "AUTH")
		}
		return evidence, &transport.TransportError{Code: transport.CodeSMTPAuthFailed, Message: "AUTH PLAIN rejected", Err: err}
	}

	if err := bumpDeadline(conn, ctx); err != nil {
		return evidence, sessionError(ctx, "MAIL", err)
	}
	if err := sendMailCommand(client, from, requiresUTF8, eightBitSupported); err != nil {
		return evidence, sessionError(ctx, "MAIL", err)
	}

	seen := make(map[string]bool, len(rcpts))
	for _, rcpt := range rcpts {
		if seen[rcpt] {
			continue
		}
		seen[rcpt] = true
		if err := bumpDeadline(conn, ctx); err != nil {
			return evidence, sessionError(ctx, "RCPT", err)
		}
		if err := client.Rcpt(rcpt); err != nil {
			return evidence, sessionError(ctx, "RCPT", err)
		}
	}

	resp, err := sendData(conn, ctx, client, msg, size)
	if err != nil {
		return evidence, err
	}

	evidence.ServerResponse = resp
	evidence.MessageID = messageID
	return evidence, nil
}

// sendData runs the DATA phase and returns the server's final response line.
// It bypasses smtp.Client.Data because that discards the final reply text.
func sendData(conn net.Conn, ctx context.Context, client *smtp.Client, msg io.Reader, size int64) (string, error) {
	if err := bumpDeadline(conn, ctx); err != nil {
		return "", sessionError(ctx, "DATA", err)
	}
	id, err := client.Text.Cmd("DATA")
	if err != nil {
		return "", sessionError(ctx, "DATA", err)
	}
	client.Text.StartResponse(id)
	_, _, err = client.Text.ReadResponse(354)
	client.Text.EndResponse(id)
	if err != nil {
		return "", sessionError(ctx, "DATA", err)
	}

	// The payload (body plus attachments) gets a size-aware budget instead
	// of the flat command budget: large sends legitimately outlast 30 s.
	if err := bumpTransferDeadline(conn, ctx, size); err != nil {
		return "", submissionUnknownError(ctx, "DATA", sessionError(ctx, "DATA", err))
	}

	w := client.Text.DotWriter()
	written, err := io.Copy(w, msg)
	if err != nil {
		return "", submissionUnknownError(ctx, "DATA", transferError(ctx, err))
	}
	if size >= 0 && written != size {
		return "", submissionUnknownError(ctx, "DATA", fmt.Errorf("message source ended after %d of %d bytes", written, size))
	}
	if err := w.Close(); err != nil {
		return "", submissionUnknownError(ctx, "DATA", transferError(ctx, err))
	}

	if err := bumpDeadline(conn, ctx); err != nil {
		return "", submissionUnknownError(ctx, "final reply", sessionError(ctx, "final reply", err))
	}
	code, text, err := readFinalSMTPResponse(client.Text.R)
	if err != nil {
		return "", classifyFinalReplyError(ctx, err)
	}
	return fmt.Sprintf("%d %s", code, text), nil
}

func readFinalSMTPResponse(reader *bufio.Reader) (int, string, error) {
	line, err := readCRLFLine(reader)
	if err != nil {
		return 0, "", err
	}
	code, continued, message, err := parseSMTPResponseLine(line)
	if err != nil {
		return 0, "", err
	}
	var builder strings.Builder
	builder.WriteString(message)
	for continued {
		line, err = readCRLFLine(reader)
		if err != nil {
			return 0, "", err
		}
		lineCode, lineContinued, lineMessage, lineErr := parseSMTPResponseLine(line)
		if lineErr != nil {
			return 0, "", lineErr
		}
		if lineCode != code {
			return 0, "", textproto.ProtocolError(fmt.Sprintf("SMTP multiline response code changed from %03d to %03d", code, lineCode))
		}
		builder.WriteByte('\n')
		builder.WriteString(lineMessage)
		continued = lineContinued
	}
	message = builder.String()
	if code != 250 {
		return code, message, &textproto.Error{Code: code, Msg: message}
	}
	return code, message, nil
}

func readCRLFLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadSlice('\n')
	if err != nil {
		if len(line) > 0 && errors.Is(err, io.EOF) {
			return "", fmt.Errorf("SMTP final reply ended before CRLF: %w", err)
		}
		return "", err
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return "", textproto.ProtocolError("SMTP final reply line is not CRLF-terminated")
	}
	return string(line[:len(line)-2]), nil
}

func parseSMTPResponseLine(line string) (int, bool, string, error) {
	if len(line) < 4 || (line[3] != ' ' && line[3] != '-') {
		return 0, false, "", textproto.ProtocolError(fmt.Sprintf("invalid SMTP response line: %q", line))
	}
	code, err := strconv.Atoi(line[:3])
	if err != nil || code < 100 || code > 599 {
		return 0, false, "", textproto.ProtocolError(fmt.Sprintf("invalid SMTP response code: %q", line))
	}
	return code, line[3] == '-', line[4:], nil
}

// bumpDeadline caps the next command at commandBudget or the ctx deadline,
// whichever is earlier.
func bumpDeadline(conn net.Conn, ctx context.Context) error {
	deadline := time.Now().Add(commandBudget)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	return conn.SetDeadline(deadline)
}

// transferBudgetForSize returns commandBudget plus one second per
// bandwidthFloorBytesPerSec of payload, capped at transferCap. Small
// messages keep the flat command budget; large ones scale with size.
func transferBudgetForSize(size int64) time.Duration {
	return transport.TransferBudgetForSize(size)
}

// bumpTransferDeadline applies the size-aware budget to the DATA payload
// write, still honoring an earlier ctx deadline.
func bumpTransferDeadline(conn net.Conn, ctx context.Context, size int64) error {
	deadline := time.Now().Add(transferBudgetForSize(size))
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	return conn.SetDeadline(deadline)
}

// transferError classifies a DATA payload failure: a done context stays a
// command timeout, a live-context socket timeout means the message
// outlasted its transfer budget (too big for this link), everything else
// keeps the generic session classification.
func transferError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return timeoutError(ctx, "DATA")
	}
	var netErr net.Error
	if errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return &transport.TransportError{
			Code:    transport.CodeSMTPTransferTimeout,
			Message: "DATA transfer exceeded its size budget; message too large for this link or server too slow",
			Err:     err,
		}
	}
	return sessionError(ctx, "DATA", err)
}

func submissionUnknownError(ctx context.Context, stage string, err error) error {
	if err == nil {
		err = timeoutError(ctx, stage)
	}
	return &transport.SubmissionError{Stage: stage, Err: err}
}

func classifyFinalReplyError(ctx context.Context, err error) error {
	var responseErr *textproto.Error
	if errors.As(err, &responseErr) && responseErr.Code >= 400 && responseErr.Code < 600 {
		guidance := "transient final rejection; resolve the server condition before an explicit retry"
		if responseErr.Code >= 500 {
			guidance = "permanent final rejection; correct the message or recipients before an explicit retry"
		}
		return &transport.TransportError{
			Code:    transport.CodeSMTPRejected,
			Message: fmt.Sprintf("final reply rejected: %03d %s; %s", responseErr.Code, responseErr.Msg, guidance),
			Err:     err,
		}
	}
	return submissionUnknownError(ctx, "final reply", sessionError(ctx, "final reply", err))
}

// sessionError classifies an in-session failure: server rejections become
// CodeSMTPRejected including the server text, a done context becomes
// CodeSMTPTimeout, everything else is a wrapped connection error.
func sessionError(ctx context.Context, stage string, err error) error {
	if ctx.Err() != nil {
		return timeoutError(ctx, stage)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &transport.TransportError{
			Code:    transport.CodeSMTPTimeout,
			Message: "submission command timed out during " + stage,
			Err:     err,
		}
	}
	var tpErr *textproto.Error
	if errors.As(err, &tpErr) {
		return &transport.TransportError{Code: transport.CodeSMTPRejected, Message: fmt.Sprintf("%s rejected: %d %s", stage, tpErr.Code, tpErr.Msg), Err: err}
	}
	return fmt.Errorf("smtp %s: %w", stage, err)
}

func startTLSError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return timeoutError(ctx, "STARTTLS")
	}
	var netErr net.Error
	if errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return &transport.TransportError{
			Code:    transport.CodeSMTPTimeout,
			Message: "submission command timed out during STARTTLS",
			Err:     err,
		}
	}
	var protocolErr *textproto.Error
	if errors.As(err, &protocolErr) {
		return &transport.TransportError{
			Code:    transport.CodeSMTPTLSFailed,
			Message: "STARTTLS protocol negotiation failed",
		}
	}
	return &transport.TransportError{
		Code:    transport.CodeSMTPTLSFailed,
		Message: "STARTTLS handshake failed",
		Err:     err,
	}
}

func timeoutError(ctx context.Context, stage string) error {
	return &transport.TransportError{Code: transport.CodeSMTPTimeout, Message: "submission aborted: context done during " + stage, Err: ctx.Err()}
}

func dialError(ctx context.Context, addr string, err error) error {
	if ctx.Err() != nil {
		return timeoutError(ctx, "dial "+addr)
	}
	return fmt.Errorf("smtp dial %s: %w", addr, err)
}

// messageID extracts the Message-ID header of a composed message ("" if absent).
func messageID(msg []byte) string {
	m, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		return ""
	}
	return m.Header.Get("Message-ID")
}
