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
		return mutationLockDegraded(), nil
	}
	stateInfo, err := os.Lstat(dir)
	if err != nil || !stateInfo.IsDir() || stateInfo.Mode()&os.ModeSymlink != 0 {
		return mutationLockDegraded(), nil
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return mutationLockDegraded(), nil
	}
	path := mutationLockPath(dir, key)
	descriptor, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return mutationLockDegraded(), nil
	}
	file := os.NewFile(uintptr(descriptor), path)
	if file == nil {
		_ = unix.Close(descriptor)
		return mutationLockDegraded(), nil
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return mutationLockDegraded(), nil
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
			return mutationLockDegraded(), nil
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

// mutationLockDegraded continues without a lock: filesystem failures of a
// politeness mechanism never reduce mutation availability.
func mutationLockDegraded() func() { return func() {} }
