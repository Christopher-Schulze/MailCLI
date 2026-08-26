package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

type postflightSaveGateway struct{ testGateway }

func (postflightSaveGateway) SaveDraft(context.Context, mail.Draft) (mail.MessageSummary, error) {
	return mail.MessageSummary{Ref: "msg_saved", Subject: "Saved"}, &mail.OperationError{
		Code: "bridge_cleanup_failed", Message: "private bridge request remained",
	}
}

type postflightSendGateway struct{ testGateway }

func (postflightSendGateway) SendDraft(context.Context, mail.Draft) (mail.SendEvidence, error) {
	return mail.SendEvidence{
		InvocationStarted: true, AcceptedByMail: true, SentStoreObserved: true,
		ObservedMessageRef: "msg_sent",
	}, &mail.OperationError{
		Code: "attachment_snapshot_cleanup_failed", Message: "private attachment snapshot remained",
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

func TestDraftSendReturnsObservedResultWithPostflightError(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(
		postflightSendGateway{}, filepath.Join(t.TempDir(), "drafts"),
	)
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		From: "mail@example.com", To: []mail.Recipient{{Address: "recipient@example.com"}},
		Subject: "Sent", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runDraftSend(
		context.Background(), service,
		[]string{"--ref", draft.Ref, "--confirm", "--json"}, &stdout, &stderr,
	)
	if code != 1 || !strings.Contains(stdout.String(), `"code":"send_postflight_failed"`) ||
		!strings.Contains(stdout.String(), `"outcome":"sent_store_observed"`) {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}
