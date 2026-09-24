//go:build unix

package imapclient

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func testLockIdentity() sessionIdentity {
	return sessionIdentity{host: "imap.example.com", port: 993, username: "agent@example.com"}
}

func testLockConfig() transport.ImapConfig {
	return transport.ImapConfig{Host: "imap.example.com", Port: 993, Username: "agent@example.com"}
}

func TestMutationLockPathKeyedByCredentialFreeIdentity(t *testing.T) {
	dir := t.TempDir()
	key := testLockIdentity()
	first := mutationLockPath(dir, key)
	second := mutationLockPath(dir, sessionIdentity{host: key.host, port: key.port, username: "other@example.com"})
	if first == second {
		t.Fatal("different account identities share a lock path")
	}
	if filepath.Dir(first) != dir || !strings.HasPrefix(filepath.Base(first), "imap-mutations-") {
		t.Fatalf("lock path %q outside dir or unnamed", first)
	}
	if strings.Contains(first, key.username) || strings.Contains(first, key.host) {
		t.Fatalf("lock path %q leaks account identity", first)
	}
}

func TestMutationLockSerializesContentionWithinBoundedWait(t *testing.T) {
	dir := t.TempDir()
	key := testLockIdentity()
	release, err := acquireMutationLock(context.Background(), dir, key)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	previous := mutationLockMaxWait
	mutationLockMaxWait = 150 * time.Millisecond
	defer func() { mutationLockMaxWait = previous }()

	start := time.Now()
	_, err = acquireMutationLock(context.Background(), dir, key)
	var transportErr *transport.TransportError
	if !errors.As(err, &transportErr) || transportErr.Code != transport.CodeIMAPAccountBusy {
		t.Fatalf("contended acquire error = %v, want imap_account_busy", err)
	}
	if elapsed := time.Since(start); elapsed < mutationLockMaxWait || elapsed > 5*time.Second {
		t.Fatalf("bounded wait elapsed %s", elapsed)
	}

	release()
	release, err = acquireMutationLock(context.Background(), dir, key)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	release()
}

func TestMutationLockIsolatesDistinctAccounts(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireMutationLock(context.Background(), dir, testLockIdentity())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()
	other := sessionIdentity{host: "imap.example.com", port: 993, username: "other@example.com"}
	releaseOther, err := acquireMutationLock(context.Background(), dir, other)
	if err != nil {
		t.Fatalf("distinct account acquire: %v", err)
	}
	releaseOther()
}

func TestMutationLockHonorsCallerCancellation(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireMutationLock(context.Background(), dir, testLockIdentity())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = acquireMutationLock(ctx, dir, testLockIdentity())
	var transportErr *transport.TransportError
	if !errors.As(err, &transportErr) || transportErr.Code != transport.CodeIMAPTimeout {
		t.Fatalf("canceled acquire error = %v, want imap_timeout", err)
	}
}

func TestAcquireOperationTakesMutationLockOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := testLockConfig()
	release, err := acquireMutationLock(context.Background(), dir, sessionKey(cfg))
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	defer release()

	previous := mutationLockMaxWait
	mutationLockMaxWait = 120 * time.Millisecond
	defer func() { mutationLockMaxWait = previous }()

	client, err := NewWithOptions(ClientOptions{MutationLockDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.acquireOperation(context.Background(), cfg, mutation)
	var transportErr *transport.TransportError
	if !errors.As(err, &transportErr) || transportErr.Code != transport.CodeIMAPAccountBusy {
		t.Fatalf("mutation acquireOperation error = %v, want imap_account_busy", err)
	}
	// Shared reads never touch the lock: gate acquisition succeeds even while
	// the same account's mutation lock is held by another process.
	_, releaseRead, err := client.acquireOperation(context.Background(), cfg, independentRead)
	if err != nil || releaseRead == nil {
		t.Fatalf("independentRead acquisition = %v", err)
	}
	releaseRead()
}

func TestNewLeavesMutationLockDisabled(t *testing.T) {
	if New().mutationLockDir != "" {
		t.Fatal("New() enabled the cross-process lock")
	}
	client, err := NewWithOptions(ClientOptions{MutationLockDir: t.TempDir()})
	if err != nil || client.mutationLockDir == "" {
		t.Fatalf("NewWithOptions() = %v, %v", client, err)
	}
}

func TestMutationLockSetupFailureBlocksMutationBeforeNetworkAndAllowsReads(t *testing.T) {
	client := NewWithMutationLockSetupError(errors.New("config directory unavailable"))
	if got := transport.ErrorCode(client.MutationLockSetupError()); got != transport.CodeIMAPLockUnavailable {
		t.Fatalf("MutationLockSetupError code = %q, want %q", got, transport.CodeIMAPLockUnavailable)
	}
	_, release, err := client.acquireOperation(context.Background(), testLockConfig(), independentRead)
	if err != nil {
		t.Fatalf("read operation acquisition = %v, want it to remain available", err)
	}
	release()
	assertMutationLockFailureDoesNotConnect(t, client)
}

func TestMutationLockFilesystemFailureBlocksMutationBeforeNetwork(t *testing.T) {
	lockParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(lockParent, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewWithOptions(ClientOptions{MutationLockDir: lockParent})
	if err != nil {
		t.Fatal(err)
	}
	assertMutationLockFailureDoesNotConnect(t, client)
}

func assertMutationLockFailureDoesNotConnect(t *testing.T, client *Client) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close mutation-lock test listener: %v", err)
		}
	}()
	accepted := make(chan struct{}, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
			accepted <- struct{}{}
		}
	}()
	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = client.SetFlags(ctx, transport.ImapConfig{
		Host: host, Port: port, Username: "agent@example.com",
	}, "INBOX", 42, 7, []string{"\\Seen"}, nil)
	if got := transport.ErrorCode(err); got != transport.CodeIMAPLockUnavailable {
		t.Fatalf("SetFlags() error code = %q, error %v; want %q", got, err, transport.CodeIMAPLockUnavailable)
	}
	select {
	case <-accepted:
		t.Fatal("SetFlags() connected before the mutation lock was available")
	case <-time.After(100 * time.Millisecond):
	}
}
