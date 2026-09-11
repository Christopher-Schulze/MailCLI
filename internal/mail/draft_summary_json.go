package mail

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const maximumDraftSummaryBytes = 1024 * 1024

// projectDraftSummaryJSON removes only body string contents. The resulting
// document still passes through the existing strict typed decoder and claim
// validators. This lexer validates skipped escapes and control bytes without
// building the omitted string; it does not replace JSON structure validation.
func projectDraftSummaryJSON(ctx context.Context, input io.Reader) ([]byte, int64, error) {
	limited := &io.LimitedReader{R: contextReader{ctx: ctx, reader: input}, N: maximumDraftStateBytes + 1}
	reader := bufio.NewReaderSize(limited, 4096)
	projection, err := readDraftSummaryJSON(reader)
	readBytes := maximumDraftStateBytes + 1 - limited.N
	if readBytes > maximumDraftStateBytes {
		return nil, readBytes, errors.New("draft record exceeds 20 MiB")
	}
	if len(projection) > maximumDraftSummaryBytes {
		return nil, readBytes, errors.New("draft summary metadata exceeds 1 MiB")
	}
	if err != nil {
		return nil, readBytes, err
	}
	return projection, readBytes, ctx.Err()
}

func readDraftSummaryJSON(reader *bufio.Reader) ([]byte, error) {
	output := make([]byte, 0, 4096)
	var containers []byte
	var previous byte
	var field string
	for {
		value, err := reader.ReadByte()
		if errors.Is(err, io.EOF) {
			return output, nil
		}
		if err != nil {
			return nil, err
		}
		if len(output) >= maximumDraftSummaryBytes {
			return nil, errors.New("draft summary metadata exceeds 1 MiB")
		}
		switch value {
		case ' ', '\t', '\r', '\n':
			if len(output) == 0 || output[len(output)-1] != ' ' {
				output = append(output, ' ')
			}
			continue
		case '"':
			key := len(containers) > 0 && containers[len(containers)-1] == '{' && (previous == '{' || previous == ',')
			start := len(output)
			omit := !key && previous == ':' && omittedDraftSummaryString(field)
			output, err = readDraftSummaryString(reader, output, omit)
			if err != nil {
				return nil, err
			}
			if key {
				if len(output)-start > 256 || json.Unmarshal(output[start:], &field) != nil {
					return nil, errors.New("invalid draft JSON field name")
				}
			}
		case '{', '[':
			if len(containers) == 128 {
				return nil, errors.New("draft JSON exceeds nesting limit")
			}
			containers = append(containers, value)
			output = append(output, value)
		case '}', ']':
			if len(containers) == 0 {
				return nil, errors.New("invalid draft JSON nesting")
			}
			containers = containers[:len(containers)-1]
			output = append(output, value)
		default:
			output = append(output, value)
		}
		previous = value
	}
}

func omittedDraftSummaryString(field string) bool {
	switch strings.ToLower(field) {
	case "body", "body_source", "body_html":
		return true
	default:
		return false
	}
}

func readDraftSummaryString(reader *bufio.Reader, output []byte, omit bool) ([]byte, error) {
	output = append(output, '"')
	for {
		value, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if value == '"' {
			return append(output, '"'), nil
		}
		if value < 0x20 {
			return nil, errors.New("invalid control byte in draft JSON string")
		}
		if !omit {
			if len(output) >= maximumDraftSummaryBytes {
				return nil, errors.New("draft summary metadata exceeds 1 MiB")
			}
			output = append(output, value)
		}
		if value == '\\' {
			output, err = readDraftSummaryEscape(reader, output, omit)
			if err != nil {
				return nil, err
			}
		}
	}
}

func readDraftSummaryEscape(reader *bufio.Reader, output []byte, omit bool) ([]byte, error) {
	value, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	if !strings.ContainsRune(`"\/bfnrtu`, rune(value)) {
		return nil, errors.New("invalid escape in draft JSON string")
	}
	if !omit {
		output = append(output, value)
	}
	if value != 'u' {
		return output, nil
	}
	for range 4 {
		value, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if (value < '0' || value > '9') && (value < 'a' || value > 'f') && (value < 'A' || value > 'F') {
			return nil, errors.New("invalid Unicode escape in draft JSON string")
		}
		if !omit {
			output = append(output, value)
		}
	}
	return output, nil
}
