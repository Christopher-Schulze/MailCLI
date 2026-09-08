package mailstore

import (
	"os"
	"path/filepath"
	"strings"
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

func TestParseVersionDirectoryNameRequiresNumericSuffix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		version int
		valid   bool
	}{
		{name: "V10", version: 10, valid: true},
		{name: "V010", version: 10, valid: true},
		{name: "V0", valid: false},
		{name: "V+10", valid: false},
		{name: "V10x", valid: false},
		{name: "10", valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			version, valid := parseVersionDirectoryName(test.name)
			if version != test.version || valid != test.valid {
				t.Fatalf("parseVersionDirectoryName(%q) = (%d, %t), want (%d, %t)", test.name, version, valid, test.version, test.valid)
			}
		})
	}
}

func TestDiscoverVersionRootRejectsNewerLayout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "V11", "MailData", envelopeIndexName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("database"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := discoverVersionRoot(root); errorCodeForTest(err) != "unsupported_mail_store_schema" {
		t.Fatalf("discoverVersionRoot() error = %v, want unsupported_mail_store_schema", err)
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

func TestDiscoverVersionRootRefusesAmbiguousNewerStore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeStoreGeneration(t, root, "V10", true)
	writeStoreGeneration(t, root, "V11", true)

	_, err := discoverVersionRoot(root)
	if errorCodeForTest(err) != "ambiguous_mail_store_generation" {
		t.Fatalf("discoverVersionRoot() error = %v, want ambiguous_mail_store_generation", err)
	}
	for _, value := range []string{"V10", "V11", "PersistenceInfo.plist"} {
		if !strings.Contains(err.Error(), value) {
			t.Fatalf("discoverVersionRoot() error = %v, want %q", err, value)
		}
	}
}

func TestDiscoverVersionRootUsesPersistenceInfoActiveGeneration(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	supported := writeStoreGeneration(t, root, "V10", true)
	writeStoreGeneration(t, root, "V11", true)
	writePersistenceInfo(t, root, "V10")

	got, err := discoverVersionRoot(root)
	if err != nil {
		t.Fatalf("discoverVersionRoot() error = %v", err)
	}
	if got != supported {
		t.Fatalf("discoverVersionRoot() = %q, want %q", got, supported)
	}
}

func TestDiscoverVersionRootRefusesSameGenerationPeer(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeStoreGeneration(t, root, "V10", true)
	writeStoreGeneration(t, root, "V010", true)

	_, err := discoverVersionRoot(root)
	if errorCodeForTest(err) != "ambiguous_mail_store_generation" {
		t.Fatalf("discoverVersionRoot() error = %v, want ambiguous_mail_store_generation", err)
	}
}

func TestDiscoverVersionRootRejectsActiveNewerStore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeStoreGeneration(t, root, "V10", true)
	writeStoreGeneration(t, root, "V11", true)
	writePersistenceInfo(t, root, "V11")

	_, err := discoverVersionRoot(root)
	if errorCodeForTest(err) != "unsupported_mail_store_schema" {
		t.Fatalf("discoverVersionRoot() error = %v, want unsupported_mail_store_schema", err)
	}
	if !strings.Contains(err.Error(), "V11") {
		t.Fatalf("discoverVersionRoot() error = %v, want active V11 detail", err)
	}
}

func TestDiscoverVersionRootDoesNotTrustMalformedPersistenceInfo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeStoreGeneration(t, root, "V10", true)
	writeStoreGeneration(t, root, "V11", true)
	writePersistenceInfo(t, root, "current")

	_, err := discoverVersionRoot(root)
	if errorCodeForTest(err) != "ambiguous_mail_store_generation" {
		t.Fatalf("discoverVersionRoot() error = %v, want ambiguous_mail_store_generation", err)
	}
	if !strings.Contains(err.Error(), "not a numeric generation") {
		t.Fatalf("discoverVersionRoot() error = %v, want malformed marker detail", err)
	}
}

func TestSelectVersionRootExplicitPathOverridesAutomaticAmbiguity(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	supported := writeStoreGeneration(t, root, "V10", true)
	writeStoreGeneration(t, root, "V11", true)

	got, err := selectVersionRoot(Config{MailRoot: root, MailStorePath: supported})
	if err != nil {
		t.Fatalf("selectVersionRoot() error = %v", err)
	}
	if got != supported {
		t.Fatalf("selectVersionRoot() = %q, want %q", got, supported)
	}

	_, err = selectVersionRoot(Config{MailRoot: root, MailStorePath: filepath.Join(root, "V11")})
	if errorCodeForTest(err) != "unsupported_mail_store_schema" {
		t.Fatalf("selectVersionRoot() error = %v, want unsupported_mail_store_schema", err)
	}
}

func TestDiscoverVersionRootClassifiesIncompleteAndUnsafeCandidates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	incomplete := writeStoreGeneration(t, root, "V10", false)
	_, err := discoverVersionRoot(root)
	if errorCodeForTest(err) != "mail_store_unavailable" || !strings.Contains(err.Error(), incomplete) {
		t.Fatalf("discoverVersionRoot() error = %v, want unavailable candidate %q", err, incomplete)
	}

	root = t.TempDir()
	target := writeStoreGeneration(t, root, "outside", true)
	if err := os.Symlink(target, filepath.Join(root, "V10")); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	_, err = discoverVersionRoot(root)
	if errorCodeForTest(err) != "unsafe_message_source" || !strings.Contains(err.Error(), "V10") {
		t.Fatalf("discoverVersionRoot() error = %v, want unsafe V10 candidate: %v", err, err)
	}

	root = t.TempDir()
	incomplete = writeStoreGeneration(t, root, "V11", false)
	_, err = discoverVersionRoot(root)
	if errorCodeForTest(err) != "mail_store_unavailable" || !strings.Contains(err.Error(), incomplete) {
		t.Fatalf("discoverVersionRoot() error = %v, want unavailable newer candidate %q", err, incomplete)
	}

	root = t.TempDir()
	target = writeStoreGeneration(t, root, "outside", true)
	if err := os.Symlink(target, filepath.Join(root, "V11")); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	_, err = discoverVersionRoot(root)
	if errorCodeForTest(err) != "unsafe_message_source" || !strings.Contains(err.Error(), "V11") {
		t.Fatalf("discoverVersionRoot() error = %v, want unsafe V11 candidate: %v", err, err)
	}
}

func TestParsePersistenceInfoXML(t *testing.T) {
	t.Parallel()
	source := []byte(`<?xml version="1.0"?>
<!-- Mail may include unrelated plist keys. -->
<plist version="1.0"><dict>
<key>UnreadCounts</key><array><integer>1</integer></array>
<key>LastUsedVersionDirectoryName</key><string> V10 </string>
</dict>
</plist>`)
	got, err := parsePersistenceInfoXML(source)
	if err != nil {
		t.Fatalf("parsePersistenceInfoXML() error = %v", err)
	}
	if got != "V10" {
		t.Fatalf("parsePersistenceInfoXML() = %q, want V10", got)
	}
}

func TestParsePersistenceInfoXMLRejectsInvalidDocuments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		source string
	}{
		{name: "missing key", source: `<plist><dict><key>Other</key><string>V10</string></dict></plist>`},
		{name: "wrong value type", source: `<plist><dict><key>LastUsedVersionDirectoryName</key><integer>10</integer></dict></plist>`},
		{name: "duplicate key", source: `<plist><dict><key>LastUsedVersionDirectoryName</key><string>V10</string><key>LastUsedVersionDirectoryName</key><string>V11</string></dict></plist>`},
		{name: "trailing root", source: `<plist><dict><key>LastUsedVersionDirectoryName</key><string>V10</string></dict></plist><plist/>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parsePersistenceInfoXML([]byte(test.source)); err == nil {
				t.Fatalf("parsePersistenceInfoXML() error = nil")
			}
		})
	}
}

func TestReadPersistenceInfoFileEnforcesSizeLimit(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), persistenceInfoName)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maximumPersistenceInfoBytes+1)), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat() error = %v", err)
	}
	if _, err := readPersistenceInfoFile(path, info); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("readPersistenceInfoFile() error = %v, want size-limit error", err)
	}
}

func writeStoreGeneration(t *testing.T, root string, name string, withIndex bool) string {
	t.Helper()
	path := filepath.Join(root, name)
	mailData := filepath.Join(path, "MailData")
	if err := os.MkdirAll(mailData, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if withIndex {
		if err := os.WriteFile(filepath.Join(mailData, envelopeIndexName), []byte("database"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}
	return path
}

func writePersistenceInfo(t *testing.T, root string, active string) {
	t.Helper()
	source := `<plist version="1.0"><dict><key>LastUsedVersionDirectoryName</key><string>` + active + `</string></dict></plist>`
	if err := os.WriteFile(filepath.Join(root, persistenceInfoName), []byte(source), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}
