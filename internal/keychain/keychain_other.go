//go:build !darwin || !cgo

package keychain

type osStore struct{}

func newOSStore() store { return osStore{} }

func (osStore) add(account, password string) error {
	return unsupportedErr()
}

func (osStore) update(account, password string) error {
	return unsupportedErr()
}

func (osStore) find(account string) (string, error) {
	return "", unsupportedErr()
}

func (osStore) remove(account string) error {
	return unsupportedErr()
}

func unsupportedErr() *KeychainError {
	return &KeychainError{Code: CodeUnsupported, Message: "keychain not supported on this platform"}
}
