package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

func TestDraftRevisionRemainsInEveryReviewProjection(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(nil, t.TempDir())
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		To: []mail.Recipient{{Address: "recipient@example.com"}}, Body: "**Reviewed**", BodyFormat: mail.DraftBodyMarkdown,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"drafts", "inspect", "--view", "metadata"},
		{"drafts", "inspect", "--view", "plain"},
		{"drafts", "inspect", "--view", "full"},
		{"drafts", "inspect", "--fields", "body"},
		{"drafts", "preview", "--format", "plain"},
		{"drafts", "preview", "--format", "source"},
		{"drafts", "preview", "--format", "html"},
	} {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), service, append(args, "--ref", draft.Ref, "--json"), &stdout, &stderr)
		var result envelope
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || code != 0 || stderr.Len() != 0 {
			t.Fatalf("review %v: code=%d error=%v stdout=%s stderr=%s", args, code, err, &stdout, &stderr)
		}
		revision := ""
		if result.Data.Draft != nil {
			revision = result.Data.Draft.Revision
		} else if result.Data.DraftPreview != nil {
			revision = result.Data.DraftPreview.Revision
		}
		if revision == "" || revision != draft.Revision {
			t.Fatalf("review %v revision=%q, want %q", args, revision, draft.Revision)
		}
	}
}

func TestDraftRevisionHandshakeIsRequiredByHelpSchemaAndCLI(t *testing.T) {
	for _, command := range []string{"update", "send"} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), nil, []string{"drafts", command, "--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "expected-revision") {
			t.Fatalf("%s help code=%d stdout=%s stderr=%s", command, code, &stdout, &stderr)
		}
		flag := schemaFlagsByName(decodeTestCommandSchema(t, schemaForCommand("drafts."+command)))["--expected-revision"]
		if !flag.Required || !flag.TakesValue || !flag.ValueRequired {
			t.Fatalf("%s expected revision schema = %+v", command, flag)
		}
		stdout.Reset()
		stderr.Reset()
		args := []string{"drafts", command, "--ref", "draft_ref", "--json"}
		if command == "send" {
			args = append(args, "--confirm")
		} else {
			args = append(args, "--input", "-")
		}
		// Nil service and unread stdin prove missing revision fails before
		// retrieval, editor input consumption, or any transport dependency.
		code := Run(context.Background(), nil, args, &stdout, &stderr)
		if code != 2 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "missing required --expected-revision") {
			t.Fatalf("%s missing revision code=%d stdout=%s stderr=%s", command, code, &stdout, &stderr)
		}
	}
}

func TestDraftEditorConflictPreservesConcurrentUpdateAndCandidate(t *testing.T) {
	root := t.TempDir()
	service := mail.NewServiceWithDraftRoot(nil, root)
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		To: []mail.Recipient{{Address: "recipient@example.com"}}, Subject: "Original A", Body: "Original body",
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAILCLI_REVISION_EDITOR_ROOT", root)
	t.Setenv("MAILCLI_REVISION_EDITOR_REF", draft.Ref)
	t.Setenv("MAILCLI_REVISION_EDITOR_EXPECTED", draft.Revision)
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), service, []string{
		"drafts", "edit", "--ref", draft.Ref, "--editor", os.Args[0],
		"--editor-arg=-test.run=TestDraftRevisionEditorProcess", "--editor-arg=--", "--json",
	}, &stdout, &stderr)
	var result envelope
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || code != 1 || stderr.Len() != 0 {
		t.Fatalf("editor code=%d error=%v stdout=%s stderr=%s", code, err, &stdout, &stderr)
	}
	if result.Error == nil || result.Error.Code != "draft_revision_conflict" || result.Error.DraftRevisionConflict == nil {
		t.Fatalf("editor conflict missing: %+v", result.Error)
	}
	conflict := result.Error.DraftRevisionConflict
	if conflict.CandidatePath == "" {
		t.Fatal("editor candidate path missing")
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(filepath.Dir(conflict.CandidatePath)); err != nil {
			t.Error(err)
		}
	})
	current, err := service.GetDraft(draft.Ref)
	if err != nil || current.Subject != "Concurrent B" || current.Body != "Concurrent body" || current.Revision != conflict.CurrentRevision || conflict.ExpectedRevision != draft.Revision {
		t.Fatalf("concurrent update lost: draft=%+v conflict=%+v error=%v", current, conflict, err)
	}
	candidate, err := readDraftInput(conflict.CandidatePath)
	if err != nil || candidate.Subject != "Editor candidate A" || candidate.Body != "Candidate body" {
		t.Fatalf("candidate=%+v error=%v", candidate, err)
	}
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{
		{conflict.CandidatePath, 0o600}, {filepath.Dir(conflict.CandidatePath), 0o700},
	} {
		info, err := os.Stat(item.path)
		if err != nil || info.Mode().Perm() != item.mode {
			t.Fatalf("candidate permissions for %s: info=%v error=%v", item.path, info, err)
		}
	}
	if result.Error.Guidance == nil || result.Error.Guidance.ReplayAllowed || result.Error.Guidance.Recovery.Command != "drafts.inspect" {
		t.Fatalf("conflict recovery guidance=%+v", result.Error.Guidance)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run(context.Background(), service, []string{"drafts", "update", "--ref", draft.Ref,
		"--expected-revision", current.Revision, "--input", conflict.CandidatePath, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("explicit candidate retry code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	merged, err := service.GetDraft(draft.Ref)
	if err != nil || merged.Body != candidate.Body || merged.Subject != candidate.Subject || merged.Revision == current.Revision {
		t.Fatalf("explicit retry=%+v error=%v", merged, err)
	}
}

func TestDraftRevisionEditorProcess(t *testing.T) {
	root := os.Getenv("MAILCLI_REVISION_EDITOR_ROOT")
	if root == "" {
		return
	}
	path := os.Args[len(os.Args)-1]
	candidate, err := readDraftInput(path)
	if err != nil {
		os.Exit(2)
	}
	service := mail.NewServiceWithDraftRoot(nil, root)
	_, err = service.UpdateDraft(mail.UpdateDraftRequest{
		Ref: os.Getenv("MAILCLI_REVISION_EDITOR_REF"), ExpectedRevision: os.Getenv("MAILCLI_REVISION_EDITOR_EXPECTED"),
		Input: mail.DraftInput{To: candidate.To, Subject: "Concurrent B", Body: "Concurrent body"},
	})
	if err != nil {
		os.Exit(3)
	}
	candidate.Subject, candidate.Body = "Editor candidate A", "Candidate body"
	payload, err := json.Marshal(candidate)
	if err != nil {
		os.Exit(4)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		os.Exit(5)
	}
	os.Exit(0)
}
