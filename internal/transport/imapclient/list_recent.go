package imapclient

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"mailcli/internal/transport"
)

// recentHeaderFields are the headers a triage row needs; no body is read.
const recentHeaderSection = "HEADER.FIELDS (FROM SUBJECT DATE MESSAGE-ID)"
const recentHeaderFields = "BODY.PEEK[" + recentHeaderSection + "]"

// maxRecentHeaderBytes bounds the header block of one message.
const maxRecentHeaderBytes = 16 * 1024

const (
	maxRecentProtocolOverheadBytes = 1 << 20
	maxRecentUnsolicitedResponses  = 1024
)

// ListRecentMessages selects the mailbox once and fetches UID, FLAGS and the
// header fields of its newest count messages with one FETCH by sequence number.
func (c *Client) ListRecentMessages(
	ctx context.Context,
	cfg transport.ImapConfig,
	mailbox string,
	count int,
) (transport.RecentMailbox, error) {
	result := transport.RecentMailbox{Mailbox: mailbox}
	if count < 1 || count > transport.MaximumRecentMessages {
		return result, &transport.TransportError{
			Code:    transport.CodeIMAPInvalidValue,
			Message: fmt.Sprintf("recent message count must be between 1 and %d", transport.MaximumRecentMessages),
		}
	}
	ps, release, err := c.acquire(ctx, cfg)
	if err != nil {
		return result, err
	}
	defer release()
	info, err := c.ensureSelectedFresh(ctx, ps, mailbox)
	if err != nil {
		return result, err
	}
	result.UIDValidity, result.Exists = info.uidvalidity, info.exists
	if !info.existsKnown {
		return result, recentFetchMalformed(ps.sess, fmt.Errorf("SELECT did not establish the mailbox EXISTS count"))
	}
	if info.exists == 0 {
		return result, nil
	}
	if err := checkUIDValidity(info.uidvalidity, info.uidvalidity); err != nil {
		return result, err
	}
	first := max(1, info.exists-count+1)
	tag := ps.sess.nextTag()
	cmd := fmt.Sprintf("%s FETCH %d:%d (UID FLAGS %s)", tag, first, info.exists, recentHeaderFields)
	if err := c.setTransferDeadline(ctx, ps.sess, 2*maxRecentHeaderBytes*int64(count)); err != nil {
		return result, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP FETCH deadline")
	}
	if err := c.writeLine(ps.sess, cmd); err != nil {
		return result, wrapIOError(ctx, err, transport.CodeIMAPFetchFailed, "IMAP FETCH write")
	}
	messages, err := c.readRecentResponses(ctx, ps.sess, tag, first, info.exists, info.uidvalidity)
	if err != nil {
		return result, err
	}
	result.Messages = messages
	return result, nil
}

func (c *Client) readRecentResponses(ctx context.Context, sess *session, tag string, first, last int, expectedUIDValidity uint32) ([]transport.RecentMessage, error) {
	readLiteral := func(size int) ([]byte, error) {
		source, err := readFetchSourceLiteral(ctx, sess.br, size, false)
		if err != nil {
			return nil, err
		}
		return source.data, nil
	}
	count := last - first + 1
	responseLimit := fetchResponseLimit(2 * maxRecentHeaderBytes)
	byteLimit := int64(count)*responseLimit + maxRecentProtocolOverheadBytes
	remaining := byteLimit
	seenSequences := make([]bool, count)
	seenUIDs := make(map[uint32]bool, count)
	messages := make([]transport.RecentMessage, 0, count)
	ignoredResponses := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, wrapIOError(ctx, err, transport.CodeIMAPFetchFailed, "IMAP FETCH read")
		}
		line, literals, wireBytes, err := c.readLogicalLineWithLiteralReaderCounted(
			sess, maxRecentHeaderBytes, min(responseLimit, remaining), maxFetchLiteralCount, readLiteral,
		)
		if err != nil {
			readErr := fetchReadError(ctx, err, maxRecentHeaderBytes)
			if remaining < responseLimit {
				return nil, &transport.TransportError{Code: transport.ErrorCode(readErr), Message: fmt.Sprintf("recent FETCH read failed with %d of %d aggregate wire bytes remaining", remaining, byteLimit), Err: readErr}
			}
			return nil, readErr
		}
		remaining -= wireBytes
		if remaining < 0 {
			return nil, recentFetchMalformed(sess, fmt.Errorf("aggregate recent FETCH response exceeds %d wire bytes", byteLimit))
		}
		if err := validateFetchResponseValidity(line, tag, expectedUIDValidity); err != nil {
			sess.dirty = true
			return nil, err
		}
		if strings.HasPrefix(line, tag+" ") {
			status, err := parseTaggedCompletionStatus(line, tag)
			if err != nil {
				return nil, recentFetchMalformed(sess, err)
			}
			if status != "OK" {
				return nil, &transport.TransportError{Code: transport.CodeIMAPFetchFailed, Message: "IMAP FETCH failed: " + status}
			}
			for index, seen := range seenSequences {
				if !seen {
					return nil, recentFetchMalformed(sess, fmt.Errorf("missing recent FETCH sequence %d; received %d of %d requested messages", first+index, len(messages), count))
				}
			}
			slices.SortFunc(messages, func(a, b transport.RecentMessage) int { return int(a.UID) - int(b.UID) })
			return messages, nil
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "*" && fields[1][0] >= '0' && fields[1][0] <= '9' {
			switch strings.ToUpper(fields[2]) {
			case "EXPUNGE":
				return nil, recentFetchMalformed(sess, fmt.Errorf("EXPUNGE invalidated the requested recent FETCH sequence window"))
			case "EXISTS":
				observed, err := strconv.ParseUint(fields[1], 10, 32)
				if len(fields) != 3 || err != nil || int(observed) < last {
					return nil, recentFetchMalformed(sess, fmt.Errorf("EXISTS no longer supports the requested recent FETCH window ending at %d", last))
				}
			}
		}
		var parsed fetchResponse
		if isFetchResponseCandidate(line) {
			parsed, err = parseFetchResponse(line, literals)
			if err != nil {
				return nil, recentFetchMalformed(sess, err)
			}
		}
		if !parsed.bodyPresent {
			ignoredResponses++
			if ignoredResponses > maxRecentUnsolicitedResponses {
				return nil, recentFetchMalformed(sess, fmt.Errorf("recent FETCH ignored-response count exceeds %d", maxRecentUnsolicitedResponses))
			}
			continue
		}
		if !parsed.uidPresent || parsed.uid == 0 || !parsed.flagsPresent {
			return nil, recentFetchMalformed(sess, fmt.Errorf("recent FETCH sequence %d has incomplete UID/FLAGS evidence", parsed.sequence))
		}
		if int(parsed.sequence) < first || int(parsed.sequence) > last {
			return nil, recentFetchMalformed(sess, fmt.Errorf("recent FETCH sequence %d is outside requested window %d:%d", parsed.sequence, first, last))
		}
		index := int(parsed.sequence) - first
		if seenSequences[index] || seenUIDs[parsed.uid] {
			return nil, recentFetchMalformed(sess, fmt.Errorf("duplicate recent FETCH sequence %d or UID %d", parsed.sequence, parsed.uid))
		}
		header, err := recentHeaderBlock(parsed.sections)
		if err != nil {
			return nil, recentFetchMalformed(sess, err)
		}
		seenSequences[index], seenUIDs[parsed.uid] = true, true
		messages = append(messages, transport.RecentMessage{UID: parsed.uid, Seen: hasSeenFlag(parsed.flags), Header: header})
	}
}

func recentHeaderBlock(sections []fetchBodySection) ([]byte, error) {
	if len(sections) != 1 || sections[0].name != "BODY["+recentHeaderSection+"]" || sections[0].data == nil {
		return nil, fmt.Errorf("recent FETCH has no exact non-NIL requested header section")
	}
	if len(sections[0].data) > maxRecentHeaderBytes {
		return nil, fmt.Errorf("recent FETCH header exceeds %d bytes", maxRecentHeaderBytes)
	}
	return sections[0].data, nil
}

func recentFetchMalformed(sess *session, err error) *transport.TransportError {
	sess.dirty = true
	return &transport.TransportError{Code: transport.CodeIMAPResponseMalformed, Message: "IMAP recent FETCH coverage invalid", Err: err}
}

func hasSeenFlag(flags []string) bool {
	return slices.ContainsFunc(flags, func(flag string) bool { return strings.EqualFold(flag, `\Seen`) })
}
