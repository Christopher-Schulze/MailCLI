//go:build !unix

package imapclient

import (
	"context"
	"errors"
)

// acquireMutationLock fails closed on platforms without advisory flock
// support; callers can explicitly opt out by leaving MutationLockDir empty.
func acquireMutationLock(_ context.Context, dir string, key sessionIdentity) (func(), error) {
	return nil, probeMutationLock(dir, key)
}

func probeMutationLock(dir string, _ sessionIdentity) error {
	return mutationLockUnavailable(errors.New("advisory file locking is unsupported for lock directory " + dir + " on this platform"))
}
