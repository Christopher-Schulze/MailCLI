package imapclient

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"mailcli/internal/transport"
)

// FetchMessage fetches the raw RFC 5322 bytes for a message by UID using BODY.PEEK[].
// maxBytes bounds the announced literal.
func (c *Client) FetchMessage(ctx context.Context, cfg transport.ImapConfig, mailbox string, uid uint32, expectedUIDValidity uint32, maxBytes int64) ([]byte, error) {
	source, err := c.fetchMessage(ctx, cfg, mailbox, uid, expectedUIDValidity, maxBytes, false)
	if err != nil {
		return nil, err
	}
	return source.data, nil
}

func (c *Client) fetchMessage(ctx context.Context, cfg transport.ImapConfig, mailbox string, uid uint32, expectedUIDValidity uint32, maxBytes int64, spool bool) (*fetchSource, error) {
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
	if err := c.setTransferDeadline(ctx, ps.sess, maxBytes); err != nil {
		return nil, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP FETCH deadline")
	}
	if err := c.writeLine(ps.sess, cmd); err != nil {
		return nil, wrapIOError(ctx, err, transport.CodeIMAPFetchFailed, "IMAP FETCH write")
	}

	payload, err := c.readFetchSource(ctx, ps.sess, tag, uid, expectedUIDValidity, maxBytes, spool)
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
	source, err := c.readFetchSource(ctx, sess, tag, requestedUID, 0, maxBytes, false)
	if err != nil {
		return nil, err
	}
	return source.data, nil
}

func (c *Client) readFetchSource(ctx context.Context, sess *session, tag string, requestedUID, expectedUIDValidity uint32, maxBytes int64, spool bool) (result *fetchSource, resultErr error) {
	if err := validateFetchLimit(maxBytes); err != nil {
		return nil, err
	}
	responseLimit := maxBytes
	if responseLimit <= int64(^uint64(0)>>1)-maxIMAPResponseLineBytes {
		responseLimit += maxIMAPResponseLineBytes
	}
	var payload *fetchSource
	var pending []*fetchSource
	defer func() {
		closeErr := closeFetchSources(pending)
		if resultErr != nil || closeErr != nil {
			closeErr = errors.Join(closeErr, payload.Close())
			result = nil
		}
		resultErr = errors.Join(resultErr, closeErr)
	}()
	found := false
	bodyReady := false
	var mismatchedUID uint32
	mismatchSeen := false
	for {
		if err := closeFetchSources(pending); err != nil {
			return nil, err
		}
		pending = nil
		if err := ctx.Err(); err != nil {
			return nil, wrapIOError(ctx, err, transport.CodeIMAPFetchFailed, "IMAP FETCH read")
		}
		var memoryBytes int64
		line, literals, err := c.readLogicalLineWithLiteralReader(
			sess, maxBytes, responseLimit, maxFetchLiteralCount,
			func(size int) ([]byte, error) {
				source, err := readFetchSourceLiteral(ctx, sess.br, size, spool && int64(size)+memoryBytes >= fetchMemoryThreshold)
				if err != nil {
					return nil, err
				}
				pending = append(pending, source)
				if source.file == nil {
					memoryBytes += source.size
				}
				return source.data, nil
			},
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
		if err := validateFetchResponseValidity(line, tag, expectedUIDValidity); err != nil {
			sess.dirty = true
			return nil, err
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
				if err := ctx.Err(); err != nil {
					return nil, wrapIOError(ctx, err, transport.CodeIMAPFetchFailed, "IMAP FETCH completion")
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
			if parsed.bodyIndex >= 0 {
				payload = pending[parsed.bodyIndex]
				pending[parsed.bodyIndex] = nil
			} else {
				payload = fetchedBytes(parsed.body)
			}
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
	bodyIndex    int
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
	kind         fetchValueKind
	text         string
	literal      []byte
	literalIndex int
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
	response := fetchResponse{sequence: parser.sequence, bodyIndex: -1}
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
				response.bodyIndex = value.literalIndex
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
		value := fetchValue{kind: fetchValueLiteral, literal: p.literals[p.nextLiteral], literalIndex: p.nextLiteral}
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
