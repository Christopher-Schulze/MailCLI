//go:build darwin && cgo

// Package keychaintest creates isolated temporary keychains for deterministic
// darwin tests. Only test files import it; production binaries never link it.
package keychaintest

/*
#cgo CFLAGS: -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <stdlib.h>
#include <string.h>
#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>

static void keychaintestWipeFree(void *ptr, size_t length) {
	if (ptr != NULL) {
		memset(ptr, 0, length);
		free(ptr);
	}
}

static CFArrayRef keychaintestSearchList(SecKeychainRef keychain) {
	return CFArrayCreate(kCFAllocatorDefault, (const void **)&keychain, 1, &kCFTypeArrayCallBacks);
}

static void* keychaintestKeychainPtr(SecKeychainRef keychain) {
	return (void*)keychain;
}

static void* keychaintestArrayPtr(CFArrayRef array) {
	return (void*)array;
}

static void keychaintestRelease(void* ref) {
	if (ref != NULL) {
		CFRelease((CFTypeRef)ref);
	}
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// Create creates an unlocked temporary keychain at path and returns the
// SecKeychainRef plus a one-element CFArrayRef suitable for
// kSecMatchSearchList. The returned references stay valid until Release.
func Create(path, password string) (keychain, searchList unsafe.Pointer, err error) {
	cPath := C.CString(path)
	cPassword := C.CString(password)
	defer func() {
		C.keychaintestWipeFree(unsafe.Pointer(cPath), C.size_t(len(path)+1))
		C.keychaintestWipeFree(unsafe.Pointer(cPassword), C.size_t(len(password)+1))
	}()

	var keychainRef C.SecKeychainRef
	status := C.SecKeychainCreate(
		cPath,
		C.UInt32(len(password)),
		unsafe.Pointer(cPassword),
		C.Boolean(0),
		C.SecAccessRef(0),
		&keychainRef,
	)
	if status != 0 {
		return nil, nil, fmt.Errorf("SecKeychainCreate status %d", int(status))
	}
	status = C.SecKeychainUnlock(
		keychainRef,
		C.UInt32(len(password)),
		unsafe.Pointer(cPassword),
		C.Boolean(1),
	)
	if status != 0 {
		C.CFRelease(C.CFTypeRef(keychainRef))
		return nil, nil, fmt.Errorf("SecKeychainUnlock status %d", int(status))
	}

	array := C.keychaintestSearchList(keychainRef)
	if array == 0 {
		C.CFRelease(C.CFTypeRef(keychainRef))
		return nil, nil, fmt.Errorf("CFArrayCreate returned nil")
	}
	return C.keychaintestKeychainPtr(keychainRef), C.keychaintestArrayPtr(array), nil
}

// Release drops the references returned by Create. The keychain file itself
// is removed by the caller's temporary-directory cleanup.
func Release(keychain, searchList unsafe.Pointer) {
	C.keychaintestRelease(searchList)
	C.keychaintestRelease(keychain)
}
