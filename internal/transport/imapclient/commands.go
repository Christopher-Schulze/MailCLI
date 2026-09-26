package imapclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"mailcli/internal/transport"
)

func (c *Client) doList(ctx context.Context, sess *session, tag string) ([]mailbox, error) {
	if err := c.setDeadline(ctx, sess); err != nil {
		return nil, wrapCommandIOError(ctx, err, "IMAP LIST deadline")
	}
	if err := c.writeLine(sess, tag+` LIST "" "*"`); err != nil {
		return nil, wrapCommandIOError(ctx, err, "IMAP LIST write")
	}

	var mailboxes []mailbox
	var responseBytes int64
	responseLines := 0
	for {
		line, literals, wireBytes, err := c.readLineWithLiteralCounted(sess)
		if err != nil {
			var malformed *malformedResponseError
			if errors.As(err, &malformed) {
				sess.dirty = true
				return nil, listResponseMalformed(malformed)
			}
			return nil, wrapCommandIOError(ctx, err, "IMAP LIST read")
		}
		if wireBytes > MaxListOperationResponseBytes-responseBytes {
			return nil, listResponseLimitExceeded(sess, "cumulative response bytes", MaxListOperationResponseBytes)
		}
		responseBytes += wireBytes
		if strings.HasPrefix(line, tag+" ") {
			status, statusErr := parseTaggedCompletionStatus(line, tag)
			if statusErr != nil {
				return nil, malformedTaggedCommandResponse(sess, "LIST", statusErr)
			}
			if status == "OK" {
				return mailboxes, nil
			}
			return nil, &transport.TransportError{
				Code:    transport.CodeIMAPSentMailboxNotFound,
				Message: "IMAP LIST failed: " + status,
			}
		}
		if strings.HasPrefix(line, "* LIST ") && len(mailboxes) >= MaxListOperationMailboxes {
			return nil, listResponseLimitExceeded(sess, "parsed mailboxes", int64(MaxListOperationMailboxes))
		}
		if responseLines >= MaxListOperationResponseLines {
			return nil, listResponseLimitExceeded(sess, "untagged logical response lines", int64(MaxListOperationResponseLines))
		}
		responseLines++
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

func listResponseLimitExceeded(sess *session, resource string, limit int64) *transport.TransportError {
	sess.dirty = true
	return &transport.TransportError{
		Code:            transport.CodeIMAPResourceLimitExceeded,
		Message:         fmt.Sprintf("IMAP LIST %s limit exceeded (%d)", resource, limit),
		Limit:           &transport.ResourceLimit{Name: resource, Value: limit},
		ObservedAtLeast: limit + 1,
	}
}

func (c *Client) doSearch(ctx context.Context, sess *session, tag, messageID string) (int, error) {
	normalizedMessageID, err := normalizeMessageID(messageID)
	if err != nil {
		return 0, err
	}
	if err := c.setDeadline(ctx, sess); err != nil {
		return 0, wrapCommandIOError(ctx, err, "IMAP SEARCH deadline")
	}
	quotedMessageID, err := safeQuoteIMAP(normalizedMessageID)
	if err != nil {
		return 0, err
	}
	if err := c.writeLine(sess, tag+" SEARCH HEADER Message-ID "+quotedMessageID); err != nil {
		return 0, wrapCommandIOError(ctx, err, "IMAP SEARCH write")
	}

	matchCount := 0
	for {
		line, err := c.readLine(sess)
		if err != nil {
			return 0, wrapCommandIOError(ctx, err, "IMAP SEARCH read")
		}
		if strings.HasPrefix(line, tag+" ") {
			status, statusErr := parseTaggedCompletionStatus(line, tag)
			if statusErr != nil {
				return 0, malformedTaggedCommandResponse(sess, "SEARCH", statusErr)
			}
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

	if err := c.readAppendContinuation(ctx, sess, tag); err != nil {
		return err
	}

	if err := c.setTransferDeadline(ctx, sess, size); err != nil {
		sess.dirty = true
		return appendOutcomeIncomplete(wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP APPEND transfer deadline"))
	}
	written, err := io.Copy(sess.bw, &io.LimitedReader{R: msg, N: size})
	if err != nil {
		sess.dirty = true
		return appendOutcomeIncomplete(wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP APPEND literal write"))
	}
	if size >= 0 && written != size {
		sess.dirty = true
		return appendOutcomeIncomplete(&transport.TransportError{
			Code:    transport.CodeIMAPAppendFailed,
			Message: fmt.Sprintf("message source ended after %d of %d bytes", written, size),
		})
	}
	extraRead, extraErr := io.CopyN(io.Discard, msg, 1)
	if extraRead > 0 {
		sess.dirty = true
		return appendOutcomeIncomplete(&transport.TransportError{
			Code:    transport.CodeIMAPAppendFailed,
			Message: "message source exceeds declared APPEND literal size",
			Err:     extraErr,
		})
	}
	if extraErr != io.EOF {
		sess.dirty = true
		if extraErr == nil {
			extraErr = io.ErrNoProgress
		}
		return appendOutcomeIncomplete(wrapIOError(ctx, extraErr, transport.CodeIMAPAppendFailed, "IMAP APPEND source length check"))
	}
	if err := sess.bw.Flush(); err != nil {
		sess.dirty = true
		return appendOutcomeIncomplete(wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP APPEND literal flush"))
	}
	if err := c.writeLine(sess, ""); err != nil {
		sess.dirty = true
		return appendOutcomeUnknown(wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP APPEND literal CRLF"))
	}

	if err := c.setDeadline(ctx, sess); err != nil {
		sess.dirty = true
		return appendOutcomeUnknown(wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP APPEND final reply deadline"))
	}
	status, _, err := c.readFinal(ctx, sess, tag)
	if err != nil {
		sess.dirty = true
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

const maxAppendContinuationLines = 100
const maxAppendContinuationBytes int64 = 64 << 10

func (c *Client) readAppendContinuation(ctx context.Context, sess *session, tag string) error {
	var responseBytes int64
	responseLines := 0
	for {
		line, wireBytes, err := c.readLineWithWireByteBudget(sess, maxAppendContinuationBytes-responseBytes)
		var malformed *malformedResponseError
		if errors.As(err, &malformed) || wireBytes > maxAppendContinuationBytes-responseBytes {
			sess.dirty = true
			return &transport.TransportError{
				Code:            transport.CodeIMAPResourceLimitExceeded,
				Message:         "IMAP APPEND continuation response exceeds 64 KiB",
				Limit:           &transport.ResourceLimit{Name: "APPEND continuation response bytes", Value: maxAppendContinuationBytes},
				ObservedAtLeast: maxAppendContinuationBytes + 1,
			}
		}
		if err != nil {
			return wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP APPEND continuation read")
		}
		responseBytes += wireBytes
		if strings.HasPrefix(line, "+") {
			return nil
		}
		if strings.HasPrefix(line, "* ") {
			responseLines++
			if responseLines <= maxAppendContinuationLines {
				continue
			}
			sess.dirty = true
			return &transport.TransportError{
				Code:            transport.CodeIMAPResourceLimitExceeded,
				Message:         "IMAP APPEND continuation exceeds 100 untagged responses",
				Limit:           &transport.ResourceLimit{Name: "APPEND continuation untagged responses", Value: maxAppendContinuationLines},
				ObservedAtLeast: int64(responseLines),
			}
		}
		if strings.HasPrefix(line, tag+" ") {
			status, err := parseTaggedCompletionStatus(line, tag)
			if err != nil {
				return malformedTaggedCommandResponse(sess, "APPEND", err)
			}
			if status == "NO" || status == "BAD" {
				return &transport.TransportError{Code: transport.CodeIMAPAppendFailed, Message: "IMAP APPEND rejected: " + line}
			}
		}
		sess.dirty = true
		return &transport.TransportError{
			Code: transport.CodeIMAPAppendFailed, Message: "IMAP APPEND expected continuation", Err: fmt.Errorf("got: %s", line),
		}
	}
}

func appendOutcomeUnknown(err error) error {
	return &transport.TransportError{
		Code:    transport.CodeIMAPAppendOutcomeUnknown,
		Message: "IMAP APPEND outcome is unknown after message data was sent",
		Err:     err,
	}
}

func appendOutcomeIncomplete(err error) error {
	return &transport.TransportError{
		Code:    transport.CodeIMAPAppendIncomplete,
		Message: "IMAP APPEND literal failed before its terminating CRLF was attempted",
		Err:     err,
	}
}

func (c *Client) doLogout(ctx context.Context, sess *session, tag string) error {
	if err := c.setDeadline(ctx, sess); err != nil {
		return wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP LOGOUT deadline")
	}
	return c.writeLine(sess, tag+" LOGOUT")
}
