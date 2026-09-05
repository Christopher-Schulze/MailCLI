package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestRunVersionWithoutMailStore(t *testing.T) {
	output, code := runWithArgs(t, []string{"mailcli", "version"})
	if code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
	if !strings.Contains(output, "mailcli ") {
		t.Fatalf("stdout = %q, want version line", output)
	}
}

func TestRunHelpWithoutMailStore(t *testing.T) {
	output, code := runWithArgs(t, []string{"mailcli", "help"})
	if code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
	if !strings.Contains(output, "Usage:") {
		t.Fatalf("stdout = %q, want usage", output)
	}
}

func TestRunUnknownCommandWithoutMailStore(t *testing.T) {
	_, stderr, code := runWithArgsAndStderr(t, []string{"mailcli", "not-a-command"})
	if code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if !strings.Contains(stderr, "unknown command") {
		t.Fatalf("stderr = %q, want unknown command", stderr)
	}
}

func TestRunStoreCommandReportsMissingStore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, stderr, code := runWithArgsAndStderr(t, []string{"mailcli", "accounts", "list"})
	if code == 0 {
		t.Fatal("run() = 0, want missing-store failure")
	}
	if !strings.Contains(stderr, "Mail.app") {
		t.Fatalf("stderr = %q, want Mail.app diagnostic", stderr)
	}
}

func runWithArgs(t *testing.T, args []string) (string, int) {
	t.Helper()
	stdout, _, code := runWithArgsAndStderr(t, args)
	return stdout, code
}

func runWithArgsAndStderr(t *testing.T, args []string) (string, string, int) {
	t.Helper()
	oldArgs, oldStdout, oldStderr := os.Args, os.Stdout, os.Stderr
	t.Cleanup(func() {
		os.Args = oldArgs
		os.Stdout = oldStdout
		os.Stderr = oldStderr
	})
	os.Args = args
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	os.Stdout = stdoutWriter
	os.Stderr = stderrWriter
	code := run()
	if err := stdoutWriter.Close(); err != nil {
		t.Fatalf("close stdout: %v", err)
	}
	if err := stderrWriter.Close(); err != nil {
		t.Fatalf("close stderr: %v", err)
	}
	stdout, err := io.ReadAll(stdoutReader)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	stderr, err := io.ReadAll(stderrReader)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	_ = stdoutReader.Close()
	_ = stderrReader.Close()
	os.Stdout = oldStdout
	os.Stderr = oldStderr
	return string(stdout), string(stderr), code
}
