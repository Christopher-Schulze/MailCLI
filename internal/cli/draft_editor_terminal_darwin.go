package cli

/*
#include <errno.h>
#include <pthread.h>
#include <signal.h>
#include <termios.h>
#include <unistd.h>

// Restoring a foreground group from the background requires SIGTTOU blocked
// on this thread. Do not alter process-wide signal dispositions or Go handlers.
static int mailcli_restore_editor_terminal(int fd, int group, void *attributes) {
    sigset_t blocked, original;
    if (sigemptyset(&blocked) != 0 || sigaddset(&blocked, SIGTTOU) != 0) return errno;
    int result = pthread_sigmask(SIG_BLOCK, &blocked, &original);
    if (result != 0) return result;
    if (tcsetpgrp(fd, group) != 0) result = errno;
    if (tcsetattr(fd, TCSANOW, (const struct termios *)attributes) != 0 && result == 0) result = errno;
    int mask_result = pthread_sigmask(SIG_SETMASK, &original, NULL);
    return result != 0 ? result : mask_result;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func prepareDraftEditorTerminal(process *exec.Cmd) (func() error, error) {
	input, ok := process.Stdin.(*os.File)
	if !ok || input == nil {
		return nil, nil
	}
	fd := int(input.Fd())
	attributes, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.ENODEV) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect editor input terminal: %w", err)
	}
	group, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil || group != syscall.Getpgrp() {
		return nil, fmt.Errorf("editor input must belong to the foreground controlling terminal (group %d): %v", group, err)
	}
	output, ok := process.Stdout.(*os.File)
	if !ok || output == nil {
		return nil, errors.New("interactive editor requires terminal output; with --json attach stderr to the controlling terminal")
	}
	outputGroup, err := unix.IoctlGetInt(int(output.Fd()), unix.TIOCGPGRP)
	if err != nil || outputGroup != group {
		return nil, errors.New("editor output must use the same controlling terminal as stdin; with --json use stderr")
	}
	process.SysProcAttr = &syscall.SysProcAttr{Foreground: true, Ctty: fd}
	return func() error {
		result := C.mailcli_restore_editor_terminal(C.int(fd), C.int(group), unsafe.Pointer(attributes))
		if result != 0 {
			return fmt.Errorf("restore editor terminal: %w", syscall.Errno(result))
		}
		return nil
	}, nil
}
