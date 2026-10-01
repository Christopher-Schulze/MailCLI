//go:build darwin

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSendPasswordTerminalRestoration(t *testing.T) {
	for _, mode := range []string{"success", "deadline", "restore failure"} {
		t.Run(mode, func(t *testing.T) {
			master, slave := openEditorTestTerminal(t)
			fd, err := sendPasswordInputDescriptor(slave)
			if err != nil {
				t.Fatal(err)
			}
			original, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
			if err != nil {
				t.Fatal(err)
			}
			input := slave
			if mode == "restore failure" {
				duplicate, err := unix.Dup(fd)
				if err != nil {
					t.Fatal(err)
				}
				input = os.NewFile(uintptr(duplicate), "password-terminal")
				t.Cleanup(func() {
					if err := input.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
						t.Error(err)
					}
					if err := unix.IoctlSetTermios(fd, unix.TIOCSETA, original); err != nil {
						t.Error(err)
					}
				})
			}
			previousStdin := sendSetupStdin
			sendSetupStdin = input
			t.Cleanup(func() { sendSetupStdin = previousStdin })
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			type result struct {
				password string
				err      error
			}
			done := make(chan result, 1)
			go func() { password, err := readPasswordLine(ctx, "Password: "); done <- result{password, err} }()
			waited := false
			defer func() {
				cancel()
				if !waited {
					<-done
				}
			}()
			for {
				current, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
				if err != nil {
					t.Fatal(err)
				}
				if current.Lflag&unix.ECHO == 0 {
					break
				}
				select {
				case got := <-done:
					waited = true
					t.Fatalf("password prompt exited before disabling echo: %v", got.err)
				case <-ctx.Done():
					t.Fatal("password prompt did not disable terminal echo")
				case <-time.After(5 * time.Millisecond):
				}
			}
			switch mode {
			case "success":
				if _, err := io.WriteString(master, "secret\n"); err != nil {
					t.Fatal(err)
				}
			case "restore failure":
				if err := input.Close(); err != nil {
					t.Fatal(err)
				}
			}
			got := <-done
			waited = true
			switch mode {
			case "success":
				if got.err != nil || got.password != "secret" {
					t.Fatalf("terminal success error=%v, password length=%d", got.err, len(got.password))
				}
			case "deadline":
				if !errors.Is(got.err, context.DeadlineExceeded) || got.password != "" {
					t.Fatalf("terminal deadline error=%v", got.err)
				}
			case "restore failure":
				if got.err == nil || !strings.Contains(got.err.Error(), "restore password terminal settings") || got.password != "" {
					t.Fatalf("restore failure error=%v", got.err)
				}
				return
			}
			restored, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
			if err != nil || *restored != *original {
				t.Fatalf("terminal settings were not restored: error=%v", err)
			}
		})
	}
}

func TestSendSetupUsesSignalContext(t *testing.T) {
	if !RequiresSignalContext([]string{"send", "setup", "--from", "alice@icloud.com"}) || RequiresSignalContext([]string{"send", "setup", "--help"}) {
		t.Fatal("send setup cancellation registration is inconsistent")
	}
}
