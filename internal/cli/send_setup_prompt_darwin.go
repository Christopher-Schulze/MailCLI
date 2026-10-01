//go:build darwin

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var osStdin = os.Stdin

const (
	ioctlReadTermios  = syscall.TIOCGETA
	ioctlWriteTermios = syscall.TIOCSETA
)

// readPasswordLine reads one secret line without ever echoing it. On a
// terminal it disables echo via termios and always restores the original
// settings; on a pipe it reads a plain line so scripts and tests can supply
// the secret. The prompt goes to stderr so stdout stays machine-readable.
func readPasswordLine(ctx context.Context, prompt string) (password string, resultErr error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fd, err := sendPasswordInputDescriptor(sendSetupStdin)
	if err != nil {
		return "", err
	}
	if fd < 0 || !isTerminal(fd) {
		if _, err := fmt.Fprint(os.Stderr, prompt); err != nil {
			return "", fmt.Errorf("write password prompt: %w", err)
		}
		return readSendPasswordLine(ctx, sendSetupStdin)
	}
	var original syscall.Termios
	if _, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL, uintptr(fd), uintptr(ioctlReadTermios), uintptr(unsafe.Pointer(&original)),
	); errno != 0 {
		return "", errno
	}
	noEcho := original
	noEcho.Lflag &^= syscall.ECHO
	noEcho.Lflag |= syscall.ICANON | syscall.ECHONL
	if _, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL, uintptr(fd), uintptr(ioctlWriteTermios), uintptr(unsafe.Pointer(&noEcho)),
	); errno != 0 {
		return "", errno
	}
	defer func() {
		_, _, errno := syscall.Syscall(
			syscall.SYS_IOCTL, uintptr(fd), uintptr(ioctlWriteTermios), uintptr(unsafe.Pointer(&original)),
		)
		if errno != 0 {
			password = ""
			resultErr = errors.Join(resultErr, fmt.Errorf("restore password terminal settings: %w", errno))
		}
	}()
	if _, err := fmt.Fprint(os.Stderr, prompt); err != nil {
		return "", fmt.Errorf("write password prompt: %w", err)
	}
	password, resultErr = readSendPasswordLine(ctx, sendSetupStdin)
	if _, err := fmt.Fprintln(os.Stderr); err != nil {
		password = ""
		resultErr = errors.Join(resultErr, fmt.Errorf("finish password prompt: %w", err))
	}
	return password, resultErr
}

func isTerminal(fd int) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL, uintptr(fd), uintptr(ioctlReadTermios), uintptr(unsafe.Pointer(&termios)),
	)
	return errno == 0
}
