package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareReleaseVersions(t *testing.T) {
	tests := []struct {
		name       string
		current    string
		latest     string
		comparison int
	}{
		{name: "newer release", current: "1.0.4", latest: "v1.0.5", comparison: -1},
		{name: "same release", current: "1.0.4", latest: "v1.0.4", comparison: 0},
		{name: "newer installed", current: "2.0.0", latest: "v1.9.9", comparison: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			latest, comparison, err := compareReleaseVersions(test.current, test.latest)
			if err != nil || latest != strings.TrimPrefix(test.latest, "v") || comparison != test.comparison {
				t.Fatalf("compareReleaseVersions() = %q, %d, %v", latest, comparison, err)
			}
		})
	}
}

func TestPerformUpdateAlreadyUpToDate(t *testing.T) {
	server := newUpdateTestServer(t, "1.0.4", nil, nil)
	defer server.Close()
	environment := updateTestEnvironment(t, server, "1.0.4")
	result, err := performUpdate(
		context.Background(), environment, newUpdateReporter(io.Discard, false, false),
	)
	if err != nil || result.Updated || result.LatestVersion != "1.0.4" {
		t.Fatalf("performUpdate() = %+v, %v", result, err)
	}
}

func TestPerformUpdateInstallsVerifiedRelease(t *testing.T) {
	archive := buildTestUpdateArchive(t, "1.0.5")
	checksums := checksumFile("mailcli_1.0.5_darwin_arm64.tar.gz", archive)
	server := newUpdateTestServer(t, "1.0.5", archive, checksums)
	defer server.Close()
	environment := updateTestEnvironment(t, server, "1.0.4")
	createInstalledUpdateFixture(t, environment, "1.0.4", "old skill")

	result, err := performUpdate(
		context.Background(), environment, newUpdateReporter(io.Discard, false, false),
	)
	if err != nil || !result.Updated || result.LatestVersion != "1.0.5" {
		t.Fatalf("performUpdate() = %+v, %v", result, err)
	}
	if err := verifyBinaryVersion(context.Background(), environment.executablePath, "1.0.5"); err != nil {
		t.Fatal(err)
	}
	skill, err := os.ReadFile(filepath.Join(environment.homeDirectory, ".agents", "skills", "mailcli", "SKILL.md"))
	if err != nil || string(skill) != "new skill\n" {
		t.Fatalf("installed skill = %q, %v", skill, err)
	}
	if _, err := os.Stat(environment.executablePath + ".mailcli-backup"); !os.IsNotExist(err) {
		t.Fatalf("binary backup remains after update: %v", err)
	}
}

func TestPerformUpdateRejectsChecksumMismatch(t *testing.T) {
	archive := buildTestUpdateArchive(t, "1.0.5")
	server := newUpdateTestServer(
		t, "1.0.5", archive,
		[]byte(strings.Repeat("0", 64)+"  mailcli_1.0.5_darwin_arm64.tar.gz\n"),
	)
	defer server.Close()
	environment := updateTestEnvironment(t, server, "1.0.4")
	createInstalledUpdateFixture(t, environment, "1.0.4", "old skill")

	_, err := performUpdate(
		context.Background(), environment, newUpdateReporter(io.Discard, false, false),
	)
	if updateErrorCodeForTest(err) != "update_checksum_mismatch" {
		t.Fatalf("performUpdate() error = %v", err)
	}
	if verifyErr := verifyBinaryVersion(context.Background(), environment.executablePath, "1.0.4"); verifyErr != nil {
		t.Fatalf("previous binary changed after checksum rejection: %v", verifyErr)
	}
}

func TestPerformUpdateRejectsInvalidReleaseSignature(t *testing.T) {
	archive := buildTestUpdateArchive(t, "1.0.5")
	checksums := checksumFile("mailcli_1.0.5_darwin_arm64.tar.gz", archive)
	server := newUpdateTestServerWithSignature(t, "1.0.5", archive, checksums, true)
	defer server.Close()
	environment := updateTestEnvironment(t, server, "1.0.4")
	createInstalledUpdateFixture(t, environment, "1.0.4", "old skill")

	_, err := performUpdate(
		context.Background(), environment, newUpdateReporter(io.Discard, false, false),
	)
	if updateErrorCodeForTest(err) != "update_signature_invalid" {
		t.Fatalf("performUpdate() error = %v", err)
	}
	if verifyErr := verifyBinaryVersion(context.Background(), environment.executablePath, "1.0.4"); verifyErr != nil {
		t.Fatalf("previous binary changed after signature rejection: %v", verifyErr)
	}
}

func TestPerformUpdatePreservesInstallationWhenInstallerFails(t *testing.T) {
	archive := buildTestUpdateArchive(t, "1.0.5")
	server := newUpdateTestServer(
		t, "1.0.5", archive, checksumFile("mailcli_1.0.5_darwin_arm64.tar.gz", archive),
	)
	defer server.Close()
	environment := updateTestEnvironment(t, server, "1.0.4")
	createInstalledUpdateFixture(t, environment, "1.0.4", "old skill")
	environment.installPackage = func(context.Context, string, string, string) error {
		return errors.New("injected installer failure")
	}

	_, err := performUpdate(
		context.Background(), environment, newUpdateReporter(io.Discard, false, false),
	)
	if updateErrorCodeForTest(err) != "update_install_failed" {
		t.Fatalf("performUpdate() error = %v", err)
	}
	if verifyErr := verifyBinaryVersion(context.Background(), environment.executablePath, "1.0.4"); verifyErr != nil {
		t.Fatalf("previous binary was not restored: %v", verifyErr)
	}
	skill, readErr := os.ReadFile(filepath.Join(environment.homeDirectory, ".agents", "skills", "mailcli", "SKILL.md"))
	if readErr != nil || string(skill) != "old skill\n" {
		t.Fatalf("previous skill was not restored: %q, %v", skill, readErr)
	}
}

func TestDownloadUpdateResourceRejectsDeclaredOversize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Length", fmt.Sprint(maximumChecksumFile+1))
		_, _ = writer.Write([]byte("too large"))
	}))
	defer server.Close()
	_, err := downloadUpdateResource(
		context.Background(), server.Client(), server.URL, maximumChecksumFile,
	)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("downloadUpdateResource() error = %v", err)
	}
}

func TestExtractReleaseArchiveRejectsTraversal(t *testing.T) {
	archive := buildUpdateArchive(t, map[string]updateArchiveFile{
		"../escape": {content: "escape", mode: 0o600},
	})
	err := extractReleaseArchive(archive, t.TempDir(), "mailcli_1.0.5_darwin_arm64")
	if updateErrorCodeForTest(err) != "update_package_invalid" {
		t.Fatalf("extractReleaseArchive() error = %v", err)
	}
}

func TestDisabledUpdateReporterWritesNothing(t *testing.T) {
	var output bytes.Buffer
	err := newUpdateReporter(&output, false, false).step("Checking", func() error { return nil })
	if err != nil || output.Len() != 0 {
		t.Fatalf("step() output = %q, error = %v", output.String(), err)
	}
}

func TestUpdateRejectsInsecureReleaseURLs(t *testing.T) {
	if err := validateUpdateURL("http://github.com/release", false); err == nil {
		t.Fatal("validateUpdateURL(http) error = nil")
	}
	if err := validateUpdateURL("https://user@example.com/release", false); err == nil {
		t.Fatal("validateUpdateURL(credentials) error = nil")
	}
	if err := validateUpdateURL("https://github.com/release", false); err != nil {
		t.Fatalf("validateUpdateURL(https) error = %v", err)
	}
}

func TestUpdateRedirectRejectsCredentials(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://user@example.com/release", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := secureUpdateRedirect(request, []*http.Request{{}}); err == nil {
		t.Fatal("secureUpdateRedirect(credentials) error = nil")
	}
}

func TestUpdateInstallerEnvironmentDropsShellInjection(t *testing.T) {
	environment := updateInstallerEnvironment([]string{
		"PATH=/tmp/injected", "BASH_ENV=/tmp/inject", "ENV=/tmp/inject",
		"MAILCLI_SKILL_DESTINATION=/tmp/skill", "BASH_FUNC_mv%%=() { false; }",
		"DYLD_INSERT_LIBRARIES=/tmp/inject.dylib", "LD_PRELOAD=/tmp/inject.so",
	}, "/Users/test", "/Users/test/.local/bin/mailcli")
	joined := strings.Join(environment, "\n")
	for _, forbidden := range []string{
		"PATH=/tmp/injected", "BASH_ENV=", "ENV=", "MAILCLI_SKILL_DESTINATION=", "BASH_FUNC_",
		"DYLD_", "LD_",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("installer environment retained %q: %q", forbidden, joined)
		}
	}
	for _, required := range []string{
		"PATH=/usr/bin:/bin", "HOME=/Users/test",
		"MAILCLI_BINARY_DESTINATION=/Users/test/.local/bin/mailcli",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("installer environment is missing %q: %q", required, joined)
		}
	}
}

func TestUpdateLockRejectsSymbolicLink(t *testing.T) {
	home := t.TempDir()
	stateParent := filepath.Join(home, "Library", "Application Support")
	if err := os.MkdirAll(stateParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(stateParent, "MailCLI")); err != nil {
		t.Fatal(err)
	}
	_, err := acquireUpdateLock(context.Background(), home)
	if updateErrorCodeForTest(err) != "update_lock_failed" {
		t.Fatalf("acquireUpdateLock() error = %v", err)
	}
}

func TestUpdateLockSerializesConcurrentInstallers(t *testing.T) {
	home := t.TempDir()
	release, err := acquireUpdateLock(context.Background(), home)
	if err != nil {
		t.Fatalf("first acquireUpdateLock() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = acquireUpdateLock(ctx, home)
	if updateErrorCodeForTest(err) != "update_busy" {
		t.Fatalf("second acquireUpdateLock() error = %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release update lock error = %v", err)
	}
}

type updateTestServer struct {
	*httptest.Server
	publicKey ed25519.PublicKey
}

func newUpdateTestServer(
	t *testing.T,
	releaseVersion string,
	archive []byte,
	checksums []byte,
) *updateTestServer {
	return newUpdateTestServerWithSignature(t, releaseVersion, archive, checksums, false)
}

func newUpdateTestServerWithSignature(
	t *testing.T,
	releaseVersion string,
	archive []byte,
	checksums []byte,
	corruptSignature bool,
) *updateTestServer {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(privateKey, checksums)
	if corruptSignature {
		signature[0] ^= 0xff
	}
	encodedSignature := []byte(base64.StdEncoding.EncodeToString(signature) + "\n")
	archiveName := "mailcli_" + releaseVersion + "_darwin_arm64.tar.gz"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/latest":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(
				writer,
				`{"tag_name":"v%s","html_url":"%s/release","assets":[`+
					`{"name":"%s","browser_download_url":"%s/archive"},`+
					`{"name":"SHA256SUMS","browser_download_url":"%s/checksums"},`+
					`{"name":"SHA256SUMS.sig","browser_download_url":"%s/signature"}]}`,
				releaseVersion, server.URL, archiveName, server.URL, server.URL, server.URL,
			)
		case "/archive":
			_, _ = writer.Write(archive)
		case "/checksums":
			_, _ = writer.Write(checksums)
		case "/signature":
			_, _ = writer.Write(encodedSignature)
		default:
			http.NotFound(writer, request)
		}
	}))
	return &updateTestServer{Server: server, publicKey: publicKey}
}

func updateTestEnvironment(t *testing.T, server *updateTestServer, currentVersion string) updateEnvironment {
	t.Helper()
	testRoot := t.TempDir()
	environment := updateEnvironment{
		client: server.Client(), metadataURL: server.URL + "/latest", currentVersion: currentVersion,
		executablePath: filepath.Join(testRoot, "bin", "mailcli"), homeDirectory: filepath.Join(testRoot, "home"),
		operatingSystem: "darwin", architecture: "arm64",
		allowInsecureURLs: true,
		verifyPackage:     verifyBinaryVersion, installPackage: runReleaseInstaller,
		verifyInstallation: verifyBinaryVersion,
		releasePublicKey:   server.publicKey,
	}
	if err := os.MkdirAll(environment.homeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	return environment
}

func createInstalledUpdateFixture(
	t *testing.T,
	environment updateEnvironment,
	installedVersion string,
	skillContent string,
) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(environment.executablePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		environment.executablePath, []byte(testUpdateBinary(installedVersion)), 0o755,
	); err != nil {
		t.Fatal(err)
	}
	skillRoot := filepath.Join(environment.homeDirectory, ".agents", "skills", "mailcli")
	if err := os.MkdirAll(filepath.Join(skillRoot, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillRoot, "SKILL.md"), []byte(skillContent+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillRoot, "agents", "openai.yaml"), []byte("old agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func buildTestUpdateArchive(t *testing.T, releaseVersion string) []byte {
	t.Helper()
	installer, err := os.ReadFile(filepath.Join("..", "..", "scripts", "release", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	root := "mailcli_" + releaseVersion + "_darwin_arm64"
	return buildUpdateArchive(t, map[string]updateArchiveFile{
		root + "/bin/mailcli":                       {content: testUpdateBinary(releaseVersion), mode: 0o755},
		root + "/skills/mailcli/SKILL.md":           {content: "new skill\n", mode: 0o600},
		root + "/skills/mailcli/agents/openai.yaml": {content: "new agent\n", mode: 0o600},
		root + "/install.sh":                        {content: string(installer), mode: 0o755},
		root + "/README.md":                         {content: "readme\n", mode: 0o600},
		root + "/LICENSE":                           {content: "license\n", mode: 0o600},
	})
}

type updateArchiveFile struct {
	content string
	mode    int64
}

func buildUpdateArchive(t *testing.T, files map[string]updateArchiveFile) []byte {
	t.Helper()
	var archive bytes.Buffer
	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, file := range files {
		header := &tar.Header{Name: name, Mode: file.mode, Size: int64(len(file.content)), Typeflag: tar.TypeReg}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(file.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func checksumFile(archiveName string, archive []byte) []byte {
	digest := sha256.Sum256(archive)
	return []byte(fmt.Sprintf("%x  %s\n", digest, archiveName))
}

func testUpdateBinary(releaseVersion string) string {
	return "#!/bin/sh\nif [ \"${1:-}\" = version ]; then printf 'mailcli " + releaseVersion + "\\n'; exit 0; fi\nexit 2\n"
}

func updateErrorCodeForTest(err error) string {
	var coded codedError
	if errors.As(err, &coded) {
		return coded.ErrorCode()
	}
	return ""
}
