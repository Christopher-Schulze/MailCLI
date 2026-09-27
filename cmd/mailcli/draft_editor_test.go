package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"mailcli/internal/mail"
)

type editorCLIResponse struct {
	OK   bool `json:"ok"`
	Data struct {
		Draft mail.Draft `json:"draft"`
	} `json:"data"`
	Error *struct {
		Code     string                  `json:"code"`
		Message  string                  `json:"message"`
		Guidance *mail.OperationGuidance `json:"guidance"`
		Editor   *struct {
			Ref              string `json:"ref"`
			ExpectedRevision string `json:"expected_revision"`
			CandidatePath    string `json:"candidate_path"`
			ExitCode         *int   `json:"exit_code"`
		} `json:"draft_editor"`
	} `json:"error"`
}

func TestDraftCLIProcess(t *testing.T) {
	if os.Getenv("MAILCLI_TEST_ENTRYPOINT") != "1" {
		return
	}
	for index, argument := range os.Args {
		if argument == "--" {
			os.Args = append([]string{"mailcli"}, os.Args[index+1:]...)
			main()
			return
		}
	}
	os.Exit(90)
}

func newEditorCLIProcess(t *testing.T, home string, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestDraftCLIProcess$", "--"}, args...)...)
	command.Env = append(os.Environ(), "MAILCLI_TEST_ENTRYPOINT=1", "HOME="+home, "TMPDIR="+home)
	command.WaitDelay = time.Second
	return command
}

func runEditorCLI(t *testing.T, home string, args ...string) (editorCLIResponse, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	command := newEditorCLIProcess(t, home, args...)
	command.Stdout, command.Stderr = &stdout, &stderr
	var finishTerminal func()
	if len(args) >= 2 && args[0] == "drafts" && args[1] == "edit" {
		finishTerminal = attachEditorCLITerminal(t, command, &stderr)
	}
	err := command.Run()
	if finishTerminal != nil {
		finishTerminal()
	}
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	var response editorCLIResponse
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("one JSON envelope required: error=%v code=%d stdout=%q stderr=%q", err, code, &stdout, &stderr)
	}
	return response, stderr.String(), code
}

func openEditorCLITerminal(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := master.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Error(err)
		}
	})
	for _, request := range []uint{unix.TIOCPTYGRANT, unix.TIOCPTYUNLK} {
		if err := unix.IoctlSetInt(int(master.Fd()), request, 0); err != nil {
			t.Fatal(err)
		}
	}
	var name [128]byte
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0])))
	end := bytes.IndexByte(name[:], 0)
	if errno != 0 || end < 1 {
		t.Fatalf("PTY slave name: errno=%v end=%d", errno, end)
	}
	slave, err := os.OpenFile(string(name[:end]), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := slave.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Error(err)
		}
	})
	return master, slave
}

func attachEditorCLITerminal(t *testing.T, command *exec.Cmd, diagnostics io.Writer) func() {
	t.Helper()
	master, slave := openEditorCLITerminal(t)
	attributes, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TIOCGETA)
	if err != nil {
		t.Fatal(err)
	}
	attributes.Oflag &^= unix.ONLCR
	if err := unix.IoctlSetTermios(int(slave.Fd()), unix.TIOCSETA, attributes); err != nil {
		t.Fatal(err)
	}
	command.Stdin, command.Stderr = slave, slave
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	drained := make(chan error, 1)
	go func() { _, err := io.Copy(diagnostics, master); drained <- err }()
	closed := false
	finish := func() {
		if closed {
			return
		}
		closed = true
		if err := master.Close(); err != nil {
			t.Error(err)
		}
		if err := <-drained; err != nil && !errors.Is(err, os.ErrClosed) && !errors.Is(err, syscall.EIO) {
			t.Errorf("terminal diagnostics: %v", err)
		}
	}
	t.Cleanup(finish)
	return finish
}

func TestDraftEditorCLISeparatesNoisyOutputAndRetainsFailure(t *testing.T) {
	for _, test := range []struct {
		name     string
		exit     string
		wantCode int
	}{
		{name: "success", exit: "0"},
		{name: "failure", exit: "23", wantCode: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			created, diagnostics, code := runEditorCLI(t, home, "drafts", "create", "--to", "recipient@example.com", "--body", "original", "--json")
			if code != 0 || !created.OK || diagnostics != "" || created.Data.Draft.Ref == "" {
				t.Fatalf("create: %+v diagnostics=%q code=%d", created, diagnostics, code)
			}
			editor := filepath.Join(home, "editor.sh")
			script := "#!/bin/sh\nprintf 'editor stdout %s\\n' \"$1\"\nprintf 'editor stderr\\n' >&2\nprintf '%s' '{\"body\":\"edited\",\"to\":[{\"address\":\"recipient@example.com\"}]}' > \"$1\"\nexit " + test.exit + "\n"
			if err := os.WriteFile(editor, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			response, diagnostics, code := runEditorCLI(t, home, "drafts", "edit", "--ref", created.Data.Draft.Ref, "--editor", editor, "--json")
			if code != test.wantCode || !strings.Contains(diagnostics, "editor stdout ") || !strings.Contains(diagnostics, "editor stderr\n") {
				t.Fatalf("edit: error=%+v diagnostics=%q code=%d", response.Error, diagnostics, code)
			}
			stored, _, inspectCode := runEditorCLI(t, home, "drafts", "inspect", "--ref", created.Data.Draft.Ref, "--view", "full", "--json")
			if inspectCode != 0 {
				t.Fatal("inspect failed")
			}
			if test.wantCode == 0 {
				if !response.OK || stored.Data.Draft.Body != "edited" {
					t.Fatalf("successful edit not stored: %+v", stored)
				}
				return
			}
			if response.OK || response.Error == nil || response.Error.Code != "editor_failed" || response.Error.Editor == nil ||
				response.Error.Editor.ExitCode == nil || *response.Error.Editor.ExitCode != 23 || stored.Data.Draft.Revision != created.Data.Draft.Revision ||
				response.Error.Editor.Ref != created.Data.Draft.Ref || response.Error.Editor.ExpectedRevision != created.Data.Draft.Revision ||
				response.Error.Guidance == nil || response.Error.Guidance.ReplayAllowed || response.Error.Guidance.EffectCertainty != mail.EffectNone ||
				response.Error.Guidance.Recovery.Command != "drafts.inspect" {
				t.Fatalf("failed edit lost state or status: response=%+v stored=%+v", response, stored)
			}
			assertEditorCandidate(t, response.Error.Editor.CandidatePath, "edited")
		})
	}
}

func TestDraftEditorCLIEchoReproduction(t *testing.T) {
	home := t.TempDir()
	created, _, code := runEditorCLI(t, home, "drafts", "create", "--to", "recipient@example.com", "--body", "unchanged", "--json")
	if code != 0 {
		t.Fatal("create failed")
	}
	result, diagnostics, code := runEditorCLI(t, home, "drafts", "edit", "--ref", created.Data.Draft.Ref, "--editor", "/bin/echo", "--json")
	if code != 0 || !result.OK || result.Data.Draft.Revision != created.Data.Draft.Revision {
		t.Fatalf("echo edit: code=%d error=%+v", code, result.Error)
	}
	path := strings.TrimSpace(diagnostics)
	if !strings.HasPrefix(path, home+string(os.PathSeparator)) || filepath.Base(path) != "draft.json" {
		t.Fatalf("echo diagnostics=%q", diagnostics)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful candidate was not cleaned: %v", err)
	}
}

func assertEditorCandidate(t *testing.T, path string, body string) {
	t.Helper()
	payload, err := os.ReadFile(path)
	var input mail.DraftInput
	if err != nil || json.Unmarshal(payload, &input) != nil || input.Body != body {
		t.Fatalf("candidate %q payload=%q error=%v", path, payload, err)
	}
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{{path, 0o600}, {filepath.Dir(path), 0o700}} {
		info, err := os.Stat(item.path)
		if err != nil || info.Mode().Perm() != item.mode {
			t.Fatalf("candidate permissions: %s info=%v error=%v", item.path, info, err)
		}
	}
}

func TestDraftEditorCLICancellationKeepsJSONAndCandidate(t *testing.T) {
	home := t.TempDir()
	created, _, code := runEditorCLI(t, home, "drafts", "create", "--to", "recipient@example.com", "--body", "original", "--json")
	if code != 0 {
		t.Fatal("create failed")
	}
	editor := filepath.Join(home, "editor.sh")
	ready := filepath.Join(home, "ready")
	script := "#!/bin/sh\ntrap 'printf late-output; exit 0' TERM\nprintf '%s' '{\"body\":\"interrupted\",\"to\":[]}' > \"$1\"\nprintf noisy-output\nprintf noisy-error >&2\nprintf '%s' \"$$\" > \"$HOME/ready\"\nwhile :; do /bin/sleep 1; done\n"
	if err := os.WriteFile(editor, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	command := newEditorCLIProcess(t, home, "drafts", "edit", "--ref", created.Data.Draft.Ref, "--editor", editor, "--json")
	command.Stdout, command.Stderr = &stdout, &stderr
	finishTerminal := attachEditorCLITerminal(t, command, &stderr)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	waited := false
	defer func() {
		if waited {
			return
		}
		if err := command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		<-done
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("editor never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	started := time.Now()
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		waited = true
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("cancel exit=%v", err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("editor cancellation did not finish")
	}
	finishTerminal()
	var response editorCLIResponse
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || response.OK || response.Error == nil ||
		response.Error.Code != "editor_canceled" || response.Error.Editor == nil {
		t.Fatalf("cancel: error=%v stdout=%q stderr=%q", err, &stdout, &stderr)
	}
	if !strings.Contains(stderr.String(), "noisy-output") || !strings.Contains(stderr.String(), "noisy-error") || !strings.Contains(stderr.String(), "late-output") {
		t.Fatalf("editor diagnostics lost: %q", &stderr)
	}
	assertEditorCandidate(t, response.Error.Editor.CandidatePath, "interrupted")
	stored, _, code := runEditorCLI(t, home, "drafts", "inspect", "--ref", created.Data.Draft.Ref, "--json")
	if code != 0 || stored.Data.Draft.Revision != created.Data.Draft.Revision {
		t.Fatal("canceled edit changed stored draft")
	}
	t.Logf("cancellation=%s stdout=%d bytes stderr=%d bytes", time.Since(started), stdout.Len(), stderr.Len())
}
