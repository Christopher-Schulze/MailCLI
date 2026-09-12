package mail

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestPlainTextTrimmedResultOwnership(t *testing.T) {
	for _, padding := range []int{0, 2047, 2048, 4096, 1024 * 1024} {
		t.Run(fmt.Sprint(padding), func(t *testing.T) {
			source := "<pre>" + strings.Repeat("\n", padding) + "kept" + strings.Repeat("\n", padding) + "</pre>"
			root, err := html.Parse(strings.NewReader(source))
			if err != nil {
				t.Fatal(err)
			}
			result, err := renderPlainText(context.Background(), root, MaximumDraftBodyBytes)
			if err != nil || result != "kept" {
				t.Fatalf("trimmed text = %q, error = %v", result, err)
			}
			for range 3 {
				small, err := htmlDraftText(strings.NewReader("<p>next</p>"))
				if err != nil || small != "next" || result != "kept" {
					t.Fatalf("result lifetime changed: %q, %q, %v", result, small, err)
				}
			}
		})
	}
}

func BenchmarkPlainTextOwnership(b *testing.B) {
	for _, fixture := range []struct{ name, source, want string }{
		{"ordinary", "<p>kept</p>", "kept"},
		{"trimmed_2MiB", "<pre>" + strings.Repeat("\n", 1024*1024-2) + "kept" + strings.Repeat("\n", 1024*1024-2) + "</pre>", "kept"},
		{"empty_2MiB", "<pre>" + strings.Repeat("\n", 2*1024*1024) + "</pre>", ""},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			root, err := html.Parse(strings.NewReader(fixture.source))
			if err != nil {
				b.Fatal(err)
			}
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			var result string
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				result, err = renderPlainText(context.Background(), root, MaximumDraftBodyBytes)
				if err != nil || result != fixture.want {
					b.Fatalf("result = %q, error = %v", result, err)
				}
			}
			b.StopTimer()
			runtime.GC()
			runtime.ReadMemStats(&after)
			b.ReportMetric(float64(max(0, int64(after.HeapAlloc)-int64(before.HeapAlloc))), "retained_B")
			runtime.KeepAlive(result)
			runtime.KeepAlive(root)
		})
	}
}

func BenchmarkContentDiagnosticKeys(b *testing.B) {
	for _, fixture := range []struct {
		name            string
		count, distinct int
	}{
		{"single", 1, 1},
		{"repeated", 4096, 4},
		{"distinct", 128, 128},
	} {
		keys := make([]string, fixture.distinct)
		for i := range keys {
			keys[i] = fmt.Sprintf("data-attribute-%d", i)
		}
		b.Run(fixture.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				var collector contentDiagnosticCollector
				for i := range fixture.count {
					collector.add(ContentDiagnosticRemovedAttribute, "p", keys[i%len(keys)])
				}
				if len(collector.values) != fixture.distinct {
					b.Fatal("diagnostic deduplication changed")
				}
			}
		})
	}
}
