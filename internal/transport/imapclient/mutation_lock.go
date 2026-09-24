package imapclient

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"time"

	"mailcli/internal/transport"
)

// Cross-invocation mutation politeness. The per-process exclusive account
// gate serializes IMAP mutations inside one Client; parallel MailCLI
// invocations otherwise race the same provider connection and rate limits,
// and contention then surfaces as transport failures indistinguishable from
// real faults. When MutationLockDir is configured, every exclusive mutation
// (STORE, COPY, EXPUNGE, APPEND) additionally takes one per-account lock file
// so concurrent processes serialize. Failure to establish the lock rejects
// the mutation instead of silently continuing without serialization.
//
// The lock is keyed by the credential-free host/port/username session
// identity — the same identity that keys the connection pool. Reads and
// hydration never take it. Explicitly omitting MutationLockDir leaves only
// process-local ordering; callers must keep that opt-out separate from an
// attempted but unavailable cross-process lock.

var mutationLockMaxWait = 30 * time.Second

const mutationLockPollInterval = 50 * time.Millisecond

func mutationLockUnavailable(cause error) error {
	return &transport.TransportError{
		Code:    transport.CodeIMAPLockUnavailable,
		Message: "required cross-process IMAP mutation lock is unavailable",
		Err:     cause,
	}
}

func mutationLockPath(dir string, key sessionIdentity) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%s", key.host, key.port, key.username))
	return filepath.Join(dir, fmt.Sprintf("imap-mutations-%x.lock", sum[:16]))
}
