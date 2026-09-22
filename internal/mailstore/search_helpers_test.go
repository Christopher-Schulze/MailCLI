package mailstore

import (
	"testing"
)

func TestEmptySearchPage(t *testing.T) {
	t.Parallel()

	sqlPage := emptySearchPage(false)
	if sqlPage.Coverage.Backend != "envelope_sql" || !sqlPage.Coverage.Complete {
		t.Fatalf("sql page = %#v", sqlPage)
	}
	if len(sqlPage.Messages) != 0 {
		t.Fatalf("sql messages = %d", len(sqlPage.Messages))
	}
	streamPage := emptySearchPage(true)
	if streamPage.Coverage.Backend != "emlx_stream" {
		t.Fatalf("stream backend = %q", streamPage.Coverage.Backend)
	}
}

func TestRecipientAddressSet(t *testing.T) {
	t.Parallel()

	sets := newRecipientAddressSets()
	to, ok := recipientAddressSet(&sets, 0)
	if !ok || to == nil {
		t.Fatal("To set missing")
	}
	cc, ok := recipientAddressSet(&sets, 1)
	if !ok || cc == nil {
		t.Fatal("CC set missing")
	}
	bcc, ok := recipientAddressSet(&sets, 2)
	if !ok || bcc == nil {
		t.Fatal("BCC set missing")
	}
	if _, ok := recipientAddressSet(&sets, 9); ok {
		t.Fatal("unknown type returned a set")
	}
}

func TestEscapeLike(t *testing.T) {
	t.Parallel()

	got := escapeLike(`a%b_c\d`)
	want := `a\%b\_c\\d`
	if got != want {
		t.Fatalf("escapeLike() = %q, want %q", got, want)
	}
}

func TestEqualAddressSets(t *testing.T) {
	t.Parallel()

	left := newRecipientAddressSets()
	right := newRecipientAddressSets()
	left.To["a@example.com"] = struct{}{}
	right.To["a@example.com"] = struct{}{}
	if !equalAddressSets(left, right) {
		t.Fatal("equal sets reported unequal")
	}
	right.CC["b@example.com"] = struct{}{}
	if equalAddressSets(left, right) {
		t.Fatal("unequal sets reported equal")
	}
}

func TestNormalizedAddress(t *testing.T) {
	t.Parallel()

	if got := normalizedAddress("Name <A@Example.COM>"); got != "a@example.com" {
		t.Fatalf("normalizedAddress() = %q", got)
	}
}
