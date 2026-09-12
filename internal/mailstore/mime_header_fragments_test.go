package mailstore

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestRawHeaderBorrowedFragments(t *testing.T) {
	for _, size := range []int{16, mimeHeaderReaderBuffer} {
		for _, ending := range []string{"\n", "\r\n"} {
			for offset := -2; offset <= 2; offset++ {
				header := "X: " + strings.Repeat("v", size+offset-3) + ending + " Folded" + ending + "Subject: keep" + ending + ending
				for _, mode := range []string{"ordinary", "one-byte", "short", "data-eof"} {
					t.Run(fmt.Sprintf("%d/%q/%d/%s", size, ending, offset, mode), func(t *testing.T) {
						var source io.Reader = strings.NewReader(header + strings.Repeat("body", size))
						switch mode {
						case "one-byte":
							source = iotest.OneByteReader(source)
						case "short":
							source = iotest.HalfReader(source)
						case "data-eof":
							source = iotest.DataErrReader(source)
						}
						reader := bufio.NewReaderSize(source, size)
						got, err := readRawHeaderBlock(reader)
						if err != nil || got != header {
							t.Fatalf("header = %q, %v; want %q", got, err, header)
						}
						if _, err := io.Copy(io.Discard, reader); err != nil {
							t.Fatal(err)
						}
						if got != header {
							t.Fatal("header aliased the reused read buffer")
						}
					})
				}
			}
		}
	}
}

func TestRawHeaderFragmentErrors(t *testing.T) {
	failure := errors.New("read failure")
	for _, test := range []struct {
		name   string
		source io.Reader
		want   error
	}{
		{"partial line", io.MultiReader(strings.NewReader(strings.Repeat("x", 64)), iotest.ErrReader(failure)), failure},
		{"partial separator", io.MultiReader(strings.NewReader("X: v\r\n\r"), iotest.ErrReader(failure)), failure},
		{"empty", iotest.ErrReader(failure), failure},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := readRawHeaderBlock(bufio.NewReaderSize(test.source, 16))
			if got != "" || !errors.Is(err, test.want) {
				t.Fatalf("partial header escaped: %q, %v", got, err)
			}
		})
	}
	for _, input := range []string{"", "X: v\r\n", strings.Repeat("v", 64) + "\n"} {
		got, err := readRawHeaderBlock(bufio.NewReaderSize(strings.NewReader(input), 16))
		if got != "" || errorCodeForTest(err) != "invalid_message_source" {
			t.Fatalf("missing boundary accepted: %q, %v", got, err)
		}
	}
}
