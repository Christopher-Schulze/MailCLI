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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

func TestPerformUpdateStagesNetworkBeforeLockAndSerializesConcurrentUpdates(t *testing.T) {
	fixture := newGatedUpdateFixture(t, "1.0.4", 2)
	installCount := countUpdateInstalls(&fixture.environment)
	lockIntervals := recordUpdateLockIntervals(&fixture.environment)
	updateContext, cancelUpdates := context.WithCancel(context.Background())
	defer cancelUpdates()
	startedAt := time.Now()
	attempts := startUpdateAttempts(updateContext, fixture.environment, 2)
	networkStart := waitForArchiveStart(t, fixture.archiveStarted)
	secondNetworkStart := waitForArchiveStart(t, fixture.archiveStarted)
	if secondNetworkStart.Before(networkStart) {
		networkStart = secondNetworkStart
	}
	assertInstallerLockAvailable(t, fixture.environment.homeDirectory)
	networkEnd := fixture.releaseArchiveAfter(networkStart, 250*time.Millisecond)
	updates := collectUpdateAttempts(t, attempts, 2)
	assertConcurrentUpdateResults(t, updates, installCount, fixture.environment)
	lockIntervals.assertExcludesNetworkDelay(t, startedAt, networkStart, networkEnd)
}

func TestPerformUpdateSkipsWhenSameOrNewerVersionWasInstalledDuringStaging(t *testing.T) {
	for _, installedVersion := range []string{"1.0.5", "1.0.6"} {
		t.Run(installedVersion, func(t *testing.T) {
			assertConcurrentVersionSkipsStagedUpdate(t, installedVersion)
		})
	}
}

func assertConcurrentVersionSkipsStagedUpdate(t *testing.T, installedVersion string) {
	t.Helper()
	fixture := newGatedUpdateFixture(t, "1.0.4", 1)
	installCount := countUpdateInstalls(&fixture.environment)
	updateContext, cancelUpdate := context.WithCancel(context.Background())
	defer cancelUpdate()
	attempts := startUpdateAttempts(updateContext, fixture.environment, 1)
	waitForArchiveStart(t, fixture.archiveStarted)
	installVersionDuringDownload(t, fixture.environment, installedVersion, "concurrent skill")
	fixture.releaseArchive()
	attempt := collectUpdateAttempts(t, attempts, 1)[0]
	if attempt.err != nil || attempt.result.Updated || attempt.result.CurrentVersion != installedVersion {
		t.Fatalf("stale update result = %+v, %v", attempt.result, attempt.err)
	}
	if installCount.Load() != 0 {
		t.Fatalf("stale update invoked installer %d times", installCount.Load())
	}
	if err := verifyBinaryVersion(context.Background(), fixture.environment.executablePath, installedVersion); err != nil {
		t.Fatalf("concurrently installed binary changed: %v", err)
	}
	skill, err := os.ReadFile(filepath.Join(fixture.environment.homeDirectory, ".agents", "skills", "mailcli", "SKILL.md"))
	if err != nil || string(skill) != "concurrent skill\n" {
		t.Fatalf("concurrently installed skill changed: %q, %v", skill, err)
	}
}

func TestRevalidateStagedUpdateRejectsChangedArtifacts(t *testing.T) {
	archive := buildTestUpdateArchive(t, "1.0.5")
	checksums := checksumFile("mailcli_1.0.5_darwin_arm64.tar.gz", archive)
	server := newUpdateTestServer(t, "1.0.5", archive, checksums)
	defer server.Close()
	environment := updateTestEnvironment(t, server, "1.0.4")
	release, metadata, err := fetchLatestRelease(context.Background(), environment)
	if err != nil {
		t.Fatal(err)
	}
	latestVersion, comparison, err := compareReleaseVersions(environment.currentVersion, release.TagName)
	if err != nil || comparison >= 0 {
		t.Fatalf("compare staged release: version=%s comparison=%d error=%v", latestVersion, comparison, err)
	}
	staged, err := stageUpdateResources(
		context.Background(), environment, newUpdateReporter(io.Discard, false, false), release, latestVersion, metadata,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(staged.root); err != nil {
			t.Errorf("remove staged update fixture: %v", err)
		}
	})
	for _, artifact := range []struct {
		name     string
		filename string
		contents []byte
	}{
		{name: "metadata", filename: updateMetadataStageName, contents: metadata},
		{name: "checksums", filename: updateChecksumsStageName, contents: checksums},
		{name: "signature", filename: updateSignatureStageName, contents: staged.signature},
		{name: "archive", filename: staged.archiveName, contents: archive},
	} {
		t.Run(artifact.name, func(t *testing.T) {
			assertChangedStagedArtifactRejected(t, staged, environment, artifact.filename, artifact.contents)
		})
	}
}

func assertChangedStagedArtifactRejected(
	t *testing.T,
	staged stagedUpdate,
	environment updateEnvironment,
	filename string,
	original []byte,
) {
	t.Helper()
	path := filepath.Join(staged.root, filename)
	changed := append([]byte(nil), original...)
	changed[len(changed)/2] ^= 0xff
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Errorf("restore staged %q fixture: %v", filename, err)
		}
	}()
	if _, err := revalidateStagedUpdate(staged, environment); updateErrorCodeForTest(err) != "update_package_invalid" {
		t.Fatalf("changed staged %q error = %v", filename, err)
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
	environment.installPackage = func(context.Context, string, string, string, *os.File) error {
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

func TestUpdateReportsVerificationFailureAfterInstallation(t *testing.T) {
	for _, jsonOutput := range []bool{true, false} {
		mode := "human"
		if jsonOutput {
			mode = "json"
		}
		t.Run(mode, func(t *testing.T) {
			environment := newIsolatedUpdateEnvironment(t)
			environment.verifyInstallation = func(context.Context, string, string) error {
				return errors.New("injected installed-binary verification failure")
			}
			assertPostInstallUpdateFailure(t, environment, jsonOutput, updatePhaseVerification, "unknown", false)
		})
	}
}

func TestUpdateReportsInstallerErrorAfterCommitAsUnknown(t *testing.T) {
	environment := newIsolatedUpdateEnvironment(t)
	installPackage := environment.installPackage
	environment.installPackage = func(
		ctx context.Context,
		installerPath string,
		binaryPath string,
		homeDirectory string,
		installationLock *os.File,
	) error {
		if err := installPackage(ctx, installerPath, binaryPath, homeDirectory, installationLock); err != nil {
			return err
		}
		return errors.New("injected post-commit installer cleanup failure")
	}
	assertPostInstallUpdateFailure(t, environment, true, updatePhaseInstaller, "unknown", false)
}

func TestUpdateReportsPostInstallCleanupAndLockFailures(t *testing.T) {
	tests := []struct {
		name  string
		phase string
		setup func(*testing.T, *updateEnvironment)
	}{
		{
			name: "package cleanup", phase: updatePhasePackageCleanup,
			setup: func(t *testing.T, environment *updateEnvironment) {
				cleanupRoot := ""
				t.Cleanup(func() {
					if cleanupRoot != "" {
						if err := os.RemoveAll(cleanupRoot); err != nil {
							t.Errorf("remove injected package root: %v", err)
						}
					}
				})
				environment.removePackageRoot = func(path string) error {
					cleanupRoot = path
					return errors.New("injected package cleanup failure")
				}
			},
		},
		{
			name: "lock validation", phase: updatePhaseLockValidation,
			setup: func(_ *testing.T, environment *updateEnvironment) {
				environment.validateLock = func(*os.File) error {
					return errors.New("injected update lock validation failure")
				}
			},
		},
		{
			name: "lock close", phase: updatePhaseLockClose,
			setup: func(_ *testing.T, environment *updateEnvironment) {
				environment.closeLock = func(lock *os.File) error {
					if err := lock.Close(); err != nil {
						return err
					}
					return errors.New("injected update lock close failure")
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := newIsolatedUpdateEnvironment(t)
			test.setup(t, &environment)
			assertPostInstallUpdateFailure(t, environment, true, test.phase, "complete", true)
		})
	}
}

func TestUpdatePreInstallFailureOmitsPostInstallEvidence(t *testing.T) {
	archive := buildTestUpdateArchive(t, "1.0.5")
	server := newUpdateTestServer(t, "1.0.5", archive,
		[]byte(strings.Repeat("0", 64)+"  mailcli_1.0.5_darwin_arm64.tar.gz\n"))
	defer server.Close()
	environment := updateTestEnvironment(t, server, "1.0.4")
	createInstalledUpdateFixture(t, environment, "1.0.4", "old skill")
	var stdout, stderr bytes.Buffer
	code := runUpdateWithEnvironment(context.Background(), true, environment, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("update exit code = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode update error: %v, output=%q", err, stdout.String())
	}
	if response.Data.UpdateResult != nil || response.Error == nil ||
		response.Error.Guidance.EffectCertainty != "none" || response.Error.Guidance.Recovery.Command != "" {
		t.Fatalf("pre-install error claimed post-install state: %+v", response)
	}
	if err := verifyBinaryVersion(context.Background(), environment.executablePath, "1.0.4"); err != nil {
		t.Fatalf("previous installed binary changed: %v", err)
	}
}

func newIsolatedUpdateEnvironment(t *testing.T) updateEnvironment {
	t.Helper()
	archive := buildTestUpdateArchive(t, "1.0.5")
	server := newUpdateTestServer(t, "1.0.5", archive,
		checksumFile("mailcli_1.0.5_darwin_arm64.tar.gz", archive))
	t.Cleanup(server.Close)
	environment := updateTestEnvironment(t, server, "1.0.4")
	createInstalledUpdateFixture(t, environment, "1.0.4", "old skill")
	return environment
}

func assertPostInstallUpdateFailure(
	t *testing.T,
	environment updateEnvironment,
	jsonOutput bool,
	wantPhase string,
	wantCertainty string,
	wantUpdated bool,
) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runUpdateWithEnvironment(context.Background(), jsonOutput, environment, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("update exit code = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if jsonOutput {
		assertJSONPostInstallUpdateFailure(t, environment, stdout.Bytes(), wantPhase, wantCertainty, wantUpdated)
	} else if !strings.Contains(stderr.String(), "failed_phase="+wantPhase) ||
		!strings.Contains(stderr.String(), "binary_path=\""+environment.executablePath+"\"") ||
		!strings.Contains(stderr.String(), "target_version=1.0.5") ||
		!strings.Contains(stderr.String(), "effect_certainty="+wantCertainty) ||
		!strings.Contains(stderr.String(), "mailcli version --json") {
		t.Fatalf("human post-install evidence = %q", stderr.String())
	}
	assertInstalledUpdatePayload(t, environment)
}

func assertJSONPostInstallUpdateFailure(
	t *testing.T,
	environment updateEnvironment,
	output []byte,
	wantPhase string,
	wantCertainty string,
	wantUpdated bool,
) {
	t.Helper()
	var response envelope
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("decode update error: %v, output=%q", err, output)
	}
	result := response.Data.UpdateResult
	if response.OK || response.Error == nil || result == nil || result.Updated != wantUpdated ||
		result.FailedPhase != wantPhase || result.BinaryPath != environment.executablePath || result.LatestVersion != "1.0.5" {
		t.Fatalf("post-install error result = %+v", response)
	}
	guidance := response.Error.Guidance
	if string(guidance.EffectCertainty) != wantCertainty || guidance.Retryability != "observe_required" ||
		guidance.ReplayAllowed || guidance.Recovery.Action != "observe" || guidance.Recovery.Command != "version" ||
		len(guidance.Recovery.Args) != 1 || guidance.Recovery.Args[0] != "--json" {
		t.Fatalf("post-install recovery guidance = %+v", guidance)
	}
}

func assertInstalledUpdatePayload(t *testing.T, environment updateEnvironment) {
	t.Helper()
	if err := verifyBinaryVersion(context.Background(), environment.executablePath, "1.0.5"); err != nil {
		t.Fatalf("read-only installed version recovery failed: %v", err)
	}
	binary, err := os.ReadFile(environment.executablePath)
	if err != nil || string(binary) != testUpdateBinary("1.0.5") {
		t.Fatalf("installed binary bytes do not match target: error=%v", err)
	}
	for path, want := range map[string]string{
		filepath.Join(environment.homeDirectory, ".agents", "skills", "mailcli", "SKILL.md"):              "new skill\n",
		filepath.Join(environment.homeDirectory, ".agents", "skills", "mailcli", "agents", "openai.yaml"): "new agent\n",
	} {
		contents, readErr := os.ReadFile(path)
		if readErr != nil || string(contents) != want {
			t.Errorf("installed update payload at %s = %q, %v; want %q", path, contents, readErr, want)
		}
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

func TestUpdateURLPolicyAcceptsTrustedHosts(t *testing.T) {
	for _, value := range []string{
		"https://api.github.com/repos/Christopher-Schulze/MailCLI/releases/latest",
		"https://github.com/Christopher-Schulze/MailCLI/releases/download/v1.3.0/archive",
		"https://objects.githubusercontent.com/release/archive",
		"https://release-assets.githubusercontent.com/release/archive?signature=redacted",
		"https://github.com:443/release",
	} {
		if err := validateUpdateURL(value, false); err != nil {
			t.Errorf("validateUpdateURL(%q) error = %v", value, err)
		}
	}
}

func TestUpdateURLPolicyRejectsUnsafeURLs(t *testing.T) {
	tests := []struct {
		name  string
		value string
		code  string
	}{
		{name: "untrusted host", value: "https://updates.example/release", code: "update_host_untrusted"},
		{name: "alternate port", value: "https://github.com:444/release", code: "update_url_invalid_port"},
		{name: "downgrade", value: "http://github.com/release", code: "update_url_insecure"},
		{name: "malformed", value: "https://github.com:bad/release", code: "update_url_invalid"},
		{name: "credentials", value: "https://user:secret@github.com/release", code: "update_url_invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateUpdateURL(test.value, false)
			if updateErrorCodeForTest(err) != test.code {
				t.Fatalf("validateUpdateURL() error = %v, want %s", err, test.code)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), test.value) {
				t.Fatalf("validateUpdateURL() leaked URL data: %v", err)
			}
		})
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

func TestUpdateRedirectRejectsUntrustedHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "https://updates.example/release", http.StatusFound)
	}))
	defer server.Close()
	policy := testUpdateURLPolicy(t, server.URL)
	client := server.Client()
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		return secureUpdateRedirectWithPolicy(request, via, policy)
	}
	_, err := downloadUpdateResource(
		context.Background(), client, server.URL, maximumChecksumFile,
	)
	if updateErrorCodeForTest(err) != "update_host_untrusted" {
		t.Fatalf("downloadUpdateResource() error = %v", err)
	}
	if strings.Contains(err.Error(), "updates.example") {
		t.Fatalf("redirect error leaked untrusted URL: %v", err)
	}
}

func TestUpdateRedirectRejectsDowngrade(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "http://github.com/release", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := secureUpdateRedirect(request, nil); updateErrorCodeForTest(err) != "update_url_insecure" {
		t.Fatalf("secureUpdateRedirect(downgrade) error = %v", err)
	}
}

func TestUpdateRedirectLoopIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		target := "/a"
		if request.URL.Path == "/a" {
			target = "/b"
		}
		http.Redirect(writer, request, target, http.StatusFound)
	}))
	defer server.Close()
	policy := testUpdateURLPolicy(t, server.URL)
	client := server.Client()
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		return secureUpdateRedirectWithPolicy(request, via, policy)
	}
	_, err := downloadUpdateResource(
		context.Background(), client, server.URL+"/a", maximumChecksumFile,
	)
	if updateErrorCodeForTest(err) != "update_redirect_limit" {
		t.Fatalf("downloadUpdateResource() error = %v", err)
	}
}

func TestUpdateInstallerEnvironmentDropsShellInjection(t *testing.T) {
	environment := updateInstallerEnvironment([]string{
		"PATH=/tmp/injected", "BASH_ENV=/tmp/inject", "ENV=/tmp/inject",
		"MAILCLI_SKILL_DESTINATION=/tmp/skill", "BASH_FUNC_mv%%=() { false; }",
		"MAILCLI_INSTALL_PACKAGE_ROOT=/tmp/alternate", "MAILCLI_INSTALL_PACKAGE_ROOT=",
		"DYLD_INSERT_LIBRARIES=/tmp/inject.dylib", "LD_PRELOAD=/tmp/inject.so",
	}, "/Users/test", "/Users/test/.local/bin/mailcli")
	joined := strings.Join(environment, "\n")
	for _, forbidden := range []string{
		"PATH=/tmp/injected", "BASH_ENV=", "ENV=", "MAILCLI_SKILL_DESTINATION=", "BASH_FUNC_",
		"MAILCLI_INSTALL_PACKAGE_ROOT=", "DYLD_", "LD_",
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
	if err := release.Close(); err != nil {
		t.Fatalf("release update lock error = %v", err)
	}
}

type updateTestServer struct {
	*httptest.Server
	publicKey ed25519.PublicKey
}

type updateAttemptResult struct {
	result updateResult
	err    error
}

type gatedUpdateFixture struct {
	environment    updateEnvironment
	archiveStarted chan time.Time
	archiveGate    chan struct{}
	archiveOnce    sync.Once
}

type updateLockIntervals struct {
	mutex  sync.Mutex
	starts []time.Time
	ends   []time.Time
}

func newGatedUpdateFixture(t *testing.T, currentVersion string, attemptCount int) *gatedUpdateFixture {
	t.Helper()
	archive := buildTestUpdateArchive(t, "1.0.5")
	checksums := checksumFile("mailcli_1.0.5_darwin_arm64.tar.gz", archive)
	fixture := &gatedUpdateFixture{
		archiveStarted: make(chan time.Time, attemptCount),
		archiveGate:    make(chan struct{}),
	}
	server := newGatedUpdateTestServer(
		t, "1.0.5", archive, checksums, fixture.archiveStarted, fixture.archiveGate,
	)
	fixture.environment = updateTestEnvironment(t, server, currentVersion)
	createInstalledUpdateFixture(t, fixture.environment, currentVersion, "old skill")
	t.Cleanup(func() {
		fixture.releaseArchive()
		server.Close()
	})
	return fixture
}

func (fixture *gatedUpdateFixture) releaseArchive() {
	fixture.archiveOnce.Do(func() { close(fixture.archiveGate) })
}

func (fixture *gatedUpdateFixture) releaseArchiveAfter(
	networkStart time.Time,
	delay time.Duration,
) time.Time {
	if wait := time.Until(networkStart.Add(delay)); wait > 0 {
		time.Sleep(wait)
	}
	fixture.releaseArchive()
	return time.Now()
}

func countUpdateInstalls(environment *updateEnvironment) *atomic.Int32 {
	var installs atomic.Int32
	install := environment.installPackage
	environment.installPackage = func(
		ctx context.Context,
		installerPath string,
		binaryPath string,
		homeDirectory string,
		installationLock *os.File,
	) error {
		installs.Add(1)
		return install(ctx, installerPath, binaryPath, homeDirectory, installationLock)
	}
	return &installs
}

func recordUpdateLockIntervals(environment *updateEnvironment) *updateLockIntervals {
	intervals := &updateLockIntervals{}
	readVersion := environment.readInstalledVersion
	environment.readInstalledVersion = func(ctx context.Context, path string) (string, error) {
		intervals.recordStart(time.Now())
		return readVersion(ctx, path)
	}
	closeLock := environment.closeLock
	environment.closeLock = func(lock *os.File) error {
		var err error
		if closeLock == nil {
			err = lock.Close()
		} else {
			err = closeLock(lock)
		}
		intervals.recordEnd(time.Now())
		return err
	}
	return intervals
}

func (intervals *updateLockIntervals) recordStart(at time.Time) {
	intervals.mutex.Lock()
	defer intervals.mutex.Unlock()
	intervals.starts = append(intervals.starts, at)
}

func (intervals *updateLockIntervals) recordEnd(at time.Time) {
	intervals.mutex.Lock()
	defer intervals.mutex.Unlock()
	intervals.ends = append(intervals.ends, at)
}

func (intervals *updateLockIntervals) assertExcludesNetworkDelay(
	t *testing.T,
	operationStart time.Time,
	networkStart time.Time,
	networkEnd time.Time,
) {
	t.Helper()
	intervals.mutex.Lock()
	starts := append([]time.Time(nil), intervals.starts...)
	ends := append([]time.Time(nil), intervals.ends...)
	intervals.mutex.Unlock()
	if len(starts) != 2 || len(ends) != 2 {
		t.Fatalf("recorded lock intervals = %d starts, %d ends; want 2 each", len(starts), len(ends))
	}
	var lockDuration time.Duration
	for index := range starts {
		if starts[index].Before(networkEnd) || ends[index].Before(starts[index]) {
			t.Fatalf("lock interval %d overlaps network staging: %s to %s", index, starts[index], ends[index])
		}
		lockDuration += ends[index].Sub(starts[index])
	}
	networkDelay := networkEnd.Sub(networkStart)
	if elapsedOutsideLock := time.Since(operationStart) - lockDuration; elapsedOutsideLock < networkDelay {
		t.Fatalf("network delay %s was included in lock-held time %s", networkDelay, lockDuration)
	}
}

func assertInstallerLockAvailable(t *testing.T, homeDirectory string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lock, err := acquireUpdateLock(ctx, homeDirectory)
	if err != nil {
		t.Fatalf("installation lock unavailable during staged download: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("close installation lock probe: %v", err)
	}
}

func startUpdateAttempts(
	ctx context.Context,
	environment updateEnvironment,
	attemptCount int,
) <-chan updateAttemptResult {
	results := make(chan updateAttemptResult, attemptCount)
	for range attemptCount {
		go func() {
			result, err := performUpdate(ctx, environment, newUpdateReporter(io.Discard, false, false))
			results <- updateAttemptResult{result: result, err: err}
		}()
	}
	return results
}

func collectUpdateAttempts(
	t *testing.T,
	results <-chan updateAttemptResult,
	attemptCount int,
) []updateAttemptResult {
	t.Helper()
	attempts := make([]updateAttemptResult, 0, attemptCount)
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for range attemptCount {
		select {
		case attempt := <-results:
			attempts = append(attempts, attempt)
		case <-timeout.C:
			t.Fatalf("timed out waiting for %d update attempts", attemptCount)
		}
	}
	return attempts
}

func assertConcurrentUpdateResults(
	t *testing.T,
	attempts []updateAttemptResult,
	installCount *atomic.Int32,
	environment updateEnvironment,
) {
	t.Helper()
	if installCount.Load() != 1 {
		t.Fatalf("concurrent update invoked installer %d times; want 1", installCount.Load())
	}
	updated := 0
	for _, attempt := range attempts {
		if attempt.err != nil || attempt.result.LatestVersion != "1.0.5" {
			t.Fatalf("concurrent update result = %+v, %v", attempt.result, attempt.err)
		}
		if attempt.result.Updated {
			updated++
		}
	}
	if updated != 1 {
		t.Fatalf("concurrent updates reported %d installations; want 1", updated)
	}
	if err := verifyBinaryVersion(context.Background(), environment.executablePath, "1.0.5"); err != nil {
		t.Fatalf("final binary version = %v", err)
	}
}

func installVersionDuringDownload(
	t *testing.T,
	environment updateEnvironment,
	version string,
	skill string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lock, err := acquireUpdateLock(ctx, environment.homeDirectory)
	if err != nil {
		t.Fatalf("acquire competing update lock: %v", err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			t.Errorf("close competing update lock: %v", err)
		}
	}()
	createInstalledUpdateFixture(t, environment, version, skill)
}

func waitForArchiveStart(t *testing.T, archiveStarted <-chan time.Time) time.Time {
	t.Helper()
	select {
	case startedAt := <-archiveStarted:
		return startedAt
	case <-time.After(10 * time.Second):
		t.Fatal("update did not reach its delayed archive request")
		return time.Time{}
	}
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
	return newUpdateTestServerFixture(t, releaseVersion, archive, checksums, corruptSignature, nil)
}

func newGatedUpdateTestServer(
	t *testing.T,
	releaseVersion string,
	archive []byte,
	checksums []byte,
	archiveStarted chan<- time.Time,
	archiveGate <-chan struct{},
) *updateTestServer {
	return newUpdateTestServerFixture(t, releaseVersion, archive, checksums, false, func() {
		archiveStarted <- time.Now()
		<-archiveGate
	})
}

func newUpdateTestServerFixture(
	t *testing.T,
	releaseVersion string,
	archive []byte,
	checksums []byte,
	corruptSignature bool,
	beforeArchiveWrite func(),
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
	server = httptest.NewServer(updateTestHandler(
		func() string { return server.URL }, releaseVersion, archiveName, archive, checksums, encodedSignature,
		beforeArchiveWrite,
	))
	return &updateTestServer{Server: server, publicKey: publicKey}
}

func updateTestHandler(
	serverURL func() string,
	releaseVersion string,
	archiveName string,
	archive []byte,
	checksums []byte,
	encodedSignature []byte,
	beforeArchiveWrite func(),
) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/latest":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(
				writer,
				`{"tag_name":"v%s","html_url":"%s/release","assets":[`+
					`{"name":"%s","browser_download_url":"%s/archive"},`+
					`{"name":"SHA256SUMS","browser_download_url":"%s/checksums"},`+
					`{"name":"SHA256SUMS.sig","browser_download_url":"%s/signature"}]}`,
				releaseVersion, serverURL(), archiveName, serverURL(), serverURL(), serverURL(),
			)
		case "/archive":
			if beforeArchiveWrite != nil {
				beforeArchiveWrite()
			}
			_, _ = writer.Write(archive)
		case "/checksums":
			_, _ = writer.Write(checksums)
		case "/signature":
			_, _ = writer.Write(encodedSignature)
		default:
			http.NotFound(writer, request)
		}
	})
}

func updateTestEnvironment(t *testing.T, server *updateTestServer, currentVersion string) updateEnvironment {
	t.Helper()
	testRoot := t.TempDir()
	policy := testUpdateURLPolicy(t, server.URL)
	client := server.Client()
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		return secureUpdateRedirectWithPolicy(request, via, policy)
	}
	environment := updateEnvironment{
		client: client, metadataURL: server.URL + "/latest", currentVersion: currentVersion,
		readInstalledVersion: readInstalledBinaryVersion,
		executablePath:       filepath.Join(testRoot, "bin", "mailcli"), homeDirectory: filepath.Join(testRoot, "home"),
		operatingSystem: "darwin", architecture: "arm64",
		urlPolicy:     policy,
		verifyPackage: verifyBinaryVersion, installPackage: runReleaseInstaller,
		verifyInstallation: verifyBinaryVersion,
		releasePublicKey:   server.publicKey,
	}
	if err := os.MkdirAll(environment.homeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	return environment
}

func testUpdateURLPolicy(t *testing.T, serverURL string) updateURLPolicy {
	t.Helper()
	parsed, err := url.Parse(serverURL)
	if err != nil || parsed.Hostname() == "" {
		t.Fatalf("parse update test server URL: %v", err)
	}
	return updateURLPolicy{
		trustedHosts:        map[string]struct{}{strings.ToLower(parsed.Hostname()): {}},
		allowHTTP:           true,
		allowNonDefaultPort: true,
	}
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
