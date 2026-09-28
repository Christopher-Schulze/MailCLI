package imapclient

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"mailcli/internal/transport"
)

// excerptHeaderFields are the top-level headers an excerpt parse needs to
// decode the body text, plus Message-ID for identity checks.
const excerptHeaderFields = "BODY.PEEK[HEADER.FIELDS (MESSAGE-ID MIME-VERSION CONTENT-TYPE CONTENT-TRANSFER-ENCODING)]"

// FetchMessageExcerpts selects the mailbox once and fetches, for every UID in
// one UID FETCH, the MIME header fields and a body-text prefix of at most
// maxTextBytes. Each literal is bounded by maxTextBytes. An expectedUIDValidity
// of 0 skips the UIDVALIDITY check; the caller then verifies each Message-ID.
func (c *Client) FetchMessageExcerpts(
	ctx context.Context,
	cfg transport.ImapConfig,
	mailbox string,
	expectedUIDValidity uint32,
	uids []uint32,
	maxTextBytes int64,
) (map[uint32]transport.MessageExcerptSource, error) {
	if err := validateFetchLimit(maxTextBytes); err != nil {
		return nil, err
	}
	requested := make(map[uint32]bool, len(uids))
	set := make([]string, 0, len(uids))
	for _, uid := range uids {
		if err := validateMessageUID(uid); err != nil {
			return nil, err
		}
		if !requested[uid] {
			requested[uid] = true
			set = append(set, strconv.FormatUint(uint64(uid), 10))
		}
	}
	results := make(map[uint32]transport.MessageExcerptSource, len(set))
	if len(set) == 0 {
		return results, nil
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
	if expectedUIDValidity != 0 {
		if err := checkUIDValidity(expectedUIDValidity, info.uidvalidity); err != nil {
			return nil, err
		}
	}
	tag := ps.sess.nextTag()
	cmd := fmt.Sprintf("%s UID FETCH %s (UID %s BODY.PEEK[TEXT]<0.%d>)", tag, strings.Join(set, ","), excerptHeaderFields, maxTextBytes)
	if err := c.setTransferDeadline(ctx, ps.sess, 2*maxTextBytes*int64(len(set))); err != nil {
		return nil, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP FETCH deadline")
	}
	if err := c.writeLine(ps.sess, cmd); err != nil {
		return nil, wrapIOError(ctx, err, transport.CodeIMAPFetchFailed, "IMAP FETCH write")
	}
	return c.readExcerptResponses(ctx, ps.sess, tag, expectedUIDValidity, requested, maxTextBytes, results)
}

func (c *Client) readExcerptResponses(
	ctx context.Context,
	sess *session,
	tag string,
	expectedUIDValidity uint32,
	requested map[uint32]bool,
	maxTextBytes int64,
	results map[uint32]transport.MessageExcerptSource,
) (map[uint32]transport.MessageExcerptSource, error) {
	readLiteral := func(size int) ([]byte, error) {
		source, err := readFetchSourceLiteral(ctx, sess.br, size, false)
		if err != nil {
			return nil, err
		}
		return source.data, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, wrapIOError(ctx, err, transport.CodeIMAPFetchFailed, "IMAP FETCH read")
		}
		line, literals, _, err := c.readLogicalLineWithLiteralReaderCounted(
			sess, maxTextBytes, fetchResponseLimit(2*maxTextBytes), maxFetchLiteralCount, readLiteral,
		)
		if err != nil {
			return nil, fetchReadError(ctx, err, maxTextBytes)
		}
		if err := validateFetchResponseValidity(line, tag, expectedUIDValidity); err != nil {
			sess.dirty = true
			return nil, err
		}
		if strings.HasPrefix(line, tag+" ") {
			if status := parseStatus(line, tag); status != "OK" {
				return nil, &transport.TransportError{Code: transport.CodeIMAPFetchFailed, Message: "IMAP FETCH failed: " + status}
			}
			return results, nil
		}
		if !isFetchResponseCandidate(line) {
			continue
		}
		parsed, err := parseFetchResponse(line, literals)
		if err != nil {
			sess.dirty = true
			return nil, &transport.TransportError{Code: transport.CodeIMAPResponseMalformed, Message: "IMAP FETCH response malformed", Err: err}
		}
		if !parsed.bodyPresent || !parsed.uidPresent || !requested[parsed.uid] {
			continue
		}
		source, err := excerptSourceFromSections(parsed.sections, maxTextBytes)
		if err == nil {
			if _, duplicate := results[parsed.uid]; duplicate {
				err = fmt.Errorf("duplicate excerpt response for UID %d", parsed.uid)
			}
		}
		if err != nil {
			sess.dirty = true
			return nil, &transport.TransportError{Code: transport.CodeIMAPResponseMalformed, Message: "IMAP excerpt FETCH response malformed", Err: err}
		}
		results[parsed.uid] = source
	}
}

// excerptSourceFromSections joins the header-fields block and the text prefix
// into one minimal MIME message.
func excerptSourceFromSections(sections []fetchBodySection, maxTextBytes int64) (transport.MessageExcerptSource, error) {
	var header, text []byte
	var headerSeen, textSeen bool
	for _, section := range sections {
		switch {
		case strings.HasPrefix(section.name, "BODY[HEADER.FIELDS"):
			header, headerSeen = section.data, true
		case section.name == "BODY[TEXT]<0>" || section.name == "BODY[TEXT]":
			text, textSeen = section.data, true
		default:
			return transport.MessageExcerptSource{}, fmt.Errorf("unexpected BODY section %q", section.name)
		}
	}
	if !headerSeen || !textSeen {
		return transport.MessageExcerptSource{}, fmt.Errorf("excerpt response lacks the header fields or the text prefix")
	}
	source := make([]byte, 0, len(header)+len(text))
	source = append(append(source, header...), text...)
	return transport.MessageExcerptSource{Source: source, Complete: int64(len(text)) < maxTextBytes}, nil
}
