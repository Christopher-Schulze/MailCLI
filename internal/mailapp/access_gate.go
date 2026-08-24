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
	file      *os.File
	targetPID int
	mailPID   func(context.Context) (int, error)
}

type uncertainMailStateError struct{}

type mailNotRunningError struct{}

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
	if err := os.MkdirAll(filepath.Dir(g.path), 0o700); err != nil {
		return nil, fmt.Errorf("create MailCLI access directory: %w", err)
	}
	file, err := os.OpenFile(g.path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open Mail.app access gate: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, errors.Join(
			fmt.Errorf("secure Mail.app access gate: %w", err),
			file.Close(),
		)
	}
	if err := g.acquireFile(ctx, file); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	mailPID := g.mailPID
	if mailPID == nil {
		mailPID = currentMailPID
	}
	if err := validateAccessGateState(ctx, file, mailPID); err != nil {
		return nil, errors.Join(err, releaseFileLock(file))
	}
	targetPID := 0
	if g.mailPID != nil {
		pid, err := g.mailPID(ctx)
		if err != nil {
			return nil, errors.Join(err, releaseFileLock(file))
		}
		if pid <= 0 {
			return nil, errors.Join(&mailNotRunningError{}, releaseFileLock(file))
		}
		targetPID = pid
	}
	return &fileAccessLease{file: file, targetPID: targetPID, mailPID: mailPID}, nil
}

func (g *fileAccessGate) acquireFile(ctx context.Context, file *os.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return errors.Join(contextErr, syscall.Flock(int(file.Fd()), syscall.LOCK_UN))
		}
		return nil
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		return fmt.Errorf("lock Mail.app access gate: %w", err)
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
		if contextErr := waitCtx.Err(); contextErr != nil {
			return contextErr
		}
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			if contextErr := waitCtx.Err(); contextErr != nil {
				return errors.Join(contextErr, syscall.Flock(int(file.Fd()), syscall.LOCK_UN))
			}
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("lock Mail.app access gate: %w", err)
		}
	}
}

func (l *fileAccessLease) Release(uncertain bool) error {
	var stateErr error
	if uncertain {
		stateErr = writeUncertainState(l.file, l.targetPID, l.mailPID)
	}
	return errors.Join(stateErr, releaseFileLock(l.file))
}

func (l *fileAccessLease) TargetPID() int {
	return l.targetPID
}

func releaseFileLock(file *os.File) error {
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return errors.Join(unlockErr, file.Close())
}

func validateAccessGateState(
	ctx context.Context,
	file *os.File,
	mailPID func(context.Context) (int, error),
) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect Mail.app access gate: %w", err)
	}
	if info.Size() == 0 {
		return nil
	}
	if info.Size() > 4096 {
		return fmt.Errorf("mail.app access gate state is invalid")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek Mail.app access gate: %w", err)
	}
	payload, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return fmt.Errorf("read Mail.app access gate: %w", err)
	}
	var state accessGateState
	if err := json.Unmarshal(payload, &state); err != nil || state.MailPID <= 0 {
		return fmt.Errorf("mail.app access gate state is invalid")
	}
	currentPID, err := mailPID(ctx)
	if err != nil {
		return err
	}
	if currentPID == state.MailPID {
		return &uncertainMailStateError{}
	}
	return clearAccessGateState(file)
}

func writeUncertainState(
	file *os.File,
	targetPID int,
	mailPID func(context.Context) (int, error),
) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	currentPID, err := mailPID(ctx)
	if err != nil {
		return err
	}
	if targetPID <= 0 {
		return fmt.Errorf("record Mail.app uncertainty: target process is invalid")
	}
	if currentPID != targetPID {
		return clearAccessGateState(file)
	}
	payload, err := json.Marshal(accessGateState{MailPID: targetPID})
	if err != nil {
		return fmt.Errorf("encode Mail.app access gate state: %w", err)
	}
	if err := clearAccessGateState(file); err != nil {
		return err
	}
	if _, err := file.Write(payload); err != nil {
		return fmt.Errorf("write Mail.app access gate state: %w", err)
	}
	return file.Sync()
}

func clearAccessGateState(file *os.File) error {
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("clear Mail.app access gate state: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek cleared Mail.app access gate: %w", err)
	}
	return nil
}

func currentMailPID(ctx context.Context) (int, error) {
	output, err := exec.CommandContext(ctx, "/usr/bin/pgrep", "-x", "Mail").Output()
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

func (*mailNotRunningError) Error() string {
	return "Mail.app is not running"
}
