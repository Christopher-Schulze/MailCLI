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
	"syscall"
	"testing"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/mailstore"
)

type localRefStoreReadGateway struct {
	testGateway
	store mailstore.Store
}

func (g *localRefStoreReadGateway) OpenDraft(ctx context.Context, ref string) (mail.Message, error) {
	return g.store.GetMessage(ctx, ref)
}

func TestDraftOpenRejectsLocalDraftRefWhileInspectReviewsIt(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(&localRefStoreReadGateway{}, t.TempDir())
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{To: []mail.Recipient{{Address: "recipient@example.com"}}, Body: "local body"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		command   string
		wantCode  int
		errorCode string
	}{
		{command: "open", wantCode: 2, errorCode: "invalid_reference"},
		{command: "inspect"},
		{command: "preview"},
	} {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), service, []string{"drafts", test.command, "--ref", draft.Ref, "--json"}, &stdout, &stderr)
		var response envelope
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if code != test.wantCode || stderr.Len() != 0 {
			t.Fatalf("%s code=%d response=%+v stderr=%s", test.command, code, response, &stderr)
		}
		if test.errorCode != "" {
			if response.OK || response.Error == nil || response.Error.Code != test.errorCode {
				t.Fatalf("response=%+v", response)
			}
		} else if !response.OK || (response.Data.Draft == nil && response.Data.DraftPreview == nil) {
			t.Fatalf("local review missing: %+v", response)
		}
	}
}

func TestDraftEditAudienceAndNonTerminalRecovery(t *testing.T) {
	manifest := mustCapabilities(t)
	for _, command := range manifest.Commands {
		want := commandAudienceAgent
		if command.ID == "drafts.edit" {
			want = commandAudienceHuman
		}
		if command.Audience != want {
			t.Fatalf("%s audience=%s, want %s", command.ID, command.Audience, want)
		}
	}
	root := t.TempDir()
	service := mail.NewServiceWithDraftRoot(nil, root)
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{To: []mail.Recipient{{Address: "recipient@example.com"}}, Body: "reviewed body"}})
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := input.Close(); err != nil {
			t.Error(err)
		}
	})
	oldInput := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = oldInput })
	before, err := os.ReadFile(filepath.Join(root, draft.Ref+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), service, []string{"drafts", "edit", "--ref", draft.Ref, "--editor", "/absent-editor", "--json"}, &stdout, &stderr)
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if code != 1 || stderr.Len() != 0 || response.Error == nil || response.Error.Code != "interactive_required" || response.Error.DraftEditor != nil {
		t.Fatalf("code=%d response=%+v stderr=%s", code, response, &stderr)
	}
	after, err := os.ReadFile(filepath.Join(root, draft.Ref+".json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("draft changed: %v", err)
	}
	guidance := response.Error.Guidance
	if guidance == nil || guidance.ReplayAllowed || guidance.EffectCertainty != mail.EffectNone || guidance.Recovery.Command != "drafts.update" {
		t.Fatalf("guidance=%+v", guidance)
	}
	updateInput := filepath.Join(t.TempDir(), "update.json")
	if err := os.WriteFile(updateInput, []byte(`{"subject":"authorized update"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, err := os.Open(updateInput)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stdin.Close(); err != nil {
			t.Error(err)
		}
	})
	os.Stdin = stdin
	stdout.Reset()
	args := append([]string{"drafts", "update"}, guidance.Recovery.Args...)
	if code := Run(context.Background(), service, args, &stdout, &stderr); code != 0 {
		t.Fatalf("recovery code=%d output=%s stderr=%s", code, &stdout, &stderr)
	}
	updated, err := service.GetDraft(draft.Ref)
	if err != nil || updated.Subject != "authorized update" || updated.Body != draft.Body || updated.Revision == draft.Revision {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
}

// Existing editor regressions must exercise the public human route with a real
// controlling terminal; this helper never changes production terminal checks.
func runDraftEditWithTestTerminal(t *testing.T, root string, args []string, stdout, stderr io.Writer) int {
	t.Helper()
	master, slave := openEditorTestTerminal(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	commandArgs := append([]string{"-test.run=^TestDraftAudienceTerminalProcess$", "--"}, args...)
	process := exec.CommandContext(ctx, os.Args[0], commandArgs...)
	process.Env = append(os.Environ(), "MAILCLI_AUDIENCE_EDITOR_ROOT="+root)
	process.Stdin, process.Stderr, process.Stdout = slave, slave, stdout
	process.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	drained := make(chan error, 1)
	go func() {
		_, err := io.Copy(stderr, master)
		drained <- err
	}()
	err := process.Wait()
	if closeErr := master.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if drainErr := <-drained; drainErr != nil && !errors.Is(drainErr, os.ErrClosed) && !errors.Is(drainErr, syscall.EIO) {
		t.Fatalf("terminal diagnostics: %v", drainErr)
	}
	if err != nil {
		if process.ProcessState == nil || !process.ProcessState.Exited() {
			t.Fatalf("terminal command: %v", err)
		}
	}
	return process.ProcessState.ExitCode()
}

func TestDraftAudienceTerminalProcess(t *testing.T) {
	root := os.Getenv("MAILCLI_AUDIENCE_EDITOR_ROOT")
	if root == "" {
		return
	}
	separator := -1
	for i, value := range os.Args {
		if value == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		t.Fatal("missing command arguments")
	}
	service := mail.NewServiceWithDraftRoot(nil, root)
	os.Exit(Run(context.Background(), service, os.Args[separator+1:], os.Stdout, os.Stderr))
}
