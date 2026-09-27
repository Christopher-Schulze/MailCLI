package mailapp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type expiresAfterFirstCheckContext struct {
	context.Context
	checks int
}

func (ctx *expiresAfterFirstCheckContext) Err() error {
	ctx.checks++
	if ctx.checks == 1 {
		return nil
	}
	return context.DeadlineExceeded
}

func testAccessGateRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temporary directory: %v", err)
	}
	return root
}

func testAccessGatePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(testAccessGateRoot(t), "mail.lock")
}

func TestFileAccessGateHonorsContextWhileContended(t *testing.T) {
	path := testAccessGatePath(t)
	first := &fileAccessGate{path: path, pollInterval: time.Millisecond}
	release, err := first.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first Acquire() error = %v", err)
	}
	t.Cleanup(func() {
		if err := release.Release(false); err != nil {
			t.Errorf("release() error = %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	second := &fileAccessGate{path: path, pollInterval: time.Millisecond}
	if _, err := second.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended Acquire() error = %v, want deadline exceeded", err)
	}
}

func TestFileAccessGateRechecksContextAfterAcquiringLock(t *testing.T) {
	path := testAccessGatePath(t)
	ctx := &expiresAfterFirstCheckContext{Context: context.Background()}
	gate := &fileAccessGate{path: path}

	if _, err := gate.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire() error = %v, want deadline exceeded", err)
	}
	lease, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatalf("lock remained held after expired acquisition: %v", err)
	}
	if err := lease.Release(false); err != nil {
		t.Fatalf("release() error = %v", err)
	}
}

func TestFileAccessGateBoundsItsOwnWait(t *testing.T) {
	path := testAccessGatePath(t)
	release, err := (&fileAccessGate{path: path}).Acquire(context.Background())
	if err != nil {
		t.Fatalf("first Acquire() error = %v", err)
	}
	t.Cleanup(func() {
		if err := release.Release(false); err != nil {
			t.Errorf("release() error = %v", err)
		}
	})
	second := &fileAccessGate{path: path, maxWait: 20 * time.Millisecond, pollInterval: time.Millisecond}
	if _, err := second.Acquire(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded Acquire() error = %v, want deadline exceeded", err)
	}
}

func TestFileAccessGateFailsClosedAfterTimedOutOperation(t *testing.T) {
	path := testAccessGatePath(t)
	pid := 42
	lookup := func(context.Context) (int, error) { return pid, nil }
	gate := &fileAccessGate{path: path, mailPID: lookup}
	lease, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first Acquire() error = %v", err)
	}
	if lease.TargetPID() != 42 {
		t.Fatalf("lease target PID = %d, want 42", lease.TargetPID())
	}
	if err := lease.ArmUncertainState(); err != nil {
		t.Fatalf("ArmUncertainState() error = %v", err)
	}
	payload, err := os.ReadFile(path)
	if err != nil || string(payload) != `{"mail_pid":42}` {
		t.Fatalf("armed gate state = %q, error = %v", payload, err)
	}
	if err := lease.Release(true); err != nil {
		t.Fatalf("uncertain Release() error = %v", err)
	}

	if _, err := gate.Acquire(context.Background()); err == nil {
		t.Fatal("Acquire() error = nil after uncertain operation")
	} else {
		var uncertain *uncertainMailStateError
		if !errors.As(err, &uncertain) {
			t.Fatalf("Acquire() error = %v, want uncertain state", err)
		}
	}

	pid = 43
	lease, err = gate.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() after Mail restart error = %v", err)
	}
	if err := lease.Release(false); err != nil {
		t.Fatalf("final Release() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 {
		t.Fatalf("gate state after restart = %+v, error = %v", info, err)
	}
}

func TestFileAccessGateRecordsUncertaintyWithoutASecondPIDLookup(t *testing.T) {
	path := testAccessGatePath(t)
	lookups := 0
	lookup := func(context.Context) (int, error) {
		lookups++
		if lookups > 1 {
			return 0, fmt.Errorf("transient pgrep failure")
		}
		return 42, nil
	}
	lease, err := (&fileAccessGate{path: path, mailPID: lookup}).Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if err := lease.ArmUncertainState(); err != nil {
		t.Fatalf("ArmUncertainState() error = %v", err)
	}
	if err := lease.Release(true); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if lookups != 1 {
		t.Fatalf("Mail PID lookups = %d, want 1", lookups)
	}
	payload, err := os.ReadFile(path)
	if err != nil || string(payload) != `{"mail_pid":42}` {
		t.Fatalf("gate state = %q, error = %v", payload, err)
	}
}

func TestFileAccessGateClearsPrearmedStateAfterDefiniteCompletion(t *testing.T) {
	path := testAccessGatePath(t)
	gate := &fileAccessGate{
		path: path, mailPID: func(context.Context) (int, error) { return 42, nil },
	}
	lease, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if err := lease.ArmUncertainState(); err != nil {
		t.Fatalf("ArmUncertainState() error = %v", err)
	}
	if err := lease.Release(false); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 {
		t.Fatalf("gate state after definite completion = %+v, error = %v", info, err)
	}
}

func TestFileAccessGatePrearmedStateSurvivesCallerExit(t *testing.T) {
	path := testAccessGatePath(t)
	command := exec.Command(os.Args[0], "-test.run=TestFileAccessGatePrearmedCrashHelper")
	command.Env = append(os.Environ(), "MAILCLI_GATE_CRASH_HELPER=1", "MAILCLI_GATE_PATH="+path)
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "armed\n" {
		t.Fatalf("crash helper output = %q, error = %v", output, err)
	}

	gate := &fileAccessGate{
		path: path, mailPID: func(context.Context) (int, error) { return 42, nil },
	}
	_, err = gate.Acquire(context.Background())
	var uncertain *uncertainMailStateError
	if !errors.As(err, &uncertain) {
		t.Fatalf("Acquire() after caller exit error = %v, want uncertain state", err)
	}
}

func TestFileAccessGatePrearmedCrashHelper(t *testing.T) {
	if os.Getenv("MAILCLI_GATE_CRASH_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	gate := &fileAccessGate{
		path:    os.Getenv("MAILCLI_GATE_PATH"),
		mailPID: func(context.Context) (int, error) { return 42, nil },
	}
	lease, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.ArmUncertainState(); err != nil {
		t.Fatal(err)
	}
	fmt.Println("armed")
	os.Exit(0)
}

func TestFileAccessGateRejectsCorruptRecoveryState(t *testing.T) {
	path := testAccessGatePath(t)
	corrupt := []byte(`{"mail_pid":`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	gate := &fileAccessGate{path: path, mailPID: func(context.Context) (int, error) { return 42, nil }}
	_, err := gate.Acquire(context.Background())
	var invalid *invalidAccessGateStateError
	if !errors.As(err, &invalid) {
		t.Fatalf("Acquire() error = %v, want invalidAccessGateStateError", err)
	}
	mapped := mapAccessGateError(err)
	var operation *OperationError
	if !errors.As(mapped, &operation) || operation.Code != "mail_access_gate_corrupt" {
		t.Fatalf("mapAccessGateError() = %v", mapped)
	}
	if payload, err := os.ReadFile(path); err != nil || string(payload) != string(corrupt) {
		t.Fatalf("running Mail corrupt state changed to %q, error = %v", payload, err)
	}
}

func TestFileAccessGateRepairsCorruptStateOnlyWhenMailIsProvenStopped(t *testing.T) {
	tests := []struct {
		name      string
		mailPID   func(context.Context) (int, error)
		wantError string
	}{
		{name: "running", mailPID: func(context.Context) (int, error) { return 42, nil }, wantError: "corrupt"},
		{name: "invalid PID", mailPID: func(context.Context) (int, error) { return -1, nil }, wantError: "corrupt"},
		{name: "unknown", mailPID: func(context.Context) (int, error) { return 0, errors.New("process lookup failed") }, wantError: "corrupt"},
		{name: "stopped", mailPID: func(context.Context) (int, error) { return 0, nil }, wantError: "not-running"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertCorruptAccessGateState(t, test.mailPID, test.wantError)
		})
	}
}

func assertCorruptAccessGateState(
	t *testing.T,
	mailPID func(context.Context) (int, error),
	wantError string,
) {
	t.Helper()
	path := testAccessGatePath(t)
	corrupt := []byte(`{"mail_pid":`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&fileAccessGate{path: path, mailPID: mailPID}).Acquire(context.Background())
	assertCorruptAccessGateOutcome(t, path, corrupt, before, err, wantError)
}

func assertCorruptAccessGateOutcome(
	t *testing.T,
	path string,
	corrupt []byte,
	before os.FileInfo,
	err error,
	wantError string,
) {
	t.Helper()
	if wantError == "corrupt" {
		var invalid *invalidAccessGateStateError
		if !errors.As(err, &invalid) {
			t.Fatalf("Acquire() error = %v, want corrupt-state error", err)
		}
		payload, readErr := os.ReadFile(path)
		if readErr != nil || string(payload) != string(corrupt) {
			t.Fatalf("unrecovered state = %q, error = %v", payload, readErr)
		}
		return
	}
	var stopped *mailNotRunningError
	if !errors.As(err, &stopped) {
		t.Fatalf("Acquire() error = %v, want Mail-not-running error", err)
	}
	payload, readErr := os.ReadFile(path)
	if readErr != nil || len(payload) != 0 {
		t.Fatalf("repaired state = %q, error = %v", payload, readErr)
	}
	after, statErr := os.Stat(path)
	if statErr != nil || !os.SameFile(before, after) {
		t.Fatalf("repair replaced the gate inode: before=%v after=%v error=%v", before, after, statErr)
	}
}

func TestFileAccessGateRejectsSymbolicLinkLock(t *testing.T) {
	root := testAccessGateRoot(t)
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("preserve"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "mail.lock")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	gate := &fileAccessGate{path: path, mailPID: func(context.Context) (int, error) { return 42, nil }}
	if _, err := gate.Acquire(context.Background()); !isUnsafeAccessGateError(err) {
		t.Fatalf("Acquire() error = %v, want unsafe-path error", err)
	}
	payload, err := os.ReadFile(target)
	if err != nil || string(payload) != "preserve" {
		t.Fatalf("lock target changed to %q, error = %v", payload, err)
	}
}

func TestFileAccessGateRejectsSymbolicLinkDirectory(t *testing.T) {
	root := testAccessGateRoot(t)
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "MailCLI")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(link, "mail.lock")
	if _, err := (&fileAccessGate{path: path}).Acquire(context.Background()); !isUnsafeAccessGateError(err) {
		t.Fatalf("Acquire() error = %v, want unsafe-path error", err)
	}
	if _, err := os.Lstat(filepath.Join(target, "mail.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink target gained a lock file: %v", err)
	}
}

func TestFileAccessGateRejectsHardlinkedLockBeforeChangingIt(t *testing.T) {
	path := testAccessGatePath(t)
	alias := path + ".alias"
	if err := os.WriteFile(path, []byte("preserve"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := (&fileAccessGate{path: path}).Acquire(context.Background()); !isUnsafeAccessGateError(err) {
		t.Fatalf("Acquire() error = %v, want unsafe-path error", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o666 {
		t.Fatalf("hardlinked lock mode changed to %v, error = %v", info, err)
	}
	if payload, err := os.ReadFile(alias); err != nil || string(payload) != "preserve" {
		t.Fatalf("hardlink contents changed to %q, error = %v", payload, err)
	}
}

func TestFileAccessGateRejectsForeignOwnerBeforeChangingLock(t *testing.T) {
	path := testAccessGatePath(t)
	if err := os.WriteFile(path, []byte("preserve"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	foreignUID := 0
	if os.Geteuid() == 0 {
		foreignUID = 65534
	}
	if err := os.Chown(path, foreignUID, -1); err != nil {
		t.Skipf("cannot create foreign-owner fixture: %v", err)
	}
	if _, err := (&fileAccessGate{path: path}).Acquire(context.Background()); !isUnsafeAccessGateError(err) {
		t.Fatalf("Acquire() error = %v, want unsafe-path error", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o666 {
		t.Fatalf("foreign-owned lock mode changed to %v, error = %v", info, err)
	}
	if payload, err := os.ReadFile(path); err != nil || string(payload) != "preserve" {
		t.Fatalf("foreign-owned lock contents changed to %q, error = %v", payload, err)
	}
}

func TestFileAccessGateRejectsForeignOwnerDirectoryBeforeChangingIt(t *testing.T) {
	root := testAccessGateRoot(t)
	directory := filepath.Join(root, "MailCLI")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	foreignUID := 0
	if os.Geteuid() == 0 {
		foreignUID = 65534
	}
	if err := os.Chown(directory, foreignUID, -1); err != nil {
		t.Skipf("cannot create foreign-owner directory fixture: %v", err)
	}
	path := filepath.Join(directory, "mail.lock")
	if _, err := (&fileAccessGate{path: path}).Acquire(context.Background()); !isUnsafeAccessGateError(err) {
		t.Fatalf("Acquire() error = %v, want unsafe-path error", err)
	}
	info, err := os.Stat(directory)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("foreign-owned directory mode changed to %v, error = %v", info, err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign-owned directory gained a lock: %v", err)
	}
}

func TestFileAccessGateRejectsFIFOWithoutBlockingOpen(t *testing.T) {
	path := testAccessGatePath(t)
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	command, output := startAccessGateTestProcess(t, "TestFileAccessGateFIFOHelper",
		"MAILCLI_GATE_FIFO_HELPER=1", "MAILCLI_GATE_PATH="+path)
	assertAccessGateTestLine(t, output, "rejected\n")
	assertAccessGateTestLine(t, output, "PASS\n")
	if err := command.Wait(); err != nil {
		t.Fatalf("FIFO helper exit: %v", err)
	}
}

func TestFileAccessGateFIFOHelper(t *testing.T) {
	if os.Getenv("MAILCLI_GATE_FIFO_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	_, err := (&fileAccessGate{path: os.Getenv("MAILCLI_GATE_PATH")}).Acquire(context.Background())
	if !isUnsafeAccessGateError(err) {
		t.Fatalf("Acquire() error = %v, want unsafe-path error", err)
	}
	fmt.Println("rejected")
}

func TestFileAccessGateRefusesStateMutationAfterLockReplacement(t *testing.T) {
	path := testAccessGatePath(t)
	gate := &fileAccessGate{path: path, mailPID: func(context.Context) (int, error) { return 42, nil }}
	lease, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	oldPath := path + ".original"
	if err := os.Rename(path, oldPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lease.ArmUncertainState(); !isUnsafeAccessGateError(err) {
		t.Fatalf("ArmUncertainState() error = %v, want unsafe-path error", err)
	}
	if err := lease.Release(false); !isUnsafeAccessGateError(err) {
		t.Fatalf("Release() error = %v, want unsafe-path error", err)
	}
	if payload, err := os.ReadFile(oldPath); err != nil || len(payload) != 0 {
		t.Fatalf("detached lock state changed to %q, error = %v", payload, err)
	}
	if payload, err := os.ReadFile(path); err != nil || string(payload) != "replacement" {
		t.Fatalf("replacement lock changed to %q, error = %v", payload, err)
	}
}

func TestFileAccessGateRevalidatesPathAfterMailPIDLookup(t *testing.T) {
	path := testAccessGatePath(t)
	oldPath := path + ".original"
	gate := &fileAccessGate{
		path: path,
		mailPID: func(context.Context) (int, error) {
			if err := os.Rename(path, oldPath); err != nil {
				return 0, err
			}
			if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
				return 0, err
			}
			return 42, nil
		},
	}
	if _, err := gate.Acquire(context.Background()); !isUnsafeAccessGateError(err) {
		t.Fatalf("Acquire() error = %v, want unsafe-path error", err)
	}
	if payload, err := os.ReadFile(oldPath); err != nil || len(payload) != 0 {
		t.Fatalf("detached lock state changed to %q, error = %v", payload, err)
	}
	if payload, err := os.ReadFile(path); err != nil || string(payload) != "replacement" {
		t.Fatalf("replacement lock changed to %q, error = %v", payload, err)
	}
}

func TestFileAccessGateRefusesStateMutationAfterParentReplacement(t *testing.T) {
	root := testAccessGateRoot(t)
	directory := filepath.Join(root, "MailCLI")
	path := filepath.Join(directory, "mail.lock")
	gate := &fileAccessGate{path: path, mailPID: func(context.Context) (int, error) { return 42, nil }}
	lease, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	oldDirectory := filepath.Join(root, "MailCLI.original")
	if err := os.Rename(directory, oldDirectory); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := lease.ArmUncertainState(); !isUnsafeAccessGateError(err) {
		t.Fatalf("ArmUncertainState() error = %v, want unsafe-path error", err)
	}
	if err := lease.Release(false); !isUnsafeAccessGateError(err) {
		t.Fatalf("Release() error = %v, want unsafe-path error", err)
	}
	if payload, err := os.ReadFile(filepath.Join(oldDirectory, "mail.lock")); err != nil || len(payload) != 0 {
		t.Fatalf("detached parent gate state changed to %q, error = %v", payload, err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement parent unexpectedly gained a lock: %v", err)
	}
}

func isUnsafeAccessGateError(err error) bool {
	var unsafePath *unsafeAccessGatePathError
	return errors.As(err, &unsafePath)
}

func TestFileAccessGateDoesNotBindUncertaintyToReplacementMailProcess(t *testing.T) {
	path := testAccessGatePath(t)
	pid := 42
	lookup := func(context.Context) (int, error) { return pid, nil }
	gate := &fileAccessGate{path: path, mailPID: lookup}
	lease, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	pid = 43
	if err := lease.ArmUncertainState(); err != nil {
		t.Fatalf("ArmUncertainState() error = %v", err)
	}
	if err := lease.Release(true); err != nil {
		t.Fatalf("Release() after Mail replacement error = %v", err)
	}
	lease, err = gate.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() for replacement Mail process error = %v", err)
	}
	if lease.TargetPID() != 43 {
		t.Fatalf("replacement target PID = %d, want 43", lease.TargetPID())
	}
	if err := lease.Release(false); err != nil {
		t.Fatalf("final Release() error = %v", err)
	}
}

func TestFileAccessGateDoesNotLaunchStoppedMail(t *testing.T) {
	gate := &fileAccessGate{
		path: testAccessGatePath(t),
		mailPID: func(context.Context) (int, error) {
			return 0, nil
		},
	}
	_, err := gate.Acquire(context.Background())
	var notRunning *mailNotRunningError
	if !errors.As(err, &notRunning) {
		t.Fatalf("Acquire() error = %v, want mailNotRunningError", err)
	}
}

func TestFileAccessGateSerializesProcesses(t *testing.T) {
	path := testAccessGatePath(t)
	command := exec.Command(os.Args[0], "-test.run=TestFileAccessGateProcessHelper")
	command.Env = append(os.Environ(), "MAILCLI_GATE_HELPER=1", "MAILCLI_GATE_PATH="+path)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() error = %v", err)
	}
	if err := command.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		if killErr := command.Process.Kill(); killErr != nil {
			t.Logf("kill helper after readiness failure: %v", killErr)
		}
		t.Fatalf("helper readiness = %q, error = %v", line, err)
	}

	started := time.Now()
	release, err := (&fileAccessGate{path: path, pollInterval: time.Millisecond}).Acquire(context.Background())
	if err != nil {
		if killErr := command.Process.Kill(); killErr != nil {
			t.Logf("kill helper after acquire failure: %v", killErr)
		}
		t.Fatalf("parent Acquire() error = %v", err)
	}
	waited := time.Since(started)
	if err := release.Release(false); err != nil {
		t.Fatalf("release() error = %v", err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("helper Wait() error = %v", err)
	}
	if waited < 150*time.Millisecond {
		t.Fatalf("parent acquired after %s, want serialized wait", waited)
	}
}

func TestFileAccessGateRepairsCorruptStateForWaitingProcess(t *testing.T) {
	path := testAccessGatePath(t)
	holder, holderOutput := startAccessGateTestProcess(t, "TestFileAccessGateProcessHelper",
		"MAILCLI_GATE_HELPER=1", "MAILCLI_GATE_HOLD=30s", "MAILCLI_GATE_PATH="+path)
	assertAccessGateTestLine(t, holderOutput, "locked\n")
	corrupt := []byte(`{"mail_pid":`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	waiter, waiterOutput := startAccessGateTestProcess(t, "TestFileAccessGateRepairWaiterHelper",
		"MAILCLI_GATE_REPAIR_HELPER=1", "MAILCLI_GATE_PATH="+path)
	assertAccessGateTestLine(t, waiterOutput, "waiting\n")
	if err := holder.Process.Kill(); err != nil {
		t.Fatalf("stop gate holder: %v", err)
	}
	_ = holder.Wait()
	assertAccessGateTestLine(t, waiterOutput, "repaired\n")
	assertAccessGateTestLine(t, waiterOutput, "PASS\n")
	if err := waiter.Wait(); err != nil {
		t.Fatalf("repair waiter failed: %v", err)
	}
	payload, err := os.ReadFile(path)
	if err != nil || len(payload) != 0 {
		t.Fatalf("repaired gate contents = %q, error = %v", payload, err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("repair changed gate inode: before=%v after=%v error=%v", before, after, err)
	}
}

func TestFileAccessGateRepairWaiterHelper(t *testing.T) {
	if os.Getenv("MAILCLI_GATE_REPAIR_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	files, err := openAccessGateFiles(os.Getenv("MAILCLI_GATE_PATH"))
	if err != nil {
		t.Fatalf("open gate files: %v", err)
	}
	defer func() {
		if err := files.close(); err != nil {
			t.Errorf("close gate files: %v", err)
		}
	}()
	locked, err := tryAccessGateLock(ctx, files)
	if err != nil || locked {
		t.Fatalf("nonblocking lock probe = (%t, %v), want held by first process", locked, err)
	}
	fmt.Println("waiting")
	gate := &fileAccessGate{maxWait: 4 * time.Second, pollInterval: time.Millisecond}
	if err := gate.acquireFile(ctx, files); err != nil {
		t.Fatalf("acquire probed gate descriptor: %v", err)
	}
	err = validateAccessGateState(ctx, files, func(context.Context) (int, error) { return 0, nil })
	var stopped *mailNotRunningError
	if !errors.As(err, &stopped) {
		t.Fatalf("validateAccessGateState() error = %v, want stopped-Mail repair", err)
	}
	if err := files.release(); err != nil {
		t.Fatalf("release repaired gate: %v", err)
	}
	fmt.Println("repaired")
}

func startAccessGateTestProcess(t *testing.T, testName string, environment ...string) (*exec.Cmd, *bufio.Reader) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+testName+"$")
	command.Env = append(os.Environ(), environment...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("StdoutPipe() error = %v", err)
	}
	if err := command.Start(); err != nil {
		cancel()
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	return command, bufio.NewReader(stdout)
}

func assertAccessGateTestLine(t *testing.T, output *bufio.Reader, want string) {
	t.Helper()
	line, err := output.ReadString('\n')
	if err != nil || line != want {
		t.Fatalf("helper output = %q, error = %v; want %q", line, err, want)
	}
}

func TestFileAccessGateProcessHelper(t *testing.T) {
	if os.Getenv("MAILCLI_GATE_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	gate := &fileAccessGate{path: os.Getenv("MAILCLI_GATE_PATH"), pollInterval: time.Millisecond}
	release, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("locked")
	hold := 250 * time.Millisecond
	if value := os.Getenv("MAILCLI_GATE_HOLD"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			t.Fatal(err)
		}
		hold = parsed
	}
	time.Sleep(hold)
	if err := release.Release(false); err != nil {
		t.Fatal(err)
	}
}
