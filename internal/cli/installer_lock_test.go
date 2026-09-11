package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type installerFixture struct {
	environment updateEnvironment
	packageRoot string
	lockPath    string
	stateRoot   string
}

func newInstallerFixture(t *testing.T) installerFixture {
	t.Helper()
	root := t.TempDir()
	environment := updateEnvironment{
		homeDirectory:  filepath.Join(root, "home"),
		executablePath: filepath.Join(root, "bin", "mailcli"),
	}
	createInstalledUpdateFixture(t, environment, "1.0.4", "old skill")
	packageName := "mailcli_1.0.5_darwin_arm64"
	if err := extractReleaseArchive(buildTestUpdateArchive(t, "1.0.5"), root, packageName); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(environment.homeDirectory, "Library", "Application Support", "MailCLI")
	return installerFixture{environment: environment, packageRoot: filepath.Join(root, packageName),
		lockPath: filepath.Join(state, "update.lock"), stateRoot: filepath.Join(state, "install-transactions")}
}

type installerTestProcess struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	output bytes.Buffer
}

func startInstallerTestProcess(t *testing.T, fixture installerFixture, extraEnvironment ...string) *installerTestProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	command := exec.CommandContext(ctx, "/bin/bash", filepath.Join(fixture.packageRoot, "install.sh"))
	command.Env = append([]string{"PATH=/usr/bin:/bin", "HOME=" + fixture.environment.homeDirectory,
		"MAILCLI_BINARY_DESTINATION=" + fixture.environment.executablePath}, extraEnvironment...)
	process := &installerTestProcess{cancel: cancel, done: make(chan struct{})}
	command.Stdout = &process.output
	command.Stderr = &process.output
	go func() {
		process.err = runOwnedProcess(command, time.Second)
		close(process.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-process.done:
		case <-time.After(5 * time.Second):
			t.Error("installer process did not stop during test cleanup")
		}
	})
	return process
}

func finishInstallerTestProcess(t *testing.T, process *installerTestProcess, wantSuccess bool) {
	t.Helper()
	select {
	case <-process.done:
		if (process.err == nil) != wantSuccess {
			t.Fatalf("installer error = %v, want success %t: %s", process.err, wantSuccess, process.output.String())
		}
	case <-time.After(35 * time.Second):
		t.Fatal("installer exceeded its bounded wait")
	}
}

func pauseInstallerTestProcess(t *testing.T, fixture installerFixture, destination string) (*installerTestProcess, int, string) {
	t.Helper()
	root := t.TempDir()
	ready := filepath.Join(root, "ready")
	resume := filepath.Join(root, "resume")
	script := filepath.Join(root, "pause.sh")
	writeExecutableTestScript(t, script, `mv() {
  command mv "$@" || return
  if [[ "${2:-}" == "$MAILCLI_TEST_PAUSE_PATH" ]]; then
    printf '%s\n' "$$" > "$MAILCLI_TEST_READY"
    while [[ ! -e "$MAILCLI_TEST_RESUME" ]]; do /bin/sleep 0.02; done
  fi
}
`)
	process := startInstallerTestProcess(t, fixture, "BASH_ENV="+script, "MAILCLI_TEST_PAUSE_PATH="+destination,
		"MAILCLI_TEST_READY="+ready, "MAILCLI_TEST_RESUME="+resume)
	processIDs := waitForTestProcessIDs(t, ready, 1)
	return process, processIDs[0], resume
}

func installerTestManifest(t *testing.T, fixture installerFixture) (string, []byte) {
	t.Helper()
	entries, err := os.ReadDir(fixture.stateRoot)
	if err != nil || len(entries) != 1 {
		t.Fatalf("transaction entries = %v, error = %v", entries, err)
	}
	path := filepath.Join(fixture.stateRoot, entries[0].Name(), "manifest")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, contents
}

func TestInstallerSharedLockSerializesRecoveryAndCancellation(t *testing.T) {
	fixture := newInstallerFixture(t)
	owner, _, resume := pauseInstallerTestProcess(t, fixture, fixture.environment.executablePath)
	manifest, before := installerTestManifest(t, fixture)
	contender := startInstallerTestProcess(t, fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	lock, err := acquireUpdateLock(ctx, fixture.environment.homeDirectory)
	if lock != nil {
		t.Error("updater acquired a live installer's lock")
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	}
	if updateErrorCodeForTest(err) != "update_busy" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("competing updater = %v", err)
	}
	select {
	case <-contender.done:
		t.Fatalf("contender finished while owner was paused: %v: %s", contender.err, contender.output.String())
	default:
	}
	contender.cancel()
	finishInstallerTestProcess(t, contender, false)
	after, err := os.ReadFile(manifest)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("contender changed owner's transaction: %q, %v", after, err)
	}
	if err := os.WriteFile(resume, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	finishInstallerTestProcess(t, owner, true)
	finishInstallerTestProcess(t, startInstallerTestProcess(t, fixture), true)
}

func TestInstallerSharedDescriptorRemainsLockedAfterChildExit(t *testing.T) {
	fixture := newInstallerFixture(t)
	lock, err := acquireUpdateLock(context.Background(), fixture.environment.homeDirectory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("MAILCLI_INSTALL_LOCK_FD", "999")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runReleaseInstaller(ctx, filepath.Join(fixture.packageRoot, "install.sh"),
		fixture.environment.executablePath, fixture.environment.homeDirectory, lock); err != nil {
		t.Fatal(err)
	}
	waitContext, stopWaiting := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stopWaiting()
	unexpected, err := acquireUpdateLock(waitContext, fixture.environment.homeDirectory)
	if unexpected != nil {
		if closeErr := unexpected.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("child exit unlocked the parent descriptor")
	}
	if updateErrorCodeForTest(err) != "update_busy" {
		t.Fatalf("parent lock after child exit = %v", err)
	}
}

func TestInstallerDirectLockWaitIsBounded(t *testing.T) {
	fixture := newInstallerFixture(t)
	lock, err := acquireUpdateLock(context.Background(), fixture.environment.homeDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	}()
	start := time.Now()
	contender := startInstallerTestProcess(t, fixture)
	finishInstallerTestProcess(t, contender, false)
	if elapsed := time.Since(start); elapsed < 29*time.Second || elapsed > 35*time.Second ||
		!strings.Contains(contender.output.String(), "wait exceeded 30 seconds") {
		t.Fatalf("lock wait = %s: %s", elapsed, contender.output.String())
	}
	if _, err := os.Stat(fixture.stateRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("waiting installer created transaction state: %v", err)
	}
}

func TestInstallerRejectsUnsafeLockFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "fifo", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newInstallerFixture(t)
			if err := os.MkdirAll(filepath.Dir(fixture.lockPath), 0o700); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "unrelated")
			if err := os.WriteFile(outside, []byte("untouched"), 0o644); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(outside, fixture.lockPath)
			case "directory":
				err = os.Mkdir(fixture.lockPath, 0o700)
			case "fifo":
				err = syscall.Mkfifo(fixture.lockPath, 0o600)
			case "hardlink":
				err = os.Link(outside, fixture.lockPath)
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			lock, err := acquireUpdateLock(ctx, fixture.environment.homeDirectory)
			if lock != nil {
				if err := lock.Close(); err != nil {
					t.Error(err)
				}
				t.Fatal("unsafe updater lock accepted")
			}
			if updateErrorCodeForTest(err) != "update_lock_failed" {
				t.Fatalf("unsafe updater lock = %v", err)
			}
			finishInstallerTestProcess(t, startInstallerTestProcess(t, fixture), false)
			contents, err := os.ReadFile(outside)
			info, statErr := os.Stat(outside)
			if err != nil || statErr != nil || string(contents) != "untouched" || info.Mode().Perm() != 0o644 {
				t.Fatalf("unsafe lock changed unrelated file: %q, %v, %v", contents, err, statErr)
			}
		})
	}
}

func TestInstallerRecoversKilledOwnerAndPreservesReplacements(t *testing.T) {
	for _, phase := range []string{"binary", "skill"} {
		for _, replacement := range []string{"none", "destination", "backup"} {
			t.Run(phase+"/"+replacement, func(t *testing.T) {
				fixture := newInstallerFixture(t)
				destination := fixture.environment.executablePath
				if phase == "skill" {
					destination = filepath.Join(fixture.environment.homeDirectory, ".agents", "skills", "mailcli")
				}
				owner, processID, _ := pauseInstallerTestProcess(t, fixture, destination)
				if err := syscall.Kill(processID, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				finishInstallerTestProcess(t, owner, false)
				installerTestManifest(t, fixture)
				if replacement == "none" {
					source := filepath.Join(fixture.packageRoot, "bin", "mailcli")
					if err := os.Rename(source, source+".saved"); err != nil {
						t.Fatal(err)
					}
					finishInstallerTestProcess(t, startInstallerTestProcess(t, fixture), false)
					if err := verifyBinaryVersion(context.Background(), fixture.environment.executablePath, "1.0.4"); err != nil {
						t.Fatalf("interrupted binary was not restored: %v", err)
					}
					skill, err := os.ReadFile(filepath.Join(fixture.environment.homeDirectory, ".agents", "skills", "mailcli", "SKILL.md"))
					if err != nil || string(skill) != "old skill\n" {
						t.Fatalf("interrupted skill was not restored: %q, %v", skill, err)
					}
					if err := os.Rename(source+".saved", source); err != nil {
						t.Fatal(err)
					}
					finishInstallerTestProcess(t, startInstallerTestProcess(t, fixture), true)
					if err := verifyBinaryVersion(context.Background(), fixture.environment.executablePath, "1.0.5"); err != nil {
						t.Fatal(err)
					}
					return
				}
				if replacement == "backup" {
					destination += ".mailcli-backup"
				}
				if err := os.Rename(destination, destination+".original"); err != nil {
					t.Fatal(err)
				}
				if phase == "skill" {
					if err := os.Mkdir(destination, 0o700); err != nil {
						t.Fatal(err)
					}
					destination = filepath.Join(destination, "unrelated")
				}
				if err := os.WriteFile(destination, []byte("unrelated replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
				finishInstallerTestProcess(t, startInstallerTestProcess(t, fixture), false)
				contents, err := os.ReadFile(destination)
				if err != nil || string(contents) != "unrelated replacement" {
					t.Fatalf("replacement changed: %q, %v", contents, err)
				}
				installerTestManifest(t, fixture)
			})
		}
	}
}

func TestPerformUpdateKeepsLockThroughFinalVerification(t *testing.T) {
	archive := buildTestUpdateArchive(t, "1.0.5")
	server := newUpdateTestServer(t, "1.0.5", archive, checksumFile("mailcli_1.0.5_darwin_arm64.tar.gz", archive))
	defer server.Close()
	environment := updateTestEnvironment(t, server, "1.0.4")
	createInstalledUpdateFixture(t, environment, "1.0.4", "old skill")
	environment.verifyInstallation = func(ctx context.Context, binary, version string) error {
		waitContext, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		lock, err := acquireUpdateLock(waitContext, environment.homeDirectory)
		if lock != nil {
			return errors.Join(errors.New("updater released lock before verification"), lock.Close())
		}
		if updateErrorCodeForTest(err) != "update_busy" {
			return err
		}
		return verifyBinaryVersion(ctx, binary, version)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := performUpdate(ctx, environment, newUpdateReporter(io.Discard, false, false)); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireUpdateLock(ctx, environment.homeDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInstallerRejectsReplacedLockOwnership(t *testing.T) {
	for _, target := range []string{"lock", "state directory", "transaction directory"} {
		t.Run(target, func(t *testing.T) {
			fixture := newInstallerFixture(t)
			owner, _, resume := pauseInstallerTestProcess(t, fixture, fixture.environment.executablePath)
			manifest, before := installerTestManifest(t, fixture)
			path := fixture.lockPath
			switch target {
			case "state directory":
				path = filepath.Dir(fixture.lockPath)
			case "transaction directory":
				path = fixture.stateRoot
			}
			if err := os.Rename(path, path+".original"); err != nil {
				t.Fatal(err)
			}
			if target == "lock" {
				if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				manifest = strings.Replace(manifest, path+string(os.PathSeparator), path+".original"+string(os.PathSeparator), 1)
			}
			if err := os.WriteFile(resume, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			finishInstallerTestProcess(t, owner, false)
			after, err := os.ReadFile(manifest)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("lost owner modified transaction: %q, %v", after, err)
			}
			if err := verifyBinaryVersion(context.Background(), fixture.environment.executablePath, "1.0.5"); err != nil {
				t.Fatalf("lost owner rolled back live binary: %v", err)
			}
		})
	}
}

func TestInstallerRejectsInheritedDescriptorForAnotherFile(t *testing.T) {
	fixture := newInstallerFixture(t)
	other := newInstallerFixture(t)
	lock, err := acquireUpdateLock(context.Background(), other.environment.homeDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = runReleaseInstaller(ctx, filepath.Join(fixture.packageRoot, "install.sh"),
		fixture.environment.executablePath, fixture.environment.homeDirectory, lock)
	if err == nil || !strings.Contains(err.Error(), "lock identity changed") {
		t.Fatalf("wrong inherited descriptor = %v", err)
	}
	if err := verifyBinaryVersion(ctx, fixture.environment.executablePath, "1.0.4"); err != nil {
		t.Fatal(err)
	}
}

func TestInstallerPreservesByteIdenticalReplacement(t *testing.T) {
	fixture := newInstallerFixture(t)
	owner, processID, _ := pauseInstallerTestProcess(t, fixture, fixture.environment.executablePath)
	if err := syscall.Kill(processID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	finishInstallerTestProcess(t, owner, false)
	binary := fixture.environment.executablePath
	contents, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(binary, binary+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, contents, 0o755); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.Stat(binary)
	if err != nil {
		t.Fatal(err)
	}
	finishInstallerTestProcess(t, startInstallerTestProcess(t, fixture), false)
	current, err := os.Stat(binary)
	if err != nil || !os.SameFile(replacement, current) {
		t.Fatalf("byte-identical replacement identity changed: %v", err)
	}
	installerTestManifest(t, fixture)
}
