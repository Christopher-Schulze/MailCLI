package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mailmodel "mailcli/internal/mail"
)

func TestReadBoundedDraftBody(t *testing.T) {
	t.Parallel()

	body, err := readBoundedDraftBody(strings.NewReader("hello body"))
	if err != nil {
		t.Fatalf("readBoundedDraftBody() error = %v", err)
	}
	if body != "hello body" {
		t.Fatalf("body = %q", body)
	}
}

func TestReadBoundedDraftBodyRejectsOversize(t *testing.T) {
	t.Parallel()

	_, err := readBoundedDraftBody(bytes.NewReader(bytes.Repeat([]byte("x"), mailmodel.MaximumDraftBodyBytes+1)))
	if err == nil {
		t.Fatal("error = nil, want oversize")
	}
}

func TestReadDraftBodyRequiresPath(t *testing.T) {
	t.Parallel()

	if _, err := readDraftBody(""); err == nil {
		t.Fatal("error = nil, want path required")
	}
}

func TestReadDraftBodyFromFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(path, []byte("file body"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	body, err := readDraftBody(path)
	if err != nil {
		t.Fatalf("readDraftBody() error = %v", err)
	}
	if body != "file body" {
		t.Fatalf("body = %q", body)
	}
}

func TestParseRecipientFlagsRejectsInvalid(t *testing.T) {
	t.Parallel()

	if _, err := parseRecipientFlags([]string{"not-an-address"}); err == nil {
		t.Fatal("error = nil, want invalid recipient")
	}
	got, err := parseRecipientFlags([]string{"Name <ok@example.com>"})
	if err != nil {
		t.Fatalf("parseRecipientFlags() error = %v", err)
	}
	if len(got) != 1 || got[0].Address != "ok@example.com" {
		t.Fatalf("got = %#v", got)
	}
}
