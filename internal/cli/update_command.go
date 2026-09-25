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
	updatePhaseInstaller      = "installer"
	updatePhaseVerification   = "installed_binary_verification"
	updatePhasePackageCleanup = "package_cleanup"
	updatePhaseLockValidation = "update_lock_validation"
	updatePhaseLockClose      = "update_lock_close"
	updateMetadataStageName   = "release.json"
	updateChecksumsStageName  = "SHA256SUMS"
	updateSignatureStageName  = "SHA256SUMS.sig"
)

type updateResult struct {
	CurrentVersion   string `json:"current_version"`
	LatestVersion    string `json:"latest_version"`
	Updated          bool   `json:"updated"`
	ReleaseURL       string `json:"release_url"`
	BinaryPath       string `json:"binary_path"`
	FailedPhase      string `json:"failed_phase,omitempty"`
	failureCertainty string `json:"-"`
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
	client               *http.Client
	metadataURL          string
	currentVersion       string
	readInstalledVersion func(context.Context, string) (string, error)
	executablePath       string
	homeDirectory        string
	operatingSystem      string
	architecture         string
	verifyPackage        func(context.Context, string, string) error
	installPackage       func(context.Context, string, string, string, *os.File) error
	verifyInstallation   func(context.Context, string, string) error
	removePackageRoot    func(string) error
	validateLock         func(*os.File) error
	closeLock            func(*os.File) error
	urlPolicy            updateURLPolicy
	releasePublicKey     ed25519.PublicKey
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
	return runUpdateWithEnvironment(ctx, *jsonOutput, environment, stdout, stderr)
}

func runUpdateWithEnvironment(
	ctx context.Context,
	jsonOutput bool,
	environment updateEnvironment,
	stdout io.Writer,
	stderr io.Writer,
) int {
	operationCtx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	reporter := newUpdateReporter(stdout, !jsonOutput, !jsonOutput && writerIsTerminal(stdout))
	result, err := performUpdate(operationCtx, environment, reporter)
	if err != nil {
		if result.FailedPhase != "" {
			if jsonOutput {
				return failCommandWithData("update", true, responseData{UpdateResult: &result}, err, stdout, stderr)
			}
			return failUpdateWithResult(result, err, stderr)
		}
		return failCommand("update", jsonOutput, err, stdout, stderr)
	}
	if jsonOutput {
		return writeSuccess(stdout, "update", responseData{UpdateResult: &result})
	}
	if result.Updated {
		writeFormat(stdout, "Updated mailcli from %s to %s.\n", result.CurrentVersion, result.LatestVersion)
		return 0
	}
	writeFormat(stdout, "Already up to date (mailcli %s).\n", result.CurrentVersion)
	return 0
}

func failUpdateWithResult(result updateResult, err error, stderr io.Writer) int {
	writeLine(stderr, err)
	certainty := result.failureCertainty
	if certainty == "" {
		certainty = "unknown"
	}
	writeFormat(stderr,
		"update result: failed_phase=%s; binary_path=%q; target_version=%s; effect_certainty=%s\n",
		result.FailedPhase, result.BinaryPath, result.LatestVersion, certainty,
	)
	if certainty == "complete" {
		writeLine(stderr, "installed version was verified; recovery: run `mailcli version --json` to inspect the installed identity before any further update decision.")
	} else {
		writeLine(stderr, "installation outcome may be partial; recovery: run `mailcli version --json` to inspect the installed identity before considering another update. No rollback is claimed.")
	}
	return commandExitCode(err)
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
		readInstalledVersion: readInstalledBinaryVersion,
		executablePath:       executablePath, homeDirectory: homeDirectory,
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
	result, staged, err := prepareUpdate(ctx, environment, reporter)
	if err != nil {
		return updateResult{}, err
	}
	if staged.root == "" {
		return result, nil
	}
	defer cleanupStagedUpdate(staged, &result, &resultErr)
	return installStagedUpdate(ctx, environment, reporter, result, staged)
}

func prepareUpdate(
	ctx context.Context,
	environment updateEnvironment,
	reporter *updateReporter,
) (updateResult, stagedUpdate, error) {
	var release updateRelease
	var metadata []byte
	if err := reporter.step("Checking for updates", func() error {
		var fetchErr error
		release, metadata, fetchErr = fetchLatestRelease(ctx, environment)
		return fetchErr
	}); err != nil {
		return updateResult{}, stagedUpdate{}, err
	}
	latestVersion, comparison, err := compareReleaseVersions(environment.currentVersion, release.TagName)
	if err != nil {
		return updateResult{}, stagedUpdate{}, err
	}
	result := updateResult{
		CurrentVersion: environment.currentVersion, LatestVersion: latestVersion,
		ReleaseURL: release.HTMLURL, BinaryPath: environment.executablePath,
	}
	if comparison >= 0 {
		return result, stagedUpdate{}, nil
	}
	staged, err := stageUpdateResources(
		ctx, environment, reporter, release, latestVersion, metadata,
	)
	if err != nil {
		return updateResult{}, stagedUpdate{}, err
	}
	staged.latestVersion = latestVersion
	staged.release = release
	return result, staged, nil
}

func installStagedUpdate(
	ctx context.Context,
	environment updateEnvironment,
	reporter *updateReporter,
	result updateResult,
	staged stagedUpdate,
) (resultOut updateResult, resultErr error) {
	installationLock, err := acquireUpdateLock(ctx, environment.homeDirectory)
	if err != nil {
		return updateResult{}, err
	}
	defer finishUpdateLock(environment, installationLock, &resultOut, &resultErr)
	return installStagedUpdateWithLock(ctx, environment, reporter, result, staged, installationLock)
}

func installStagedUpdateWithLock(
	ctx context.Context,
	environment updateEnvironment,
	reporter *updateReporter,
	result updateResult,
	staged stagedUpdate,
	installationLock *os.File,
) (updateResult, error) {
	installedVersion, err := environment.readInstalledVersion(ctx, environment.executablePath)
	if err != nil {
		return updateResult{}, updateFailure("update_install_failed", "read installed version: %v", err)
	}
	result.CurrentVersion = installedVersion
	_, installedComparison, err := compareReleaseVersions(installedVersion, staged.release.TagName)
	if err != nil {
		return updateResult{}, err
	}
	if installedComparison >= 0 {
		return result, nil
	}
	archive, err := revalidateStagedUpdate(staged, environment)
	if err != nil {
		return result, err
	}
	return installVerifiedStagedArchive(ctx, environment, reporter, result, staged, archive, installationLock)
}

func installVerifiedStagedArchive(
	ctx context.Context,
	environment updateEnvironment,
	reporter *updateReporter,
	result updateResult,
	staged stagedUpdate,
	archive []byte,
	installationLock *os.File,
) (updateResult, error) {
	outcome, err := installVerifiedArchive(
		ctx, environment, reporter, archive, staged.latestVersion, installationLock,
	)
	result.FailedPhase = outcome.failedPhase
	if outcome.verified {
		result.Updated = true
		result.failureCertainty = "complete"
	} else if outcome.attempted && outcome.failedPhase != "" {
		result.failureCertainty = "unknown"
	}
	if err != nil {
		if result.FailedPhase != "" {
			return result, err
		}
		return updateResult{}, err
	}
	return result, nil
}

func finishUpdateLock(
	environment updateEnvironment,
	installationLock *os.File,
	result *updateResult,
	resultErr *error,
) {
	validationErr := validateUpdateLockForEnvironment(environment, installationLock)
	closeErr := closeUpdateLockForEnvironment(environment, installationLock)
	if result.Updated && result.FailedPhase == "" {
		switch {
		case validationErr != nil:
			result.FailedPhase = updatePhaseLockValidation
		case closeErr != nil:
			result.FailedPhase = updatePhaseLockClose
		}
		if result.FailedPhase != "" {
			result.failureCertainty = "complete"
		}
	}
	*resultErr = errors.Join(*resultErr, validationErr, closeErr)
}

func cleanupStagedUpdate(staged stagedUpdate, result *updateResult, resultErr *error) {
	if cleanupErr := os.RemoveAll(staged.root); cleanupErr != nil {
		if result.Updated && result.FailedPhase == "" {
			result.FailedPhase = updatePhasePackageCleanup
		}
		*resultErr = errors.Join(
			*resultErr,
			updateFailure("update_install_failed", "remove private staged update: %v", cleanupErr),
		)
	}
}

func stageUpdateResources(
	ctx context.Context,
	environment updateEnvironment,
	reporter *updateReporter,
	release updateRelease,
	latestVersion string,
	metadata []byte,
) (staged stagedUpdate, resultErr error) {
	staged, err := downloadUpdateAssets(ctx, environment, reporter, release, latestVersion, metadata)
	if err != nil {
		return stagedUpdate{}, err
	}
	temporaryRoot, err := os.MkdirTemp("", "mailcli-update-stage-*")
	if err != nil {
		return stagedUpdate{}, updateFailure("update_install_failed", "create private update staging: %v", err)
	}
	staged.root = temporaryRoot
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, os.RemoveAll(temporaryRoot))
		}
	}()
	if err := writeStagedUpdateFiles(staged); err != nil {
		return stagedUpdate{}, err
	}
	return staged, nil
}

func downloadUpdateAssets(
	ctx context.Context,
	environment updateEnvironment,
	reporter *updateReporter,
	release updateRelease,
	latestVersion string,
	metadata []byte,
) (stagedUpdate, error) {
	archiveName := fmt.Sprintf("mailcli_%s_darwin_arm64.tar.gz", latestVersion)
	archiveURL, checksumURL, signatureURL, err := validatedUpdateAssetURLs(environment, release, archiveName)
	if err != nil {
		return stagedUpdate{}, err
	}
	checksums, signature, err := downloadAndVerifyUpdateManifest(ctx, environment, checksumURL, signatureURL, reporter)
	if err != nil {
		return stagedUpdate{}, err
	}
	archive, err := downloadAndVerifyUpdateArchive(ctx, environment, archiveURL, archiveName, latestVersion, checksums, reporter)
	if err != nil {
		return stagedUpdate{}, err
	}
	return stagedUpdate{
		archiveName: archiveName, metadata: metadata, checksums: checksums,
		signature: signature, archive: archive,
	}, nil
}

func validatedUpdateAssetURLs(
	environment updateEnvironment,
	release updateRelease,
	archiveName string,
) (string, string, string, error) {
	archiveURL, checksumURL, signatureURL, err := releaseAssetURLs(release.Assets, archiveName)
	if err != nil {
		return "", "", "", err
	}
	for _, asset := range []struct {
		name string
		url  string
	}{
		{name: "archive", url: archiveURL},
		{name: "checksum", url: checksumURL},
		{name: "signature", url: signatureURL},
	} {
		if err := environment.urlPolicy.validate(asset.url); err != nil {
			return "", "", "", contextualUpdateFailure(
				"update_package_invalid", "invalid release "+asset.name+" URL", err,
			)
		}
	}
	return archiveURL, checksumURL, signatureURL, nil
}

func downloadAndVerifyUpdateManifest(
	ctx context.Context,
	environment updateEnvironment,
	checksumURL string,
	signatureURL string,
	reporter *updateReporter,
) ([]byte, []byte, error) {
	checksums, err := downloadUpdateResource(ctx, environment.client, checksumURL, maximumChecksumFile)
	if err != nil {
		return nil, nil, contextualUpdateFailure("update_download_failed", "download release checksums", err)
	}
	signature, err := downloadUpdateResource(ctx, environment.client, signatureURL, maximumSignatureFile)
	if err != nil {
		return nil, nil, contextualUpdateFailure("update_download_failed", "download release signature", err)
	}
	if err := reporter.step("Verifying release signature", func() error {
		return verifyReleaseSignature(checksums, signature, environment.releasePublicKey)
	}); err != nil {
		return nil, nil, err
	}
	return checksums, signature, nil
}

func downloadAndVerifyUpdateArchive(
	ctx context.Context,
	environment updateEnvironment,
	archiveURL string,
	archiveName string,
	latestVersion string,
	checksums []byte,
	reporter *updateReporter,
) ([]byte, error) {
	var archive []byte
	if err := reporter.step("Downloading mailcli "+latestVersion, func() error {
		var downloadErr error
		archive, downloadErr = downloadUpdateResource(ctx, environment.client, archiveURL, maximumReleaseArchive)
		return downloadErr
	}); err != nil {
		return nil, contextualUpdateFailure("update_download_failed", "download release archive", err)
	}
	if err := reporter.step("Verifying release checksum", func() error {
		return verifyReleaseChecksum(archiveName, archive, checksums)
	}); err != nil {
		return nil, err
	}
	return archive, nil
}

func writeStagedUpdateFiles(staged stagedUpdate) error {
	for _, artifact := range []struct {
		name     string
		contents []byte
	}{
		{name: updateMetadataStageName, contents: staged.metadata},
		{name: updateChecksumsStageName, contents: staged.checksums},
		{name: updateSignatureStageName, contents: staged.signature},
		{name: staged.archiveName, contents: staged.archive},
	} {
		if err := writeStagedUpdateFile(staged.root, artifact.name, artifact.contents); err != nil {
			return err
		}
	}
	return nil
}

func writeStagedUpdateFile(root string, name string, contents []byte) error {
	file, err := os.OpenFile(filepath.Join(root, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return updateFailure("update_install_failed", "stage release artifact %s: %v", name, err)
	}
	written, writeErr := file.Write(contents)
	if writeErr == nil && written != len(contents) {
		writeErr = io.ErrShortWrite
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errors.Join(
			updateFailure("update_install_failed", "stage release artifact %s", name),
			writeErr,
			closeErr,
		)
	}
	return nil
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
