package cli

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

func TestDraftEditorUsesInjectedWriters(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "json"}[jsonOutput], func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			command := exec.CommandContext(context.Background(), "/bin/sh", "-c", "printf output; printf diagnostic >&2")
			err := runDraftEditor(context.Background(), command, draftEditorStreams{stdout: &stdout, stderr: &stderr, jsonOutput: jsonOutput})
			if err != nil {
				t.Fatal(err)
			}
			if jsonOutput {
				if stdout.Len() != 0 || !strings.Contains(stderr.String(), "output") || !strings.Contains(stderr.String(), "diagnostic") {
					t.Fatalf("machine streams: stdout=%q stderr=%q", &stdout, &stderr)
				}
			} else if stdout.String() != "output" || stderr.String() != "diagnostic" {
				t.Fatalf("human streams: stdout=%q stderr=%q", &stdout, &stderr)
			}
		})
	}
}

func openEditorTestTerminal(t *testing.T) (*os.File, *os.File) {
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
	if errno != 0 {
		t.Fatal(errno)
	}
	end := bytes.IndexByte(name[:], 0)
	if end < 1 {
		t.Fatal("invalid PTY slave name")
	}
	slave, err := os.OpenFile(string(name[:end]), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := slave.Close(); err != nil {
			t.Error(err)
		}
	})
	return master, slave
}

func TestDraftEditorTerminalIOAndRestoration(t *testing.T) {
	for _, mode := range []string{"json", "human", "vi", "cancel", "unsupported", "start-failure"} {
		t.Run(mode, func(t *testing.T) {
			master, slave := openEditorTestTerminal(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDraftEditorTerminalHelperProcess$")
			root := t.TempDir()
			command.Env = append(os.Environ(), "MAILCLI_TEST_EDITOR_TERMINAL="+mode, "MAILCLI_TEST_EDITOR_ROOT="+root, "TMPDIR="+root, "HOME="+root)
			command.Stdin, command.Stderr = slave, slave
			var stdout bytes.Buffer
			command.Stdout = &stdout
			command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			ready, drained := drainEditorTestTerminal(master)
			waited := false
			defer func() {
				if err := master.Close(); err != nil {
					t.Error(err)
				}
				<-drained
				cancel()
				if !waited {
					<-done
				}
			}()
			// Keep draining until the process exits: macOS terminal teardown
			// can wait for unread output even after the child has been killed.
			if mode != "unsupported" && mode != "start-failure" {
				select {
				case seen := <-ready:
					if !strings.Contains(seen, "EDITOR_READY") {
						t.Fatal(seen)
					}
				case err := <-done:
					waited = true
					t.Fatalf("terminal helper exited before prompt: %v stdout=%q", err, &stdout)
				case <-ctx.Done():
					t.Fatal("terminal editor did not display its prompt")
				}
				input := "saved\n"
				switch mode {
				case "cancel":
					input = "\x03"
				case "vi":
					input = "\x1b:%s/original/saved/g\n:wq\n"
				}
				if _, err := io.WriteString(master, input); err != nil {
					t.Fatal(err)
				}
			}
			err := <-done
			waited = true
			if err != nil {
				t.Fatalf("terminal helper: %v stdout=%q", err, &stdout)
			}
			if stdout.String() != "TERMINAL_RESTORED\n" {
				t.Fatalf("terminal proof missing: %q", &stdout)
			}
		})
	}
}

func drainEditorTestTerminal(master *os.File) (<-chan string, <-chan struct{}) {
	ready := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var seen strings.Builder
		announced := false
		buffer := make([]byte, 4096)
		for {
			count, err := master.Read(buffer)
			if err != nil {
				if !announced {
					ready <- seen.String() + " READ_ERROR: " + err.Error()
				}
				return
			}
			if !announced {
				seen.Write(buffer[:count])
				if strings.Contains(seen.String(), "EDITOR_READY") {
					ready <- seen.String()
					announced = true
				}
			}
		}
	}()
	return ready, done
}

func TestDraftEditorTerminalHelperProcess(t *testing.T) {
	mode := os.Getenv("MAILCLI_TEST_EDITOR_TERMINAL")
	if mode == "" {
		return
	}
	root := os.Getenv("MAILCLI_TEST_EDITOR_ROOT")
	initial, err := unix.IoctlGetTermios(0, unix.TIOCGETA)
	if err != nil {
		t.Fatal(err)
	}
	service := mail.NewServiceWithDraftRoot(nil, filepath.Join(root, "drafts"))
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{To: []mail.Recipient{{Address: "recipient@example.com"}}, Body: "original"}})
	if err != nil {
		t.Fatal(err)
	}
	editor := filepath.Join(root, "editor.sh")
	script := "#!/bin/sh\nstty -echo\nprintf EDITOR_READY\nIFS= read -r answer || exit 31\n[ \"$answer\" = saved ] || exit 32\nprintf '%s' '{\"body\":\"saved\",\"to\":[{\"address\":\"recipient@example.com\"}]}' > \"$1\"\n"
	if err := os.WriteFile(editor, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var output io.Writer = &stdout
	var diagnostics io.Writer = os.Stderr
	args := []string{"drafts", "edit", "--ref", draft.Ref, "--editor", editor}
	if mode == "human" {
		output = os.Stderr
	} else {
		args = append(args, "--json")
	}
	if mode == "unsupported" {
		diagnostics = &stderr
	}
	if mode == "start-failure" {
		args[5] = filepath.Join(root, "absent-editor")
	}
	if mode == "vi" {
		args[5] = "/usr/bin/vi"
		for _, argument := range []string{"--clean", "-n", "-T", "vt100", "-c", `echo "EDITOR_READY"`} {
			args = append(args, "--editor-arg="+argument)
		}
	}
	code := Run(context.Background(), service, args, output, diagnostics)
	group, err := unix.IoctlGetInt(0, unix.TIOCGPGRP)
	if err != nil || group != syscall.Getpgrp() {
		t.Fatalf("foreground not restored: group=%d error=%v", group, err)
	}
	current, err := unix.IoctlGetTermios(0, unix.TIOCGETA)
	if err != nil || *current != *initial {
		t.Fatalf("terminal settings not restored: initial=%+v current=%+v error=%v", initial, current, err)
	}
	stored, err := service.GetDraft(draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if mode == "human" || mode == "json" || mode == "vi" {
		if code != 0 || stored.Body != "saved" {
			t.Fatalf("terminal save: code=%d stdout=%s body=%q", code, &stdout, stored.Body)
		}
	} else {
		var response envelope
		wantCode := map[string]string{"cancel": "editor_canceled", "unsupported": "editor_terminal_unavailable", "start-failure": "editor_failed"}[mode]
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != 1 || response.Error == nil || response.Error.Code != wantCode || stored.Revision != draft.Revision {
			t.Fatalf("terminal failure: code=%d error=%v stdout=%s", code, err, &stdout)
		}
	}
	if _, err := io.WriteString(os.Stdout, "TERMINAL_RESTORED\n"); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestDraftEditorRetainsInvalidCandidate(t *testing.T) {
	for _, contents := range []string{`{"body":"edited","Body":"duplicate"}`, `{"body":"edited","to":[]}`} {
		t.Run(contents, func(t *testing.T) {
			service := mail.NewServiceWithDraftRoot(nil, t.TempDir())
			draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{To: []mail.Recipient{{Address: "recipient@example.com"}}, Body: "original"}})
			if err != nil {
				t.Fatal(err)
			}
			editor := filepath.Join(t.TempDir(), "editor.sh")
			writeExecutableTestScript(t, editor, "#!/bin/sh\nprintf '%s' '"+contents+"' > \"$1\"\n")
			_, err = editDraftInput(context.Background(), service, draft.Ref, draft.Revision, draftInputFromStored(draft), editor, nil, draftEditorStreams{})
			var editorErr *draftEditorError
			if !errors.As(err, &editorErr) {
				t.Fatalf("missing candidate evidence: %v", err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(filepath.Dir(editorErr.evidence.CandidatePath)); err != nil {
					t.Error(err)
				}
			})
			payload, err := os.ReadFile(editorErr.evidence.CandidatePath)
			if err != nil || string(payload) != contents {
				t.Fatalf("candidate lost: %q error=%v", payload, err)
			}
			stored, err := service.GetDraft(draft.Ref)
			if err != nil || stored.Revision != draft.Revision {
				t.Fatal("invalid candidate changed draft")
			}
		})
	}
}
