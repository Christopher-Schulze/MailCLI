package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const maximumSendPasswordInputBytes = 4096

func sendPasswordInputDescriptor(reader io.Reader) (int, error) {
	file, ok := reader.(*os.File)
	if !ok {
		return -1, nil
	}
	raw, err := file.SyscallConn()
	if err != nil {
		return -1, err
	}
	fd := -1
	err = raw.Control(func(value uintptr) { fd = int(value) })
	return fd, err
}

// Read one bounded line without consuming bytes belonging to the next input.
// Borrowed file descriptors retain their flags and ownership. Real stdin waits
// use the same cancellation-aware readiness loop as invocation JSON input.
func readSendPasswordLine(ctx context.Context, reader io.Reader) (string, error) {
	fd, err := sendPasswordInputDescriptor(reader)
	if err != nil {
		return "", err
	}
	line := make([]byte, 0, 64)
	var buffer [1]byte
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if fd >= 0 {
			if err := waitInputReadable(ctx, fd); err != nil {
				return "", err
			}
		}
		n, err := reader.Read(buffer[:])
		if ctx.Err() != nil {
			return "", errors.Join(ctx.Err(), err)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read password input: %w", err)
		}
		if n > 0 {
			line = append(line, buffer[0])
			if len(line) > maximumSendPasswordInputBytes {
				return "", &commandError{code: "invalid_argument", message: "password input exceeds the 4096-byte line limit"}
			}
			if buffer[0] == '\n' {
				return strings.TrimRight(string(line), "\r\n"), nil
			}
		}
		if errors.Is(err, io.EOF) {
			return strings.TrimRight(string(line), "\r\n"), nil
		}
		if n == 0 {
			return "", fmt.Errorf("read password input: %w", io.ErrNoProgress)
		}
	}
}
