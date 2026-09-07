//go:build plan9 || (js && wasm)

package main

import (
	"fmt"
	"os"
)

func openPrivateKeyFile(_ string) (*os.File, os.FileInfo, error) {
	return nil, nil, fmt.Errorf("secure private-key opening is unavailable on this platform")
}
