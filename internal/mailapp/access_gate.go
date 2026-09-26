package mailapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	accessGateMaxWait      = 2 * time.Second
	accessGatePollInterval = 25 * time.Millisecond
)

type accessGate interface {
	Acquire(context.Context) (accessLease, error)
}

type accessLease interface {
	TargetPID() int
	ArmUncertainState() error
	Release(uncertain bool) error
}

type fileAccessGate struct {
	path         string
	pathError    error
	maxWait      time.Duration
	pollInterval time.Duration
	mailPID      func(context.Context) (int, error)
}

type fileAccessLease struct {
	files        *accessGateFiles
	targetPID    int
	stateTouched bool
	armed        bool
}

type accessGateFiles struct {
	parentPath     string
	stateDirectory string
	stateName      string
	lockName       string
	parent         *os.File
	directory      *os.File
	file           *os.File
}

type uncertainMailStateError struct{}

type invalidAccessGateStateError struct{}

type mailNotRunningError struct{}

type unsafeAccessGatePathError struct {
	cause error
}

type accessGateState struct {
	MailPID int `json:"mail_pid"`
}

func newFileAccessGate() *fileAccessGate {
	root, err := os.UserConfigDir()
	if err != nil {
		return &fileAccessGate{pathError: fmt.Errorf("resolve Application Support directory: %w", err)}
	}
	return &fileAccessGate{
		path:    filepath.Join(root, "MailCLI", "mail-access.lock"),
		maxWait: accessGateMaxWait, pollInterval: accessGatePollInterval, mailPID: currentMailPID,
	}
}

func (g *fileAccessGate) Acquire(ctx context.Context) (accessLease, error) {
	if g.pathError != nil {
		return nil, g.pathError
	}
	files, err := openAccessGateFiles(g.path)
	if err != nil {
		return nil, err
	}
	if err := g.acquireFile(ctx, files); err != nil {
		return nil, errors.Join(err, files.close())
	}
	mailPID := g.mailPID
	if mailPID == nil {
		mailPID = currentMailPID
	}
	if err := validateAccessGateState(ctx, files, mailPID); err != nil {
		return nil, errors.Join(err, files.release())
	}
	targetPID := 0
	if g.mailPID != nil {
		pid, err := g.mailPID(ctx)
		if err != nil {
			return nil, errors.Join(err, files.release())
		}
		if pid <= 0 {
			return nil, errors.Join(&mailNotRunningError{}, files.release())
		}
		targetPID = pid
	}
	if err := files.verify(); err != nil {
		return nil, errors.Join(err, files.release())
	}
	return &fileAccessLease{files: files, targetPID: targetPID}, nil
}

func openAccessGateFiles(path string) (*accessGateFiles, error) {
	cleanPath := filepath.Clean(path)
	stateDirectory := filepath.Dir(cleanPath)
	files := &accessGateFiles{
		parentPath:     filepath.Dir(stateDirectory),
		stateDirectory: stateDirectory,
		stateName:      filepath.Base(stateDirectory),
		lockName:       filepath.Base(cleanPath),
	}
	if !filepath.IsAbs(cleanPath) || files.stateName == "." || files.stateName == string(os.PathSeparator) ||
		files.lockName == "." || files.lockName == string(os.PathSeparator) {
		return nil, &unsafeAccessGatePathError{cause: fmt.Errorf("access gate path must be absolute")}
	}
	if err := files.openStateDirectory(); err != nil {
		return nil, files.failUnsafe(err)
	}
	if err := files.openLockFile(); err != nil {
		return nil, files.failUnsafe(err)
	}
	if err := files.verify(); err != nil {
		return nil, files.failUnsafe(err)
	}
	return files, nil
}

func (files *accessGateFiles) openStateDirectory() error {
	parent, err := openDirectoryPath(files.parentPath)
	if err != nil {
		return err
	}
	files.parent = parent
	if err := files.verifyParent(); err != nil {
		return err
	}
	if err := unix.Mkdirat(int(files.parent.Fd()), files.stateName, 0o700); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("create MailCLI access directory: %w", err)
	}
	directoryDescriptor, err := unix.Openat(
		int(files.parent.Fd()), files.stateName,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return fmt.Errorf("open MailCLI access directory: %w", err)
	}
	files.directory = os.NewFile(uintptr(directoryDescriptor), files.stateDirectory)
	if files.directory == nil {
		_ = unix.Close(directoryDescriptor)
		return fmt.Errorf("open MailCLI access directory: invalid file descriptor")
	}
	return files.secureDirectory()
}

func (files *accessGateFiles) secureDirectory() error {
	if err := files.verifyDirectory(false); err != nil {
		return err
	}
	if err := files.directory.Chmod(0o700); err != nil {
		return fmt.Errorf("secure MailCLI access directory: %w", err)
	}
	return files.verifyDirectory(true)
}

func (files *accessGateFiles) openLockFile() error {
	fileDescriptor, err := unix.Openat(
		int(files.directory.Fd()), files.lockName,
		unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600,
	)
	if err != nil {
		return fmt.Errorf("open Mail.app access gate: %w", err)
	}
	files.file = os.NewFile(uintptr(fileDescriptor), filepath.Join(files.stateDirectory, files.lockName))
	if files.file == nil {
		_ = unix.Close(fileDescriptor)
		return fmt.Errorf("open Mail.app access gate: invalid file descriptor")
	}
	if err := files.verifyFile(false); err != nil {
		return err
	}
	if err := files.file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure Mail.app access gate: %w", err)
	}
	return files.verify()
}

func (files *accessGateFiles) failUnsafe(err error) error {
	return errors.Join(&unsafeAccessGatePathError{cause: err}, files.close())
}

func openDirectoryPath(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("directory path must be absolute")
	}
	directoryDescriptor, err := unix.Open(
		string(os.PathSeparator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0,
	)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root: %w", err)
	}
	directory := os.NewFile(uintptr(directoryDescriptor), string(os.PathSeparator))
	if directory == nil {
		_ = unix.Close(directoryDescriptor)
		return nil, fmt.Errorf("open filesystem root: invalid file descriptor")
	}
	components := strings.Split(strings.TrimPrefix(filepath.Clean(path), string(os.PathSeparator)), string(os.PathSeparator))
	for _, component := range components {
		if component == "" {
			continue
		}
		nextDescriptor, err := unix.Openat(
			int(directory.Fd()), component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
		)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("open directory component: %w", err), directory.Close())
		}
		nextDirectory := os.NewFile(uintptr(nextDescriptor), filepath.Join(directory.Name(), component))
		if nextDirectory == nil {
			_ = unix.Close(nextDescriptor)
			return nil, errors.Join(fmt.Errorf("open directory component: invalid file descriptor"), directory.Close())
		}
		if err := directory.Close(); err != nil {
			return nil, errors.Join(err, nextDirectory.Close())
		}
		directory = nextDirectory
	}
	return directory, nil
}

func (files *accessGateFiles) verifyParent() error {
	info, err := files.parent.Stat()
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("MailCLI access parent is not a real directory")
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect MailCLI access parent ownership")
	}
	if metadata.Uid != uint32(os.Geteuid()) && (metadata.Uid != 0 || info.Mode()&os.ModeSticky == 0) {
		return fmt.Errorf("MailCLI access parent has an untrusted owner")
	}
	if metadata.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("MailCLI access parent is writable by other users")
	}
	current, err := openDirectoryPath(files.parentPath)
	if err != nil {
		return fmt.Errorf("reopen MailCLI access parent: %w", err)
	}
	currentInfo, inspectErr := current.Stat()
	closeErr := current.Close()
	if inspectErr != nil {
		return errors.Join(fmt.Errorf("inspect reopened MailCLI access parent: %w", inspectErr), closeErr)
	}
	if !os.SameFile(info, currentInfo) {
		return errors.Join(fmt.Errorf("MailCLI access parent identity changed"), closeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close reopened MailCLI access parent: %w", closeErr)
	}
	return nil
}

func (files *accessGateFiles) verifyDirectory(requirePrivate bool) error {
	if err := files.verifyParent(); err != nil {
		return err
	}
	info, err := files.directory.Stat()
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("MailCLI access path is not a real directory")
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !ok || metadata.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("MailCLI access directory has an untrusted owner")
	}
	pathInfo, err := os.Lstat(files.stateDirectory)
	if err != nil || !pathInfo.IsDir() || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) {
		return fmt.Errorf("MailCLI access directory identity changed")
	}
	if requirePrivate && info.Mode().Perm() != 0o700 {
		return fmt.Errorf("MailCLI access directory is not private")
	}
	return nil
}

func (files *accessGateFiles) verifyFile(requirePrivate bool) error {
	if err := files.verifyDirectory(true); err != nil {
		return err
	}
	info, err := files.file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("MailCLI access gate is not a regular file")
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !ok || metadata.Uid != uint32(os.Geteuid()) || metadata.Nlink != 1 {
		return fmt.Errorf("MailCLI access gate ownership or link count is unsafe")
	}
	pathInfo, err := os.Lstat(filepath.Join(files.stateDirectory, files.lockName))
	if err != nil || !pathInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) {
		return fmt.Errorf("MailCLI access gate identity changed")
	}
	if requirePrivate && info.Mode().Perm() != 0o600 {
		return fmt.Errorf("MailCLI access gate is not private")
	}
	return nil
}

func (files *accessGateFiles) verify() error {
	if err := files.verifyFile(true); err != nil {
		return &unsafeAccessGatePathError{cause: err}
	}
	return nil
}

func (files *accessGateFiles) close() error {
	var errs []error
	for _, file := range []*os.File{files.file, files.directory, files.parent} {
		if file != nil {
			errs = append(errs, file.Close())
		}
	}
	files.file = nil
	files.directory = nil
	files.parent = nil
	return errors.Join(errs...)
}

func (files *accessGateFiles) release() error {
	verifyErr := files.verify()
	var unlockErr error
	if files.file != nil {
		unlockErr = syscall.Flock(int(files.file.Fd()), syscall.LOCK_UN)
	}
	return errors.Join(verifyErr, unlockErr, files.close())
}

func (g *fileAccessGate) acquireFile(ctx context.Context, files *accessGateFiles) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	locked, err := tryAccessGateLock(ctx, files)
	if err != nil || locked {
		return err
	}
	waitCtx := ctx
	cancel := func() {}
	if g.maxWait > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, g.maxWait)
	}
	defer cancel()
	interval := g.pollInterval
	if interval <= 0 {
		interval = accessGatePollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-ticker.C:
		}
		locked, err := tryAccessGateLock(waitCtx, files)
		if err != nil || locked {
			return err
		}
	}
}

func tryAccessGateLock(ctx context.Context, files *accessGateFiles) (bool, error) {
	if err := files.verify(); err != nil {
		return false, err
	}
	err := syscall.Flock(int(files.file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock Mail.app access gate: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return false, errors.Join(err, syscall.Flock(int(files.file.Fd()), syscall.LOCK_UN))
	}
	if err := files.verify(); err != nil {
		return false, errors.Join(err, syscall.Flock(int(files.file.Fd()), syscall.LOCK_UN))
	}
	return true, nil
}

func (l *fileAccessLease) Release(uncertain bool) error {
	var stateErr error
	if uncertain {
		stateErr = l.ArmUncertainState()
	} else if l.stateTouched {
		stateErr = l.files.clearState()
	}
	return errors.Join(stateErr, l.files.release())
}

func (l *fileAccessLease) TargetPID() int {
	return l.targetPID
}

func (l *fileAccessLease) ArmUncertainState() error {
	if l.armed {
		return l.files.verify()
	}
	l.stateTouched = true
	if err := l.files.writeUncertainState(l.targetPID); err != nil {
		return err
	}
	l.armed = true
	return nil
}

func validateAccessGateState(
	ctx context.Context,
	files *accessGateFiles,
	mailPID func(context.Context) (int, error),
) error {
	if err := files.verify(); err != nil {
		return err
	}
	state, err := readAccessGateState(files)
	if err != nil {
		var invalid *invalidAccessGateStateError
		if errors.As(err, &invalid) {
			return repairCorruptAccessGateState(ctx, files, mailPID)
		}
		return err
	}
	if state.MailPID == 0 {
		return nil
	}
	currentPID, err := mailPID(ctx)
	if err != nil {
		return err
	}
	if err := files.verify(); err != nil {
		return err
	}
	if currentPID == state.MailPID {
		return &uncertainMailStateError{}
	}
	return files.clearState()
}

func readAccessGateState(files *accessGateFiles) (accessGateState, error) {
	info, err := files.file.Stat()
	if err != nil {
		return accessGateState{}, fmt.Errorf("inspect Mail.app access gate: %w", err)
	}
	if info.Size() == 0 {
		return accessGateState{}, nil
	}
	if info.Size() > 4096 {
		return accessGateState{}, &invalidAccessGateStateError{}
	}
	if _, err := files.file.Seek(0, io.SeekStart); err != nil {
		return accessGateState{}, fmt.Errorf("seek Mail.app access gate: %w", err)
	}
	payload, err := io.ReadAll(io.LimitReader(files.file, 4096))
	if err != nil {
		return accessGateState{}, fmt.Errorf("read Mail.app access gate: %w", err)
	}
	if err := files.verify(); err != nil {
		return accessGateState{}, err
	}
	var state accessGateState
	if err := json.Unmarshal(payload, &state); err != nil || state.MailPID <= 0 {
		return accessGateState{}, &invalidAccessGateStateError{}
	}
	return state, nil
}

func repairCorruptAccessGateState(
	ctx context.Context,
	files *accessGateFiles,
	mailPID func(context.Context) (int, error),
) error {
	currentPID, err := mailPID(ctx)
	if identityErr := files.verify(); identityErr != nil {
		return identityErr
	}
	if err != nil || currentPID != 0 {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return &invalidAccessGateStateError{}
	}
	if err := files.clearState(); err != nil {
		return err
	}
	return &mailNotRunningError{}
}

func (files *accessGateFiles) writeUncertainState(targetPID int) error {
	if targetPID <= 0 {
		return fmt.Errorf("record Mail.app uncertainty: target process is invalid")
	}
	if err := files.verify(); err != nil {
		return err
	}
	payload, err := json.Marshal(accessGateState{MailPID: targetPID})
	if err != nil {
		return fmt.Errorf("encode Mail.app access gate state: %w", err)
	}
	written, err := files.file.WriteAt(payload, 0)
	if err != nil {
		return fmt.Errorf("write Mail.app access gate state: %w", err)
	}
	if written != len(payload) {
		return fmt.Errorf("write Mail.app access gate state: wrote %d of %d bytes", written, len(payload))
	}
	if err := files.verify(); err != nil {
		return err
	}
	if err := files.file.Truncate(int64(len(payload))); err != nil {
		return fmt.Errorf("truncate Mail.app access gate state: %w", err)
	}
	if err := files.verify(); err != nil {
		return err
	}
	if err := files.file.Sync(); err != nil {
		return fmt.Errorf("sync Mail.app access gate state: %w", err)
	}
	return files.verify()
}

func (files *accessGateFiles) clearState() error {
	if err := files.verify(); err != nil {
		return err
	}
	if err := files.file.Truncate(0); err != nil {
		return fmt.Errorf("clear Mail.app access gate state: %w", err)
	}
	if err := files.verify(); err != nil {
		return err
	}
	if err := files.file.Sync(); err != nil {
		return fmt.Errorf("sync cleared Mail.app access gate state: %w", err)
	}
	if _, err := files.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek cleared Mail.app access gate: %w", err)
	}
	return files.verify()
}

func currentMailPID(ctx context.Context) (int, error) {
	output, err := exec.CommandContext(
		ctx, "/usr/bin/pgrep", "-f", "-x", "/System/Applications/Mail.app/Contents/MacOS/Mail",
	).Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
			return 0, nil
		}
		return 0, fmt.Errorf("resolve Mail.app process: %w", err)
	}
	value := strings.SplitN(strings.TrimSpace(string(output)), "\n", 2)[0]
	pid, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse Mail.app process ID: %w", err)
	}
	return pid, nil
}

func (*uncertainMailStateError) Error() string {
	return "a previous Mail.app operation timed out and may still be running"
}

func (*invalidAccessGateStateError) Error() string {
	return "Mail.app access gate recovery state is invalid"
}

func (*mailNotRunningError) Error() string {
	return "Mail.app is not running"
}

func (err *unsafeAccessGatePathError) Error() string {
	return "Mail.app access gate path is unsafe"
}

func (err *unsafeAccessGatePathError) Unwrap() error {
	return err.cause
}
