package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func acquireUpdateLock(ctx context.Context, homeDirectory string) (*os.File, error) {
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
	fileDescriptor, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, updateFailure("update_lock_failed", "open update lock: %v", err)
	}
	file := os.NewFile(uintptr(fileDescriptor), lockPath)
	if file == nil {
		_ = unix.Close(fileDescriptor)
		return nil, updateFailure("update_lock_failed", "open update lock: invalid file descriptor")
	}
	if err := validateUpdateLock(file); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, errors.Join(updateFailure("update_lock_failed", "secure update lock: %v", err), file.Close())
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(updateFailure("update_busy", "another mailcli installation may be running"), err, file.Close())
		}
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			currentRoot, rootErr := os.Lstat(stateRoot)
			if rootErr != nil || !os.SameFile(stateInfo, currentRoot) {
				return nil, errors.Join(updateFailure("update_lock_failed", "update state directory changed while waiting"), file.Close())
			}
			if err := validateUpdateLock(file); err != nil {
				return nil, errors.Join(err, file.Close())
			}
			return file, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(updateFailure("update_lock_failed", "lock update state: %v", err), file.Close())
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(updateFailure("update_busy", "another mailcli installation is already running"), ctx.Err(), file.Close())
		case <-ticker.C:
		}
	}
}

func validateUpdateLock(file *os.File) error {
	if file == nil {
		return updateFailure("update_lock_failed", "installation lock is missing")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return updateFailure("update_lock_failed", "installation lock is not a regular file")
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !ok || metadata.Nlink != 1 || metadata.Uid != uint32(os.Geteuid()) {
		return updateFailure("update_lock_failed", "installation lock has unsafe ownership or links")
	}
	current, err := os.Lstat(file.Name())
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return updateFailure("update_lock_failed", "installation lock path changed")
	}
	return nil
}

type updateInstallOutcome struct {
	attempted   bool
	verified    bool
	failedPhase string
}

type stagedUpdate struct {
	root          string
	latestVersion string
	archiveName   string
	release       updateRelease
	metadata      []byte
	checksums     []byte
	signature     []byte
	archive       []byte
}

func revalidateStagedUpdate(staged stagedUpdate, environment updateEnvironment) ([]byte, error) {
	rootInfo, err := os.Lstat(staged.root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootInfo.Mode().Perm() != 0o700 {
		return nil, updateFailure("update_package_invalid", "private update staging directory changed")
	}
	metadata, err := readStagedUpdateFile(filepath.Join(staged.root, updateMetadataStageName), maximumReleaseMetadata)
	if err != nil || !bytes.Equal(metadata, staged.metadata) {
		return nil, updateFailure("update_package_invalid", "staged release metadata changed after verification")
	}
	checksums, err := readStagedUpdateFile(filepath.Join(staged.root, updateChecksumsStageName), maximumChecksumFile)
	if err != nil || !bytes.Equal(checksums, staged.checksums) {
		return nil, updateFailure("update_package_invalid", "staged checksum manifest changed after verification")
	}
	signature, err := readStagedUpdateFile(filepath.Join(staged.root, updateSignatureStageName), maximumSignatureFile)
	if err != nil || !bytes.Equal(signature, staged.signature) {
		return nil, updateFailure("update_package_invalid", "staged release signature changed after verification")
	}
	archive, err := readStagedUpdateFile(filepath.Join(staged.root, staged.archiveName), maximumReleaseArchive)
	if err != nil || !bytes.Equal(archive, staged.archive) {
		return nil, updateFailure("update_package_invalid", "staged release archive changed after verification")
	}
	if err := verifyReleaseSignature(checksums, signature, environment.releasePublicKey); err != nil {
		return nil, err
	}
	if err := verifyReleaseChecksum(staged.archiveName, archive, checksums); err != nil {
		return nil, err
	}
	return archive, nil
}

func readStagedUpdateFile(path string, maximumBytes int64) ([]byte, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil || !pathInfo.Mode().IsRegular() || pathInfo.Mode().Perm() != 0o600 || pathInfo.Size() > maximumBytes {
		return nil, fmt.Errorf("staged file is missing, unsafe, or oversized")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fileInfo, statErr := file.Stat()
	if statErr != nil || !os.SameFile(pathInfo, fileInfo) {
		return nil, errors.Join(fmt.Errorf("staged file identity changed"), statErr, file.Close())
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, maximumBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if int64(len(contents)) > maximumBytes {
		return nil, fmt.Errorf("staged file is oversized")
	}
	return contents, nil
}

func readInstalledBinaryVersion(ctx context.Context, binaryPath string) (string, error) {
	output, err := exec.CommandContext(ctx, binaryPath, "version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("run installed binary: %w: %s", err, strings.TrimSpace(string(output)))
	}
	value := strings.TrimSpace(string(output))
	prefix := name + " "
	if !strings.HasPrefix(value, prefix) {
		return "", fmt.Errorf("installed binary returned invalid version output %q", value)
	}
	_, normalized, err := parseReleaseVersion(strings.TrimPrefix(value, prefix))
	if err != nil {
		return "", fmt.Errorf("installed binary returned invalid version: %w", err)
	}
	return normalized, nil
}

func installVerifiedArchive(
	ctx context.Context,
	environment updateEnvironment,
	reporter *updateReporter,
	archive []byte,
	latestVersion string,
	installationLock *os.File,
) (outcome updateInstallOutcome, resultErr error) {
	temporaryRoot, err := os.MkdirTemp("", "mailcli-update-*")
	if err != nil {
		return updateInstallOutcome{}, updateFailure("update_install_failed", "create private update directory: %v", err)
	}
	defer func() {
		if cleanupErr := removeUpdatePackageRoot(environment, temporaryRoot); cleanupErr != nil {
			if outcome.attempted && outcome.failedPhase == "" {
				outcome.failedPhase = updatePhasePackageCleanup
			}
			resultErr = errors.Join(
				resultErr,
				updateFailure("update_install_failed", "remove private update directory: %v", cleanupErr),
			)
		}
	}()
	packageRoot := filepath.Join(temporaryRoot, fmt.Sprintf("mailcli_%s_darwin_arm64", latestVersion))
	if err := extractReleaseArchive(archive, temporaryRoot, filepath.Base(packageRoot)); err != nil {
		return updateInstallOutcome{}, err
	}
	packageBinary := filepath.Join(packageRoot, "bin", "mailcli")
	if err := environment.verifyPackage(ctx, packageBinary, latestVersion); err != nil {
		return updateInstallOutcome{}, updateFailure("update_package_invalid", "verify release binary: %v", err)
	}
	installerPath := filepath.Join(packageRoot, "install.sh")
	if err := reporter.step("Installing mailcli "+latestVersion, func() error {
		outcome.attempted = true
		return environment.installPackage(ctx, installerPath, environment.executablePath, environment.homeDirectory, installationLock)
	}); err != nil {
		if !outcome.attempted {
			return updateInstallOutcome{}, updateFailure("update_install_failed", "start release install: %v", err)
		}
		outcome.failedPhase = updatePhaseInstaller
		return outcome, updateFailure("update_install_failed", "install release: %v", err)
	}
	outcome.failedPhase = updatePhaseVerification
	if err := environment.verifyInstallation(ctx, environment.executablePath, latestVersion); err != nil {
		return outcome, updateFailure("update_install_failed", "verify installed release: %v", err)
	}
	outcome.failedPhase = ""
	outcome.verified = true
	return outcome, nil
}

func removeUpdatePackageRoot(environment updateEnvironment, path string) error {
	if environment.removePackageRoot != nil {
		return environment.removePackageRoot(path)
	}
	return os.RemoveAll(path)
}

func validateUpdateLockForEnvironment(environment updateEnvironment, lock *os.File) error {
	if environment.validateLock != nil {
		return environment.validateLock(lock)
	}
	return validateUpdateLock(lock)
}

func closeUpdateLockForEnvironment(environment updateEnvironment, lock *os.File) error {
	if environment.closeLock != nil {
		return environment.closeLock(lock)
	}
	return lock.Close()
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

func runReleaseInstaller(
	ctx context.Context,
	installerPath string,
	binaryPath string,
	homeDirectory string,
	installationLock *os.File,
) error {
	if err := validateUpdateLock(installationLock); err != nil {
		return err
	}
	info, err := os.Stat(installerPath)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("release installer is missing or invalid")
	}
	command := exec.CommandContext(ctx, "/bin/bash", installerPath)
	command.Env = updateInstallerEnvironment(os.Environ(), homeDirectory, binaryPath)
	command.ExtraFiles = []*os.File{installationLock}
	var output boundedUpdateOutput
	command.Stdout = &output
	command.Stderr = &output
	if err := runOwnedProcess(command, 5*time.Second); err != nil {
		return fmt.Errorf("release installer failed: %w: %s", err, strings.TrimSpace(output.String()))
	}
	return nil
}

func updateInstallerEnvironment(base []string, homeDirectory string, binaryPath string) []string {
	filtered := make([]string, 0, len(base)+2)
	for _, value := range base {
		name, _, _ := strings.Cut(value, "=")
		switch name {
		case "HOME", "PATH", "MAILCLI_BINARY_DESTINATION", "MAILCLI_SKILL_DESTINATION", "MAILCLI_INSTALL_LOCK_FD",
			"MAILCLI_INSTALL_PACKAGE_ROOT", "BASH_ENV", "ENV", "CDPATH", "SHELLOPTS", "BASHOPTS", "GLOBIGNORE":
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
		"MAILCLI_INSTALL_LOCK_FD=3",
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
