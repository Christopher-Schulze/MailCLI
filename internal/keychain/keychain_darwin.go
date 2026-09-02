//go:build darwin && cgo

package keychain

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <stdlib.h>
#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>

static void keychainDictSet(CFMutableDictionaryRef dict, CFStringRef key, CFTypeRef value) {
	CFDictionarySetValue(dict, key, value);
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

type osStore struct{}

func newOSStore() store { return osStore{} }

func (osStore) add(account, password string) error {
	serviceCF, err := cfString(serviceName)
	if err != nil {
		return err
	}
	defer C.CFRelease(C.CFTypeRef(serviceCF))

	accountCF, err := cfString(account)
	if err != nil {
		return err
	}
	defer C.CFRelease(C.CFTypeRef(accountCF))

	passwordData, err := cfData(password)
	if err != nil {
		return err
	}
	defer C.CFRelease(C.CFTypeRef(passwordData))

	query := C.CFDictionaryCreateMutable(C.kCFAllocatorDefault, 0, &C.kCFTypeDictionaryKeyCallBacks, &C.kCFTypeDictionaryValueCallBacks)
	if query == 0 {
		return &KeychainError{Code: CodeStoreFailed, Message: "failed to create keychain query"}
	}
	defer C.CFRelease(C.CFTypeRef(query))

	C.keychainDictSet(query, C.kSecClass, C.CFTypeRef(C.kSecClassGenericPassword))
	C.keychainDictSet(query, C.kSecAttrService, C.CFTypeRef(serviceCF))
	C.keychainDictSet(query, C.kSecAttrAccount, C.CFTypeRef(accountCF))
	C.keychainDictSet(query, C.kSecValueData, C.CFTypeRef(passwordData))

	status := C.SecItemAdd(C.CFDictionaryRef(query), nil)
	if status == C.errSecSuccess {
		return nil
	}
	if status == C.errSecDuplicateItem {
		return &KeychainError{Code: CodeDuplicate, Message: "item already exists", Err: fmt.Errorf("SecItemAdd status %d", int(status))}
	}
	return &KeychainError{Code: CodeStoreFailed, Message: fmt.Sprintf("SecItemAdd status %d", int(status))}
}

func (osStore) update(account, password string) error {
	serviceCF, err := cfString(serviceName)
	if err != nil {
		return err
	}
	defer C.CFRelease(C.CFTypeRef(serviceCF))

	accountCF, err := cfString(account)
	if err != nil {
		return err
	}
	defer C.CFRelease(C.CFTypeRef(accountCF))

	passwordData, err := cfData(password)
	if err != nil {
		return err
	}
	defer C.CFRelease(C.CFTypeRef(passwordData))

	query := C.CFDictionaryCreateMutable(C.kCFAllocatorDefault, 0, &C.kCFTypeDictionaryKeyCallBacks, &C.kCFTypeDictionaryValueCallBacks)
	if query == 0 {
		return &KeychainError{Code: CodeStoreFailed, Message: "failed to create keychain query"}
	}
	defer C.CFRelease(C.CFTypeRef(query))

	C.keychainDictSet(query, C.kSecClass, C.CFTypeRef(C.kSecClassGenericPassword))
	C.keychainDictSet(query, C.kSecAttrService, C.CFTypeRef(serviceCF))
	C.keychainDictSet(query, C.kSecAttrAccount, C.CFTypeRef(accountCF))

	attrsToUpdate := C.CFDictionaryCreateMutable(C.kCFAllocatorDefault, 0, &C.kCFTypeDictionaryKeyCallBacks, &C.kCFTypeDictionaryValueCallBacks)
	if attrsToUpdate == 0 {
		return &KeychainError{Code: CodeStoreFailed, Message: "failed to create keychain update dictionary"}
	}
	defer C.CFRelease(C.CFTypeRef(attrsToUpdate))

	C.keychainDictSet(attrsToUpdate, C.kSecValueData, C.CFTypeRef(passwordData))

	status := C.SecItemUpdate(C.CFDictionaryRef(query), C.CFDictionaryRef(attrsToUpdate))
	if status == C.errSecSuccess {
		return nil
	}
	if status == C.errSecItemNotFound {
		return &KeychainError{Code: CodeNotFound, Message: "keychain item not found"}
	}
	return &KeychainError{Code: CodeStoreFailed, Message: fmt.Sprintf("SecItemUpdate status %d", int(status))}
}

func (osStore) find(account string) (string, error) {
	serviceCF, err := cfString(serviceName)
	if err != nil {
		return "", err
	}
	defer C.CFRelease(C.CFTypeRef(serviceCF))

	accountCF, err := cfString(account)
	if err != nil {
		return "", err
	}
	defer C.CFRelease(C.CFTypeRef(accountCF))

	query := C.CFDictionaryCreateMutable(C.kCFAllocatorDefault, 0, &C.kCFTypeDictionaryKeyCallBacks, &C.kCFTypeDictionaryValueCallBacks)
	if query == 0 {
		return "", &KeychainError{Code: CodeLoadFailed, Message: "failed to create keychain query"}
	}
	defer C.CFRelease(C.CFTypeRef(query))

	C.keychainDictSet(query, C.kSecClass, C.CFTypeRef(C.kSecClassGenericPassword))
	C.keychainDictSet(query, C.kSecAttrService, C.CFTypeRef(serviceCF))
	C.keychainDictSet(query, C.kSecAttrAccount, C.CFTypeRef(accountCF))
	C.keychainDictSet(query, C.kSecMatchLimit, C.CFTypeRef(C.kSecMatchLimitOne))
	C.keychainDictSet(query, C.kSecReturnData, C.CFTypeRef(C.kCFBooleanTrue))

	var result C.CFTypeRef
	status := C.SecItemCopyMatching(C.CFDictionaryRef(query), &result)
	if status == C.errSecItemNotFound {
		return "", &KeychainError{Code: CodeNotFound, Message: "keychain item not found"}
	}
	if status != C.errSecSuccess {
		return "", &KeychainError{Code: CodeLoadFailed, Message: fmt.Sprintf("SecItemCopyMatching status %d", int(status))}
	}
	if result == 0 {
		return "", &KeychainError{Code: CodeLoadFailed, Message: "SecItemCopyMatching returned no data"}
	}
	defer C.CFRelease(C.CFTypeRef(result))

	data := C.CFDataRef(result)
	n := C.CFDataGetLength(data)
	if n <= 0 {
		return "", nil
	}

	ptr := C.CFDataGetBytePtr(data)
	if ptr == nil {
		return "", nil
	}
	return string(C.GoBytes(unsafe.Pointer(ptr), C.int(n))), nil
}

func (osStore) remove(account string) error {
	serviceCF, err := cfString(serviceName)
	if err != nil {
		return err
	}
	defer C.CFRelease(C.CFTypeRef(serviceCF))

	accountCF, err := cfString(account)
	if err != nil {
		return err
	}
	defer C.CFRelease(C.CFTypeRef(accountCF))

	query := C.CFDictionaryCreateMutable(C.kCFAllocatorDefault, 0, &C.kCFTypeDictionaryKeyCallBacks, &C.kCFTypeDictionaryValueCallBacks)
	if query == 0 {
		return &KeychainError{Code: CodeDeleteFailed, Message: "failed to create keychain query"}
	}
	defer C.CFRelease(C.CFTypeRef(query))

	C.keychainDictSet(query, C.kSecClass, C.CFTypeRef(C.kSecClassGenericPassword))
	C.keychainDictSet(query, C.kSecAttrService, C.CFTypeRef(serviceCF))
	C.keychainDictSet(query, C.kSecAttrAccount, C.CFTypeRef(accountCF))

	status := C.SecItemDelete(C.CFDictionaryRef(query))
	if status == C.errSecItemNotFound {
		return &KeychainError{Code: CodeNotFound, Message: "keychain item not found"}
	}
	if status != C.errSecSuccess {
		return &KeychainError{Code: CodeDeleteFailed, Message: fmt.Sprintf("SecItemDelete status %d", int(status))}
	}
	return nil
}

func cfString(s string) (C.CFStringRef, error) {
	cstr := C.CString(s)
	defer C.free(unsafe.Pointer(cstr))

	cf := C.CFStringCreateWithCString(C.kCFAllocatorDefault, cstr, C.kCFStringEncodingUTF8)
	if cf == 0 {
		return 0, &KeychainError{Code: CodeStoreFailed, Message: "failed to create CFString"}
	}
	return cf, nil
}

func cfData(s string) (C.CFDataRef, error) {
	b := C.CBytes([]byte(s))
	if b == nil {
		return 0, &KeychainError{Code: CodeStoreFailed, Message: "failed to allocate data buffer"}
	}
	defer C.free(b)

	data := C.CFDataCreate(C.kCFAllocatorDefault, (*C.UInt8)(b), C.CFIndex(len(s)))
	if data == 0 {
		return 0, &KeychainError{Code: CodeStoreFailed, Message: "failed to create CFData"}
	}
	return data, nil
}
