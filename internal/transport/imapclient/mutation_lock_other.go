//go:build !unix

package imapclient

import (
	"context"
	"errors"
)

// acquireMutationLock fails closed on platforms without advisory flock
// support; callers can explicitly opt out by leaving MutationLockDir empty.
func acquireMutationLock(_ context.Context, _ string, _ sessionIdentity) (func(), error) {
	return nil, mutationLockUnavailable(errors.New("advisory file locking is unsupported on this platform"))
}
