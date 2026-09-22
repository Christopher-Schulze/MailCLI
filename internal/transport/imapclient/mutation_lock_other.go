//go:build !unix

package imapclient

import "context"

// acquireMutationLock is a no-op on platforms without advisory flock
// support; cross-process politeness is best-effort and unavailable there.
func acquireMutationLock(_ context.Context, _ string, _ sessionIdentity) (func(), error) {
	return func() {}, nil
}
