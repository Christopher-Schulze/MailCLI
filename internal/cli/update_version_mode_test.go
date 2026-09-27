package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionConsumerCompatibility(t *testing.T) {
	for _, implementation := range []string{"legacy", "current"} {
		t.Run(implementation, func(t *testing.T) {
			t.Setenv("MAILCLI_TEST_VERSION_MODE", implementation)
			t.Setenv("MAILCLI_TEST_VERSION_HELPER", os.Args[0])
			t.Setenv("MAILCLI_OUTPUT", "json")
			binary := filepath.Join(t.TempDir(), "mailcli")
			wrapper := "#!/bin/sh\nexec \"$MAILCLI_TEST_VERSION_HELPER\" -test.run=^TestVersionModeHelperProcess$ -- \"$@\"\n"
			if err := os.WriteFile(binary, []byte(wrapper), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := verifyBinaryVersion(context.Background(), binary, version); err != nil {
				t.Fatal(err)
			}
			if err := verifyBinaryVersion(context.Background(), binary, "0.0.0"); err == nil {
				t.Fatal("incorrect version accepted")
			}
			// Legacy verifyBinaryVersion invoked only `version` and compared plain
			// output (baseline 55c3adede94517d8dfe5e9d7c4194c8abdc7b8e1).
			// The documented parent environment bridges that unchanged consumer.
			t.Setenv("MAILCLI_OUTPUT", "human")
			output, err := exec.CommandContext(context.Background(), binary, "version").CombinedOutput()
			if err != nil || strings.TrimSpace(string(output)) != "mailcli "+version {
				t.Fatalf("legacy probe output=%q error=%v", output, err)
			}
		})
	}
}

func TestVersionModeHelperProcess(t *testing.T) {
	implementation := os.Getenv("MAILCLI_TEST_VERSION_MODE")
	if implementation == "" {
		return
	}
	index := 0
	for index < len(os.Args) && os.Args[index] != "--" {
		index++
	}
	if index == len(os.Args) {
		os.Exit(2)
	}
	args := os.Args[index+1:]
	if implementation == "current" {
		var err error
		args, _, err = ResolveOutputMode(args, os.Stdout, os.Getenv("MAILCLI_OUTPUT"))
		if err != nil {
			os.Exit(2)
		}
	}
	// Legacy uses the retained dispatcher directly, matching its old default.
	os.Exit(Run(context.Background(), nil, args, os.Stdout, os.Stderr))
}
