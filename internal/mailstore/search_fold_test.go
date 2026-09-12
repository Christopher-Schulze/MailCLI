package mailstore

import (
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

func TestSearchFoldUnicodePolicy(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"", ""}, {"ASCII Words\t123", "ascii words\t123"},
		{"Cafe\u0301", "café"}, {"İKẞΣς", "ikßσς"},
		{"\xffA\xc0\xaf", "�a��"}, {"�", "�"},
	} {
		t.Run(test.input, func(t *testing.T) {
			if got := foldSearchText(test.input); got != test.want {
				t.Fatalf("foldSearchText(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestSearchFoldMatchesStandardUnicodeMapping(t *testing.T) {
	check := func(value string) {
		t.Helper()
		if got, want := foldSearchText(value), strings.ToLower(norm.NFC.String(value)); got != want {
			t.Fatalf("foldSearchText(%q) = %q, want %q", value, got, want)
		}
	}
	for character := rune(0); character <= utf8.MaxRune; character++ {
		check(string(character))
	}
	for first := range 256 {
		for second := range 256 {
			check(string([]byte{byte(first), byte(second)}))
		}
	}
	random := rand.New(rand.NewPCG(17, 91))
	for range 10000 {
		var input [64]byte
		for index := range input {
			input[index] = byte(random.Uint32())
		}
		check(string(input[:]))
	}
}

func TestSearchFoldUnchangedASCIIAllocatesNothing(t *testing.T) {
	input := strings.Repeat("lowercase words 0123\n", 1024)
	var output string
	allocations := testing.AllocsPerRun(20, func() { output = foldSearchText(input) })
	if output != input || allocations != 0 {
		t.Fatalf("unchanged fold: equal=%t, allocations=%g", output == input, allocations)
	}
}

func BenchmarkSearchFold(b *testing.B) {
	for _, fixture := range []struct{ name, value string }{
		{"ascii_lower_1MiB", strings.Repeat("a", 1<<20)},
		{"ascii_mixed_1MiB", strings.Repeat("Hello World ", 87381)},
		{"unicode_normalization", strings.Repeat("Cafe\u0301 ", 1<<17)},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(fixture.value)))
			for b.Loop() {
				if foldSearchText(fixture.value) == "" {
					b.Fatal("folding lost content")
				}
			}
		})
	}
}
