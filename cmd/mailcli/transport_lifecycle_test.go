package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInvocationTransportCloseIsIdempotentConcurrent(t *testing.T) {
	wantErr := errors.New("imap close failed")
	var closeCalls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	transport := &invocationTransport{
		closeResource: func() error {
			closeCalls.Add(1)
			close(entered)
			<-release
			return wantErr
		},
	}

	const callers = 32
	start := make(chan struct{})
	results := make(chan error, callers)
	var waitGroup sync.WaitGroup
	waitGroup.Add(callers)
	for range callers {
		go func() {
			defer waitGroup.Done()
			<-start
			results <- transport.Close()
		}()
	}
	close(start)
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("Close did not start its owned resource close")
	}
	close(release)

	done := make(chan struct{})
	go func() {
		waitGroup.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("concurrent Close calls did not return")
	}
	close(results)
	for err := range results {
		if !errors.Is(err, wantErr) {
			t.Errorf("Close() error = %v, want %v", err, wantErr)
		}
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("owned close calls = %d, want 1", got)
	}
	if err := transport.Close(); !errors.Is(err, wantErr) {
		t.Fatalf("repeated Close() error = %v, want %v", err, wantErr)
	}
}

func TestCloseInvocationResourcesOrdersTransportBeforeStore(t *testing.T) {
	var order []string
	transportErr := errors.New("transport close failed")
	storeErr := errors.New("store close failed")
	gotErr := closeInvocationResources(
		func() error {
			order = append(order, "transport")
			return transportErr
		},
		func() error {
			order = append(order, "store")
			return storeErr
		},
	)

	if got := strings.Join(order, ","); got != "transport,store" {
		t.Fatalf("close order = %q, want transport,store", got)
	}
	if !errors.Is(gotErr, transportErr) || !errors.Is(gotErr, storeErr) {
		t.Fatalf("joined close error = %v, want both errors", gotErr)
	}
}

func TestInvocationTransportCloseCycles(t *testing.T) {
	const cycles = 64
	var closeCalls atomic.Int32
	for range cycles {
		transport := &invocationTransport{
			closeResource: func() error {
				closeCalls.Add(1)
				return nil
			},
		}
		if err := transport.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		if err := transport.Close(); err != nil {
			t.Fatalf("repeated Close() error = %v", err)
		}
	}
	if got := closeCalls.Load(); got != cycles {
		t.Fatalf("owned close calls = %d, want %d", got, cycles)
	}
}

func TestRunWithFactoriesClosesNoServiceTransport(t *testing.T) {
	for _, test := range []struct {
		name     string
		args     []string
		wantCode int
	}{
		{name: "success becomes cleanup failure", args: []string{"version"}, wantCode: 1},
		{name: "existing failure is preserved", args: []string{"not-a-command"}, wantCode: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			closeErr := errors.New("transport close failed")
			var factoryCalls atomic.Int32
			var closeCalls atomic.Int32
			transport := &invocationTransport{
				closeResource: func() error {
					closeCalls.Add(1)
					return closeErr
				},
			}
			_, stderr, code := runWithArgsAndStderrUsing(t, append([]string{"mailcli"}, test.args...), func() int {
				return runWithFactories(nil, func() *invocationTransport {
					factoryCalls.Add(1)
					return transport
				})
			})
			if code != test.wantCode {
				t.Fatalf("run() = %d, want %d; stderr = %q", code, test.wantCode, stderr)
			}
			if got := factoryCalls.Load(); got != 1 {
				t.Fatalf("transport factory calls = %d, want 1", got)
			}
			if got := closeCalls.Load(); got != 1 {
				t.Fatalf("transport close calls = %d, want 1", got)
			}
			if !strings.Contains(stderr, "close Mail transport: transport close failed") {
				t.Fatalf("stderr = %q, want cleanup diagnostic", stderr)
			}
		})
	}
}

func TestRunWithFactoriesClosesTransportAfterStoreInitializationFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "Library", "Mail"), 0o700); err != nil {
		t.Fatalf("create isolated Mail root: %v", err)
	}

	closeErr := errors.New("transport close failed")
	var factoryCalls atomic.Int32
	var closeCalls atomic.Int32
	transport := &invocationTransport{
		closeResource: func() error {
			closeCalls.Add(1)
			return closeErr
		},
	}
	_, stderr, code := runWithArgsAndStderrUsing(t, []string{"mailcli", "accounts", "list"}, func() int {
		return runWithFactories(nil, func() *invocationTransport {
			factoryCalls.Add(1)
			return transport
		})
	})
	if code == 0 {
		t.Fatalf("run() = 0, want store initialization failure; stderr = %q", stderr)
	}
	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("transport factory calls = %d, want 1", got)
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("transport close calls = %d, want 1", got)
	}
	if !strings.Contains(stderr, "Mail Envelope Index") {
		t.Fatalf("stderr = %q, want local Mail store diagnostic", stderr)
	}
	if !strings.Contains(stderr, "close Mail resources: transport close failed") {
		t.Fatalf("stderr = %q, want cleanup diagnostic", stderr)
	}
}
