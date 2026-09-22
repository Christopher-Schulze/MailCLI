//go:build darwin && cgo

package keychain

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <stdlib.h>
#include <string.h>
#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>

static void keychainDictSet(CFMutableDictionaryRef dict, CFStringRef key, CFTypeRef value) {
	CFDictionarySetValue(dict, key, value);
}

static void wipeFree(void *ptr, size_t length) {
	if (ptr != NULL) {
		memset(ptr, 0, length);
		free(ptr);
	}
}
*/
import "C"

import (
	"unsafe"
)

type osStore struct{}

func newOSStore() store { return osStore{} }

// scopedQueryKeychain and scopedQuerySearchList pin SecItem queries to an
// isolated keychain. They stay nil in production; darwin tests point them at
// a dedicated temporary keychain so real SecItem calls never touch the login
// keychain or mutate session-wide keychain defaults. They hold a
// C.SecKeychainRef and a C.CFArrayRef respectively.
var scopedQueryKeychain unsafe.Pointer
var scopedQuerySearchList unsafe.Pointer

// scopeQueryTarget pins the keychain an add writes to, when a test scope is
// active. It is a no-op in production.
func scopeQueryTarget(query C.CFMutableDictionaryRef) {
	if scopedQueryKeychain != nil {
		C.keychainDictSet(query, C.kSecUseKeychain, C.CFTypeRef(scopedQueryKeychain))
	}
}

// scopeQuerySearch pins the search list of a query, when a test scope is
// active. It is a no-op in production.
func scopeQuerySearch(query C.CFMutableDictionaryRef) {
	if scopedQuerySearchList != nil {
		C.keychainDictSet(query, C.kSecMatchSearchList, C.CFTypeRef(scopedQuerySearchList))
	}
}

func (osStore) add(account, password string) error {
	if err := validateIdentifier(account); err != nil {
		return err
	}
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
	scopeQueryTarget(query)

	return mapAddStatus(int(C.SecItemAdd(C.CFDictionaryRef(query), nil)))
}

func (osStore) update(account, password string) error {
	if err := validateIdentifier(account); err != nil {
		return err
	}
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
	scopeQuerySearch(query)

	attrsToUpdate := C.CFDictionaryCreateMutable(C.kCFAllocatorDefault, 0, &C.kCFTypeDictionaryKeyCallBacks, &C.kCFTypeDictionaryValueCallBacks)
	if attrsToUpdate == 0 {
		return &KeychainError{Code: CodeStoreFailed, Message: "failed to create keychain update dictionary"}
	}
	defer C.CFRelease(C.CFTypeRef(attrsToUpdate))

	C.keychainDictSet(attrsToUpdate, C.kSecValueData, C.CFTypeRef(passwordData))

	return mapUpdateStatus(int(C.SecItemUpdate(C.CFDictionaryRef(query), C.CFDictionaryRef(attrsToUpdate))))
}

func (osStore) find(account string) (string, error) {
	if err := validateIdentifier(account); err != nil {
		return "", err
	}
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
	scopeQuerySearch(query)

	var result C.CFTypeRef
	if err := mapFindStatus(int(C.SecItemCopyMatching(C.CFDictionaryRef(query), &result))); err != nil {
		return "", err
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
	if err := validateIdentifier(account); err != nil {
		return err
	}
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
	scopeQuerySearch(query)

	return mapDeleteStatus(int(C.SecItemDelete(C.CFDictionaryRef(query))))
}

func cfString(s string) (C.CFStringRef, error) {
	if err := validateIdentifier(s); err != nil {
		return 0, err
	}
	cstr := C.CString(s)
	defer C.wipeFree(unsafe.Pointer(cstr), C.size_t(len(s)+1))

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
	defer C.wipeFree(b, C.size_t(len(s)))

	data := C.CFDataCreate(C.kCFAllocatorDefault, (*C.UInt8)(b), C.CFIndex(len(s)))
	if data == 0 {
		return 0, &KeychainError{Code: CodeStoreFailed, Message: "failed to create CFData"}
	}
	return data, nil
}

func releaseCFString(ref C.CFStringRef) {
	C.CFRelease(C.CFTypeRef(ref))
}

func releaseCFData(ref C.CFDataRef) {
	C.CFRelease(C.CFTypeRef(ref))
}
