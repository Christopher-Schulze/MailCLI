//go:build plan9 || (js && wasm)

package main

import (
	"fmt"
	"os"
)

func openExclusiveOutput(_ string, _ os.FileMode) (exclusiveOutput, error) {
	return nil, fmt.Errorf("secure release output handling is unavailable on this platform")
}
