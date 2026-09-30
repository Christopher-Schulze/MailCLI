//go:build unix

package imapclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func testLockIdentity() sessionIdentity {
	return sessionIdentity{host: "imap.example.com", port: 993, username: "agent@example.com"}
}

func TestMutationLockPreflightProcessHelper(t *testing.T) {
	dir := os.Getenv("MAILCLI_PREFLIGHT_LOCK_DIRECTORY")
	if dir == "" {
		return
	}
	release, err := acquireMutationLock(context.Background(), dir, testLockIdentity())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := fmt.Fprintln(os.Stdout, "lock-held"); err != nil {
		t.Fatal(err)
	}
	var signal [1]byte
	if _, err := io.ReadFull(os.Stdin, signal[:]); err != nil {
		t.Fatal(err)
	}
}

func TestMutationLockPreflightDoesNotWaitForOtherProcess(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMutationLockPreflightProcessHelper$")
	command.Env = append(os.Environ(), "MAILCLI_PREFLIGHT_LOCK_DIRECTORY="+dir)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := input.Write([]byte{1}); err != nil {
			t.Error(err)
		}
		if err := input.Close(); err != nil {
			t.Error(err)
		}
		if _, err := io.Copy(io.Discard, output); err != nil {
			t.Error(err)
		}
		if err := command.Wait(); err != nil {
			t.Error(err)
		}
	})
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "lock-held\n" {
		t.Fatalf("lock handshake: %q, %v", line, err)
	}
	client, err := NewWithOptions(ClientOptions{MutationLockDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = client.CheckMutationLock(ctx, testLockConfig())
	elapsed := time.Since(start)
	if err != nil || elapsed >= 10*time.Millisecond {
		t.Fatalf("contended preflight: %v after %s, want success below 10ms", err, elapsed)
	}
	t.Logf("contended preflight duration: %s", elapsed)
	previous := mutationLockMaxWait
	mutationLockMaxWait = 20 * time.Millisecond
	defer func() { mutationLockMaxWait = previous }()
	if release, err := acquireMutationLock(ctx, dir, testLockIdentity()); release != nil || transport.ErrorCode(err) != transport.CodeIMAPAccountBusy {
		if release != nil {
			release()
		}
		t.Fatalf("probe altered held lock: %v", err)
	}
}

func TestMutationLockPreflightRejectsUnsafeSetupWithRemedy(t *testing.T) {
	for _, kind := range []string{"directory occupied", "lock symlink", "lock hardlink", "lock directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "locks")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := mutationLockPath(dir, testLockIdentity())
			foreign := filepath.Join(t.TempDir(), "foreign")
			if err := os.WriteFile(foreign, []byte("retained"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(foreign, 0o644); err != nil {
				t.Fatal(err)
			}
			original, statErr := os.Lstat(foreign)
			if statErr != nil || !original.Mode().IsRegular() || original.Mode().Perm() != 0o644 {
				t.Fatalf("foreign fixture mode = %v, error = %v", original, statErr)
			}
			if kind == "directory occupied" {
				dir = foreign
			} else if kind == "lock symlink" {
				if err := os.Symlink(foreign, path); err != nil {
					t.Fatal(err)
				}
			} else if kind == "lock hardlink" {
				if err := os.Link(foreign, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			err := probeMutationLock(dir, testLockIdentity())
			if transport.ErrorCode(err) != transport.CodeIMAPLockUnavailable || !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "MAILCLI_IMAP_MUTATION_LOCK=off") {
				t.Fatalf("setup error lacks path/remedy: %v", err)
			}
			payload, err := os.ReadFile(foreign)
			info, statErr := os.Lstat(foreign)
			if err != nil || statErr != nil || string(payload) != "retained" ||
				!os.SameFile(original, info) || info.Mode().Perm() != 0o644 {
				t.Fatalf("foreign object changed: %v, %v", err, statErr)
			}
		})
	}
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
	if !errors.As(err, &transportErr) || transportErr.Code != transport.CodeIMAPCanceled || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquire error = %v, want imap_canceled with original cause", err)
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

type mutationLockCountingDialer struct {
	dialer *tls.Dialer
	calls  atomic.Int64
}

func (d *mutationLockCountingDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d.calls.Add(1)
	return d.dialer.DialContext(ctx, network, address)
}

func assertMutationLockFailureDoesNotConnect(t *testing.T, client *Client) {
	t.Helper()
	server := newFakeServer(t, fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX"}})
	host, portText, err := net.SplitHostPort(server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	dialer := &mutationLockCountingDialer{dialer: &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: imapDialTimeout}, Config: client.tlsConfig(host),
	}}
	client.dialer = dialer
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close mutation-lock test client: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "agent@example.com"}
	_, err = client.SetFlags(ctx, cfg, "INBOX", 42, 7, []string{"\\Seen"}, nil)
	if got := transport.ErrorCode(err); got != transport.CodeIMAPLockUnavailable {
		t.Fatalf("SetFlags() error code = %q, error %v; want %q", got, err, transport.CodeIMAPLockUnavailable)
	}
	if calls := dialer.calls.Load(); calls != 0 {
		t.Fatalf("SetFlags() made %d dial attempts before the mutation lock was available", calls)
	}
	mailboxes, err := client.ListMailboxes(ctx, cfg)
	if err != nil || len(mailboxes) != 1 || mailboxes[0].Name != "INBOX" {
		t.Fatalf("ListMailboxes() after mutation refusal = %+v, %v", mailboxes, err)
	}
	if calls := dialer.calls.Load(); calls != 1 {
		t.Fatalf("ListMailboxes() made %d dial attempts, want one real TLS connection", calls)
	}
}
