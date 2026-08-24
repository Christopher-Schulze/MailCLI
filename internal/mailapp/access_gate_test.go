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

func TestFileAccessGateHonorsContextWhileContended(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.lock")
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
	path := filepath.Join(t.TempDir(), "mail.lock")
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
	path := filepath.Join(t.TempDir(), "mail.lock")
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
	path := filepath.Join(t.TempDir(), "mail.lock")
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

func TestFileAccessGateDoesNotBindUncertaintyToReplacementMailProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.lock")
	pid := 42
	lookup := func(context.Context) (int, error) { return pid, nil }
	gate := &fileAccessGate{path: path, mailPID: lookup}
	lease, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	pid = 43
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
		path: filepath.Join(t.TempDir(), "mail.lock"),
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
	path := filepath.Join(t.TempDir(), "mail.lock")
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
