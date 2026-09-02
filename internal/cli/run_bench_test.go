package cli

import (
	"context"
	"io"
	"testing"

	"mailcli/internal/mail"
)

// BenchmarkRunVersionJSON measures the full in-process command dispatch for
// `mailcli version --json`: argument normalization, routing, envelope
// construction, and JSON serialization. This isolates the CLI-layer share of
// startup cost (everything in the process except Go runtime and package
// initialization).
func BenchmarkRunVersionJSON(b *testing.B) {
	ctx := context.Background()
	service := mail.NewServiceWithTransport(nil, "", mail.SendTransport{})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if code := Run(ctx, service, []string{"version", "--json"}, io.Discard, io.Discard); code != 0 {
			b.Fatalf("unexpected exit code %d", code)
		}
	}
}

// BenchmarkRunHelp measures bare `mailcli` help rendering for comparison
// against the JSON path.
func BenchmarkRunHelp(b *testing.B) {
	ctx := context.Background()
	service := mail.NewServiceWithTransport(nil, "", mail.SendTransport{})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if code := Run(ctx, service, nil, io.Discard, io.Discard); code != 0 {
			b.Fatalf("unexpected exit code %d", code)
		}
	}
}
