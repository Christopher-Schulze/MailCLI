package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// An invocation exclusively reads its borrowed stdin, without closing it or
// changing descriptor flags. Explicit paths must resolve to regular files.
func readInvocationInput(ctx context.Context, path string, limit int64) (payload []byte, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file := os.Stdin
	if path != "-" {
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, fmt.Errorf("open input: %w", err)
		}
		file = os.NewFile(uintptr(fd), path)
		defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
		info, err := file.Stat()
		if err != nil {
			return nil, fmt.Errorf("inspect input: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, invalidDraftInput("input path must resolve to a regular file; pipe streams through stdin with -")
		}
	}
	return readInvocationFile(ctx, file, path == "-", limit)
}

func readInvocationFile(ctx context.Context, file *os.File, stdin bool, limit int64) ([]byte, error) {
	fd := 0
	if stdin {
		raw, err := file.SyscallConn()
		if err != nil {
			return nil, err
		}
		if err := raw.Control(func(value uintptr) { fd = int(value) }); err != nil {
			_, statErr := file.Stat()
			return nil, errors.Join(err, statErr)
		}
	}
	buffer := make([]byte, 32*1024)
	var payload []byte
	for int64(len(payload)) <= limit {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if stdin {
			if err := waitInputReadable(ctx, fd); err != nil {
				return nil, err
			}
		}
		chunk := buffer[:min(int64(len(buffer)), limit+1-int64(len(payload)))]
		var n int
		var err error
		if stdin {
			n, err = unix.Read(fd, chunk)
			if err == nil && n == 0 {
				err = io.EOF
			}
		} else {
			n, err = file.Read(chunk)
		}
		if n > 0 {
			payload = append(payload, chunk[:n]...)
		}
		if errors.Is(err, io.EOF) {
			return payload, ctx.Err()
		}
		if stdin && (errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR)) {
			continue
		}
		if err != nil {
			return nil, err
		}
	}
	return payload, ctx.Err()
}

func waitInputReadable(ctx context.Context, fd int) error {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := unix.Poll(fds, 50)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n > 0 {
			return ctx.Err()
		}
	}
}
