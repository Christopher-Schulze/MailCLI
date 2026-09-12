package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func BenchmarkProjectedRawOutput(b *testing.B) {
	for _, fixture := range []struct {
		name  string
		size  int
		limit int64
		want  int
	}{
		{"tiny_allowed", 64, 1 << 20, 0},
		{"1MiB_allowed", 1 << 20, 64 << 20, 0},
		{"8MiB_allowed", 8 << 20, 64 << 20, 0},
		{"8MiB_rejected", 8 << 20, 1 << 20, 1},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			body := strings.Repeat("x", fixture.size)
			data := responseData{RawSource: &body}
			options := outputOptions{target: projectionTargetRaw, view: outputViewFull, maxBytes: fixture.limit}
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			for b.Loop() {
				var output bytes.Buffer
				code := writeProjectedSuccess(&output, "messages.raw", data, options)
				if code != fixture.want || FinalizeJSON(io.Discard, []string{"messages", "raw", "--json"}, output.Bytes(), code, nil) != fixture.want {
					b.Fatal("unexpected output status", code)
				}
			}
		})
	}
}
