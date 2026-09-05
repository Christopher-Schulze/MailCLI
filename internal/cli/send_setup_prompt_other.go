//go:build !darwin

package cli

import (
	"fmt"
	"os"
)

var osStdin = os.Stdin

func readPasswordLine(prompt string) (string, error) {
	return "", fmt.Errorf("interactive password prompts are only supported on macOS")
}
