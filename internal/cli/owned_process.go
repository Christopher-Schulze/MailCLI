package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const ownedProcessPollInterval = 10 * time.Millisecond

func runOwnedProcess(command *exec.Cmd, cancellationGrace time.Duration) error {
	if cancellationGrace <= 0 {
		return errors.New("owned process cancellation grace must be positive")
	}
	processAttributes := &syscall.SysProcAttr{}
	if command.SysProcAttr != nil {
		copiedAttributes := *command.SysProcAttr
		processAttributes = &copiedAttributes
	}
	processAttributes.Setpgid = true
	command.SysProcAttr = processAttributes
	command.WaitDelay = cancellationGrace
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		return signalOwnedProcessGroup(command.Process.Pid, syscall.SIGTERM)
	}
	if err := command.Start(); err != nil {
		return err
	}
	groupID := command.Process.Pid
	waitErr := command.Wait()
	cleanupErr := stopOwnedProcessGroup(groupID, cancellationGrace)
	return errors.Join(waitErr, cleanupErr)
}

func stopOwnedProcessGroup(groupID int, grace time.Duration) error {
	exists, err := ownedProcessGroupExists(groupID)
	if err != nil || !exists {
		return err
	}
	if err := signalOwnedProcessGroup(groupID, syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	exited, err := waitForOwnedProcessGroup(groupID, grace)
	if err != nil {
		return err
	}
	if exited {
		return nil
	}
	if err := signalOwnedProcessGroup(groupID, syscall.SIGKILL); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	exited, err = waitForOwnedProcessGroup(groupID, grace)
	if err != nil {
		return err
	}
	if exited {
		return nil
	}
	return fmt.Errorf("owned process group %d remained after SIGKILL", groupID)
}

func signalOwnedProcessGroup(groupID int, signal syscall.Signal) error {
	err := syscall.Kill(-groupID, signal)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func waitForOwnedProcessGroup(groupID int, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		exists, err := ownedProcessGroupExists(groupID)
		if err != nil {
			return false, err
		}
		if !exists {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(ownedProcessPollInterval)
	}
}

func ownedProcessGroupExists(groupID int) (bool, error) {
	err := syscall.Kill(-groupID, syscall.Signal(0))
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
}
