//go:build unix

package imapclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"mailcli/internal/transport"
)

// acquireMutationLock holds one per-account flock for the duration of an
// exclusive mutation. Contention within the bounded wait resolves into
// serialized operations; only a wait past mutationLockMaxWait or caller
// cancellation returns an error.
func acquireMutationLock(ctx context.Context, dir string, key sessionIdentity) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, mutationLockUnavailable(fmt.Errorf("create lock directory: %w", err))
	}
	stateInfo, err := os.Lstat(dir)
	if err != nil {
		return nil, mutationLockUnavailable(fmt.Errorf("inspect lock directory: %w", err))
	}
	if !stateInfo.IsDir() || stateInfo.Mode()&os.ModeSymlink != 0 {
		return nil, mutationLockUnavailable(fmt.Errorf("lock directory is not a real directory"))
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, mutationLockUnavailable(fmt.Errorf("secure lock directory: %w", err))
	}
	path := mutationLockPath(dir, key)
	descriptor, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, mutationLockUnavailable(fmt.Errorf("open account lock file: %w", err))
	}
	file := os.NewFile(uintptr(descriptor), path)
	if file == nil {
		_ = unix.Close(descriptor)
		return nil, mutationLockUnavailable(fmt.Errorf("wrap account lock file descriptor"))
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, mutationLockUnavailable(fmt.Errorf("secure account lock file: %w", err))
	}
	release := func() { _ = file.Close() }
	deadline := time.Now().Add(mutationLockMaxWait)
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			var once sync.Once
			return func() { once.Do(release) }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			release()
			return nil, mutationLockUnavailable(fmt.Errorf("acquire account lock: %w", err))
		}
		if err := ctx.Err(); err != nil {
			release()
			return nil, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP mutation lock acquisition")
		}
		if time.Now().After(deadline) {
			release()
			return nil, &transport.TransportError{
				Code: transport.CodeIMAPAccountBusy,
				Message: fmt.Sprintf(
					"IMAP account mutation lock wait exceeded %s; another MailCLI process still holds the account gate",
					mutationLockMaxWait,
				),
			}
		}
		select {
		case <-ctx.Done():
			release()
			return nil, wrapIOError(ctx, ctx.Err(), transport.CodeIMAPTimeout, "IMAP mutation lock acquisition")
		case <-time.After(mutationLockPollInterval):
		}
	}
}
