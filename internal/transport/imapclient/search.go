package imapclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	mail "net/mail"
	"strconv"
	"strings"
	"unicode"

	"mailcli/internal/transport"
)

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
		return nil, wrapCommandIOError(ctx, err, "IMAP UID SEARCH deadline")
	}
	if err := c.writeLine(sess, tag+" UID SEARCH "+criteria); err != nil {
		return nil, wrapCommandIOError(ctx, err, "IMAP UID SEARCH write")
	}

	var uids []uint32
	seenSearch := false
	seenUIDs := make(map[uint32]struct{})
	for {
		line, err := c.readLine(sess)
		if err != nil {
			return nil, wrapCommandIOError(ctx, err, "IMAP UID SEARCH read")
		}
		if strings.HasPrefix(line, tag+" ") {
			status, statusErr := parseTaggedCompletionStatus(line, tag)
			if statusErr != nil {
				return nil, malformedTaggedCommandResponse(sess, "UID SEARCH", statusErr)
			}
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
