package keychain

import (
	"fmt"
	"os"
	"runtime"
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
