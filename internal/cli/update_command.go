package cli

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"mailcli/internal/releaseauth"
)

const (
	latestReleaseURL          = "https://api.github.com/repos/Christopher-Schulze/MailCLI/releases/latest"
	maximumReleaseMetadata    = 2 * 1024 * 1024
	maximumChecksumFile       = 1024 * 1024
	maximumSignatureFile      = 4 * 1024
	maximumReleaseArchive     = 64 * 1024 * 1024
	maximumExtractedPackage   = 192 * 1024 * 1024
	maximumExtractedFileCount = 256
	maximumUpdateRedirects    = 10
	updateTimeout             = 5 * time.Minute
)

type updateResult struct {
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version"`
	Updated        bool   `json:"updated"`
	ReleaseURL     string `json:"release_url"`
	BinaryPath     string `json:"binary_path"`
}

type updateAsset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
}

type updateURLPolicy struct {
	trustedHosts        map[string]struct{}
	allowHTTP           bool
	allowNonDefaultPort bool
}

func githubUpdateURLPolicy() updateURLPolicy {
	return updateURLPolicy{trustedHosts: map[string]struct{}{
		"api.github.com":                       {},
		"github.com":                           {},
		"objects.githubusercontent.com":        {},
		"release-assets.githubusercontent.com": {},
	}}
}

func (policy updateURLPolicy) validate(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil || parsed.Scheme == "" || parsed.Host == "" ||
		parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" ||
		strings.ContainsAny(value, "\x00\r\n") {
		return updateFailure("update_url_invalid", "release URL is malformed or contains disallowed components")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && (scheme != "http" || !policy.allowHTTP) {
		return updateFailure("update_url_insecure", "release URL must use HTTPS")
	}
	if !policy.allowNonDefaultPort {
		expectedPort := "443"
		if scheme == "http" {
			expectedPort = "80"
		}
		if port := parsed.Port(); port != "" && port != expectedPort {
			return updateFailure("update_url_invalid_port", "release URL uses a non-default port")
		}
	}
	if _, ok := policy.trustedHosts[strings.ToLower(parsed.Hostname())]; !ok {
		return updateFailure("update_host_untrusted", "release URL host is outside the trusted update host policy")
	}
	return nil
}

type updateRelease struct {
	TagName string        `json:"tag_name"`
	HTMLURL string        `json:"html_url"`
	Assets  []updateAsset `json:"assets"`
}

type updateEnvironment struct {
	client             *http.Client
	metadataURL        string
	currentVersion     string
	executablePath     string
	homeDirectory      string
	operatingSystem    string
	architecture       string
	verifyPackage      func(context.Context, string, string) error
	installPackage     func(context.Context, string, string, string, *os.File) error
	verifyInstallation func(context.Context, string, string) error
	urlPolicy          updateURLPolicy
	releasePublicKey   ed25519.PublicKey
}

type updateError struct {
	code    string
	message string
}

func (e *updateError) Error() string {
	return e.message
}

func (e *updateError) ErrorCode() string {
	return e.code
}

func runUpdate(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("update", stderr)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	environment, err := defaultUpdateEnvironment()
	if err != nil {
		return failCommand("update", *jsonOutput, err, stdout, stderr)
	}
	operationCtx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	reporter := newUpdateReporter(stdout, !*jsonOutput, !*jsonOutput && writerIsTerminal(stdout))
	result, err := performUpdate(operationCtx, environment, reporter)
	if err != nil {
		return failCommand("update", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "update", responseData{UpdateResult: &result})
	}
	if result.Updated {
		writeFormat(stdout, "Updated mailcli from %s to %s.\n", result.CurrentVersion, result.LatestVersion)
		return 0
	}
	writeFormat(stdout, "Already up to date (mailcli %s).\n", result.CurrentVersion)
	return 0
}

func defaultUpdateEnvironment() (updateEnvironment, error) {
	publicKey, err := parseReleasePublicKey(releaseauth.PublicKeyBase64)
	if err != nil {
		return updateEnvironment{}, updateFailure("update_signature_invalid", "decode pinned release key: %v", err)
	}
	executablePath, err := os.Executable()
	if err != nil {
		return updateEnvironment{}, updateFailure("update_install_failed", "resolve installed MailCLI binary: %v", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executablePath); resolveErr == nil {
		executablePath = resolved
	}
	homeDirectory, err := os.UserHomeDir()
	if err != nil {
		return updateEnvironment{}, updateFailure("update_install_failed", "resolve user home directory: %v", err)
	}
	return updateEnvironment{
		client: &http.Client{
			Timeout:       2 * time.Minute,
			CheckRedirect: secureUpdateRedirect,
		},
		metadataURL: latestReleaseURL, currentVersion: version,
		executablePath: executablePath, homeDirectory: homeDirectory,
		operatingSystem: runtime.GOOS, architecture: runtime.GOARCH,
		verifyPackage: verifyReleaseBinary, installPackage: runReleaseInstaller,
		verifyInstallation: verifyInstalledBinary,
		urlPolicy:          githubUpdateURLPolicy(),
		releasePublicKey:   publicKey,
	}, nil
}

func secureUpdateRedirect(request *http.Request, via []*http.Request) error {
	return secureUpdateRedirectWithPolicy(request, via, githubUpdateURLPolicy())
}

func secureUpdateRedirectWithPolicy(
	request *http.Request,
	via []*http.Request,
	policy updateURLPolicy,
) error {
	if len(via) >= maximumUpdateRedirects {
		return updateFailure("update_redirect_limit", "release download exceeded the maximum redirect count")
	}
	if request == nil || request.URL == nil {
		return updateFailure("update_redirect_invalid", "release download redirect is malformed")
	}
	return policy.validate(request.URL.String())
}

func performUpdate(
	ctx context.Context,
	environment updateEnvironment,
	reporter *updateReporter,
) (result updateResult, resultErr error) {
	if environment.operatingSystem != "darwin" || environment.architecture != "arm64" {
		return updateResult{}, updateFailure(
			"update_unsupported_platform", "self-update requires darwin/arm64",
		)
	}
	installationLock, err := acquireUpdateLock(ctx, environment.homeDirectory)
	if err != nil {
		return updateResult{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, validateUpdateLock(installationLock), installationLock.Close())
	}()
	var release updateRelease
	if err := reporter.step("Checking for updates", func() error {
		var fetchErr error
		release, fetchErr = fetchLatestRelease(ctx, environment)
		return fetchErr
	}); err != nil {
		return updateResult{}, err
	}
	latestVersion, comparison, err := compareReleaseVersions(environment.currentVersion, release.TagName)
	if err != nil {
		return updateResult{}, err
	}
	result = updateResult{
		CurrentVersion: environment.currentVersion, LatestVersion: latestVersion,
		ReleaseURL: release.HTMLURL, BinaryPath: environment.executablePath,
	}
	if comparison >= 0 {
		return result, nil
	}
	if err := downloadAndInstallUpdate(ctx, environment, reporter, release, latestVersion, installationLock); err != nil {
		return updateResult{}, err
	}
	result.Updated = true
	return result, nil
}

func downloadAndInstallUpdate(
	ctx context.Context,
	environment updateEnvironment,
	reporter *updateReporter,
	release updateRelease,
	latestVersion string,
	installationLock *os.File,
) error {
	archiveName := fmt.Sprintf("mailcli_%s_darwin_arm64.tar.gz", latestVersion)
	archiveURL, checksumURL, signatureURL, err := releaseAssetURLs(release.Assets, archiveName)
	if err != nil {
		return err
	}
	if err := environment.urlPolicy.validate(archiveURL); err != nil {
		return contextualUpdateFailure("update_package_invalid", "invalid release archive URL", err)
	}
	if err := environment.urlPolicy.validate(checksumURL); err != nil {
		return contextualUpdateFailure("update_package_invalid", "invalid release checksum URL", err)
	}
	if err := environment.urlPolicy.validate(signatureURL); err != nil {
		return contextualUpdateFailure("update_package_invalid", "invalid release signature URL", err)
	}
	checksums, err := downloadUpdateResource(ctx, environment.client, checksumURL, maximumChecksumFile)
	if err != nil {
		return contextualUpdateFailure("update_download_failed", "download release checksums", err)
	}
	signature, err := downloadUpdateResource(ctx, environment.client, signatureURL, maximumSignatureFile)
	if err != nil {
		return contextualUpdateFailure("update_download_failed", "download release signature", err)
	}
	if err := reporter.step("Verifying release signature", func() error {
		return verifyReleaseSignature(checksums, signature, environment.releasePublicKey)
	}); err != nil {
		return err
	}
	var archive []byte
	if err := reporter.step("Downloading mailcli "+latestVersion, func() error {
		var downloadErr error
		archive, downloadErr = downloadUpdateResource(
			ctx, environment.client, archiveURL, maximumReleaseArchive,
		)
		return downloadErr
	}); err != nil {
		return contextualUpdateFailure("update_download_failed", "download release archive", err)
	}
	if err := reporter.step("Verifying release checksum", func() error {
		return verifyReleaseChecksum(archiveName, archive, checksums)
	}); err != nil {
		return err
	}
	return installVerifiedArchive(ctx, environment, reporter, archive, latestVersion, installationLock)
}

func validateUpdateURL(value string, allowInsecure bool) error {
	policy := githubUpdateURLPolicy()
	policy.allowHTTP = allowInsecure
	return policy.validate(value)
}

func contextualUpdateFailure(fallbackCode string, context string, err error) error {
	var typed *updateError
	if errors.As(err, &typed) {
		return updateFailure(typed.code, "%s: %s", context, typed.message)
	}
	return updateFailure(fallbackCode, "%s: %v", context, err)
}

func sanitizeUpdateRequestError(err error) error {
	var typed *updateError
	if errors.As(err, &typed) {
		return typed
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errors.New("release request failed")
}

func updateFailure(code string, format string, values ...any) error {
	return &updateError{code: code, message: fmt.Sprintf(format, values...)}
}
