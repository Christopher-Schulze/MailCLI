package keychain

import (
	"errors"
	"fmt"

	"mailcli/internal/transport"
)

const serviceName = "mailcli-smtp"

// KeychainError is the typed error for all keychain failures.
type KeychainError struct {
	Code    string
	Message string
	Err     error
}

func (e *KeychainError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap returns the wrapped error.
func (e *KeychainError) Unwrap() error { return e.Err }

// ErrorCode returns the typed error code.
func (e *KeychainError) ErrorCode() string { return e.Code }

// Is reports whether this error matches the target keychain error by code.
func (e *KeychainError) Is(target error) bool {
	t, ok := target.(*KeychainError)
	return ok && e.Code == t.Code
}

// Typed error codes.
const (
	CodeNotFound     = "keychain_item_not_found"
	CodeDuplicate    = "keychain_item_duplicate"
	CodeUnsupported  = "keychain_unsupported"
	CodeStoreFailed  = "keychain_store_failed"
	CodeLoadFailed   = "keychain_load_failed"
	CodeDeleteFailed = "keychain_delete_failed"
)

// ErrItemExists is returned by a backend when an item already exists.
// It is matched by errors.Is via the shared ErrorCode implementation.
var ErrItemExists = &KeychainError{Code: CodeDuplicate, Message: "keychain item already exists"}

// store is the platform-independent keychain backend.
type store interface {
	add(account, password string) error
	update(account, password string) error
	find(account string) (string, error)
	remove(account string) error
}

// keychain implements transport.CredentialStore.
type keychain struct {
	service string
	backend store
}

// Load returns the stored password for the account.
func (k *keychain) Load(account string) (string, error) {
	return k.backend.find(account)
}

// Store stores the password for the account, replacing an existing item.
func (k *keychain) Store(account, password string) error {
	if err := k.backend.add(account, password); err != nil {
		if errors.Is(err, ErrItemExists) {
			return k.backend.update(account, password)
		}
		return err
	}
	return nil
}

// Delete removes the stored password for the account.
func (k *keychain) Delete(account string) error {
	return k.backend.remove(account)
}

// New returns a transport.CredentialStore backed by the platform keychain.
func New() transport.CredentialStore {
	return &keychain{
		service: serviceName,
		backend: newOSStore(),
	}
}

// newForTest returns a CredentialStore using the provided backend.
func newForTest(backend store) transport.CredentialStore {
	return &keychain{
		service: serviceName,
		backend: backend,
	}
}
