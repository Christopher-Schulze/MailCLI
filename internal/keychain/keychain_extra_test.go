package keychain

import (
	"testing"

	"mailcli/internal/transport"
)

func TestLoadFailurePropagatesBackendError(t *testing.T) {
	expected := &KeychainError{Code: CodeLoadFailed, Message: "backend unavailable"}
	kc := newForTest(&failingFakeStore{findErr: expected})
	if _, err := kc.Load("any@example.com"); err != expected {
		t.Errorf("Load() error = %v, want %v", err, expected)
	}
}

func TestDeleteFailurePropagatesBackendError(t *testing.T) {
	expected := &KeychainError{Code: CodeDeleteFailed, Message: "backend unavailable"}
	kc := newForTest(&failingFakeStore{removeErr: expected})
	if err := kc.Delete("any@example.com"); err != expected {
		t.Errorf("Delete() error = %v, want %v", err, expected)
	}
}

func TestStoreUpdateFailurePropagates(t *testing.T) {
	expected := &KeychainError{Code: CodeStoreFailed, Message: "update rejected"}
	kc := newForTest(&failingFakeStore{addErr: ErrItemExists, updateErr: expected})
	if err := kc.Store("exists@example.com", "pw"); err != expected {
		t.Errorf("Store() error = %v, want %v", err, expected)
	}
}

func TestPlainErrorIsNotMatchedAsDuplicate(t *testing.T) {
	plain := &KeychainError{Code: CodeStoreFailed, Message: "locked"}
	kc := newForTest(&failingFakeStore{addErr: plain})
	err := kc.Store("locked@example.com", "pw")
	if err == nil {
		t.Fatal("Store() error = nil, want store failed")
	}
	if code := transport.ErrorCode(err); code != CodeStoreFailed {
		t.Errorf("ErrorCode() = %q, want %q", code, CodeStoreFailed)
	}
}
