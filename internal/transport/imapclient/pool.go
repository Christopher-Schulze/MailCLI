package imapclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"mailcli/internal/transport"
)

const (
	// DefaultMaxConnectionsPerAccount is the CLI-safe connection limit.
	DefaultMaxConnectionsPerAccount = 2
	// MaximumConnectionsPerAccount is the largest supported configured limit.
	MaximumConnectionsPerAccount = 8
)

// ClientOptions configures immutable IMAP client resource limits. A zero
// MaxConnectionsPerAccount selects DefaultMaxConnectionsPerAccount.
type ClientOptions struct {
	MaxConnectionsPerAccount int
}

// PoolStats is a point-in-time, credential-free view of pooled IMAP resources.
type PoolStats struct {
	MaxConnectionsPerAccount int
	AccountPools             int
	PooledSessions           int
	InUseSessions            int
	ConnectingSessions       int
	DedicatedConnections     int
}

// Client is a minimal IMAPv4 client that can mirror a message into the Sent
// mailbox and perform message mutations. It keeps a bounded authenticated
// session pool per host/port/username. LIST and STATUS may overlap on
// independent sessions. SEARCH and FETCH may overlap each other but exclude
// APPEND and message mutations for the same identity, preserving selected
// mailbox and UIDVALIDITY invariants. Different identities never share a pool
// or operation gate. The zero value is usable with default limits. Set
// TLSConfig before first use and do not copy a Client after first use. Close
// waits for acquired operations and leaves the Client reusable.
type Client struct {
	TLSConfig *tls.Config

	lifecycle sync.RWMutex
	mu        sync.Mutex
	pools     map[sessionIdentity]*accountSessionPool

	credentialGenerations    map[sessionIdentity]uint64
	maxConnectionsPerAccount int
}

type sessionIdentity struct {
	host     string
	port     int
	username string
}

type accountSessionPool struct {
	sessions   []*pooledSession
	connecting int
	dedicated  int
	operations int
	changed    chan struct{}
	stateGate  *semaphore.Weighted
}

// pooledSession wraps one authenticated connection with its selected mailbox
// state, so follow-up commands on the same mailbox can skip SELECT.
type pooledSession struct {
	sess                 *session
	key                  sessionIdentity
	credentialGeneration uint64
	selected             string
	uidvalidity          uint32
	cancelWatch          context.CancelFunc
	inUse                bool
	invalidated          bool
}

type operationClass uint8

const (
	independentRead operationClass = iota
	selectedStateRead
	mutation
)

// New returns a Client with conservative CLI-safe defaults.
func New() *Client {
	return &Client{maxConnectionsPerAccount: DefaultMaxConnectionsPerAccount}
}

// NewWithOptions returns a Client with validated immutable pool limits.
func NewWithOptions(options ClientOptions) (*Client, error) {
	limit, err := normalizeConnectionLimit(options.MaxConnectionsPerAccount)
	if err != nil {
		return nil, err
	}
	return &Client{maxConnectionsPerAccount: limit}, nil
}

func normalizeConnectionLimit(limit int) (int, error) {
	if limit == 0 {
		return DefaultMaxConnectionsPerAccount, nil
	}
	if limit < 1 || limit > MaximumConnectionsPerAccount {
		return 0, &transport.TransportError{
			Code: transport.CodeIMAPInvalidValue,
			Message: fmt.Sprintf(
				"IMAP max connections per account must be between 1 and %d",
				MaximumConnectionsPerAccount,
			),
		}
	}
	return limit, nil
}

func (c *Client) connectionLimit() int {
	if c.maxConnectionsPerAccount == 0 {
		return DefaultMaxConnectionsPerAccount
	}
	return c.maxConnectionsPerAccount
}

// PoolStats returns a credential-free resource snapshot across all identities.
func (c *Client) PoolStats() PoolStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	stats := PoolStats{
		MaxConnectionsPerAccount: c.connectionLimit(),
		AccountPools:             len(c.pools),
	}
	for _, pool := range c.pools {
		stats.PooledSessions += len(pool.sessions)
		stats.ConnectingSessions += pool.connecting
		stats.DedicatedConnections += pool.dedicated
		for _, pooled := range pool.sessions {
			if pooled.inUse {
				stats.InUseSessions++
			}
		}
	}
	return stats
}

func sessionKey(cfg transport.ImapConfig) sessionIdentity {
	return sessionIdentity{host: cfg.Host, port: cfg.Port, username: cfg.Username}
}

func nextCredentialGeneration(generation uint64) uint64 {
	generation++
	if generation == 0 {
		return 1
	}
	return generation
}

func (c *Client) poolLocked(key sessionIdentity) *accountSessionPool {
	if c.pools == nil {
		c.pools = make(map[sessionIdentity]*accountSessionPool)
	}
	pool := c.pools[key]
	if pool == nil {
		limit := c.connectionLimit()
		pool = &accountSessionPool{
			changed:   make(chan struct{}),
			stateGate: semaphore.NewWeighted(int64(limit)),
		}
		c.pools[key] = pool
	}
	return pool
}

func signalPoolLocked(pool *accountSessionPool) {
	close(pool.changed)
	pool.changed = make(chan struct{})
}

func (c *Client) removeEmptyPoolLocked(key sessionIdentity, pool *accountSessionPool) {
	if pool.operations == 0 && len(pool.sessions) == 0 && pool.connecting == 0 && pool.dedicated == 0 &&
		c.pools[key] == pool {
		delete(c.pools, key)
	}
}

func (c *Client) acquireOperation(
	ctx context.Context,
	cfg transport.ImapConfig,
	class operationClass,
) (*accountSessionPool, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP operation acquisition")
	}
	c.lifecycle.RLock()
	if err := ctx.Err(); err != nil {
		c.lifecycle.RUnlock()
		return nil, nil, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP operation acquisition")
	}
	key := sessionKey(cfg)
	c.mu.Lock()
	pool := c.poolLocked(key)
	pool.operations++
	c.mu.Unlock()

	weight := int64(0)
	switch class {
	case independentRead, selectedStateRead:
		weight = 1
	case mutation:
		weight = int64(c.connectionLimit())
	}
	if weight > 0 {
		if err := pool.stateGate.Acquire(ctx, weight); err != nil {
			c.mu.Lock()
			pool.operations--
			c.removeEmptyPoolLocked(key, pool)
			c.mu.Unlock()
			c.lifecycle.RUnlock()
			return nil, nil, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP selected-state acquisition")
		}
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			if weight > 0 {
				pool.stateGate.Release(weight)
			}
			c.mu.Lock()
			pool.operations--
			c.removeEmptyPoolLocked(key, pool)
			c.mu.Unlock()
			c.lifecycle.RUnlock()
		})
	}
	return pool, release, nil
}

// acquire keeps the historical helper for selected-state tests and callers.
func (c *Client) acquire(ctx context.Context, cfg transport.ImapConfig) (*pooledSession, func(), error) {
	return c.acquirePooled(ctx, cfg, selectedStateRead)
}

func (c *Client) acquireIndependent(ctx context.Context, cfg transport.ImapConfig) (*pooledSession, func(), error) {
	return c.acquirePooled(ctx, cfg, independentRead)
}

func (c *Client) acquireMutation(ctx context.Context, cfg transport.ImapConfig) (*pooledSession, func(), error) {
	return c.acquirePooled(ctx, cfg, mutation)
}

func (c *Client) acquirePooled(
	ctx context.Context,
	cfg transport.ImapConfig,
	class operationClass,
) (*pooledSession, func(), error) {
	pool, releaseOperation, err := c.acquireOperation(ctx, cfg, class)
	if err != nil {
		return nil, nil, err
	}
	pooled, err := c.borrowSession(ctx, cfg, pool)
	if err != nil {
		releaseOperation()
		return nil, nil, err
	}
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	pooled.cancelWatch = cancelWatch
	connection := pooled.sess.conn
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-watchCtx.Done():
		}
	}()
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			cancelWatch()
			c.releaseSession(ctx, pool, pooled)
			releaseOperation()
		})
	}
	return pooled, release, nil
}

func (c *Client) borrowSession(
	ctx context.Context,
	cfg transport.ImapConfig,
	pool *accountSessionPool,
) (*pooledSession, error) {
	key := sessionKey(cfg)
	for {
		if err := ctx.Err(); err != nil {
			return nil, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP pool acquisition")
		}
		var stale []*pooledSession
		c.mu.Lock()
		generation := c.credentialGenerations[key]
		for index := len(pool.sessions) - 1; index >= 0; index-- {
			pooled := pool.sessions[index]
			if pooled.inUse || !pooled.invalidated && pooled.credentialGeneration == generation {
				continue
			}
			pool.sessions = append(pool.sessions[:index], pool.sessions[index+1:]...)
			stale = append(stale, pooled)
		}
		if len(stale) > 0 {
			signalPoolLocked(pool)
			c.mu.Unlock()
			closePooledConnections(stale)
			continue
		}
		for _, pooled := range pool.sessions {
			if !pooled.inUse && !pooled.invalidated && pooled.credentialGeneration == generation {
				pooled.inUse = true
				c.mu.Unlock()
				return pooled, nil
			}
		}
		if len(pool.sessions)+pool.connecting+pool.dedicated < c.connectionLimit() {
			pool.connecting++
			c.mu.Unlock()
			sess, err := c.connect(ctx, cfg)
			c.mu.Lock()
			pool.connecting--
			if err == nil {
				pooled := &pooledSession{
					sess: sess, key: key, credentialGeneration: generation, inUse: true,
				}
				pool.sessions = append(pool.sessions, pooled)
				signalPoolLocked(pool)
				c.mu.Unlock()
				return pooled, nil
			}
			signalPoolLocked(pool)
			c.mu.Unlock()
			return nil, err
		}
		changed := pool.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, wrapIOError(ctx, ctx.Err(), transport.CodeIMAPTimeout, "IMAP pool acquisition")
		case <-changed:
		}
	}
}

func (c *Client) releaseSession(ctx context.Context, pool *accountSessionPool, pooled *pooledSession) {
	stale := false
	c.mu.Lock()
	pooled.inUse = false
	stale = ctx.Err() != nil || pooled.sess.dirty || pooled.invalidated ||
		c.credentialGenerations[pooled.key] != pooled.credentialGeneration
	if stale {
		removePooledSessionLocked(pool, pooled)
	}
	signalPoolLocked(pool)
	c.mu.Unlock()
	if stale {
		_ = pooled.sess.conn.Close()
	}
}

func removePooledSessionLocked(pool *accountSessionPool, target *pooledSession) {
	for index, pooled := range pool.sessions {
		if pooled == target {
			pool.sessions = append(pool.sessions[:index], pool.sessions[index+1:]...)
			return
		}
	}
}

func closePooledConnections(sessions []*pooledSession) {
	for _, pooled := range sessions {
		_ = pooled.sess.conn.Close()
	}
}

func (c *Client) acquireDedicated(ctx context.Context, cfg transport.ImapConfig) (func(), error) {
	pool, releaseOperation, err := c.acquireOperation(ctx, cfg, mutation)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			releaseOperation()
			return nil, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP pool acquisition")
		}
		var evicted *pooledSession
		c.mu.Lock()
		if len(pool.sessions)+pool.connecting+pool.dedicated < c.connectionLimit() {
			pool.dedicated++
			signalPoolLocked(pool)
			c.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					c.mu.Lock()
					pool.dedicated--
					signalPoolLocked(pool)
					c.mu.Unlock()
					releaseOperation()
				})
			}, nil
		}
		for _, pooled := range pool.sessions {
			if !pooled.inUse {
				evicted = pooled
				removePooledSessionLocked(pool, pooled)
				signalPoolLocked(pool)
				break
			}
		}
		if evicted != nil {
			c.mu.Unlock()
			_ = evicted.sess.conn.Close()
			continue
		}
		changed := pool.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			releaseOperation()
			return nil, wrapIOError(ctx, ctx.Err(), transport.CodeIMAPTimeout, "IMAP pool acquisition")
		case <-changed:
		}
	}
}

// InvalidateCredentials advances the non-secret credential generation for one
// IMAP identity. Idle sessions are closed immediately. In-flight sessions are
// discarded on release; password material is never retained in the pool key.
func (c *Client) InvalidateCredentials(cfg transport.ImapConfig) {
	key := sessionKey(cfg)
	var stale []*pooledSession
	c.mu.Lock()
	if c.credentialGenerations == nil {
		c.credentialGenerations = make(map[sessionIdentity]uint64)
	}
	c.credentialGenerations[key] = nextCredentialGeneration(c.credentialGenerations[key])
	pool := c.pools[key]
	if pool != nil {
		for index := len(pool.sessions) - 1; index >= 0; index-- {
			pooled := pool.sessions[index]
			pooled.invalidated = true
			if pooled.inUse {
				continue
			}
			pool.sessions = append(pool.sessions[:index], pool.sessions[index+1:]...)
			stale = append(stale, pooled)
		}
		signalPoolLocked(pool)
		c.removeEmptyPoolLocked(key, pool)
	}
	c.mu.Unlock()
	closePooledConnections(stale)
}

// Close waits for acquired operations, then logs out of and closes every
// pooled session. It is safe to call repeatedly; later operations reconnect.
func (c *Client) Close() error {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()

	c.mu.Lock()
	var pooled []*pooledSession
	for _, pool := range c.pools {
		pooled = append(pooled, pool.sessions...)
	}
	c.pools = nil
	c.mu.Unlock()
	var joined []error
	for _, session := range pooled {
		logoutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.doLogout(logoutCtx, session.sess, session.sess.nextTag())
		cancel()
		if err := session.sess.conn.Close(); err != nil {
			joined = append(joined, err)
		}
	}
	return errors.Join(joined...)
}
