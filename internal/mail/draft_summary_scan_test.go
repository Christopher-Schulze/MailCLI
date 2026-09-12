package mail

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDraftSummaryChunkBoundaries(t *testing.T) {
	for _, size := range []int{16, 4096, 32768} {
		for _, field := range []string{"body", "subject"} {
			for _, suffix := range []string{`"}`, `\n"}`, `\u00e4"}`, `\""}`, `\\\"x"}`, `\q"}`, `\u12xx"}`, `\`, "\x00", "\n", ""} {
				for offset := -8; offset <= 8; offset++ {
					input := `{"` + field + `":"` + strings.Repeat("x", max(0, size+offset-len(field)-5)) + suffix
					want, oldErr := referenceSummaryJSON(bufio.NewReaderSize(strings.NewReader(input), size))
					got, err := readDraftSummaryJSON(bufio.NewReaderSize(strings.NewReader(input), size))
					if (err == nil) != (oldErr == nil) || err == nil && !bytes.Equal(got, want) {
						t.Fatalf("size=%d field=%s offset=%d suffix=%q: got %q, %v; want %q, %v", size, field, offset, suffix, got, err, want, oldErr)
					}
				}
			}
		}
	}
}

func FuzzDraftSummaryChunkEquivalence(f *testing.F) {
	for _, input := range []string{`{"body":"hello","subject":"keep"}`, `{"body":"\u1234\\"}`, `{"b\u006fdy":"skip"}`} {
		f.Add(input)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 64*1024 {
			return
		}
		got, err := readDraftSummaryJSON(bufio.NewReaderSize(strings.NewReader(input), 16))
		want, oldErr := referenceSummaryJSON(bufio.NewReaderSize(strings.NewReader(input), 16))
		if (err == nil) != (oldErr == nil) || err == nil && !bytes.Equal(got, want) {
			t.Fatalf("projection differs: %q, %v; reference %q, %v", got, err, want, oldErr)
		}
	})
}

func BenchmarkDraftSummaryBuffer(b *testing.B) {
	for _, bodySize := range []int{1, 1 << 20} {
		path := filepath.Join(b.TempDir(), "draft.json")
		payload := `{"body":"` + strings.Repeat("x", bodySize) + `","subject":"retained"}`
		if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
			b.Fatal(err)
		}
		for _, size := range []int{4096, 16384, 32768, 65536} {
			b.Run(fmt.Sprintf("body-%d/buffer-%d", bodySize, size), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(payload)))
				for range b.N {
					file, err := os.Open(path)
					if err != nil {
						b.Fatal(err)
					}
					out, err := readDraftSummaryJSON(bufio.NewReaderSize(file, size))
					err = errors.Join(err, file.Close())
					if err != nil || string(out) != `{"body":"","subject":"retained"}` {
						b.Fatalf("projection %q: %v", out, err)
					}
				}
			})
		}
	}
}

// Frozen bytewise lexer from the parent commit, used as a semantic oracle.
func referenceSummaryJSON(reader *bufio.Reader) ([]byte, error) {
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
			output, err = referenceSummaryString(reader, output, omit)
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

func referenceSummaryString(reader *bufio.Reader, output []byte, omit bool) ([]byte, error) {
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
			output, err = referenceSummaryEscape(reader, output, omit)
			if err != nil {
				return nil, err
			}
		}
	}
}

func referenceSummaryEscape(reader *bufio.Reader, output []byte, omit bool) ([]byte, error) {
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
