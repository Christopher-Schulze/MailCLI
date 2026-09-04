package mail

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

type draftGateway struct {
	gatewayStub
	saves   int
	saveErr error
}

type durableDraftSaveGateway struct {
	gatewayStub
	saveCalls      int
	reconcileCalls int
}

func (g *durableDraftSaveGateway) PrepareDraftSave(
	context.Context,
	Draft,
) (SendObservationBaseline, error) {
	return SendObservationBaseline{
		StoreUUID: "store", MaximumRowID: 10, CapturedUnix: 1, SentMailboxIDs: []int64{5},
	}, nil
}

func (g *durableDraftSaveGateway) SaveDraftWithEvidence(
	_ context.Context,
	draft Draft,
) (DraftSaveEvidence, error) {
	g.saveCalls++
	body := draft.Body
	return DraftSaveEvidence{
		InvocationStarted: true, AcceptedByMail: true,
		Materialized: &SendMaterialization{
			From: draft.From, To: draft.To, CC: draft.CC, BCC: draft.BCC,
			Subject: draft.Subject, Body: &body, AttachmentCount: len(draft.Attachments),
		},
	}, context.DeadlineExceeded
}

func (g *durableDraftSaveGateway) ReconcileDraftSave(
	_ context.Context,
	_ Draft,
	attempt DraftSaveAttempt,
) (DraftSaveEvidence, error) {
	g.reconcileCalls++
	return DraftSaveEvidence{
		InvocationStarted: true, AcceptedByMail: true,
		ObservedMessage:     MessageSummary{Ref: "msg_observed", Subject: "Saved once"},
		ObservationBaseline: attempt.ObservationBaseline, Materialized: attempt.Materialized,
	}, nil
}

func (g *draftGateway) SaveDraft(_ context.Context, draft Draft) (MessageSummary, error) {
	g.saves++
	return MessageSummary{Ref: "msg_saved", Subject: draft.Subject}, g.saveErr
}

func TestPrepareDraftRejectsResourceExhaustion(t *testing.T) {
	recipient := Recipient{Address: "recipient@example.com"}
	tests := []struct {
		name  string
		input DraftInput
	}{
		{name: "subject", input: DraftInput{
			To: []Recipient{recipient}, Subject: strings.Repeat("x", MaximumDraftSubjectBytes+1), Body: "Body",
		}},
		{name: "body", input: DraftInput{
			To: []Recipient{recipient}, Body: strings.Repeat("x", MaximumDraftBodyBytes+1),
		}},
		{name: "recipients", input: DraftInput{
			To: make([]Recipient, MaximumDraftRecipients+1), Body: "Body",
		}},
		{name: "attachment count", input: DraftInput{
			To: []Recipient{recipient}, Body: "Body",
			Attachments: make([]string, MaximumDraftAttachments+1),
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := prepareDraft(CreateDraftRequest{Input: test.input}); err == nil {
				t.Fatal("prepareDraft() error = nil")
			}
		})
	}

	attachment, err := os.CreateTemp(t.TempDir(), "oversized-attachment-")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	if err := attachment.Truncate(MaximumDraftAttachmentBytes + 1); err != nil {
		t.Fatalf("Truncate() error = %v", err)
	}
	if err := attachment.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := prepareDraft(CreateDraftRequest{Input: DraftInput{
		To: []Recipient{recipient}, Body: "Body", Attachments: []string{attachment.Name()},
	}}); err == nil {
		t.Fatal("prepareDraft(oversized attachment) error = nil")
	}
}

func TestMissingGatewayRejectsComposeWithoutPanicking(t *testing.T) {
	service := NewServiceWithDraftRoot(nil, filepath.Join(t.TempDir(), "drafts"))
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Local review", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	if _, err := service.SaveDraft(context.Background(), draft.Ref); errorCode(err) != "compose_automation_unsupported" {
		t.Fatalf("SaveDraft() error = %v", err)
	}
	if _, err := service.SendDraft(context.Background(), draft.Ref); errorCode(err) != "send_transport_unavailable" {
		t.Fatalf("SendDraft() error = %v", err)
	}
	if _, err := service.GetDraft(draft.Ref); err != nil {
		t.Fatalf("GetDraft() after blocked compose error = %v", err)
	}
}

func TestPrepareDraftHandoffRejectsUnsupportedSemantics(t *testing.T) {
	tests := []struct {
		name  string
		input DraftInput
	}{
		{name: "explicit sender", input: DraftInput{
			From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}}, Body: "Body",
		}},
		{name: "CC", input: DraftInput{
			To: []Recipient{{Address: "recipient@example.com"}}, CC: []Recipient{{Address: "cc@example.com"}}, Body: "Body",
		}},
		{name: "BCC", input: DraftInput{
			To: []Recipient{{Address: "recipient@example.com"}}, BCC: []Recipient{{Address: "bcc@example.com"}}, Body: "Body",
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := NewServiceWithDraftRoot(&gatewayStub{}, filepath.Join(t.TempDir(), "drafts"))
			draft, err := service.CreateDraft(CreateDraftRequest{Input: test.input})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.PrepareDraftHandoff(draft.Ref); err == nil {
				t.Fatal("PrepareDraftHandoff() error = nil")
			}
		})
	}
}

func TestSaveDraftPersistsToMailBeforeLocalCleanup(t *testing.T) {
	gateway := &draftGateway{}
	gateway.mailboxes = nil
	service := NewServiceWithDraftRoot(gateway, filepath.Join(t.TempDir(), "drafts"))
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		From: "mail@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Saved", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	gateway.accounts = []Account{{EmailAddresses: []string{"mail@example.com"}}}
	saved, err := service.SaveDraft(context.Background(), draft.Ref)
	if err != nil || saved.Message.Ref != "msg_saved" || gateway.saves != 1 {
		t.Fatalf("SaveDraft() = %+v, error = %v, saves = %d", saved, err, gateway.saves)
	}
	if _, err := service.GetDraft(draft.Ref); err == nil {
		t.Fatal("saved local draft still exists")
	}
}

func TestSaveDraftDurablyReconcilesWithoutDuplicateInvocation(t *testing.T) {
	gateway := &durableDraftSaveGateway{}
	gateway.accounts = []Account{{EmailAddresses: []string{"mail@example.com"}}}
	service := NewServiceWithDraftRoot(gateway, filepath.Join(t.TempDir(), "drafts"))
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		From: "mail@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Saved once", Body: strings.Repeat("body", 20*1024),
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	if _, err := service.SaveDraft(context.Background(), draft.Ref); errorCode(err) != "draft_save_outcome_unknown" {
		t.Fatalf("first SaveDraft() error = %v", err)
	}
	retained, err := service.GetDraft(draft.Ref)
	if err != nil || retained.SaveAttempt == nil || retained.SaveAttempt.Materialized == nil {
		t.Fatalf("retained draft = %+v, error = %v", retained, err)
	}
	if _, err := service.UpdateDraft(UpdateDraftRequest{Ref: draft.Ref, Input: DraftInput{
		To: draft.To, Body: draft.Body,
	}}); errorCode(err) != "draft_save_retry_blocked" {
		t.Fatalf("UpdateDraft() error = %v", err)
	}
	saved, err := service.SaveDraft(context.Background(), draft.Ref)
	if err != nil || saved.Message.Ref != "msg_observed" {
		t.Fatalf("reconciled SaveDraft() = %+v, error = %v", saved, err)
	}
	if gateway.saveCalls != 1 || gateway.reconcileCalls != 1 {
		t.Fatalf("save calls = %d, reconcile calls = %d", gateway.saveCalls, gateway.reconcileCalls)
	}
	if _, err := service.GetDraft(draft.Ref); errorCode(err) != "not_found" {
		t.Fatalf("GetDraft() after reconcile error = %v", err)
	}
}

func TestSaveDraftReportsPostflightFailureWithoutRetainingLocalDraft(t *testing.T) {
	gateway := &draftGateway{saveErr: &OperationError{
		Code: "bridge_cleanup_failed", Message: "private bridge request remained",
	}}
	service := NewServiceWithDraftRoot(gateway, filepath.Join(t.TempDir(), "drafts"))
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		From: "mail@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Saved", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	gateway.accounts = []Account{{EmailAddresses: []string{"mail@example.com"}}}
	saved, err := service.SaveDraft(context.Background(), draft.Ref)
	if errorCode(err) != "draft_postflight_failed" || saved.Message.Ref != "msg_saved" {
		t.Fatalf("SaveDraft() = %+v, error = %v", saved, err)
	}
	if _, err := service.GetDraft(draft.Ref); err == nil {
		t.Fatal("observed native draft left a retryable local draft")
	}
}

// Listing returns summaries without body content and without canonical
// re-rendering: the render counter must not move during ListDrafts, even
// with Markdown and HTML drafts present.
func TestListDraftsReturnsSummariesWithoutRender(t *testing.T) {
	service := NewServiceWithDraftRoot(nil, filepath.Join(t.TempDir(), "drafts"))
	plain, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		From: "sender@example.com", To: []Recipient{{Address: "a@example.com"}},
		Subject: "Plain", Body: "Hello\n",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	rich, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		From: "sender@example.com", To: []Recipient{{Address: "b@example.com"}},
		Subject: "Rich", Body: "# Title\n\nSome *text*.\n", BodyFormat: DraftBodyMarkdown,
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	html, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		From: "sender@example.com", To: []Recipient{{Address: "c@example.com"}},
		Subject: "Page", Body: "<p>Hi</p>", BodyFormat: DraftBodyHTML,
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	prepareDraftContentCalls = 0
	summaries, err := service.ListDrafts()
	if err != nil {
		t.Fatalf("ListDrafts() error = %v", err)
	}
	if prepareDraftContentCalls != 0 {
		t.Fatalf("ListDrafts() rendered %d bodies, want zero", prepareDraftContentCalls)
	}
	if len(summaries) != 3 {
		t.Fatalf("ListDrafts() = %d summaries, want 3", len(summaries))
	}
	byRef := make(map[string]DraftSummary, len(summaries))
	for _, summary := range summaries {
		byRef[summary.Ref] = summary
	}
	for _, want := range []struct {
		ref     string
		subject string
		format  DraftBodyFormat
	}{
		{plain.Ref, "Plain", DraftBodyPlain},
		{rich.Ref, "Rich", DraftBodyMarkdown},
		{html.Ref, "Page", DraftBodyHTML},
	} {
		got, ok := byRef[want.ref]
		if !ok {
			t.Fatalf("missing summary for %s", want.ref)
		}
		if got.Subject != want.subject || got.From != "sender@example.com" || got.BodyFormat != want.format {
			t.Fatalf("summary = %+v, want subject/format envelope", got)
		}
		if len(got.To) != 1 || got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
			t.Fatalf("summary = %+v, want recipients and timestamps", got)
		}
		if got.EverSent || got.SendAttempt != nil || got.StateError != "" {
			t.Fatalf("summary = %+v, want pristine state", got)
		}
	}
	raw, err := json.Marshal(summaries)
	if err != nil {
		t.Fatalf("marshal summaries: %v", err)
	}
	for _, leaked := range []string{"Hello", "Title", "Hi</p>", "body_html", "body_source", `"body":`} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("list output contains %q: body content leaked into summaries", leaked)
		}
	}
}

// A draft whose body fails canonical validation still appears in the list
// (inspect keeps the full gate and rejects it).
func TestListDraftsKeepsCorruptBodyDraft(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(nil, root)
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		From: "sender@example.com", To: []Recipient{{Address: "a@example.com"}},
		Subject: "Rich", Body: "# Title\n", BodyFormat: DraftBodyMarkdown,
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	path := filepath.Join(root, draft.Ref+".json")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read draft file: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatalf("decode draft file: %v", err)
	}
	document["body"] = "tampered body that no longer matches the canonical rendering"
	tampered, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode tampered draft: %v", err)
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatalf("write tampered draft: %v", err)
	}
	if _, err := service.GetDraft(draft.Ref); err == nil {
		t.Fatal("GetDraft() accepted a tampered body; the inspect gate is broken")
	}
	prepareDraftContentCalls = 0
	summaries, err := service.ListDrafts()
	if err != nil {
		t.Fatalf("ListDrafts() error = %v", err)
	}
	if len(summaries) != 1 || summaries[0].Ref != draft.Ref || summaries[0].Subject != "Rich" {
		t.Fatalf("ListDrafts() = %+v, want the tampered draft listed by envelope", summaries)
	}
	if prepareDraftContentCalls != 0 {
		t.Fatalf("ListDrafts() rendered %d bodies, want zero", prepareDraftContentCalls)
	}
}

// The summary carries send-attempt state and derives EverSent from it.
func TestDraftSummaryFromKeepsAttemptState(t *testing.T) {
	attempt := &SendAttempt{ID: "attempt_1", Outcome: SendOutcomeUnknown}
	summary := draftSummaryFrom(Draft{
		Ref: "draft_ref", Subject: "S", BodyFormat: DraftBodyPlain,
		SendAttempt: attempt,
	})
	if !summary.EverSent || summary.SendAttempt != attempt {
		t.Fatalf("summary = %+v, want attempt state with EverSent", summary)
	}
	plain := draftSummaryFrom(Draft{Ref: "draft_other"})
	if plain.EverSent || plain.SendAttempt != nil {
		t.Fatalf("summary = %+v, want pristine state", plain)
	}
}

func TestDraftLifecycleAndAttachmentIntegrity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	attachmentPath := filepath.Join(t.TempDir(), "brief.txt")
	if err := os.WriteFile(attachmentPath, []byte("version one"), 0o600); err != nil {
		t.Fatalf("write attachment: %v", err)
	}
	submitter := &stubSubmitter{evidence: transport.SubmitEvidence{ServerResponse: "250 2.0.0 OK", MessageID: "<id@icloud.com>"}}
	mirror := &stubMirror{evidence: transport.AppendEvidence{Mailbox: "Sent", Appended: true}}
	service := NewServiceWithTransport(nil, root, SendTransport{
		Submitter: submitter, Mirror: mirror, Credentials: &stubCredentials{password: "secret"},
	})
	draft, err := service.CreateDraft(CreateDraftRequest{Kind: DraftKindNew, Input: DraftInput{
		From:    "sender@icloud.com",
		To:      []Recipient{{Name: "Ada", Address: "ada@example.com"}},
		Subject: "Subject", Body: "Exact body\n", Attachments: []string{attachmentPath},
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	if len(draft.Attachments) != 1 || draft.Attachments[0].SHA256 == "" {
		t.Fatalf("draft attachments = %+v", draft.Attachments)
	}
	updated, err := service.UpdateDraft(UpdateDraftRequest{Ref: draft.Ref, Input: DraftInput{
		From: "sender@icloud.com", To: []Recipient{{Address: "ada@example.com"}},
		Subject: "Updated", Body: "Exact body\n",
		Attachments: []string{attachmentPath},
	}})
	if err != nil || updated.Ref != draft.Ref || updated.CreatedAt != draft.CreatedAt {
		t.Fatalf("UpdateDraft() = %+v, error = %v", updated, err)
	}
	drafts, err := service.ListDrafts()
	if err != nil || len(drafts) != 1 || drafts[0].Subject != "Updated" {
		t.Fatalf("ListDrafts() = %+v, error = %v", drafts, err)
	}
	if err := os.WriteFile(attachmentPath, []byte("version two"), 0o600); err != nil {
		t.Fatalf("change attachment: %v", err)
	}
	result, sendErr := service.SendDraft(context.Background(), draft.Ref)
	if sendErr == nil || submitter.calls != 0 || !strings.Contains(sendErr.Error(), filepath.Base(attachmentPath)) {
		t.Fatalf("changed attachment send result = %+v, error = %v, submits = %d", result, sendErr, submitter.calls)
	}
	if err := os.WriteFile(attachmentPath, []byte("version one"), 0o600); err != nil {
		t.Fatalf("restore attachment: %v", err)
	}
	result, err = service.SendDraft(context.Background(), draft.Ref)
	if err != nil || result.Outcome != SendOutcomeSent || !result.Accepted || submitter.calls != 1 || mirror.calls != 1 {
		t.Fatalf("SendDraft() = %+v, error = %v, submits = %d", result, err, submitter.calls)
	}
	if _, err := service.GetDraft(draft.Ref); err == nil {
		t.Fatal("sent draft still exists")
	}
}

// The credential lookup mutates the path after send-time verification. The
// submitter must still receive the verified in-memory snapshot rather than
// causing a second read during composition.
func TestSendDraftUsesVerifiedAttachmentSnapshot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	attachmentPath := filepath.Join(t.TempDir(), "brief.txt")
	original := []byte("version one")
	if err := os.WriteFile(attachmentPath, original, 0o600); err != nil {
		t.Fatalf("write attachment: %v", err)
	}
	submitter, mirror := sendTransportStubs()
	credentials := &stubCredentials{
		password: "secret",
		loadHook: func() {
			if err := os.WriteFile(attachmentPath, []byte("version two"), 0o600); err != nil {
				t.Fatalf("mutate attachment during credential load: %v", err)
			}
		},
	}
	service := newTransportService(root, submitter, mirror, credentials)
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		From: "sender@icloud.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Send test", Body: "Body", Attachments: []string{attachmentPath},
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	if _, err := service.SendDraft(context.Background(), draft.Ref); err != nil {
		t.Fatalf("SendDraft() error = %v", err)
	}
	if submitter.calls != 1 {
		t.Fatalf("submitter calls = %d, want one", submitter.calls)
	}
	message := string(submitter.lastMessage)
	originalEncoded := base64.StdEncoding.EncodeToString(original)
	changedEncoded := base64.StdEncoding.EncodeToString([]byte("version two"))
	if !strings.Contains(message, originalEncoded) || strings.Contains(message, changedEncoded) {
		t.Fatalf("submitted message did not use the verified snapshot: %q", message)
	}
}

func TestSendNewDraftRequiresExplicitSender(t *testing.T) {
	service := NewServiceWithDraftRoot(&draftGateway{}, filepath.Join(t.TempDir(), "drafts"))
	draft, err := service.CreateDraft(CreateDraftRequest{Kind: DraftKindNew, Input: DraftInput{
		To: []Recipient{{Address: "recipient@example.com"}}, Subject: "Subject", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	if _, err := service.SendDraft(context.Background(), draft.Ref); errorCode(err) != "invalid_argument" {
		t.Fatalf("SendDraft() error = %v, want invalid_argument", err)
	}
}

func TestDerivedDraftValidation(t *testing.T) {
	service := NewServiceWithDraftRoot(&draftGateway{}, filepath.Join(t.TempDir(), "drafts"))
	if _, err := service.CreateDraft(CreateDraftRequest{Kind: DraftKindReply, Input: DraftInput{Body: "Reply"}}); err == nil {
		t.Fatal("reply without source error = nil")
	}
	if _, err := service.CreateDraft(CreateDraftRequest{
		Kind: DraftKindReply, SourceRef: "msg_ref", Input: DraftInput{Body: "Reply"},
	}); errorCode(err) != "invalid_argument" {
		t.Fatalf("reply with non-store source error = %v", err)
	}
	if _, err := service.CreateDraft(CreateDraftRequest{
		Kind: DraftKindForward, SourceRef: storeBoundSourceRef(t), Input: DraftInput{Body: "Forward"},
	}); errorCode(err) != "invalid_argument" {
		t.Fatalf("forward without recipient error = %v", err)
	}
	draft, err := service.CreateDraft(CreateDraftRequest{
		Kind: DraftKindReply, SourceRef: storeBoundSourceRef(t), ReplyAll: true, Input: DraftInput{Body: "Reply"},
	})
	if err != nil || draft.Kind != DraftKindReply || !draft.ReplyAll {
		t.Fatalf("reply draft = %+v, error = %v", draft, err)
	}
}

func TestDraftValidationRejectsDuplicateRecipientAcrossRoles(t *testing.T) {
	service := NewServiceWithDraftRoot(&draftGateway{}, filepath.Join(t.TempDir(), "drafts"))
	_, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		To: []Recipient{{Address: "person@example.com"}},
		CC: []Recipient{{Address: "PERSON@example.com"}}, Body: "Body",
	}})
	if errorCode(err) != "invalid_argument" {
		t.Fatalf("CreateDraft() error = %v, want invalid_argument", err)
	}
}

func storeBoundSourceRef(t *testing.T) string {
	t.Helper()
	ref, err := mailref.EncodeMessage(mailref.Message{
		AccountID: "account", MailboxPath: []string{"Inbox"}, LibraryID: "42",
		ExpectedStoreUUID: "store", ExpectedStoreMailboxID: 1,
	})
	if err != nil {
		t.Fatalf("mailref.EncodeMessage() error = %v", err)
	}
	return ref
}

func TestClaimedDraftBlocksMutationUntilExplicitDiscard(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&draftGateway{}, root)
	draft := createSendTestDraft(t, service)
	if _, err := beginSendAttempt(root, draft.Ref, "", ""); err != nil {
		t.Fatalf("beginSendAttempt() error = %v", err)
	}
	_, updateErr := service.UpdateDraft(UpdateDraftRequest{Ref: draft.Ref, Input: DraftInput{
		To: []Recipient{{Address: "recipient@example.com"}}, Body: "changed",
	}})
	if errorCode(updateErr) != "send_retry_blocked" {
		t.Fatalf("UpdateDraft() error = %v", updateErr)
	}
	if err := service.DiscardDraft(draft.Ref); err != nil {
		t.Fatalf("DiscardDraft() error = %v", err)
	}
	if _, err := service.GetDraft(draft.Ref); err == nil {
		t.Fatal("discarded claimed draft still exists")
	}
	claimPath, err := sendClaimPath(root, draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(claimPath); !os.IsNotExist(err) {
		t.Fatalf("send claim still exists: %v", err)
	}
}

func TestDraftLeaseSerializesProcesses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	ref := "draft_abcdefghijklmnopqrstuvwx"
	command := exec.Command(os.Args[0], "-test.run=TestDraftLeaseProcessHelper")
	command.Env = append(os.Environ(),
		"MAILCLI_DRAFT_LEASE_HELPER=1", "MAILCLI_DRAFT_ROOT="+root, "MAILCLI_DRAFT_REF="+ref,
	)
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
			t.Fatalf("helper readiness = %q, error = %v, kill error = %v", line, err, killErr)
		}
		t.Fatalf("helper readiness = %q, error = %v", line, err)
	}
	started := time.Now()
	lease, err := acquireDraftLease(context.Background(), root, ref)
	if err != nil {
		if killErr := command.Process.Kill(); killErr != nil {
			t.Fatalf("acquireDraftLease() error = %v, kill error = %v", err, killErr)
		}
		t.Fatalf("acquireDraftLease() error = %v", err)
	}
	waited := time.Since(started)
	if err := lease.release(); err != nil {
		t.Fatalf("release() error = %v", err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("helper Wait() error = %v", err)
	}
	if waited < 150*time.Millisecond {
		t.Fatalf("draft lease waited %s, want process serialization", waited)
	}
}

func TestDraftLeaseProcessHelper(t *testing.T) {
	if os.Getenv("MAILCLI_DRAFT_LEASE_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	root := os.Getenv("MAILCLI_DRAFT_ROOT")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	lease, err := acquireDraftLease(context.Background(), root, os.Getenv("MAILCLI_DRAFT_REF"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("locked")
	time.Sleep(250 * time.Millisecond)
	if err := lease.release(); err != nil {
		t.Fatal(err)
	}
}

func createSendTestDraft(t *testing.T, service *Service) Draft {
	t.Helper()
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Send test", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	return draft
}

func errorCode(err error) string {
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		return coded.ErrorCode()
	}
	return ""
}

func TestDiscardDraftRemovesLockFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&draftGateway{}, root)
	draft := createSendTestDraft(t, service)
	if err := service.DiscardDraft(draft.Ref); err != nil {
		t.Fatalf("DiscardDraft() error = %v", err)
	}
	assertNoDraftLockFiles(t, root)
}

func TestSendDraftRemovesLockFileOnSuccess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)
	result, err := service.SendDraft(context.Background(), draft.Ref)
	if err != nil {
		t.Fatalf("SendDraft() error = %v", err)
	}
	if result.Outcome != SendOutcomeSent {
		t.Fatalf("SendDraft() outcome = %s, want %s", result.Outcome, SendOutcomeSent)
	}
	assertNoDraftLockFiles(t, root)
}

func TestReconcileUnknownDraftLeavesNoLockFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&draftGateway{}, root)
	_, err := service.ReconcileDraft(context.Background(), "draft_000000000000000000000000")
	if errorCode(err) != "not_found" {
		t.Fatalf("ReconcileDraft() error = %v, want not_found", err)
	}
	assertNoDraftLockFiles(t, root)
}

func assertNoDraftLockFiles(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".lock") {
			t.Fatalf("orphan draft lock left behind: %s", entry.Name())
		}
	}
}

func ageDraftFile(t *testing.T, root string, ref string, days int) {
	t.Helper()
	path := filepath.Join(root, ref+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("Unmarshal draft error = %v", err)
	}
	stored["updated_at"] = time.Now().Add(-time.Duration(days) * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("Marshal draft error = %v", err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("WriteFile draft error = %v", err)
	}
}

func TestPruneDryRunListsOnlyStaleNeverSentDrafts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&draftGateway{}, root)
	stale := createSendTestDraft(t, service)
	fresh := createSendTestDraft(t, service)
	ageDraftFile(t, root, stale.Ref, 40)
	result, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("PruneDrafts() error = %v", err)
	}
	if !result.DryRun {
		t.Fatal("PruneDrafts() without confirm must be a dry run")
	}
	if len(result.Candidates) != 1 || result.Candidates[0].Ref != stale.Ref || result.Candidates[0].AgeDays < 39 {
		t.Fatalf("candidates = %+v, want only %s", result.Candidates, stale.Ref)
	}
	for _, candidate := range result.Candidates {
		if candidate.Ref == fresh.Ref {
			t.Fatalf("fresh draft %s selected for prune", fresh.Ref)
		}
	}
	if _, err := os.Stat(filepath.Join(root, stale.Ref+".json")); err != nil {
		t.Fatalf("dry run deleted the stale draft: %v", err)
	}
}

// Sub-day cutoffs would select every never-sent draft (or reach into the
// future for negative values); the service rejects them like the CLI does.
func TestPruneRejectsSubDayThresholds(t *testing.T) {
	service := NewServiceWithDraftRoot(&draftGateway{}, filepath.Join(t.TempDir(), "drafts"))
	draft := createSendTestDraft(t, service)
	for _, olderThan := range []time.Duration{0, -24 * time.Hour} {
		result, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: olderThan, Confirm: true})
		if err == nil || len(result.Candidates) != 0 || result.Removed != nil {
			t.Fatalf("PruneDrafts(%v) = %+v, error = %v; want rejection before any listing", olderThan, result, err)
		}
		if errorCode(err) != "invalid_argument" {
			t.Fatalf("PruneDrafts(%v) error = %v, want invalid_argument", olderThan, err)
		}
	}
	if _, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: 24 * time.Hour}); err != nil {
		t.Fatalf("PruneDrafts(24h) error = %v; the exact 1-day floor is valid", err)
	}
	if _, err := service.GetDraft(draft.Ref); err != nil {
		t.Fatalf("rejected prune deleted the draft: %v", err)
	}
}

func TestPruneConfirmRemovesStaleDraftsAndLockFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&draftGateway{}, root)
	stale := createSendTestDraft(t, service)
	fresh := createSendTestDraft(t, service)
	ageDraftFile(t, root, stale.Ref, 40)
	result, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: 30 * 24 * time.Hour, Confirm: true})
	if err != nil {
		t.Fatalf("PruneDrafts() error = %v", err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != stale.Ref || len(result.Failed) != 0 {
		t.Fatalf("result = %+v, want only %s removed", result, stale.Ref)
	}
	if _, err := os.Stat(filepath.Join(root, stale.Ref+".json")); !os.IsNotExist(err) {
		t.Fatalf("stale draft still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, fresh.Ref+".json")); err != nil {
		t.Fatalf("fresh draft was removed: %v", err)
	}
	assertNoDraftLockFiles(t, root)
}

func TestPruneSkipsDraftsWithSendAttempt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&draftGateway{}, root)
	draft := createSendTestDraft(t, service)
	ageDraftFile(t, root, draft.Ref, 40)
	if _, err := beginSendAttempt(root, draft.Ref, "", ""); err != nil {
		t.Fatalf("beginSendAttempt() error = %v", err)
	}
	result, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: 30 * 24 * time.Hour, Confirm: true})
	if err != nil {
		t.Fatalf("PruneDrafts() error = %v", err)
	}
	if len(result.Candidates) != 0 || len(result.Removed) != 0 {
		t.Fatalf("claimed draft was selected: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(root, draft.Ref+".json")); err != nil {
		t.Fatalf("claim-guarded draft was removed: %v", err)
	}
}

func TestListDraftsSkipsCorruptFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&draftGateway{}, root)
	draft, err := service.CreateDraft(CreateDraftRequest{Kind: DraftKindNew, Input: DraftInput{
		To: []Recipient{{Address: "recipient@example.com"}}, Subject: "Valid", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	// Write a corrupt draft file alongside the valid one.
	corruptPath := filepath.Join(root, "draft_corrupt.json")
	if err := os.WriteFile(corruptPath, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("write corrupt draft: %v", err)
	}
	drafts, err := service.ListDrafts()
	if err != nil {
		t.Fatalf("ListDrafts() error = %v", err)
	}
	if len(drafts) != 1 || drafts[0].Ref != draft.Ref {
		t.Fatalf("ListDrafts() = %+v, want only the valid draft", drafts)
	}
}

func TestCreateDraftIsAtomic(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&draftGateway{}, root)
	draft, err := service.CreateDraft(CreateDraftRequest{Kind: DraftKindNew, Input: DraftInput{
		To: []Recipient{{Address: "recipient@example.com"}}, Subject: "Atomic", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	// The final draft file must exist and be valid JSON.
	path := filepath.Join(root, draft.Ref+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read draft file: %v", err)
	}
	var stored Draft
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("draft file is not valid JSON: %v", err)
	}
	if stored.Ref != draft.Ref {
		t.Fatalf("stored ref = %q, want %q", stored.Ref, draft.Ref)
	}
	// No temp files should remain.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read draft dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") || strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", entry.Name())
		}
	}
}
