//go:build darwin && cgo

package keychain

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"mailcli/internal/keychain/keychaintest"
	"mailcli/internal/transport"
)

// useScopedTestKeychain creates an isolated temporary keychain and pins all
// osStore SecItem queries to it for the duration of the test. Unlike
// SecKeychainSetDefault/SecKeychainSetSearchList — which mutate session-wide
// keychain state — the kSecUseKeychain and kSecMatchSearchList query
// attributes keep the redirection local to this process.
func useScopedTestKeychain(t *testing.T) {
	t.Helper()

	searchListBefore, err := exec.Command("security", "list-keychains", "-d", "user").Output()
	if err != nil {
		t.Fatalf("list-keychains before scope error = %v", err)
	}
	t.Cleanup(func() {
		searchListAfter, err := exec.Command("security", "list-keychains", "-d", "user").Output()
		if err != nil {
			t.Errorf("list-keychains after scope error = %v", err)
			return
		}
		if !bytes.Equal(searchListBefore, searchListAfter) {
			t.Errorf("user keychain search list changed: %q -> %q", searchListBefore, searchListAfter)
		}
	})

	path := filepath.Join(t.TempDir(), "scoped-test.keychain")
	keychain, searchList, err := keychaintest.Create(path, "mailcli-scoped-test")
	if err != nil {
		t.Fatalf("keychaintest.Create() error = %v", err)
	}

	previousKeychain, previousSearchList := scopedQueryKeychain, scopedQuerySearchList
	scopedQueryKeychain, scopedQuerySearchList = keychain, searchList
	t.Cleanup(func() {
		scopedQueryKeychain, scopedQuerySearchList = previousKeychain, previousSearchList
		keychaintest.Release(keychain, searchList)
	})
}

func TestOSStoreScopedKeychainRoundtrip(t *testing.T) {
	useScopedTestKeychain(t)

	credentials := New()
	account := fmt.Sprintf("mailcli-scoped-test-%d", os.Getpid())

	if err := credentials.Store(account, "pw-one"); err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	password, err := credentials.Load(account)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if password != "pw-one" {
		t.Fatalf("Load() = %q, want %q", password, "pw-one")
	}

	if err := credentials.Store(account, "pw-two"); err != nil {
		t.Fatalf("Store() update error = %v", err)
	}
	password, err = credentials.Load(account)
	if err != nil {
		t.Fatalf("Load() after update error = %v", err)
	}
	if password != "pw-two" {
		t.Fatalf("Load() after update = %q, want %q", password, "pw-two")
	}

	if err := credentials.Delete(account); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	_, err = credentials.Load(account)
	if code := transport.ErrorCode(err); code != CodeNotFound {
		t.Fatalf("Load() after Delete error code = %q, want %q", code, CodeNotFound)
	}
}

func TestScopedKeychainDoesNotWriteToDefaultDomain(t *testing.T) {
	useScopedTestKeychain(t)

	credentials := New()
	account := fmt.Sprintf("mailcli-scoped-test-%d", os.Getpid())
	if err := credentials.Store(account, "pw-one"); err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	t.Cleanup(func() {
		if err := credentials.Delete(account); err != nil {
			t.Errorf("Delete() cleanup error = %v", err)
		}
	})

	lookup := exec.Command("security", "find-generic-password", "-s", serviceName, "-a", account)
	if out, err := lookup.CombinedOutput(); err == nil {
		t.Fatalf("find-generic-password found scoped item in default domain: %s", out)
	}
}
