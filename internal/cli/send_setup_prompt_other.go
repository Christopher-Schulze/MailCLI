//go:build !darwin

package cli

import (
	"context"
	"fmt"
	"os"
)

var osStdin = os.Stdin

func readPasswordLine(ctx context.Context, prompt string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("interactive password prompts are only supported on macOS")
}
