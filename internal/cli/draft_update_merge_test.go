package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

func TestDraftUpdateMergesUnsetFlagFields(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(nil, t.TempDir())
	attachment := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(attachment, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		To:      []mail.Recipient{{Address: "to@example.com"}},
		CC:      []mail.Recipient{{Address: "cc@example.com"}},
		BCC:     []mail.Recipient{{Address: "bcc@example.com"}},
		Subject: "original", Body: "kept body", BodyFormat: mail.DraftBodyPlain,
		Attachments: []string{attachment},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), service, []string{
		"drafts", "update", "--ref", draft.Ref, "--expected-revision", draft.Revision,
		"--subject", "changed", "--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("drafts update code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	updated, err := service.GetDraft(draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Subject != "changed" || updated.Body != "kept body" ||
		len(updated.To) != 1 || updated.To[0].Address != "to@example.com" ||
		len(updated.CC) != 1 || updated.CC[0].Address != "cc@example.com" ||
		len(updated.BCC) != 1 || updated.BCC[0].Address != "bcc@example.com" ||
		len(updated.Attachments) != 1 || updated.Attachments[0].Path != attachment {
		t.Fatalf("merged draft = %+v", updated)
	}
	if updated.Revision == draft.Revision {
		t.Fatal("revision did not change")
	}
}

func TestDraftUpdateJSONMergeKeepsOmittedFields(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(nil, t.TempDir())
	attachment := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(attachment, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		To: []mail.Recipient{{Address: "to@example.com"}}, Subject: "original",
		Body: "kept body", Attachments: []string{attachment},
	}})
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "update.json")
	if err := os.WriteFile(input, []byte(`{"subject":"via json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), service, []string{
		"drafts", "update", "--ref", draft.Ref, "--expected-revision", draft.Revision,
		"--input", input, "--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("drafts update code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	updated, err := service.GetDraft(draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Subject != "via json" || updated.Body != "kept body" ||
		len(updated.To) != 1 || len(updated.Attachments) != 1 {
		t.Fatalf("merged draft = %+v", updated)
	}
}

func TestDraftUpdateExplicitEmptyClearsFields(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(nil, t.TempDir())
	attachment := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(attachment, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		To: []mail.Recipient{{Address: "to@example.com"}},
		CC: []mail.Recipient{{Address: "cc@example.com"}}, Subject: "original",
		Body: "kept body", Attachments: []string{attachment},
	}})
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "update.json")
	if err := os.WriteFile(input, []byte(`{"subject":"","cc":[],"attachments":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), service, []string{
		"drafts", "update", "--ref", draft.Ref, "--expected-revision", draft.Revision,
		"--input", input, "--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("drafts update code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	updated, err := service.GetDraft(draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Subject != "" || len(updated.CC) != 0 || len(updated.Attachments) != 0 ||
		updated.Body != "kept body" || len(updated.To) != 1 {
		t.Fatalf("cleared draft = %+v", updated)
	}
}

func TestDraftUpdateBodyFormatChangeRequiresBody(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(nil, t.TempDir())
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		To: []mail.Recipient{{Address: "to@example.com"}}, Body: "kept body",
	}})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), service, []string{
		"drafts", "update", "--ref", draft.Ref, "--expected-revision", draft.Revision,
		"--format", "markdown", "--json",
	}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String()+stdout.String(), "requires a new body") {
		t.Fatalf("drafts update code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	unchanged, err := service.GetDraft(draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Revision != draft.Revision || unchanged.BodyFormat != mail.DraftBodyPlain {
		t.Fatalf("draft changed despite rejected update: %+v", unchanged)
	}
}

func TestMergeDraftUpdateInputPreservesEachUnsetField(t *testing.T) {
	current := mail.Draft{
		AccountRef: "acct", From: "from@example.com",
		To:      []mail.Recipient{{Address: "to@example.com"}},
		CC:      []mail.Recipient{{Address: "cc@example.com"}},
		BCC:     []mail.Recipient{{Address: "bcc@example.com"}},
		Subject: "subject", Body: "plain body", BodyFormat: mail.DraftBodyPlain,
		Attachments: []mail.DraftAttachment{{Path: "/a"}},
	}
	merged, err := mergeDraftUpdateInput(current, mail.DraftInput{})
	if err != nil {
		t.Fatal(err)
	}
	if merged.AccountRef != "acct" || merged.From != "from@example.com" ||
		len(merged.To) != 1 || len(merged.CC) != 1 || len(merged.BCC) != 1 ||
		merged.Subject != "subject" || merged.Body != "plain body" ||
		merged.BodyFormat != mail.DraftBodyPlain ||
		len(merged.Attachments) != 1 || merged.Attachments[0] != "/a" {
		t.Fatalf("merged input = %+v", merged)
	}

	rich := mail.Draft{
		To:   []mail.Recipient{{Address: "to@example.com"}},
		Body: "rendered", BodySource: "**rendered**", BodyFormat: mail.DraftBodyMarkdown,
	}
	merged, err = mergeDraftUpdateInput(rich, mail.DraftInput{Subject: "x", SubjectSet: true})
	if err != nil {
		t.Fatal(err)
	}
	if merged.Body != "**rendered**" || merged.BodyFormat != mail.DraftBodyMarkdown {
		t.Fatalf("rich source not preserved: %+v", merged)
	}
}
