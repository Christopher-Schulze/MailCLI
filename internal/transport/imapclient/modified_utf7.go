package imapclient

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"mailcli/internal/transport"
)

func normalizedMailboxEncoding(encoding transport.MailboxEncoding) transport.MailboxEncoding {
	if encoding == transport.MailboxEncodingUTF8 {
		return transport.MailboxEncodingUTF8
	}
	return transport.MailboxEncodingModifiedUTF7
}

func greetingAdvertisesUTF8(line string) bool {
	return capabilityLineAdvertisesUTF8(line)
}

func capabilityLineAdvertisesUTF8(line string) bool {
	for _, token := range capabilityLineTokens(line) {
		if token == "UTF8=ACCEPT" || token == "UTF8=ONLY" {
			return true
		}
	}
	return false
}

func capabilityLineRequiresUTF8(line string) bool {
	for _, token := range capabilityLineTokens(line) {
		if token == "UTF8=ONLY" {
			return true
		}
	}
	return false
}

func capabilityLineTokens(line string) []string {
	upper := strings.ToUpper(line)
	capability := ""
	if start := strings.Index(upper, "[CAPABILITY "); start >= 0 {
		end := strings.IndexByte(upper[start+len("[CAPABILITY "):], ']')
		if end >= 0 {
			capability = upper[start+len("[CAPABILITY ") : start+len("[CAPABILITY ")+end]
		}
	} else if strings.HasPrefix(upper, "* CAPABILITY ") {
		capability = strings.TrimPrefix(upper, "* CAPABILITY ")
	}
	return strings.Fields(capability)
}

func decodeMailboxWireName(wireName, delimiter string, encoding transport.MailboxEncoding) (string, []string, error) {
	if encoding == "" {
		encoding = transport.MailboxEncodingModifiedUTF7
	}
	if wireName == "" {
		return "", nil, nil
	}
	if delimiter == "" {
		displayName, err := decodeMailboxComponent(wireName, encoding)
		if err != nil {
			return "", nil, err
		}
		return displayName, []string{displayName}, nil
	}
	if utf8.RuneCountInString(delimiter) != 1 || !utf8.ValidString(delimiter) {
		return "", nil, fmt.Errorf("IMAP LIST hierarchy delimiter is not one valid character")
	}
	parts, err := splitMailboxWireName(wireName, delimiter, encoding)
	if err != nil {
		return "", nil, err
	}
	displayPath := make([]string, len(parts))
	for index, part := range parts {
		if part == "" {
			return "", nil, fmt.Errorf("IMAP LIST mailbox name contains an empty hierarchy component")
		}
		displayPart, err := decodeMailboxComponent(part, encoding)
		if err != nil {
			return "", nil, fmt.Errorf("decode IMAP mailbox component %d: %w", index, err)
		}
		displayPath[index] = displayPart
	}
	return strings.Join(displayPath, delimiter), displayPath, nil
}

func splitMailboxWireName(wireName, delimiter string, encoding transport.MailboxEncoding) ([]string, error) {
	if encoding != transport.MailboxEncodingModifiedUTF7 {
		return strings.Split(wireName, delimiter), nil
	}
	if strings.ContainsRune(delimiter, '&') {
		return nil, fmt.Errorf("modified UTF-7 hierarchy delimiter cannot be '&'")
	}
	parts := make([]string, 0, 2)
	start := 0
	for index := 0; index < len(wireName); {
		if wireName[index] == '&' {
			end := strings.IndexByte(wireName[index+1:], '-')
			if end < 0 {
				return nil, fmt.Errorf("modified UTF-7 shift at %d is unterminated", index)
			}
			index += end + 2
			continue
		}
		if strings.HasPrefix(wireName[index:], delimiter) {
			parts = append(parts, wireName[start:index])
			index += len(delimiter)
			start = index
			continue
		}
		index++
	}
	parts = append(parts, wireName[start:])
	return parts, nil
}

func decodeMailboxComponent(value string, encoding transport.MailboxEncoding) (string, error) {
	switch encoding {
	case transport.MailboxEncodingModifiedUTF7:
		return decodeModifiedUTF7(value)
	case transport.MailboxEncodingUTF8:
		if !utf8.ValidString(value) {
			return "", fmt.Errorf("IMAP UTF-8 mailbox name is not valid UTF-8")
		}
		return value, nil
	default:
		return "", fmt.Errorf("unsupported IMAP mailbox encoding %q", encoding)
	}
}

func decodeModifiedUTF7(value string) (string, error) {
	var decoded strings.Builder
	for index := 0; index < len(value); {
		if value[index] != '&' {
			if value[index] < 0x20 || value[index] > 0x7e {
				return "", fmt.Errorf("modified UTF-7 contains a non-ASCII byte at %d", index)
			}
			decoded.WriteByte(value[index])
			index++
			continue
		}
		end := strings.IndexByte(value[index+1:], '-')
		if end < 0 {
			return "", fmt.Errorf("modified UTF-7 shift at %d is unterminated", index)
		}
		end += index + 1
		encoded := value[index+1 : end]
		if encoded == "" {
			decoded.WriteByte('&')
			index = end + 1
			continue
		}
		for offset := 0; offset < len(encoded); offset++ {
			if encoded[offset] == '=' || !isModifiedBase64Byte(encoded[offset]) {
				return "", fmt.Errorf("modified UTF-7 shift contains invalid base64 byte at %d", index+1+offset)
			}
		}
		base64Value := strings.ReplaceAll(encoded, ",", "/")
		bytesValue, err := base64.RawStdEncoding.DecodeString(base64Value)
		if err != nil {
			return "", fmt.Errorf("decode modified UTF-7 shift at %d: %w", index, err)
		}
		canonical := strings.TrimRight(base64.StdEncoding.EncodeToString(bytesValue), "=")
		if canonical != base64Value {
			return "", fmt.Errorf("modified UTF-7 shift at %d is not canonical base64", index)
		}
		if len(bytesValue)%2 != 0 {
			return "", fmt.Errorf("modified UTF-7 shift at %d has an odd UTF-16 byte count", index)
		}
		shift, err := decodeUTF16BE(bytesValue)
		if err != nil {
			return "", fmt.Errorf("decode modified UTF-7 shift at %d: %w", index, err)
		}
		decoded.WriteString(shift)
		index = end + 1
	}
	return decoded.String(), nil
}

func decodeUTF16BE(value []byte) (string, error) {
	units := make([]uint16, len(value)/2)
	for index := range units {
		units[index] = uint16(value[index*2])<<8 | uint16(value[index*2+1])
	}
	runes := make([]rune, 0, len(units))
	for index := 0; index < len(units); index++ {
		unit := units[index]
		switch {
		case unit >= 0xd800 && unit <= 0xdbff:
			if index+1 >= len(units) || units[index+1] < 0xdc00 || units[index+1] > 0xdfff {
				return "", fmt.Errorf("unpaired high surrogate")
			}
			runes = append(runes, utf16.Decode([]uint16{unit, units[index+1]})[0])
			index++
		case unit >= 0xdc00 && unit <= 0xdfff:
			return "", fmt.Errorf("unpaired low surrogate")
		default:
			runes = append(runes, rune(unit))
		}
	}
	return string(runes), nil
}

func encodeModifiedUTF7(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("mailbox display name is not valid UTF-8")
	}
	var encoded strings.Builder
	var shift []rune
	flush := func() {
		if len(shift) == 0 {
			return
		}
		units := utf16.Encode(shift)
		bytesValue := make([]byte, len(units)*2)
		for index, unit := range units {
			bytesValue[index*2] = byte(unit >> 8)
			bytesValue[index*2+1] = byte(unit)
		}
		base64Value := strings.TrimRight(base64.StdEncoding.EncodeToString(bytesValue), "=")
		encoded.WriteByte('&')
		encoded.WriteString(strings.ReplaceAll(base64Value, "/", ","))
		encoded.WriteByte('-')
		shift = shift[:0]
	}
	for _, character := range value {
		if character >= 0x20 && character <= 0x7e && character != '&' {
			flush()
			encoded.WriteRune(character)
			continue
		}
		if character == '&' {
			flush()
			encoded.WriteString("&-")
			continue
		}
		shift = append(shift, character)
	}
	flush()
	return encoded.String(), nil
}

func isModifiedBase64Byte(value byte) bool {
	return (value >= 'A' && value <= 'Z') || (value >= 'a' && value <= 'z') ||
		(value >= '0' && value <= '9') || value == '+' || value == ','
}
