package mailstore

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestMIMEPlainTextTrimAndOwnership(t *testing.T) {
	inputs := [][]byte{nil, []byte(""), []byte(" \t\r\n"), []byte("  kept\ninside  ")}
	for character := rune(0); character <= utf8.MaxRune; character++ {
		if unicode.IsSpace(character) {
			inputs = append(inputs, []byte(string(character)+"kept"+string(character)))
		}
	}
	for value := 0; value < 256; value++ {
		inputs = append(inputs, []byte{' ', byte(value), 'x', byte(value), ' '})
	}
	inputs = append(inputs, []byte("\xff\xc0\x80 kept \xe2\x80"), []byte(strings.Repeat(" ", 1024*1024)+"kept"+strings.Repeat(" ", 1024*1024)))
	for index, input := range inputs {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			want := strings.TrimSpace(string(input))
			raw := append([]byte("Content-Type: text/plain\r\n\r\n"), input...)
			document, err := parseMIMEDocument(bytes.NewReader(raw), false, false, true)
			if err != nil || !document.Complete || document.Content != want {
				t.Fatalf("trimmed text = %q, want %q, complete=%t, error=%v", document.Content, want, document.Complete, err)
			}
			clear(raw)
			clear(input)
			if document.Content != want {
				t.Fatal("owned result aliases mutable source storage")
			}
		})
	}
}

func BenchmarkMIMETextOwnership(b *testing.B) {
	for _, fixture := range []struct{ name, body, want string }{
		{"ordinary", "  kept  ", "kept"},
		{"trimmed_4MiB", strings.Repeat(" ", 2*1024*1024-2) + "kept" + strings.Repeat(" ", 2*1024*1024-2), "kept"},
		{"empty_4MiB", strings.Repeat(" ", 4*1024*1024), ""},
		{"untrimmed_4MiB", strings.Repeat("x", 4*1024*1024), strings.Repeat("x", 4*1024*1024)},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			raw := []byte("Content-Type: text/plain\r\n\r\n" + fixture.body)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			var document mimeDocument
			var err error
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				document, err = parseMIMEDocument(bytes.NewReader(raw), false, false, true)
				if err != nil || !document.Complete || document.Content != fixture.want {
					b.Fatalf("content length = %d, complete=%t, error=%v", len(document.Content), document.Complete, err)
				}
			}
			b.StopTimer()
			runtime.GC()
			runtime.ReadMemStats(&after)
			b.ReportMetric(float64(max(0, int64(after.HeapAlloc)-int64(before.HeapAlloc))), "retained_B")
			runtime.KeepAlive(document)
			runtime.KeepAlive(raw)
		})
	}
}
