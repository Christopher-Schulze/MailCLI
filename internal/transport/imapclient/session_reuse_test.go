package imapclient

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

func sessionTestConfig(host string, port int) transport.ImapConfig {
	return transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "secret"}
}

// Repeated operations on one client reuse a single pooled connection.
func TestSessionReuseSingleConnection(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:        true,
		otherMboxes:   []string{"INBOX"},
		searchMatchID: "<found@example.com>",
	})
	host, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := atoiPositive(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	cfg := sessionTestConfig(host, port)
	client := Client{TLSConfig: &tls.Config{InsecureSkipVerify: true}}
	ctx := context.Background()

	if _, err := client.ListMailboxes(ctx, cfg); err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}
	if _, _, _, err := client.SearchUID(ctx, cfg, "INBOX", "<found@example.com>"); err != nil {
		t.Fatalf("SearchUID: %v", err)
	}
	if _, err := client.SetFlags(ctx, cfg, "INBOX", 42, 12345, []string{"\\Seen"}, nil); err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	if _, err := client.CheckStatus(ctx, cfg, "INBOX"); err != nil {
		t.Fatalf("CheckStatus: %v", err)
	}
	if _, err := client.FetchMessage(ctx, cfg, "INBOX", 42, 12345, mail.MaximumRawSourceBytes); err != nil {
		t.Fatalf("FetchMessage: %v", err)
	}
	if got := srv.ConnectionCount(); got != 1 {
		t.Fatalf("connection count = %d, want 1", got)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// DeleteMessage resolves trash and moves the message on one pooled connection.
func TestSessionReuseDeleteSingleConnection(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:        true,
		trashMboxes:   []string{"Trash"},
		otherMboxes:   []string{"INBOX"},
		searchMatchID: "<found@example.com>",
		searchUID:     42,
	})
	host, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := atoiPositive(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	cfg := sessionTestConfig(host, port)
	client := Client{TLSConfig: &tls.Config{InsecureSkipVerify: true}}
	ctx := context.Background()

	if _, err := client.DeleteMessage(ctx, cfg, "INBOX", 42, 12345); err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}
	if got := srv.ConnectionCount(); got != 1 {
		t.Fatalf("connection count = %d, want 1", got)
	}
	_ = client.Close()
}

func TestAcquireRejectsCanceledContextBeforeConnection(t *testing.T) {
	client := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := client.acquire(ctx, transport.ImapConfig{})
	if code := transport.ErrorCode(err); code != transport.CodeIMAPTimeout {
		t.Fatalf("acquire() code = %s, want %s: %v", code, transport.CodeIMAPTimeout, err)
	}
}

func TestAcquireCancellationDuringContention(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:      true,
		otherMboxes: []string{"INBOX"},
	})
	client, cfg := newFakeClient(t, srv)
	client.maxConnectionsPerAccount = 1
	_, release, err := client.acquire(context.Background(), cfg)
	if err != nil {
		t.Fatalf("initial acquire: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err = client.acquire(ctx, cfg)
	if code := transport.ErrorCode(err); code != transport.CodeIMAPTimeout {
		t.Fatalf("contended acquire() code = %s, want %s: %v", code, transport.CodeIMAPTimeout, err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("contended acquire() took %v after its deadline", elapsed)
	}
	release()
}

func TestClientOptionsValidateConnectionLimit(t *testing.T) {
	tests := []struct {
		name      string
		limit     int
		wantLimit int
		wantCode  string
	}{
		{name: "default", wantLimit: DefaultMaxConnectionsPerAccount},
		{name: "one", limit: 1, wantLimit: 1},
		{name: "maximum", limit: MaximumConnectionsPerAccount, wantLimit: MaximumConnectionsPerAccount},
		{name: "negative", limit: -1, wantCode: transport.CodeIMAPInvalidValue},
		{name: "above maximum", limit: MaximumConnectionsPerAccount + 1, wantCode: transport.CodeIMAPInvalidValue},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, err := NewWithOptions(ClientOptions{MaxConnectionsPerAccount: test.limit})
			if test.wantCode != "" {
				if transport.ErrorCode(err) != test.wantCode {
					t.Fatalf("NewWithOptions() error = %v, want %s", err, test.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewWithOptions() error = %v", err)
			}
			if got := client.PoolStats().MaxConnectionsPerAccount; got != test.wantLimit {
				t.Fatalf("max connections = %d, want %d", got, test.wantLimit)
			}
		})
	}
}

func TestFailedAuthenticationDoesNotRetainEmptyAccountPool(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{authOK: false})
	client, cfg := newFakeClient(t, srv)
	if _, err := client.ListMailboxes(context.Background(), cfg); transport.ErrorCode(err) != transport.CodeIMAPAuthFailed {
		t.Fatalf("ListMailboxes() error = %v, want %s", err, transport.CodeIMAPAuthFailed)
	}
	stats := client.PoolStats()
	if stats.AccountPools != 0 || stats.PooledSessions != 0 || stats.ConnectingSessions != 0 {
		t.Fatalf("pool stats after failed authentication = %+v", stats)
	}
}

func TestIndependentReadsUseBoundedConcurrentSessions(t *testing.T) {
	started := make(chan struct{}, DefaultMaxConnectionsPerAccount)
	continueReads := make(chan struct{})
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX"},
		statusStartedEvents: started, statusContinue: continueReads,
	})
	client, cfg := newFakeClient(t, srv)
	results := make(chan error, DefaultMaxConnectionsPerAccount)
	for range DefaultMaxConnectionsPerAccount {
		go func() {
			_, err := client.CheckStatus(context.Background(), cfg, "INBOX")
			results <- err
		}()
	}
	for range DefaultMaxConnectionsPerAccount {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("concurrent STATUS did not reach the server")
		}
	}
	stats := client.PoolStats()
	if stats.PooledSessions != DefaultMaxConnectionsPerAccount ||
		stats.InUseSessions != DefaultMaxConnectionsPerAccount ||
		stats.PooledSessions > stats.MaxConnectionsPerAccount {
		t.Fatalf("active pool stats = %+v", stats)
	}
	close(continueReads)
	for range DefaultMaxConnectionsPerAccount {
		if err := <-results; err != nil {
			t.Fatalf("CheckStatus() error = %v", err)
		}
	}
	if got := srv.MaxActiveConnections(); got != DefaultMaxConnectionsPerAccount {
		t.Fatalf("maximum active connections = %d, want %d", got, DefaultMaxConnectionsPerAccount)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if stats := client.PoolStats(); stats.AccountPools != 0 || stats.PooledSessions != 0 {
		t.Fatalf("pool stats after Close = %+v", stats)
	}
}

func TestDifferentAccountIdentitiesUseIndependentPools(t *testing.T) {
	started := make(chan struct{}, 2)
	continueReads := make(chan struct{})
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX"},
		statusStartedEvents: started, statusContinue: continueReads,
	})
	client, first := newFakeClient(t, srv)
	client.maxConnectionsPerAccount = 1
	second := first
	second.Username = "second-user"
	results := make(chan error, 2)
	for _, cfg := range []transport.ImapConfig{first, second} {
		go func() {
			_, err := client.CheckStatus(context.Background(), cfg, "INBOX")
			results <- err
		}()
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("different account identities did not reach the server concurrently")
		}
	}
	stats := client.PoolStats()
	if stats.AccountPools != 2 || stats.PooledSessions != 2 || stats.MaxConnectionsPerAccount != 1 {
		t.Fatalf("independent account pool stats = %+v", stats)
	}
	close(continueReads)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("CheckStatus() error = %v", err)
		}
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestMutationExcludesSelectedStateRead(t *testing.T) {
	searchStarted := make(chan struct{}, 1)
	searchContinue := make(chan struct{})
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX"}, searchMatchID: "<found@example.com>",
		searchStartedEvents: searchStarted, searchContinue: searchContinue,
	})
	client, cfg := newFakeClient(t, srv)
	readResult := make(chan error, 1)
	go func() {
		_, _, _, err := client.SearchUID(context.Background(), cfg, "INBOX", "<found@example.com>")
		readResult <- err
	}()
	select {
	case <-searchStarted:
	case <-time.After(time.Second):
		t.Fatal("selected-state read did not reach the server")
	}
	mutationCtx, cancelMutation := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelMutation()
	_, err := client.SetFlags(mutationCtx, cfg, "INBOX", 42, 12345, []string{"\\Seen"}, nil)
	if code := transport.ErrorCode(err); code != transport.CodeIMAPTimeout {
		t.Fatalf("SetFlags() error = %v, want %s", err, transport.CodeIMAPTimeout)
	}
	if srv.StoreCalled() {
		t.Fatal("mutation reached the server while selected-state read held the account gate")
	}
	close(searchContinue)
	if err := <-readResult; err != nil {
		t.Fatalf("SearchUID() error = %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestPoolExhaustionHonorsCancellation(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX"}})
	client, cfg := newFakeClient(t, srv)
	client.maxConnectionsPerAccount = 1
	key := sessionKey(cfg)
	client.mu.Lock()
	pool := client.poolLocked(key)
	client.mu.Unlock()
	pooled, err := client.borrowSession(context.Background(), cfg, pool)
	if err != nil {
		t.Fatalf("initial borrowSession() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = client.borrowSession(ctx, cfg, pool)
	if code := transport.ErrorCode(err); code != transport.CodeIMAPTimeout {
		t.Fatalf("contended borrowSession() error = %v, want %s", err, transport.CodeIMAPTimeout)
	}
	client.releaseSession(context.Background(), pool, pooled)
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestAcquireCancellationAfterAcquisitionReleasesSession(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:      true,
		otherMboxes: []string{"INBOX"},
	})
	client, cfg := newFakeClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	_, release, err := client.acquire(ctx, cfg)
	if err != nil {
		t.Fatalf("initial acquire: %v", err)
	}
	cancel()
	release()

	nextCtx, nextCancel := context.WithTimeout(context.Background(), time.Second)
	defer nextCancel()
	_, nextRelease, err := client.acquire(nextCtx, cfg)
	if err != nil {
		t.Fatalf("acquire after canceled session: %v", err)
	}
	nextRelease()
	if got := srv.ConnectionCount(); got != 2 {
		t.Fatalf("connection count after canceled session = %d, want 2", got)
	}
}

func TestCredentialInvalidationBeforeAcquireUsesNewGeneration(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX"}})
	client, cfg := newFakeClient(t, srv)
	client.InvalidateCredentials(cfg)

	ps, release, err := client.acquire(context.Background(), cfg)
	if err != nil {
		t.Fatalf("acquire after credential invalidation: %v", err)
	}
	if ps.credentialGeneration != 1 {
		t.Fatalf("credential generation = %d, want 1", ps.credentialGeneration)
	}
	release()
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestCredentialInvalidationReauthenticatesIdleSession(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX"}})
	client, cfg := newFakeClient(t, srv)
	ctx := context.Background()

	if _, err := client.ListMailboxes(ctx, cfg); err != nil {
		t.Fatalf("initial ListMailboxes: %v", err)
	}
	client.InvalidateCredentials(cfg)
	if _, err := client.ListMailboxes(ctx, cfg); err != nil {
		t.Fatalf("ListMailboxes after rotation: %v", err)
	}
	if got := srv.ConnectionCount(); got != 2 {
		t.Fatalf("connection count after idle rotation = %d, want 2", got)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestCredentialInvalidationDuringOperationDefersClose(t *testing.T) {
	searchStarted := make(chan struct{})
	searchContinue := make(chan struct{})
	srv := newFakeServer(t, fakeServerConfig{
		authOK:         true,
		otherMboxes:    []string{"INBOX"},
		searchMatchID:  "<found@example.com>",
		searchStarted:  searchStarted,
		searchContinue: searchContinue,
	})
	client, cfg := newFakeClient(t, srv)
	result := make(chan error, 1)
	go func() {
		_, _, _, err := client.SearchUID(context.Background(), cfg, "INBOX", "<found@example.com>")
		result <- err
	}()

	select {
	case <-searchStarted:
	case <-time.After(time.Second):
		t.Fatal("SearchUID did not reach the server")
	}
	client.InvalidateCredentials(cfg)
	close(searchContinue)
	if err := <-result; err != nil {
		t.Fatalf("SearchUID after in-flight rotation: %v", err)
	}
	if got := srv.ConnectionCount(); got != 1 {
		t.Fatalf("connection count during in-flight rotation = %d, want 1", got)
	}
	if _, err := client.ListMailboxes(context.Background(), cfg); err != nil {
		t.Fatalf("ListMailboxes after in-flight rotation: %v", err)
	}
	if got := srv.ConnectionCount(); got != 2 {
		t.Fatalf("connection count after in-flight rotation release = %d, want 2", got)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestCredentialInvalidationAuthenticationFailureDoesNotReuseSession(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:       true,
		authPassword: "old",
		otherMboxes:  []string{"INBOX"},
	})
	client, cfg := newFakeClient(t, srv)
	cfg.Password = "old"
	if _, err := client.ListMailboxes(context.Background(), cfg); err != nil {
		t.Fatalf("initial ListMailboxes: %v", err)
	}

	srv.SetAuthPassword("new")
	client.InvalidateCredentials(cfg)
	wrong := cfg
	wrong.Password = "wrong"
	if _, err := client.ListMailboxes(context.Background(), wrong); transport.ErrorCode(err) != transport.CodeIMAPAuthFailed {
		t.Fatalf("ListMailboxes with wrong rotated credential = %v, want %s", err, transport.CodeIMAPAuthFailed)
	}
	current := cfg
	current.Password = "new"
	if _, err := client.ListMailboxes(context.Background(), current); err != nil {
		t.Fatalf("ListMailboxes with current credential: %v", err)
	}
	if got := srv.ConnectionCount(); got != 3 {
		t.Fatalf("connection count after authentication failure = %d, want 3", got)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestCredentialInvalidationCloseIsIdempotent(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX"}})
	client, cfg := newFakeClient(t, srv)
	if _, err := client.ListMailboxes(context.Background(), cfg); err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}
	client.InvalidateCredentials(cfg)
	if err := client.Close(); err != nil {
		t.Fatalf("Close after invalidation: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close after invalidation: %v", err)
	}
}

func TestSessionKeyExcludesPassword(t *testing.T) {
	first := transport.ImapConfig{Host: "imap.example.com", Port: 993, Username: "user", Password: "old"}
	second := first
	second.Password = "new"
	if sessionKey(first) != sessionKey(second) {
		t.Fatal("session key changed with password; password material must not enter pool identity")
	}
}

func TestSessionKeyHasUnambiguousFields(t *testing.T) {
	first := transport.ImapConfig{Host: "a", Port: 1, Username: "b:2/c"}
	second := transport.ImapConfig{Host: "a:1/b", Port: 2, Username: "c"}
	if sessionKey(first) == sessionKey(second) {
		t.Fatal("distinct host, port, and username tuples must not share a pool identity")
	}
}

func TestCloseWaitsForAcquiredSession(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:      true,
		otherMboxes: []string{"INBOX"},
	})
	client, cfg := newFakeClient(t, srv)
	_, release, err := client.acquire(context.Background(), cfg)
	if err != nil {
		t.Fatalf("initial acquire: %v", err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	closeDone := make(chan error, 1)
	go func() { closeDone <- client.Close() }()
	select {
	case err := <-closeDone:
		release()
		released = true
		t.Fatalf("Close returned before session release: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	release()
	released = true
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not complete after session release")
	}
}

// sync --check style usage: N mailbox checks over one connection.
func TestSessionReuseCheckStatusSingleConnection(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:      true,
		otherMboxes: []string{"INBOX", "Archive", "Work"},
	})
	host, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := atoiPositive(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	cfg := sessionTestConfig(host, port)
	client := Client{TLSConfig: &tls.Config{InsecureSkipVerify: true}}
	ctx := context.Background()

	for _, mailbox := range []string{"INBOX", "Archive", "Work"} {
		if _, err := client.CheckStatus(ctx, cfg, mailbox); err != nil {
			t.Fatalf("CheckStatus(%s): %v", mailbox, err)
		}
	}
	if got := srv.ConnectionCount(); got != 1 {
		t.Fatalf("connection count = %d, want 1", got)
	}
	_ = client.Close()
}

// A failed SELECT (protocol NO) keeps the authenticated session: the cached
// selected state is invalidated and the next command re-SELECTs on the same
// connection.
func TestFailedSelectKeepsSession(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:        true,
		otherMboxes:   []string{"INBOX"},
		selectFailBox: "Broken",
	})
	host, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := atoiPositive(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	cfg := sessionTestConfig(host, port)
	client := Client{TLSConfig: &tls.Config{InsecureSkipVerify: true}}
	ctx := context.Background()

	if _, err := client.ListMailboxes(ctx, cfg); err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}
	if _, err := client.SetFlags(ctx, cfg, "Broken", 42, 12345, []string{"\\Seen"}, nil); err == nil {
		t.Fatal("SetFlags on broken mailbox: expected error, got nil")
	}
	if _, err := client.CheckStatus(ctx, cfg, "INBOX"); err != nil {
		t.Fatalf("CheckStatus after failed select: %v", err)
	}
	if got := srv.ConnectionCount(); got != 1 {
		t.Fatalf("connection count = %d, want 1 (session survives protocol NO)", got)
	}
	_ = client.Close()
}

// An IO-level failure (server drops the connection mid-command) marks the
// session dirty: the operation fails, and the next operation reconnects.
func TestSessionDiscardedAfterIOError(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:            true,
		otherMboxes:       []string{"INBOX"},
		dropAfterCommands: 3,
	})
	host, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := atoiPositive(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	cfg := sessionTestConfig(host, port)
	client := Client{TLSConfig: &tls.Config{InsecureSkipVerify: true}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := client.ListMailboxes(ctx, cfg); err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}
	if _, err := client.CheckStatus(ctx, cfg, "INBOX"); err == nil {
		t.Fatal("CheckStatus on dropped connection: expected error, got nil")
	}
	if _, err := client.CheckStatus(ctx, cfg, "INBOX"); err != nil {
		t.Fatalf("CheckStatus after reconnect: %v", err)
	}
	if got := srv.ConnectionCount(); got != 2 {
		t.Fatalf("connection count = %d, want 2 (reconnect after IO failure)", got)
	}
	_ = client.Close()
}

// SELECT reuse: the second command on the same mailbox skips the SELECT.
func TestEnsureSelectedSkipsSelect(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:        true,
		otherMboxes:   []string{"INBOX"},
		searchMatchID: "<found@example.com>",
	})
	host, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := atoiPositive(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	cfg := sessionTestConfig(host, port)
	client := Client{TLSConfig: &tls.Config{InsecureSkipVerify: true}}
	ctx := context.Background()

	if _, _, _, err := client.SearchUID(ctx, cfg, "INBOX", "<found@example.com>"); err != nil {
		t.Fatalf("SearchUID: %v", err)
	}
	if _, _, _, err := client.SearchUID(ctx, cfg, "INBOX", "<found@example.com>"); err != nil {
		t.Fatalf("SearchUID second call: %v", err)
	}
	_ = client.Close()
}

func atoiPositive(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, errInvalidPort
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errInvalidPort
		}
		n = n*10 + int(s[i]-'0')
	}
	if n <= 0 {
		return 0, errInvalidPort
	}
	return n, nil
}

type portError struct{}

func (portError) Error() string { return "invalid port" }

var errInvalidPort = portError{}

func TestConcurrentStatusOperationsComplete(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:      true,
		otherMboxes: []string{"INBOX"},
	})
	host, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := atoiPositive(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	cfg := sessionTestConfig(host, port)
	client := Client{TLSConfig: &tls.Config{InsecureSkipVerify: true}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := client.CheckStatus(ctx, cfg, "INBOX")
		done <- err
	}()
	if _, err := client.CheckStatus(ctx, cfg, "INBOX"); err != nil {
		t.Fatalf("CheckStatus main: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("CheckStatus concurrent: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("concurrent CheckStatus deadlocked")
	}
	_ = client.Close()
}

func BenchmarkIndependentStatusConcurrency(b *testing.B) {
	tests := []struct {
		name  string
		limit int
	}{
		{name: "serialized", limit: 1},
		{name: "two sessions", limit: 2},
	}
	for _, test := range tests {
		b.Run(test.name, func(b *testing.B) {
			srv := newFakeServer(b, fakeServerConfig{
				authOK: true, otherMboxes: []string{"INBOX"}, statusDelay: 2 * time.Millisecond,
			})
			host, portValue, err := net.SplitHostPort(srv.Addr())
			if err != nil {
				b.Fatalf("split host port: %v", err)
			}
			port, err := atoiPositive(portValue)
			if err != nil {
				b.Fatalf("parse port: %v", err)
			}
			client, err := NewWithOptions(ClientOptions{MaxConnectionsPerAccount: test.limit})
			if err != nil {
				b.Fatalf("NewWithOptions() error = %v", err)
			}
			client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
			cfg := sessionTestConfig(host, port)
			b.Cleanup(func() {
				if err := client.Close(); err != nil {
					b.Errorf("Close() error = %v", err)
				}
			})
			b.ResetTimer()
			for b.Loop() {
				results := make(chan error, 2)
				for range 2 {
					go func() {
						_, err := client.CheckStatus(context.Background(), cfg, "INBOX")
						results <- err
					}()
				}
				for range 2 {
					if err := <-results; err != nil {
						b.Fatalf("CheckStatus() error = %v", err)
					}
				}
			}
			b.StopTimer()
			if got := srv.MaxActiveConnections(); got > test.limit {
				b.Fatalf("maximum active connections = %d, limit = %d", got, test.limit)
			}
		})
	}
}
