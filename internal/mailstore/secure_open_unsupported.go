//go:build plan9 || (js && wasm)

package mailstore

import "os"

func ensureSecureOpenSupported() error {
	return operationError(
		"unsupported_platform",
		"secure Mail store file opening is unavailable on this platform",
	)
}

func openRegularPath(_ *os.File, _ string, _ string) (*os.File, os.FileInfo, error) {
	return nil, nil, operationError(
		"unsupported_platform",
		"secure Mail store file opening is unavailable on this platform",
	)
}

func openDirectoryPath(_ *os.File, _ string, _ string) (*os.File, os.FileInfo, error) {
	return nil, nil, operationError(
		"unsupported_platform",
		"secure Mail store directory opening is unavailable on this platform",
	)
}
