package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"mailcli/internal/mail"
)

func TestRunOwnedProcessForceCleansStubbornProcessGroup(t *testing.T) {
	directory := t.TempDir()
	processFile := filepath.Join(directory, "processes")
	scriptPath := filepath.Join(directory, "stubborn.sh")
	script := "#!/bin/bash\n" +
		"trap '' TERM\n" +
		"/bin/bash -c 'trap \"\" TERM; while :; do /bin/sleep 1; done' &\n" +
		"printf '%s %s\\n' \"$$\" \"$!\" > \"$MAILCLI_TEST_PROCESS_FILE\"\n" +
		"while :; do /bin/sleep 1; done\n"
	writeExecutableTestScript(t, scriptPath, script)
	t.Setenv("MAILCLI_TEST_PROCESS_FILE", processFile)

	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, "/bin/bash", scriptPath)
	result := make(chan error, 1)
	go func() { result <- runOwnedProcess(command, 100*time.Millisecond) }()
	processIDs := waitForTestProcessIDs(t, processFile)
	cancel()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("runOwnedProcess() error = nil after cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runOwnedProcess() did not complete after cancellation")
	}
	assertTestProcessGone(t, processIDs[0])
	assertTestProcessGone(t, processIDs[1])
	exists, err := ownedProcessGroupExists(processIDs[0])
	if err != nil || exists {
		t.Fatalf("owned process group %d remains: exists=%t, error=%v", processIDs[0], exists, err)
	}
}

func TestRunReleaseInstallerCancellationRunsRollback(t *testing.T) {
	directory := t.TempDir()
	readyPath := filepath.Join(directory, "ready")
	rollbackPath := filepath.Join(directory, "rollback")
	processFile := filepath.Join(directory, "process")
	installerPath := filepath.Join(directory, "install.sh")
	script := "#!/bin/bash\n" +
		"set -u\n" +
		"trap 'printf rolled-back > \"$MAILCLI_TEST_ROLLBACK_FILE\"' EXIT\n" +
		"printf '%s\\n' \"$$\" > \"$MAILCLI_TEST_PROCESS_FILE\"\n" +
		": > \"$MAILCLI_TEST_READY_FILE\"\n" +
		"while :; do /bin/sleep 1; done\n"
	writeExecutableTestScript(t, installerPath, script)
	t.Setenv("MAILCLI_TEST_READY_FILE", readyPath)
	t.Setenv("MAILCLI_TEST_ROLLBACK_FILE", rollbackPath)
	t.Setenv("MAILCLI_TEST_PROCESS_FILE", processFile)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- runReleaseInstaller(ctx, installerPath, filepath.Join(directory, "mailcli"), directory)
	}()
	waitForTestFile(t, readyPath)
	processIDs := waitForTestProcessIDs(t, processFile)
	cancel()

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "release installer failed") {
			t.Fatalf("runReleaseInstaller() error = %v", err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("runReleaseInstaller() did not complete after cancellation")
	}
	payload, err := os.ReadFile(rollbackPath)
	if err != nil || string(payload) != "rolled-back" {
		t.Fatalf("rollback marker = %q, error = %v", payload, err)
	}
	assertTestProcessGone(t, processIDs[0])
}

func TestDraftEditorCancellationCleansOwnedDescendants(t *testing.T) {
	directory := t.TempDir()
	processFile := filepath.Join(directory, "editor-processes")
	editorPath := filepath.Join(directory, "editor.sh")
	script := "#!/bin/bash\n" +
		"/bin/bash -c 'trap \"\" TERM; while :; do /bin/sleep 1; done' &\n" +
		"printf '%s %s\\n' \"$$\" \"$!\" > \"$MAILCLI_TEST_PROCESS_FILE\"\n" +
		"while :; do /bin/sleep 1; done\n"
	writeExecutableTestScript(t, editorPath, script)
	t.Setenv("MAILCLI_TEST_PROCESS_FILE", processFile)
	service := mail.NewServiceWithDraftRoot(testGateway{}, filepath.Join(directory, "drafts"))
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		To: []mail.Recipient{{Address: "recipient@example.com"}}, Body: "Body",
	}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, editErr := editDraftInput(
			ctx, service, draft.Ref, draftInputFromStored(draft), editorPath, nil,
		)
		result <- editErr
	}()
	processIDs := waitForTestProcessIDs(t, processFile)
	cancel()

	select {
	case err := <-result:
		var coded codedError
		if !errors.As(err, &coded) || coded.ErrorCode() != "editor_failed" {
			t.Fatalf("editDraftInput() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("editDraftInput() did not complete after cancellation")
	}
	for _, processID := range processIDs {
		assertTestProcessGone(t, processID)
	}
	unchanged, err := service.GetDraft(draft.Ref)
	if err != nil || unchanged.Body != "Body" {
		t.Fatalf("draft after canceled edit = %+v, error = %v", unchanged, err)
	}
}

func writeExecutableTestScript(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

func waitForTestProcessIDs(t *testing.T, path string) []int {
	t.Helper()
	waitForTestFile(t, path)
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(payload))
	processIDs := make([]int, 0, len(fields))
	for _, field := range fields {
		processID, parseErr := strconv.Atoi(field)
		if parseErr != nil || processID <= 1 {
			t.Fatalf("invalid test process ID %q: %v", field, parseErr)
		}
		processIDs = append(processIDs, processID)
	}
	if len(processIDs) == 0 {
		t.Fatal("test process file contains no process IDs")
	}
	return processIDs
}

func waitForTestFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", filepath.Base(path))
		}
		time.Sleep(ownedProcessPollInterval)
	}
}

func assertTestProcessGone(t *testing.T, processID int) {
	t.Helper()
	if err := syscall.Kill(processID, syscall.Signal(0)); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("process %d remains: %v", processID, err)
	}
}
