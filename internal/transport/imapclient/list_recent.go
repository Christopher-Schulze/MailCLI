package imapclient

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"mailcli/internal/transport"
)

// recentHeaderFields are the headers a triage row needs; no body is read.
const recentHeaderFields = "BODY.PEEK[HEADER.FIELDS (FROM SUBJECT DATE MESSAGE-ID)]"

// maxRecentHeaderBytes bounds the header block of one message.
const maxRecentHeaderBytes = 16 * 1024

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
	if info.exists == 0 {
		return result, nil
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
	messages, err := c.readRecentResponses(ctx, ps.sess, tag)
	if err != nil {
		return result, err
	}
	result.Messages = messages
	return result, nil
}

func (c *Client) readRecentResponses(ctx context.Context, sess *session, tag string) ([]transport.RecentMessage, error) {
	readLiteral := func(size int) ([]byte, error) {
		source, err := readFetchSourceLiteral(ctx, sess.br, size, false)
		if err != nil {
			return nil, err
		}
		return source.data, nil
	}
	seen := make(map[uint32]bool)
	var messages []transport.RecentMessage
	for {
		if err := ctx.Err(); err != nil {
			return nil, wrapIOError(ctx, err, transport.CodeIMAPFetchFailed, "IMAP FETCH read")
		}
		line, literals, _, err := c.readLogicalLineWithLiteralReaderCounted(
			sess, maxRecentHeaderBytes, fetchResponseLimit(2*maxRecentHeaderBytes), maxFetchLiteralCount, readLiteral,
		)
		if err != nil {
			return nil, fetchReadError(ctx, err, maxRecentHeaderBytes)
		}
		if err := validateFetchResponseValidity(line, tag, 0); err != nil {
			sess.dirty = true
			return nil, err
		}
		if strings.HasPrefix(line, tag+" ") {
			if status := parseStatus(line, tag); status != "OK" {
				return nil, &transport.TransportError{Code: transport.CodeIMAPFetchFailed, Message: "IMAP FETCH failed: " + status}
			}
			slices.SortFunc(messages, func(a, b transport.RecentMessage) int { return int(a.UID) - int(b.UID) })
			return messages, nil
		}
		if !isFetchResponseCandidate(line) {
			continue
		}
		parsed, err := parseFetchResponse(line, literals)
		if err != nil {
			sess.dirty = true
			return nil, &transport.TransportError{Code: transport.CodeIMAPResponseMalformed, Message: "IMAP FETCH response malformed", Err: err}
		}
		if !parsed.uidPresent || !parsed.bodyPresent {
			continue
		}
		if seen[parsed.uid] {
			sess.dirty = true
			return nil, &transport.TransportError{
				Code:    transport.CodeIMAPResponseMalformed,
				Message: fmt.Sprintf("duplicate recent-message response for UID %d", parsed.uid),
			}
		}
		seen[parsed.uid] = true
		messages = append(messages, transport.RecentMessage{
			UID: parsed.uid, Seen: hasSeenFlag(parsed.flags), Header: recentHeaderBlock(parsed.sections),
		})
	}
}

func recentHeaderBlock(sections []fetchBodySection) []byte {
	for _, section := range sections {
		if strings.HasPrefix(section.name, "BODY[HEADER.FIELDS") {
			return section.data
		}
	}
	return nil
}

func hasSeenFlag(flags []string) bool {
	return slices.ContainsFunc(flags, func(flag string) bool { return strings.EqualFold(flag, `\Seen`) })
}
