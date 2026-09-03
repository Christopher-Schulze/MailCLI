package keychain

import "fmt"

const (
	secSuccess       = 0
	secItemNotFound  = -25300
	secDuplicateItem = -25299
)

func mapAddStatus(status int) error {
	switch status {
	case secSuccess:
		return nil
	case secDuplicateItem:
		return &KeychainError{
			Code:    CodeDuplicate,
			Message: "item already exists",
			Err:     fmt.Errorf("SecItemAdd status %d", status),
		}
	default:
		return &KeychainError{Code: CodeStoreFailed, Message: fmt.Sprintf("SecItemAdd status %d", status)}
	}
}

func mapUpdateStatus(status int) error {
	switch status {
	case secSuccess:
		return nil
	case secItemNotFound:
		return &KeychainError{Code: CodeNotFound, Message: "keychain item not found"}
	default:
		return &KeychainError{Code: CodeStoreFailed, Message: fmt.Sprintf("SecItemUpdate status %d", status)}
	}
}

func mapFindStatus(status int) error {
	switch status {
	case secSuccess:
		return nil
	case secItemNotFound:
		return &KeychainError{Code: CodeNotFound, Message: "keychain item not found"}
	default:
		return &KeychainError{Code: CodeLoadFailed, Message: fmt.Sprintf("SecItemCopyMatching status %d", status)}
	}
}

func mapDeleteStatus(status int) error {
	switch status {
	case secSuccess:
		return nil
	case secItemNotFound:
		return &KeychainError{Code: CodeNotFound, Message: "keychain item not found"}
	default:
		return &KeychainError{Code: CodeDeleteFailed, Message: fmt.Sprintf("SecItemDelete status %d", status)}
	}
}
