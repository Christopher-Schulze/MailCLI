package imapclient

import (
	"fmt"
	"strings"

	"mailcli/internal/transport"
)

func parseListLine(line string, literals ...[]byte) (string, []string, error) {
	parsed, err := parseListMailbox(line, transport.MailboxEncodingModifiedUTF7, literals...)
	if err != nil {
		return "", nil, err
	}
	return parsed.wireName, parsed.flags, nil
}

func parseListMailbox(line string, encoding transport.MailboxEncoding, literals ...[]byte) (mailbox, error) {
	parser := imapValueParser{literals: literals}
	const prefix = "* LIST "
	if !strings.HasPrefix(line, prefix) {
		return mailbox{}, fmt.Errorf("not a LIST response")
	}
	s := line[len(prefix):]
	if !strings.HasPrefix(s, "(") {
		return mailbox{}, fmt.Errorf("no attribute list")
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
		return mailbox{}, fmt.Errorf("unterminated attribute list")
	}

	attrs := s[1 : end-1]
	flags, err := parser.parseValues(attrs)
	if err != nil {
		return mailbox{}, err
	}

	rest := strings.TrimSpace(s[end:])
	delimiter, rest, err := parser.parse(rest)
	if err != nil {
		return mailbox{}, err
	}

	wireName, rest, err := parser.parse(rest)
	if err != nil {
		return mailbox{}, err
	}
	if strings.TrimSpace(rest) != "" {
		return mailbox{}, fmt.Errorf("trailing LIST response data")
	}
	if parser.nextLiteral != len(literals) {
		return mailbox{}, fmt.Errorf("unused IMAP response literal")
	}
	displayName, displayPath, err := decodeMailboxWireName(wireName, delimiter, encoding)
	if err != nil {
		return mailbox{}, err
	}
	return mailbox{
		name: wireName, wireName: wireName, displayName: displayName, displayPath: displayPath,
		delimiter: delimiter, encoding: normalizedMailboxEncoding(encoding), flags: flags,
	}, nil
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

func pickSent(mailboxes []mailbox) (string, error) {
	infos := make([]transport.MailboxInfo, len(mailboxes))
	for index, candidate := range mailboxes {
		wireName := candidate.wireName
		if wireName == "" {
			wireName = candidate.name
		}
		infos[index] = transport.MailboxInfo{
			Name: wireName, WireName: wireName,
			DisplayName: candidate.displayName, DisplayPath: append([]string(nil), candidate.displayPath...),
			Delimiter: candidate.delimiter, Encoding: candidate.encoding,
			Flags: append([]string(nil), candidate.flags...),
		}
	}
	return transport.ResolveSentMailbox(infos)
}

func parseStatus(line, tag string) string {
	rest := strings.TrimPrefix(line, tag+" ")
	fields := strings.SplitN(rest, " ", 2)
	return fields[0]
}
