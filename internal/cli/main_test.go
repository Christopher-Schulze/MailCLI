package cli

import (
	"fmt"
	"os"
	"testing"
)

// TestMain gives the package a private TMPDIR: failed draft edits retain their
// candidate by design, and tests must not leave those in the user's TMPDIR.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "mailcli-cli-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create test TMPDIR:", err)
		os.Exit(1)
	}
	if err := os.Setenv("TMPDIR", root); err != nil {
		fmt.Fprintln(os.Stderr, "set test TMPDIR:", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintln(os.Stderr, "remove test TMPDIR:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
