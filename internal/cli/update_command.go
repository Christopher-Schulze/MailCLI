package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/macho"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	latestReleaseURL          = "https://api.github.com/repos/Christopher-Schulze/MailCLI/releases/latest"
	maximumReleaseMetadata    = 2 * 1024 * 1024
	maximumChecksumFile       = 1024 * 1024
	maximumReleaseArchive     = 64 * 1024 * 1024
	maximumExtractedPackage   = 192 * 1024 * 1024
	maximumExtractedFileCount = 256
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
	installPackage     func(context.Context, string, string, string) error
	verifyInstallation func(context.Context, string, string) error
	allowInsecureURLs  bool
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
	}, nil
}

func secureUpdateRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("too many release download redirects")
	}
	if err := validateUpdateURL(request.URL.String(), false); err != nil {
		return fmt.Errorf("invalid release download redirect: %w", err)
	}
	return nil
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
	releaseLock, err := acquireUpdateLock(ctx, environment.homeDirectory)
	if err != nil {
		return updateResult{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, releaseLock()) }()
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
	if err := downloadAndInstallUpdate(ctx, environment, reporter, release, latestVersion); err != nil {
		return updateResult{}, err
	}
	result.Updated = true
	return result, nil
}

func fetchLatestRelease(ctx context.Context, environment updateEnvironment) (updateRelease, error) {
	if err := validateUpdateURL(environment.metadataURL, environment.allowInsecureURLs); err != nil {
		return updateRelease{}, updateFailure("update_check_failed", "invalid release metadata URL: %v", err)
	}
	payload, err := downloadUpdateResource(
		ctx, environment.client, environment.metadataURL, maximumReleaseMetadata,
	)
	if err != nil {
		return updateRelease{}, updateFailure("update_check_failed", "check latest GitHub release: %v", err)
	}
	var release updateRelease
	if err := json.Unmarshal(payload, &release); err != nil {
		return updateRelease{}, updateFailure("update_check_failed", "decode latest GitHub release: %v", err)
	}
	if release.TagName == "" || release.HTMLURL == "" {
		return updateRelease{}, updateFailure("update_check_failed", "latest GitHub release metadata is incomplete")
	}
	if err := validateUpdateURL(release.HTMLURL, environment.allowInsecureURLs); err != nil {
		return updateRelease{}, updateFailure("update_check_failed", "invalid release page URL: %v", err)
	}
	return release, nil
}

func compareReleaseVersions(current string, releaseTag string) (string, int, error) {
	currentParts, _, err := parseReleaseVersion(current)
	if err != nil {
		return "", 0, updateFailure("update_check_failed", "installed version is invalid: %v", err)
	}
	releaseParts, normalizedRelease, err := parseReleaseVersion(releaseTag)
	if err != nil {
		return "", 0, updateFailure("update_check_failed", "latest release version is invalid: %v", err)
	}
	for index := range currentParts {
		if currentParts[index] > releaseParts[index] {
			return normalizedRelease, 1, nil
		}
		if currentParts[index] < releaseParts[index] {
			return normalizedRelease, -1, nil
		}
	}
	return normalizedRelease, 0, nil
}

func parseReleaseVersion(value string) ([3]int, string, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return [3]int{}, "", fmt.Errorf("expected MAJOR.MINOR.PATCH, got %q", value)
	}
	var parsed [3]int
	for index, part := range parts {
		if part == "" || (len(part) > 1 && strings.HasPrefix(part, "0")) {
			return [3]int{}, "", fmt.Errorf("invalid numeric component %q", part)
		}
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return [3]int{}, "", fmt.Errorf("invalid numeric component %q", part)
		}
		parsed[index] = number
	}
	return parsed, value, nil
}

func downloadAndInstallUpdate(
	ctx context.Context,
	environment updateEnvironment,
	reporter *updateReporter,
	release updateRelease,
	latestVersion string,
) error {
	archiveName := fmt.Sprintf("mailcli_%s_darwin_arm64.tar.gz", latestVersion)
	archiveURL, checksumURL, err := releaseAssetURLs(release.Assets, archiveName)
	if err != nil {
		return err
	}
	if err := validateUpdateURL(archiveURL, environment.allowInsecureURLs); err != nil {
		return updateFailure("update_package_invalid", "invalid release archive URL: %v", err)
	}
	if err := validateUpdateURL(checksumURL, environment.allowInsecureURLs); err != nil {
		return updateFailure("update_package_invalid", "invalid release checksum URL: %v", err)
	}
	var archive []byte
	if err := reporter.step("Downloading mailcli "+latestVersion, func() error {
		var downloadErr error
		archive, downloadErr = downloadUpdateResource(
			ctx, environment.client, archiveURL, maximumReleaseArchive,
		)
		return downloadErr
	}); err != nil {
		return updateFailure("update_download_failed", "download release archive: %v", err)
	}
	checksums, err := downloadUpdateResource(ctx, environment.client, checksumURL, maximumChecksumFile)
	if err != nil {
		return updateFailure("update_download_failed", "download release checksums: %v", err)
	}
	if err := reporter.step("Verifying release checksum", func() error {
		return verifyReleaseChecksum(archiveName, archive, checksums)
	}); err != nil {
		return err
	}
	return installVerifiedArchive(ctx, environment, reporter, archive, latestVersion)
}

func validateUpdateURL(value string, allowInsecure bool) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("URL must have a host and no credentials or fragment")
	}
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !allowInsecure) {
		return fmt.Errorf("URL must use HTTPS")
	}
	return nil
}

func acquireUpdateLock(ctx context.Context, homeDirectory string) (func() error, error) {
	if !filepath.IsAbs(homeDirectory) {
		return nil, updateFailure("update_lock_failed", "home directory must be absolute")
	}
	resolvedHome, err := filepath.EvalSymlinks(homeDirectory)
	if err != nil {
		return nil, updateFailure("update_lock_failed", "resolve home directory: %v", err)
	}
	stateRoot := filepath.Join(resolvedHome, "Library", "Application Support", "MailCLI")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		return nil, updateFailure("update_lock_failed", "create update state directory: %v", err)
	}
	resolvedStateRoot, err := filepath.EvalSymlinks(stateRoot)
	if err != nil || resolvedStateRoot != stateRoot {
		return nil, updateFailure("update_lock_failed", "update state directory must not contain symbolic links")
	}
	stateInfo, err := os.Lstat(stateRoot)
	if err != nil || !stateInfo.IsDir() || stateInfo.Mode()&os.ModeSymlink != 0 {
		return nil, updateFailure("update_lock_failed", "update state path is not a real directory")
	}
	if err := os.Chmod(stateRoot, 0o700); err != nil {
		return nil, updateFailure("update_lock_failed", "secure update state directory: %v", err)
	}
	lockPath := filepath.Join(stateRoot, "update.lock")
	fileDescriptor, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, updateFailure("update_lock_failed", "open update lock: %v", err)
	}
	file := os.NewFile(uintptr(fileDescriptor), lockPath)
	if file == nil {
		_ = unix.Close(fileDescriptor)
		return nil, updateFailure("update_lock_failed", "open update lock: invalid file descriptor")
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, errors.Join(updateFailure("update_lock_failed", "secure update lock: %v", err), file.Close())
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() error {
				return errors.Join(syscall.Flock(int(file.Fd()), syscall.LOCK_UN), file.Close())
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(updateFailure("update_lock_failed", "lock update state: %v", err), file.Close())
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(updateFailure("update_busy", "another mailcli update is already running"), file.Close())
		case <-ticker.C:
		}
	}
}

func releaseAssetURLs(assets []updateAsset, archiveName string) (string, string, error) {
	var archiveURL string
	var checksumURL string
	for _, asset := range assets {
		switch asset.Name {
		case archiveName:
			if archiveURL != "" {
				return "", "", updateFailure("update_package_invalid", "release contains duplicate %s assets", archiveName)
			}
			archiveURL = asset.DownloadURL
		case "SHA256SUMS":
			if checksumURL != "" {
				return "", "", updateFailure("update_package_invalid", "release contains duplicate SHA256SUMS assets")
			}
			checksumURL = asset.DownloadURL
		}
	}
	if archiveURL == "" || checksumURL == "" {
		return "", "", updateFailure(
			"update_package_missing", "latest release is missing %s or SHA256SUMS", archiveName,
		)
	}
	return archiveURL, checksumURL, nil
}

func downloadUpdateResource(
	ctx context.Context,
	client *http.Client,
	resourceURL string,
	maximumBytes int64,
) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, resourceURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "MailCLI/"+version)
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		closeErr := response.Body.Close()
		return nil, errors.Join(fmt.Errorf("HTTP %s", response.Status), closeErr)
	}
	if response.ContentLength > maximumBytes {
		closeErr := response.Body.Close()
		return nil, errors.Join(fmt.Errorf("response exceeds %d bytes", maximumBytes), closeErr)
	}
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, maximumBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if int64(len(payload)) > maximumBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maximumBytes)
	}
	return payload, nil
}

func verifyReleaseChecksum(archiveName string, archive []byte, checksums []byte) error {
	expected, err := checksumForArchive(archiveName, string(checksums))
	if err != nil {
		return updateFailure("update_checksum_invalid", "%v", err)
	}
	actual := sha256.Sum256(archive)
	if !bytes.Equal(actual[:], expected) {
		return updateFailure("update_checksum_mismatch", "release archive checksum does not match SHA256SUMS")
	}
	return nil
}

func checksumForArchive(archiveName string, checksums string) ([]byte, error) {
	var matched string
	for _, line := range strings.Split(checksums, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != archiveName {
			continue
		}
		if matched != "" {
			return nil, fmt.Errorf("SHA256SUMS contains duplicate entries for %s", archiveName)
		}
		matched = fields[0]
	}
	if matched == "" {
		return nil, fmt.Errorf("SHA256SUMS does not contain %s", archiveName)
	}
	decoded, err := hex.DecodeString(matched)
	if err != nil || len(decoded) != sha256.Size {
		return nil, fmt.Errorf("SHA256SUMS contains an invalid digest for %s", archiveName)
	}
	return decoded, nil
}

func installVerifiedArchive(
	ctx context.Context,
	environment updateEnvironment,
	reporter *updateReporter,
	archive []byte,
	latestVersion string,
) (resultErr error) {
	temporaryRoot, err := os.MkdirTemp("", "mailcli-update-*")
	if err != nil {
		return updateFailure("update_install_failed", "create private update directory: %v", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(temporaryRoot); cleanupErr != nil {
			resultErr = errors.Join(
				resultErr,
				updateFailure("update_install_failed", "remove private update directory: %v", cleanupErr),
			)
		}
	}()
	packageRoot := filepath.Join(temporaryRoot, fmt.Sprintf("mailcli_%s_darwin_arm64", latestVersion))
	if err := extractReleaseArchive(archive, temporaryRoot, filepath.Base(packageRoot)); err != nil {
		return err
	}
	packageBinary := filepath.Join(packageRoot, "bin", "mailcli")
	if err := environment.verifyPackage(ctx, packageBinary, latestVersion); err != nil {
		return updateFailure("update_package_invalid", "verify release binary: %v", err)
	}
	installerPath := filepath.Join(packageRoot, "install.sh")
	if err := reporter.step("Installing mailcli "+latestVersion, func() error {
		return environment.installPackage(ctx, installerPath, environment.executablePath, environment.homeDirectory)
	}); err != nil {
		return updateFailure("update_install_failed", "install release: %v", err)
	}
	if err := environment.verifyInstallation(ctx, environment.executablePath, latestVersion); err != nil {
		return updateFailure("update_install_failed", "verify installed release: %v", err)
	}
	return nil
}

func extractReleaseArchive(archive []byte, destination string, expectedRoot string) (resultErr error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return updateFailure("update_package_invalid", "open release archive: %v", err)
	}
	defer func() {
		if err := gzipReader.Close(); err != nil {
			resultErr = errors.Join(resultErr, updateFailure("update_package_invalid", "close release archive: %v", err))
		}
	}()
	tarReader := tar.NewReader(io.LimitReader(gzipReader, maximumExtractedPackage+1))
	var extractedBytes int64
	fileCount := 0
	for {
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return updateFailure("update_package_invalid", "read release archive: %v", nextErr)
		}
		fileCount++
		if fileCount > maximumExtractedFileCount || header.Size < 0 {
			return updateFailure("update_package_invalid", "release archive exceeds its extraction limits")
		}
		extractedBytes += header.Size
		if extractedBytes > maximumExtractedPackage {
			return updateFailure("update_package_invalid", "release archive exceeds %d extracted bytes", maximumExtractedPackage)
		}
		if err := extractReleaseEntry(tarReader, header, destination, expectedRoot); err != nil {
			return err
		}
	}
	return nil
}

func extractReleaseEntry(
	tarReader *tar.Reader,
	header *tar.Header,
	destination string,
	expectedRoot string,
) error {
	cleanName := path.Clean(header.Name)
	if cleanName == "." || path.IsAbs(cleanName) || cleanName == ".." || strings.HasPrefix(cleanName, "../") {
		return updateFailure("update_package_invalid", "release archive contains an unsafe path")
	}
	if cleanName != expectedRoot && !strings.HasPrefix(cleanName, expectedRoot+"/") {
		return updateFailure("update_package_invalid", "release archive contains an unexpected package root")
	}
	target := filepath.Join(destination, filepath.FromSlash(cleanName))
	if !pathInsideDirectory(destination, target) {
		return updateFailure("update_package_invalid", "release archive path escapes its private directory")
	}
	switch header.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, 0o755); err != nil {
			return updateFailure("update_install_failed", "create release directory: %v", err)
		}
		return nil
	case tar.TypeReg:
		return extractReleaseFile(tarReader, header, target)
	default:
		return updateFailure("update_package_invalid", "release archive contains unsupported entry type %d", header.Typeflag)
	}
}

func pathInsideDirectory(directory string, target string) bool {
	relative, err := filepath.Rel(directory, target)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func extractReleaseFile(tarReader *tar.Reader, header *tar.Header, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return updateFailure("update_install_failed", "create release file directory: %v", err)
	}
	mode := os.FileMode(header.Mode) & 0o755
	if mode&0o600 != 0o600 {
		mode |= 0o600
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return updateFailure("update_package_invalid", "create extracted release file: %v", err)
	}
	_, copyErr := io.CopyN(file, tarReader, header.Size)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return updateFailure("update_package_invalid", "extract release file: %v", errors.Join(copyErr, closeErr))
	}
	return nil
}

func verifyReleaseBinary(ctx context.Context, binaryPath string, expectedVersion string) error {
	file, err := macho.Open(binaryPath)
	if err != nil {
		return fmt.Errorf("open Mach-O binary: %w", err)
	}
	cpu := file.Cpu
	closeErr := file.Close()
	if closeErr != nil {
		return closeErr
	}
	if cpu != macho.CpuArm64 {
		return fmt.Errorf("release binary architecture is %s, want arm64", cpu)
	}
	if output, err := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--strict", binaryPath).CombinedOutput(); err != nil {
		return fmt.Errorf("verify code signature: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return verifyBinaryVersion(ctx, binaryPath, expectedVersion)
}

func verifyInstalledBinary(ctx context.Context, binaryPath string, expectedVersion string) error {
	return verifyBinaryVersion(ctx, binaryPath, expectedVersion)
}

func verifyBinaryVersion(ctx context.Context, binaryPath string, expectedVersion string) error {
	output, err := exec.CommandContext(ctx, binaryPath, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("run installed binary: %w: %s", err, strings.TrimSpace(string(output)))
	}
	want := "mailcli " + expectedVersion
	if strings.TrimSpace(string(output)) != want {
		return fmt.Errorf("version output is %q, want %q", strings.TrimSpace(string(output)), want)
	}
	return nil
}

func runReleaseInstaller(
	ctx context.Context,
	installerPath string,
	binaryPath string,
	homeDirectory string,
) error {
	info, err := os.Stat(installerPath)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("release installer is missing or invalid")
	}
	command := exec.CommandContext(ctx, "/bin/bash", installerPath)
	command.Env = updateInstallerEnvironment(os.Environ(), homeDirectory, binaryPath)
	command.WaitDelay = 5 * time.Second
	var output boundedUpdateOutput
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("release installer failed: %w: %s", err, strings.TrimSpace(output.String()))
	}
	return nil
}

func updateInstallerEnvironment(base []string, homeDirectory string, binaryPath string) []string {
	filtered := make([]string, 0, len(base)+2)
	for _, value := range base {
		name, _, _ := strings.Cut(value, "=")
		switch name {
		case "HOME", "PATH", "MAILCLI_BINARY_DESTINATION", "MAILCLI_SKILL_DESTINATION",
			"BASH_ENV", "ENV", "CDPATH", "SHELLOPTS", "BASHOPTS", "GLOBIGNORE":
			continue
		}
		if strings.HasPrefix(name, "BASH_FUNC_") || strings.HasPrefix(name, "DYLD_") ||
			strings.HasPrefix(name, "LD_") {
			continue
		}
		filtered = append(filtered, value)
	}
	return append(
		filtered,
		"HOME="+homeDirectory,
		"PATH=/usr/bin:/bin",
		"MAILCLI_BINARY_DESTINATION="+binaryPath,
	)
}

type boundedUpdateOutput struct {
	buffer bytes.Buffer
}

func (o *boundedUpdateOutput) Write(payload []byte) (int, error) {
	const maximumOutput = 64 * 1024
	remaining := maximumOutput - o.buffer.Len()
	if remaining > 0 {
		_, _ = o.buffer.Write(payload[:min(len(payload), remaining)])
	}
	return len(payload), nil
}

func (o *boundedUpdateOutput) String() string {
	return o.buffer.String()
}

func updateFailure(code string, format string, values ...any) error {
	return &updateError{code: code, message: fmt.Sprintf(format, values...)}
}

type updateReporter struct {
	writer   io.Writer
	enabled  bool
	animated bool
}

func newUpdateReporter(writer io.Writer, enabled bool, animated bool) *updateReporter {
	return &updateReporter{writer: writer, enabled: enabled, animated: animated}
}

func (r *updateReporter) step(message string, action func() error) error {
	if !r.enabled {
		return action()
	}
	if !r.animated {
		writeLine(r.writer, message+"...")
		return action()
	}
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	done := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(1)
	writeFormat(r.writer, "\r%s %s...", frames[0], message)
	go animateUpdateStatus(r.writer, message, frames, done, &wait)
	err := action()
	close(done)
	wait.Wait()
	if err != nil {
		writeFormat(r.writer, "\r\x1b[2K✗ %s failed.\n", message)
		return err
	}
	writeFormat(r.writer, "\r\x1b[2K✓ %s.\n", message)
	return nil
}

func animateUpdateStatus(
	writer io.Writer,
	message string,
	frames []string,
	done <-chan struct{},
	wait *sync.WaitGroup,
) {
	defer wait.Done()
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	frameIndex := 1
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			writeFormat(writer, "\r%s %s...", frames[frameIndex%len(frames)], message)
			frameIndex++
		}
	}
}

func writerIsTerminal(writer io.Writer) bool {
	fileDescriptor, ok := updateWriterFileDescriptor(writer)
	if !ok {
		return false
	}
	_, err := unix.IoctlGetTermios(int(fileDescriptor), unix.TIOCGETA)
	return err == nil
}

func updateWriterFileDescriptor(writer io.Writer) (uintptr, bool) {
	switch value := writer.(type) {
	case interface{ Fd() uintptr }:
		return value.Fd(), true
	case *errorTrackingWriter:
		return updateWriterFileDescriptor(value.writer)
	case *countingWriter:
		return updateWriterFileDescriptor(value.writer)
	default:
		return 0, false
	}
}
