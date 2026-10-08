package keychain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestWrappingLogic(t *testing.T) {
	tests := []struct {
		name     string
		do       func(kc transport.CredentialStore) error
		wantCode string
	}{
		{
			name: "store and load roundtrip",
			do: func(kc transport.CredentialStore) error {
				const account = "roundtrip@example.com"
				const password = "secret-password"
				if err := kc.Store(account, password); err != nil {
					return err
				}
				got, err := kc.Load(account)
				if err != nil {
					return err
				}
				if got != password {
					return fmt.Errorf("Load() = %q, want %q", got, password)
				}
				return nil
			},
		},
		{
			name: "update on existing",
			do: func(kc transport.CredentialStore) error {
				const account = "update@example.com"
				if err := kc.Store(account, "old"); err != nil {
					return err
				}
				if err := kc.Store(account, "new"); err != nil {
					return err
				}
				got, err := kc.Load(account)
				if err != nil {
					return err
				}
				if got != "new" {
					return fmt.Errorf("Load() = %q, want %q", got, "new")
				}
				return nil
			},
		},
		{
			name: "delete missing returns not found",
			do: func(kc transport.CredentialStore) error {
				return kc.Delete("missing@example.com")
			},
			wantCode: CodeNotFound,
		},
		{
			name: "delete existing removes item",
			do: func(kc transport.CredentialStore) error {
				const account = "delete-lifecycle@example.com"
				const password = "delete-password"
				if err := kc.Store(account, password); err != nil {
					return err
				}
				if err := kc.Delete(account); err != nil {
					return err
				}
				_, err := kc.Load(account)
				return err
			},
			wantCode: CodeNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kc := newForTest(newFakeStore())
			err := tt.do(kc)
			if tt.wantCode != "" {
				if err == nil {
					t.Fatalf("do() error = nil, want %s", tt.wantCode)
				}
				if code := transport.ErrorCode(err); code != tt.wantCode {
					t.Fatalf("do() error code = %q, want %q", code, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("do() error = %v", err)
			}
		})
	}
}

func TestConstructorOnNonDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("skipping non-darwin constructor test on darwin")
	}

	kc := New()
	err := kc.Store("unsupported@example.com", "pw")
	if err == nil {
		t.Fatal("Store() error = nil, want unsupported error")
	}
	if code := transport.ErrorCode(err); code != CodeUnsupported {
		t.Fatalf("Store() error code = %q, want %q", code, CodeUnsupported)
	}
}

func TestKeychainErrorErrorWithInner(t *testing.T) {
	inner := fmt.Errorf("osstatus -25293")
	err := &KeychainError{Code: CodeLoadFailed, Message: "auth failed", Err: inner}
	got := err.Error()
	want := "keychain_load_failed: auth failed: osstatus -25293"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestKeychainErrorErrorWithoutInner(t *testing.T) {
	err := &KeychainError{Code: CodeNotFound, Message: "item not found"}
	got := err.Error()
	want := "keychain_item_not_found: item not found"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestKeychainErrorUnwrap(t *testing.T) {
	inner := fmt.Errorf("inner error")
	err := &KeychainError{Code: CodeStoreFailed, Message: "outer", Err: inner}
	if unwrapped := err.Unwrap(); unwrapped != inner {
		t.Errorf("Unwrap() = %v, want %v", unwrapped, inner)
	}
}

func TestKeychainErrorIsByCode(t *testing.T) {
	err1 := &KeychainError{Code: CodeDuplicate, Message: "first"}
	err2 := &KeychainError{Code: CodeDuplicate, Message: "second"}
	err3 := &KeychainError{Code: CodeNotFound, Message: "third"}
	if !err1.Is(err2) {
		t.Error("Is() = false for same code, want true")
	}
	if err1.Is(err3) {
		t.Error("Is() = true for different code, want false")
	}
}

func TestKeychainErrorIsNonKeychainError(t *testing.T) {
	err := &KeychainError{Code: CodeNotFound, Message: "not found"}
	if err.Is(fmt.Errorf("plain error")) {
		t.Error("Is() = true for non-KeychainError, want false")
	}
}

func TestKeychainErrorErrorCode(t *testing.T) {
	err := &KeychainError{Code: CodeDeleteFailed, Message: "delete failed"}
	if got := err.ErrorCode(); got != CodeDeleteFailed {
		t.Errorf("ErrorCode() = %q, want %q", got, CodeDeleteFailed)
	}
}

func TestStoreUpdateOnExistingItem(t *testing.T) {
	kc := newForTest(newFakeStore())
	account := "update-flow@example.com"
	if err := kc.Store(account, "first"); err != nil {
		t.Fatalf("Store(first) error = %v", err)
	}
	if err := kc.Store(account, "second"); err != nil {
		t.Fatalf("Store(second) error = %v", err)
	}
	got, err := kc.Load(account)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got != "second" {
		t.Errorf("Load() = %q, want second", got)
	}
}

func TestStoreUpdateOnMissingItemReturnsNotFound(t *testing.T) {
	kc := newForTest(&failingFakeStore{
		addErr:    ErrItemExists,
		updateErr: &KeychainError{Code: CodeNotFound, Message: "not found"},
	})
	err := kc.Store("missing@example.com", "pw")
	if err == nil {
		t.Fatal("Store() error = nil, want not found")
	}
	if code := transport.ErrorCode(err); code != CodeNotFound {
		t.Errorf("ErrorCode() = %q, want %q", code, CodeNotFound)
	}
}

func TestStoreAddFailureNonDuplicate(t *testing.T) {
	expected := &KeychainError{Code: CodeStoreFailed, Message: "keychain locked"}
	kc := newForTest(&failingFakeStore{addErr: expected})
	err := kc.Store("locked@example.com", "pw")
	if err == nil {
		t.Fatal("Store() error = nil, want store failed")
	}
	if err != expected {
		t.Errorf("Store() error = %v, want %v", err, expected)
	}
}

func TestLoadMissingReturnsNotFound(t *testing.T) {
	kc := newForTest(newFakeStore())
	_, err := kc.Load("never-stored@example.com")
	if err == nil {
		t.Fatal("Load() error = nil, want not found")
	}
	if code := transport.ErrorCode(err); code != CodeNotFound {
		t.Errorf("ErrorCode() = %q, want %q", code, CodeNotFound)
	}
}

func TestDeleteExistingSucceeds(t *testing.T) {
	kc := newForTest(newFakeStore())
	account := "delete-ok@example.com"
	if err := kc.Store(account, "pw"); err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	if err := kc.Delete(account); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	_, err := kc.Load(account)
	if err == nil {
		t.Fatal("Load() after delete = nil, want not found")
	}
}

func TestNewReturnsCredentialStore(t *testing.T) {
	kc := New()
	if kc == nil {
		t.Fatal("New() = nil, want non-nil CredentialStore")
	}
	if err := kc.Store("\x00invalid", "secret"); transport.ErrorCode(err) != CodeInvalidIdentifier {
		t.Fatalf("New().Store(NUL identifier) error = %v, want %s", err, CodeInvalidIdentifier)
	}
	if _, err := kc.Load("\x00invalid"); transport.ErrorCode(err) != CodeInvalidIdentifier {
		t.Fatalf("New().Load(NUL identifier) error = %v, want %s", err, CodeInvalidIdentifier)
	}
	if err := kc.Delete("\x00invalid"); transport.ErrorCode(err) != CodeInvalidIdentifier {
		t.Fatalf("New().Delete(NUL identifier) error = %v, want %s", err, CodeInvalidIdentifier)
	}
}

func TestCredentialStoreRejectsNULIdentifiers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		account string
	}{
		{name: "beginning", account: "\x00alice@example.com"},
		{name: "middle", account: "alice\x00@example.com"},
		{name: "end", account: "alice@example.com\x00"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			backend := newFakeStore()
			kc := newForTest(backend)
			if err := kc.Store(test.account, "secret"); transport.ErrorCode(err) != CodeInvalidIdentifier {
				t.Fatalf("Store() error = %v, want %s", err, CodeInvalidIdentifier)
			}
			if _, err := kc.Load(test.account); transport.ErrorCode(err) != CodeInvalidIdentifier {
				t.Fatalf("Load() error = %v, want %s", err, CodeInvalidIdentifier)
			}
			if err := kc.Delete(test.account); transport.ErrorCode(err) != CodeInvalidIdentifier {
				t.Fatalf("Delete() error = %v, want %s", err, CodeInvalidIdentifier)
			}
			if len(backend.items) != 0 {
				t.Fatalf("backend items = %#v, want empty", backend.items)
			}
		})
	}
}

func TestCredentialStoreAcceptsUnicodeIdentifier(t *testing.T) {
	t.Parallel()
	const account = "müller@example.com"
	kc := newForTest(newFakeStore())
	if err := kc.Store(account, "secret"); err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	password, err := kc.Load(account)
	if err != nil || password != "secret" {
		t.Fatalf("Load() = %q, %v; want secret, nil", password, err)
	}
}

// failingFakeStore is a store backend that returns configurable errors for
// testing error-handling paths in the keychain wrapper.
type failingFakeStore struct {
	addErr    error
	updateErr error
	findErr   error
	removeErr error
}

func (f *failingFakeStore) add(account, password string) error {
	return f.addErr
}

func (f *failingFakeStore) update(account, password string) error {
	return f.updateErr
}

func (f *failingFakeStore) find(account string) (string, error) {
	return "", f.findErr
}

func (f *failingFakeStore) remove(account string) error {
	return f.removeErr
}

func TestLiveKeychain(t *testing.T) {
	if os.Getenv("MAILCLI_KEYCHAIN_LIVE") != "1" {
		t.Skip("skipping live keychain test; set MAILCLI_KEYCHAIN_LIVE=1 to enable")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("live keychain test requires darwin")
	}

	kc := New()
	account := fmt.Sprintf("mailcli-live-test-%d-%d@example.com", os.Getpid(), time.Now().UnixNano())
	password := "throwaway-password"

	if err := kc.Store(account, password); err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	t.Cleanup(func() { _ = kc.Delete(account) })

	got, err := kc.Load(account)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got != password {
		t.Fatalf("Load() = %q, want %q", got, password)
	}

	if err := kc.Delete(account); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := kc.Load(account); err == nil {
		t.Fatal("Load() after Delete error = nil, want not-found")
	} else if code := transport.ErrorCode(err); code != CodeNotFound {
		t.Fatalf("Load() after Delete error code = %q, want %q", code, CodeNotFound)
	}
}

// promptStore blocks every find until release closes, like a Keychain read
// waiting on the macOS approval prompt.
type promptStore struct {
	*fakeStore
	started chan struct{}
	release chan struct{}
}

func (p *promptStore) find(account string) (string, error) {
	close(p.started)
	<-p.release
	return p.fakeStore.find(account)
}

func TestLoadContext(t *testing.T) {
	missing := &KeychainError{Code: CodeNotFound, Message: "item not found"}
	for _, test := range []struct {
		name     string
		stored   bool
		end      func(context.Context) (context.Context, context.CancelFunc)
		blocked  bool
		password string
		want     error
	}{
		{name: "completed read", stored: true, password: "secret"},
		{name: "completed missing item", want: missing},
		{name: "canceled while prompt is open", stored: true, blocked: true, end: context.WithCancel, want: context.Canceled},
		{name: "deadline while prompt is open", stored: true, blocked: true, end: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, 20*time.Millisecond)
		}, want: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &promptStore{fakeStore: newFakeStore(), started: make(chan struct{}), release: make(chan struct{})}
			if test.stored {
				backend.items["user@icloud.com"] = "secret"
			}
			if !test.blocked {
				close(backend.release)
			} else {
				t.Cleanup(func() { close(backend.release) })
			}
			ctx, cancel := context.Background(), context.CancelFunc(func() {})
			if test.end != nil {
				ctx, cancel = test.end(ctx)
			}
			defer cancel()
			if test.want == context.Canceled {
				go func() {
					<-backend.started
					cancel()
				}()
			}
			password, err := LoadContext(ctx, newForTest(backend), "user@icloud.com")
			if password != test.password || !errors.Is(err, test.want) || test.want == nil && err != nil {
				t.Fatalf("LoadContext() = %q, %v; want %q, %v", password, err, test.password, test.want)
			}
			if test.blocked && !strings.Contains(err.Error(), "approve the Keychain prompt") {
				t.Fatalf("pending prompt error lacks approval guidance: %v", err)
			}
		})
	}
}

func TestLoadContextAnnouncesPendingApprovalOnce(t *testing.T) {
	for _, test := range []struct {
		name    string
		delay   time.Duration
		blocked time.Duration
		notices int
	}{
		{name: "fast read stays silent", delay: time.Hour, notices: 0},
		{name: "blocked read announces once", delay: time.Millisecond, blocked: 50 * time.Millisecond, notices: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			previousDelay, previousOutput := keychainWaitNotice, keychainNoticeOutput
			t.Cleanup(func() { keychainWaitNotice, keychainNoticeOutput = previousDelay, previousOutput })
			var notices strings.Builder
			keychainWaitNotice, keychainNoticeOutput = test.delay, &notices
			backend := &promptStore{fakeStore: newFakeStore(), started: make(chan struct{}), release: make(chan struct{})}
			backend.items["user@icloud.com"] = "secret"
			go func() {
				<-backend.started
				time.Sleep(test.blocked)
				close(backend.release)
			}()
			password, err := LoadContext(context.Background(), newForTest(backend), "user@icloud.com")
			if password != "secret" || err != nil {
				t.Fatalf("LoadContext() = %q, %v", password, err)
			}
			text := notices.String()
			if strings.Count(text, "waiting for macOS Keychain approval") != test.notices || test.notices > 0 && !strings.Contains(text, "user@icloud.com") {
				t.Fatalf("notices = %q; want %d naming the account", text, test.notices)
			}
		})
	}
}

func TestLoadContextEndedBeforeReadSkipsBackend(t *testing.T) {
	backend := &promptStore{fakeStore: newFakeStore(), started: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LoadContext(ctx, newForTest(backend), "user@icloud.com"); !errors.Is(err, context.Canceled) {
		t.Fatalf("LoadContext() error = %v; want context.Canceled", err)
	}
	select {
	case <-backend.started:
		t.Fatal("ended context still opened a Keychain read")
	default:
	}
}

type fakeStore struct {
	items map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{items: make(map[string]string)}
}

func (f *fakeStore) add(account, password string) error {
	if _, exists := f.items[account]; exists {
		return ErrItemExists
	}
	f.items[account] = password
	return nil
}

func (f *fakeStore) update(account, password string) error {
	if _, exists := f.items[account]; !exists {
		return &KeychainError{Code: CodeNotFound, Message: "item not found"}
	}
	f.items[account] = password
	return nil
}

func (f *fakeStore) find(account string) (string, error) {
	p, exists := f.items[account]
	if !exists {
		return "", &KeychainError{Code: CodeNotFound, Message: "item not found"}
	}
	return p, nil
}

func (f *fakeStore) remove(account string) error {
	if _, exists := f.items[account]; !exists {
		return &KeychainError{Code: CodeNotFound, Message: "item not found"}
	}
	delete(f.items, account)
	return nil
}
