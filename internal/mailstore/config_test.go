package mailstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverVersionRootSelectsHighestReadableStore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, version := range []string{"V9", "V10", "Vbad"} {
		if err := os.MkdirAll(filepath.Join(root, version, "MailData"), 0o700); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
	}
	for _, version := range []string{"V9", "V10"} {
		path := filepath.Join(root, version, "MailData", envelopeIndexName)
		if err := os.WriteFile(path, []byte("database"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}
	got, err := discoverVersionRoot(root)
	if err != nil {
		t.Fatalf("discoverVersionRoot() error = %v", err)
	}
	if got != filepath.Join(root, "V10") {
		t.Fatalf("discoverVersionRoot() = %q", got)
	}
}

func TestDiscoverVersionRootRejectsSymlinkedIndex(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mailData := filepath.Join(root, "V10", "MailData")
	if err := os.MkdirAll(mailData, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	target := filepath.Join(root, "outside")
	if err := os.WriteFile(target, []byte("database"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Symlink(target, filepath.Join(mailData, envelopeIndexName)); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	if _, err := discoverVersionRoot(root); err == nil {
		t.Fatal("discoverVersionRoot() error = nil")
	}
}

func TestParseAccountOrderingXML(t *testing.T) {
	t.Parallel()
	source := []byte(`<?xml version="1.0"?><plist><array>
		<string>imap://951FB9AB-537B-4E97-8DCC-B241B71AD9DD/</string>
		<string>local://74D628F1-FF28-4691-AF4F-3679DFB2A397/</string>
	</array></plist>`)
	got, err := parseAccountOrderingXML(source)
	if err != nil {
		t.Fatalf("parseAccountOrderingXML() error = %v", err)
	}
	if len(got) != 2 || got[0] != "imap://951FB9AB-537B-4E97-8DCC-B241B71AD9DD/" {
		t.Fatalf("parseAccountOrderingXML() = %#v", got)
	}
}
