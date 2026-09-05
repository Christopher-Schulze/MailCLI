//go:build darwin

package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
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
func readPasswordLine(prompt string) (string, error) {
	fd := int(osStdin.Fd())
	if !isTerminal(fd) {
		fmt.Fprint(os.Stderr, prompt)
		line, err := bufio.NewReader(sendSetupStdin).ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
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
		_, _, _ = syscall.Syscall(
			syscall.SYS_IOCTL, uintptr(fd), uintptr(ioctlWriteTermios), uintptr(unsafe.Pointer(&original)),
		)
	}()
	fmt.Fprint(os.Stderr, prompt)
	line, err := bufio.NewReader(sendSetupStdin).ReadString('\n')
	fmt.Fprintln(os.Stderr)
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func isTerminal(fd int) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL, uintptr(fd), uintptr(ioctlReadTermios), uintptr(unsafe.Pointer(&termios)),
	)
	return errno == 0
}
