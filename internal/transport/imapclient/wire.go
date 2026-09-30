package imapclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mailcli/internal/transport"
)

func (c *Client) setDeadline(ctx context.Context, sess *session) error {
	return c.setDeadlineFor(ctx, sess, transport.TransferCommandBudget)
}

func (c *Client) setTransferDeadline(ctx context.Context, sess *session, size int64) error {
	return c.setDeadlineFor(ctx, sess, transport.TransferBudgetForSize(size))
}

func (c *Client) setDeadlineFor(ctx context.Context, sess *session, budget time.Duration) error {
	if err := ctx.Err(); err != nil {
		sess.dirty = true
		return err
	}
	deadline := time.Now().Add(budget)
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
	if strings.ContainsAny(line, "\r\n\x00") {
		return &transport.TransportError{
			Code:    transport.CodeIMAPInvalidValue,
			Message: "IMAP command contains a forbidden control character",
		}
	}
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

func (c *Client) writeLineBytes(sess *session, line []byte) error {
	if bytes.ContainsAny(line, "\r\n\x00") {
		return &transport.TransportError{
			Code:    transport.CodeIMAPInvalidValue,
			Message: "IMAP command contains a forbidden control character",
		}
	}
	if _, err := sess.bw.Write(line); err != nil {
		sess.dirty = true
		return err
	}
	if _, err := sess.bw.WriteString("\r\n"); err != nil {
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
	line, _, err := c.readLineWithWireByteCount(sess)
	return line, err
}

func (c *Client) readLineWithWireByteCount(sess *session) (string, int64, error) {
	return c.readLineWithWireByteBudget(sess, maxIMAPResponseLineBytes)
}

func (c *Client) readLineWithWireByteBudget(sess *session, limit int64) (string, int64, error) {
	var line []byte
	for {
		fragment, err := sess.br.ReadSlice('\n')
		if int64(len(line))+int64(len(fragment)) > limit {
			sess.dirty = true
			return "", 0, &malformedResponseError{err: fmt.Errorf(
				"IMAP response line exceeds %d bytes", limit,
			)}
		}
		line = append(line, fragment...)
		if err == nil {
			return strings.TrimRight(string(line), "\r\n"), int64(len(line)), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		sess.dirty = true
		return "", 0, err
	}
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

// readLineWithLiteralCounted reconstructs one logical response line from its line
// fragments and server-sent literals. Each literal is represented in the
// returned text by imapLiteralMarker and kept separately so its bytes remain
// raw when the value parser consumes it.
func (c *Client) readLineWithLiteralCounted(sess *session) (string, [][]byte, int64, error) {
	line, literals, wireBytes, err := c.readLogicalLineWithLiteralReaderCounted(
		sess, int64(maxListLiteralBytes), int64(maxListResponseBytes), maxListLiteralCount, nil,
	)
	var limitErr *literalLimitError
	if errors.As(err, &limitErr) {
		return "", nil, wireBytes, &malformedResponseError{err: fmt.Errorf(
			"IMAP LIST literal response exceeds %d bytes or %d bytes per literal",
			maxListResponseBytes, maxListLiteralBytes,
		)}
	}
	var readErr *literalReadError
	if errors.As(err, &readErr) {
		return "", nil, wireBytes, &malformedResponseError{err: readErr}
	}
	return line, literals, wireBytes, err
}

type literalLimitError struct {
	size        int64
	maxLiteral  int64
	maxResponse int64
}

func (e *literalLimitError) Error() string {
	return fmt.Sprintf(
		"IMAP response literal exceeds %d bytes or response literal budget %d bytes: %d",
		e.maxLiteral, e.maxResponse, e.size,
	)
}

type literalReadError struct{ err error }

func (e *literalReadError) Error() string {
	return "IMAP response literal read failed: " + e.err.Error()
}

func (e *literalReadError) Unwrap() error { return e.err }

func (c *Client) readLogicalLineWithLiteralReaderCounted(
	sess *session,
	maxLiteralBytes int64,
	maxResponseBytes int64,
	maxLiteralCount int,
	readLiteral func(int) ([]byte, error),
) (string, [][]byte, int64, error) {
	var reconstructed strings.Builder
	var literals [][]byte
	var literalBytes int64
	var wireBytes int64
	for {
		line, lineWireBytes, err := c.readLineWithWireByteCount(sess)
		wireBytes += lineWireBytes
		if err != nil {
			return "", nil, wireBytes, err
		}
		prefix, size, hasLiteral, err := parseLiteralSuffix(line)
		if err != nil {
			sess.dirty = true
			return "", nil, wireBytes, &malformedResponseError{err: err}
		}
		if !hasLiteral {
			if int64(reconstructed.Len()+len(line)) > maxResponseBytes-literalBytes {
				sess.dirty = true
				return "", nil, wireBytes, &malformedResponseError{err: fmt.Errorf(
					"IMAP logical response exceeds %d bytes", maxResponseBytes,
				)}
			}
			reconstructed.WriteString(line)
			return reconstructed.String(), literals, wireBytes, nil
		}
		if len(literals) >= maxLiteralCount {
			sess.dirty = true
			return "", nil, wireBytes, &malformedResponseError{err: fmt.Errorf(
				"IMAP response literal count exceeds %d", maxLiteralCount,
			)}
		}
		size64 := int64(size)
		if size64 > maxLiteralBytes || size64 > maxResponseBytes-literalBytes {
			sess.dirty = true
			return "", nil, wireBytes, &literalLimitError{
				size: size64, maxLiteral: maxLiteralBytes, maxResponse: maxResponseBytes,
			}
		}
		if int64(reconstructed.Len()+len(prefix)+1) > maxResponseBytes-literalBytes-size64 {
			sess.dirty = true
			return "", nil, wireBytes, &malformedResponseError{err: fmt.Errorf(
				"IMAP logical response exceeds %d bytes", maxResponseBytes,
			)}
		}

		reconstructed.WriteString(prefix)
		reconstructed.WriteString(imapLiteralMarker)
		var literal []byte
		if readLiteral == nil {
			literal = make([]byte, size)
			_, err = io.ReadFull(sess.br, literal)
		} else {
			literal, err = readLiteral(size)
		}
		if err != nil {
			sess.dirty = true
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return "", nil, wireBytes, &literalReadError{err: err}
			}
			return "", nil, wireBytes, err
		}
		literals = append(literals, literal)
		literalBytes += size64
		wireBytes += size64
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
	var responseBytes int64
	for range maxFlagResponseCount {
		line, wireBytes, err := c.readLineWithWireByteCount(sess)
		if err != nil {
			return "", "", wrapIOError(ctx, err, transport.CodeIMAPAppendFailed, "IMAP read final response")
		}
		responseBytes += wireBytes
		if responseBytes > maxFlagResponseBytes {
			return "", "", completionResponseLimitExceeded(sess, "command completion response bytes", maxFlagResponseBytes)
		}
		if !strings.HasPrefix(line, tag+" ") {
			continue
		}
		rest := strings.TrimPrefix(line, tag+" ")
		fields := strings.SplitN(rest, " ", 2)
		status, statusErr := parseTaggedCompletionStatus(line, tag)
		if statusErr != nil {
			return "", "", malformedTaggedCommandResponse(sess, "completion", statusErr)
		}
		var text string
		if len(fields) > 1 {
			text = fields[1]
		}
		return status, text, nil
	}
	return "", "", completionResponseLimitExceeded(sess, "command completion response lines", maxFlagResponseCount)
}

func parseTaggedCompletionStatus(line, tag string) (string, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != tag {
		return "", errors.New("IMAP tagged completion has no status atom")
	}
	status := strings.ToUpper(fields[1])
	switch status {
	case "OK", "NO", "BAD":
		return status, nil
	default:
		return "", fmt.Errorf("IMAP tagged completion has invalid status %q", fields[1])
	}
}

const maxTaggedIMAPRejectionTextBytes = 512

func rejectedTaggedCompletion(line, tag, command, status string) *transport.TransportError {
	return taggedCommandRejection(command, status, taggedCompletionText(line, tag))
}

func taggedCompletionText(line, tag string) string {
	rest, found := strings.CutPrefix(line, tag+" ")
	if !found {
		return ""
	}
	_, text, found := strings.Cut(rest, " ")
	if !found {
		return ""
	}
	return text
}

func taggedCommandRejection(command, status, responseText string) *transport.TransportError {
	responseCode := leadingTaggedResponseCode(responseText)
	evidence := &transport.IMAPCommandRejection{
		Command: command, Status: status, ResponseCode: responseCode,
		Text: boundedTaggedResponseText(responseText),
	}
	code := transport.CodeIMAPCommandRejected
	if command == "LOGIN" && status == "NO" {
		code = transport.CodeIMAPAuthFailed
	}
	if status == "NO" && responseCode != nil {
		switch {
		case *responseCode == "NONEXISTENT" && (command == "STATUS" || command == "SELECT" || command == "APPEND"):
			code = transport.CodeIMAPMailboxNotFound
		case *responseCode == "OVERQUOTA" && command == "APPEND":
			code = transport.CodeIMAPQuotaExceeded
		}
	}
	return &transport.TransportError{
		Code: code, Message: "IMAP " + command + " was rejected with " + status,
		IMAPRejection: evidence,
	}
}

func leadingTaggedResponseCode(responseText string) *string {
	if !strings.HasPrefix(responseText, "[") {
		return nil
	}
	end := strings.IndexByte(responseText[1:], ']')
	if end < 1 {
		return nil
	}
	end++
	if end+1 < len(responseText) && responseText[end+1] != ' ' {
		return nil
	}
	codeText := responseText[1:end]
	codeAtom, _, _ := strings.Cut(codeText, " ")
	if codeAtom == "" {
		return nil
	}
	for _, r := range codeAtom {
		if unicode.IsControl(r) || unicode.IsSpace(r) || strings.ContainsRune("(){}%*\\\"[]", r) {
			return nil
		}
	}
	code := strings.ToUpper(codeAtom)
	return &code
}

func boundedTaggedResponseText(responseText string) string {
	var bounded strings.Builder
	bounded.Grow(min(len(responseText), maxTaggedIMAPRejectionTextBytes))
	for _, r := range responseText {
		if unicode.IsControl(r) {
			continue
		}
		if bounded.Len()+utf8.RuneLen(r) > maxTaggedIMAPRejectionTextBytes {
			break
		}
		bounded.WriteRune(r)
	}
	return bounded.String()
}

func malformedTaggedCommandResponse(sess *session, command string, err error) *transport.TransportError {
	sess.dirty = true
	return &transport.TransportError{
		Code:    transport.CodeIMAPResponseMalformed,
		Message: "IMAP " + command + " tagged response malformed",
		Err:     err,
	}
}

// readFinalWithCodes reads a tagged command completion and retains bracketed
// response codes from both untagged and tagged lines. COPYUID is commonly sent
// as an untagged OK response before the tagged completion, so a plain final
// status parser would silently discard the only destination UID evidence.
func (c *Client) readFinalWithCodes(
	ctx context.Context,
	sess *session,
	tag string,
	code string,
	message string,
) (string, string, []string, error) {
	var responseCodes []string
	var responseBytes int64
	for range maxFlagResponseCount {
		line, wireBytes, err := c.readLineWithWireByteCount(sess)
		if err != nil {
			return "", "", responseCodes, wrapIOError(ctx, err, code, message)
		}
		responseBytes += wireBytes
		if responseBytes > maxFlagResponseBytes {
			return "", "", responseCodes, completionResponseLimitExceeded(sess, "command completion response bytes", maxFlagResponseBytes)
		}
		responseCode, codeErr := flagResponseCode(line, tag)
		if codeErr != nil {
			return "", "", responseCodes, malformedTaggedCommandResponse(sess, "completion", codeErr)
		}
		if responseCode != "" {
			responseCodes = append(responseCodes, responseCode)
		}
		if !strings.HasPrefix(line, tag+" ") {
			continue
		}
		rest := strings.TrimPrefix(line, tag+" ")
		fields := strings.SplitN(rest, " ", 2)
		status, statusErr := parseTaggedCompletionStatus(line, tag)
		if statusErr != nil {
			return "", "", responseCodes, malformedTaggedCommandResponse(sess, "completion", statusErr)
		}
		var text string
		if len(fields) > 1 {
			text = fields[1]
		}
		return status, text, responseCodes, nil
	}
	return "", "", responseCodes, completionResponseLimitExceeded(sess, "command completion response lines", maxFlagResponseCount)
}

// Generic completions share the existing flag-response byte and line limits.
func completionResponseLimitExceeded(sess *session, name string, value int64) *transport.TransportError {
	sess.dirty = true
	return &transport.TransportError{
		Code: transport.CodeIMAPResourceLimitExceeded, Message: "IMAP " + name + " budget exhausted",
		Limit: &transport.ResourceLimit{Name: name, Value: value}, ObservedAtLeast: value,
	}
}

func bracketedResponseCode(line string) (string, bool) {
	start := strings.IndexByte(line, '[')
	if start == -1 {
		return "", false
	}
	relativeEnd := strings.IndexByte(line[start+1:], ']')
	if relativeEnd == -1 {
		return "", false
	}
	value := strings.TrimSpace(line[start+1 : start+1+relativeEnd])
	if value == "" {
		return "", false
	}
	return value, true
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

func safeQuoteIMAP(s string) (string, error) {
	for index := 0; index < len(s); index++ {
		if s[index] < 0x20 || s[index] == 0x7f {
			return "", &transport.TransportError{
				Code:    transport.CodeIMAPInvalidValue,
				Message: fmt.Sprintf("IMAP value contains control character at byte %d", index),
			}
		}
	}
	return quoteIMAP(s), nil
}

func safeQuoteIMAPBytes(s string) ([]byte, error) {
	for index := 0; index < len(s); index++ {
		if s[index] < 0x20 || s[index] == 0x7f {
			return nil, &transport.TransportError{
				Code:    transport.CodeIMAPInvalidValue,
				Message: fmt.Sprintf("IMAP value contains control character at byte %d", index),
			}
		}
	}
	var b bytes.Buffer
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for index := 0; index < len(s); index++ {
		c := s[index]
		if c == '\\' || c == '"' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	b.WriteByte('"')
	return b.Bytes(), nil
}

func wipeBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
