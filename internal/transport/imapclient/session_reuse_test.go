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

// Guard: session operations must stay serialized per client.
func TestSessionOperationsSerialize(t *testing.T) {
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
