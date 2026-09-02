package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/compose"
	"mailcli/internal/mail"
)

type postflightSaveGateway struct{ testGateway }

func (postflightSaveGateway) SaveDraft(context.Context, mail.Draft) (mail.MessageSummary, error) {
	return mail.MessageSummary{Ref: "msg_saved", Subject: "Saved"}, &mail.OperationError{
		Code: "bridge_cleanup_failed", Message: "private bridge request remained",
	}
}

func TestDecodeDraftInputStrictTable(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "valid", input: `{"to":[{"address":"ada@example.com"}],"subject":"Hello","body":"Line one\\nLine two"}`},
		{name: "explicit empty body", input: `{"to":[],"body":""}`},
		{name: "missing body", input: `{"to":[{"address":"ada@example.com"}]}`, wantErr: true},
		{name: "null body", input: `{"to":[],"body":null}`, wantErr: true},
		{name: "unknown field", input: `{"to":[],"body":"","html":"no"}`, wantErr: true},
		{name: "trailing object", input: `{"to":[],"body":""} {}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input, err := decodeDraftInput(strings.NewReader(test.input))
			if (err != nil) != test.wantErr {
				t.Fatalf("decodeDraftInput() input = %+v, error = %v", input, err)
			}
		})
	}
}

func TestDraftCreateAcceptsTerminalNativeInput(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(testGateway{}, filepath.Join(t.TempDir(), "drafts"))
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runDraftCreate(service, []string{
		"--to", "Ada Lovelace <ada@example.com>",
		"--subject", "Terminal draft",
		"--body", "**Hello**",
		"--format", "markdown",
		"--json",
	}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	draft := response.Data.Draft
	if draft == nil || draft.BodyFormat != mail.DraftBodyMarkdown || draft.Body != "Hello" ||
		len(draft.To) != 1 || draft.To[0].Name != "Ada Lovelace" {
		t.Fatalf("draft = %+v", draft)
	}
}

func TestDraftInputRejectsMixedJSONAndNativeFlags(t *testing.T) {
	flags := newFlagSet("test", &bytes.Buffer{})
	options := registerDraftInputFlags(flags)
	if err := flags.Parse([]string{"--input", "draft.json", "--body", "Body"}); err != nil {
		t.Fatal(err)
	}
	if _, err := options.read(); err == nil {
		t.Fatal("read() error = nil")
	}
}

func TestDraftEditValidatesThenAtomicallyUpdates(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(testGateway{}, filepath.Join(t.TempDir(), "drafts"))
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		To: []mail.Recipient{{Address: "ada@example.com"}}, Subject: "Before", Body: "Body",
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAILCLI_TEST_EDITOR", "1")
	updated, err := editDraftInput(
		context.Background(), service, draft.Ref, draftInputFromStored(draft), os.Args[0],
		[]string{"-test.run=TestDraftEditorHelperProcess", "--"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Ref != draft.Ref || updated.Subject != "After" {
		t.Fatalf("updated draft = %+v", updated)
	}
}

func TestDraftEditorHelperProcess(t *testing.T) {
	if os.Getenv("MAILCLI_TEST_EDITOR") != "1" {
		return
	}
	path := os.Args[len(os.Args)-1]
	payload, err := os.ReadFile(path)
	if err != nil {
		os.Exit(2)
	}
	var input mail.DraftInput
	if json.Unmarshal(payload, &input) != nil {
		os.Exit(3)
	}
	input.Subject = "After"
	updated, err := json.MarshalIndent(input, "", "  ")
	if err != nil || os.WriteFile(path, append(updated, '\n'), 0o600) != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

func TestDraftHandoffUsesValidatedVisibleComposeRequest(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(testGateway{}, filepath.Join(t.TempDir(), "drafts"))
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		To:      []mail.Recipient{{Name: "Ada", Address: "ada@example.com"}},
		Subject: "Visible", Body: "**Hello**", BodyFormat: mail.DraftBodyMarkdown,
	}})
	if err != nil {
		t.Fatal(err)
	}
	var request compose.Request
	handoff := func(_ context.Context, value compose.Request) (compose.Result, error) {
		request = value
		return compose.Result{Opened: true, MailApplication: "com.apple.mail"}, nil
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runDraftHandoffWith(
		context.Background(), service, []string{"--ref", draft.Ref, "--json"},
		&stdout, &stderr, handoff,
	)
	if code != 0 || stderr.Len() != 0 || request.Subject != "Visible" ||
		request.PlainBody != "Hello" || request.HTMLBody == "" ||
		len(request.Recipients) != 1 || request.Recipients[0] != "ada@example.com" {
		t.Fatalf("code = %d, request = %+v, stdout = %q, stderr = %q", code, request, stdout.String(), stderr.String())
	}
	if _, err := service.GetDraft(draft.Ref); err != nil {
		t.Fatalf("local draft was not retained: %v", err)
	}
}

func TestDraftSendRequiresConfirmation(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runDraftSend(
		context.Background(), newTestService(), []string{"--ref", "draft_ref", "--json"}, &stdout, &stderr,
	)
	if code != 1 || !strings.Contains(stdout.String(), `"code":"confirmation_required"`) {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestDraftSaveReturnsObservedResultWithPostflightError(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(
		postflightSaveGateway{}, filepath.Join(t.TempDir(), "drafts"),
	)
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		From: "mail@example.com", To: []mail.Recipient{{Address: "recipient@example.com"}},
		Subject: "Saved", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runDraftSave(
		context.Background(), service, []string{"--ref", draft.Ref, "--json"}, &stdout, &stderr,
	)
	if code != 1 || !strings.Contains(stdout.String(), `"code":"draft_postflight_failed"`) ||
		!strings.Contains(stdout.String(), `"saved_draft":{"local_draft_ref":"`+draft.Ref+`"`) {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}
