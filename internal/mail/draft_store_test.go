package mail

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mailcli/internal/mailref"
)

type draftGateway struct {
	gatewayStub
	sends   int
	saves   int
	sendErr error
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

func (g *draftGateway) SendDraft(context.Context, Draft) (SendEvidence, error) {
	g.sends++
	return SendEvidence{
		InvocationStarted: true, AcceptedByMail: true, SentStoreObserved: true,
		ObservedMessageRef: "msg_sent",
	}, g.sendErr
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
	if _, err := service.SendDraft(context.Background(), draft.Ref); errorCode(err) != "compose_automation_unsupported" {
		t.Fatalf("SendDraft() error = %v", err)
	}
	if _, err := service.GetDraft(draft.Ref); err != nil {
		t.Fatalf("GetDraft() after blocked compose error = %v", err)
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

func TestSendDraftReportsPostflightFailureWithoutRetainingLocalDraft(t *testing.T) {
	gateway := &draftGateway{sendErr: &OperationError{
		Code: "attachment_snapshot_cleanup_failed", Message: "private attachment snapshot remained",
	}}
	gateway.accounts = []Account{{EmailAddresses: []string{"sender@example.com"}}}
	service := NewServiceWithDraftRoot(gateway, filepath.Join(t.TempDir(), "drafts"))
	draft := createSendTestDraft(t, service)
	result, err := service.SendDraft(context.Background(), draft.Ref)
	if errorCode(err) != "send_postflight_failed" || result.Outcome != SendOutcomeObserved || result.DraftRetained {
		t.Fatalf("SendDraft() = %+v, error = %v", result, err)
	}
	if _, err := service.GetDraft(draft.Ref); err == nil {
		t.Fatal("observed sent message left a retryable local draft")
	}
}

func TestDraftLifecycleAndAttachmentIntegrity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	attachmentPath := filepath.Join(t.TempDir(), "brief.txt")
	if err := os.WriteFile(attachmentPath, []byte("version one"), 0o600); err != nil {
		t.Fatalf("write attachment: %v", err)
	}
	gateway := &draftGateway{}
	gateway.accounts = []Account{{EmailAddresses: []string{"sender@example.com"}}}
	service := NewServiceWithDraftRoot(gateway, root)
	draft, err := service.CreateDraft(CreateDraftRequest{Kind: DraftKindNew, Input: DraftInput{
		From:    "sender@example.com",
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
		From: "sender@example.com", To: []Recipient{{Address: "ada@example.com"}},
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
	if _, err := service.SendDraft(context.Background(), draft.Ref); err == nil || gateway.sends != 0 {
		t.Fatalf("changed attachment send error = %v, sends = %d", err, gateway.sends)
	}
	if err := os.WriteFile(attachmentPath, []byte("version one"), 0o600); err != nil {
		t.Fatalf("restore attachment: %v", err)
	}
	result, err := service.SendDraft(context.Background(), draft.Ref)
	if err != nil || !result.AcceptedByMail || gateway.sends != 1 {
		t.Fatalf("SendDraft() = %+v, error = %v, sends = %d", result, err, gateway.sends)
	}
	if _, err := service.GetDraft(draft.Ref); err == nil {
		t.Fatal("sent draft still exists")
	}
}

func TestSendNewDraftRequiresExplicitConfiguredSender(t *testing.T) {
	gateway := &draftGateway{}
	service := NewServiceWithDraftRoot(gateway, filepath.Join(t.TempDir(), "drafts"))
	draft, err := service.CreateDraft(CreateDraftRequest{Kind: DraftKindNew, Input: DraftInput{
		To: []Recipient{{Address: "recipient@example.com"}}, Subject: "Subject", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	if _, err := service.SendDraft(context.Background(), draft.Ref); errorCode(err) != "invalid_argument" {
		t.Fatalf("SendDraft() error = %v, want invalid_argument", err)
	}
	if gateway.sends != 0 {
		t.Fatalf("gateway sends = %d, want 0", gateway.sends)
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

type controlledSendGateway struct {
	gatewayStub
	calls             atomic.Int32
	prepareCalls      atomic.Int32
	reconcileCalls    atomic.Int32
	sendSawPrepared   atomic.Bool
	sendSawPersisted  atomic.Bool
	claimRoot         string
	started           chan struct{}
	release           chan struct{}
	evidence          SendEvidence
	err               error
	reconcileEvidence SendEvidence
	reconcileErr      error
	prepareErr        error
}

func (*controlledSendGateway) ListAccounts(context.Context) ([]Account, error) {
	return []Account{{EmailAddresses: []string{"sender@example.com"}}}, nil
}

func (g *controlledSendGateway) SendDraft(ctx context.Context, draft Draft) (SendEvidence, error) {
	return g.sendDraft(ctx, draft)
}

func (g *controlledSendGateway) sendDraft(ctx context.Context, draft Draft) (SendEvidence, error) {
	g.calls.Add(1)
	g.sendSawPrepared.Store(draft.PreparedSendBaseline != nil)
	if g.claimRoot != "" {
		attempt, err := readSendAttempt(g.claimRoot, draft.Ref)
		g.sendSawPersisted.Store(err == nil && attempt != nil && attempt.ObservationBaseline != nil)
	}
	if g.started != nil {
		select {
		case g.started <- struct{}{}:
		default:
		}
	}
	if g.release != nil {
		select {
		case <-g.release:
		case <-ctx.Done():
			return SendEvidence{InvocationStarted: true}, ctx.Err()
		}
	}
	return g.evidence, g.err
}

func (g *controlledSendGateway) PrepareSend(context.Context, Draft) (SendObservationBaseline, error) {
	g.prepareCalls.Add(1)
	if g.prepareErr != nil {
		return SendObservationBaseline{}, g.prepareErr
	}
	return SendObservationBaseline{
		StoreUUID: "test-store", MaximumRowID: 10, CapturedUnix: 1,
		SentMailboxIDs: []int64{20},
	}, nil
}

func (g *controlledSendGateway) ReconcileSend(
	_ context.Context,
	_ Draft,
	attempt SendAttempt,
) (SendEvidence, error) {
	g.reconcileCalls.Add(1)
	if attempt.ObservationBaseline == nil {
		return SendEvidence{}, errors.New("missing observation baseline")
	}
	return g.reconcileEvidence, g.reconcileErr
}

func TestSendDraftIsIdempotentAcrossServiceInstances(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	gateway := &controlledSendGateway{
		claimRoot: root,
		started:   make(chan struct{}, 1), release: make(chan struct{}),
		evidence: SendEvidence{InvocationStarted: true, AcceptedByMail: true},
	}
	first := NewServiceWithDraftRoot(gateway, root)
	second := NewServiceWithDraftRoot(gateway, root)
	draft := createSendTestDraft(t, first)
	type sendResponse struct {
		result SendResult
		err    error
	}
	firstDone := make(chan sendResponse, 1)
	go func() {
		result, err := first.SendDraft(context.Background(), draft.Ref)
		firstDone <- sendResponse{result: result, err: err}
	}()
	<-gateway.started
	secondDone := make(chan sendResponse, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		result, err := second.SendDraft(ctx, draft.Ref)
		secondDone <- sendResponse{result: result, err: err}
	}()
	close(gateway.release)
	firstResponse := <-firstDone
	secondResponse := <-secondDone
	if errorCode(firstResponse.err) != "send_not_observed" || errorCode(secondResponse.err) != "send_not_observed" {
		t.Fatalf("SendDraft() errors = %v, %v", firstResponse.err, secondResponse.err)
	}
	if gateway.calls.Load() != 1 || firstResponse.result.AttemptID != secondResponse.result.AttemptID {
		t.Fatalf(
			"gateway calls = %d, attempt ids = %q / %q",
			gateway.calls.Load(), firstResponse.result.AttemptID, secondResponse.result.AttemptID,
		)
	}
	if gateway.prepareCalls.Load() != 1 || !gateway.sendSawPrepared.Load() || !gateway.sendSawPersisted.Load() {
		t.Fatalf(
			"prepare calls = %d, send saw prepared baseline = %t, persisted = %t",
			gateway.prepareCalls.Load(), gateway.sendSawPrepared.Load(), gateway.sendSawPersisted.Load(),
		)
	}
	if firstResponse.result.Replayed || !secondResponse.result.Replayed {
		t.Fatalf("replay flags = %t / %t", firstResponse.result.Replayed, secondResponse.result.Replayed)
	}
}

func TestPrepareSendFailureCreatesNoClaimAndDoesNotInvokeSend(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	gateway := &controlledSendGateway{
		prepareErr: &OperationError{Code: "sent_store_unavailable", Message: "no Sent mailbox"},
	}
	service := NewServiceWithDraftRoot(gateway, root)
	draft := createSendTestDraft(t, service)
	if _, err := service.SendDraft(context.Background(), draft.Ref); errorCode(err) != "sent_store_unavailable" {
		t.Fatalf("SendDraft() error = %v", err)
	}
	inspected, err := service.GetDraft(draft.Ref)
	if err != nil || inspected.SendAttempt != nil || gateway.calls.Load() != 0 {
		t.Fatalf("GetDraft() = %+v, error = %v, send calls = %d", inspected, err, gateway.calls.Load())
	}
	gateway.prepareErr = nil
	gateway.evidence = SendEvidence{InvocationStarted: true, AcceptedByMail: true}
	if _, err := service.SendDraft(context.Background(), draft.Ref); errorCode(err) != "send_not_observed" || gateway.calls.Load() != 1 {
		t.Fatalf("second SendDraft() error = %v, send calls = %d", err, gateway.calls.Load())
	}
}

func TestReconcileDraftObservesWithoutSendingAgain(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	gateway := &controlledSendGateway{
		evidence: SendEvidence{InvocationStarted: true}, err: context.DeadlineExceeded,
		reconcileEvidence: SendEvidence{
			InvocationStarted: true, AcceptedByMail: true, SentStoreObserved: true,
			ObservedMessageRef: "msg_reconciled",
		},
	}
	service := NewServiceWithDraftRoot(gateway, root)
	draft := createSendTestDraft(t, service)
	if _, err := service.SendDraft(context.Background(), draft.Ref); errorCode(err) != "send_outcome_unknown" {
		t.Fatalf("SendDraft() error = %v", err)
	}
	result, err := service.ReconcileDraft(context.Background(), draft.Ref)
	if err != nil || result.Outcome != SendOutcomeObserved || !result.Reconciled ||
		result.ObservedMessageRef != "msg_reconciled" || result.DraftRetained {
		t.Fatalf("ReconcileDraft() = %+v, error = %v", result, err)
	}
	if gateway.calls.Load() != 1 || gateway.reconcileCalls.Load() != 1 {
		t.Fatalf("send calls = %d, reconcile calls = %d", gateway.calls.Load(), gateway.reconcileCalls.Load())
	}
	if _, err := service.GetDraft(draft.Ref); errorCode(err) != "not_found" {
		t.Fatalf("GetDraft() error = %v, want not_found", err)
	}
}

func TestReconcileDraftCannotGuessClaimWithoutObservationBaseline(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	gateway := &controlledSendGateway{}
	service := NewServiceWithDraftRoot(gateway, root)
	draft := createSendTestDraft(t, service)
	if _, err := beginSendAttempt(root, draft.Ref); err != nil {
		t.Fatalf("beginSendAttempt() error = %v", err)
	}
	result, err := service.ReconcileDraft(context.Background(), draft.Ref)
	if errorCode(err) != "send_reconcile_unavailable" || !result.Reconciled ||
		gateway.calls.Load() != 0 || gateway.reconcileCalls.Load() != 0 {
		t.Fatalf("ReconcileDraft() = %+v, error = %v, send calls = %d, reconcile calls = %d", result, err, gateway.calls.Load(), gateway.reconcileCalls.Load())
	}
}

func TestSendDraftUnknownOutcomeIsSticky(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	gateway := &controlledSendGateway{
		evidence: SendEvidence{InvocationStarted: true},
		err:      context.DeadlineExceeded,
	}
	service := NewServiceWithDraftRoot(gateway, root)
	draft := createSendTestDraft(t, service)
	first, firstErr := service.SendDraft(context.Background(), draft.Ref)
	second, secondErr := service.SendDraft(context.Background(), draft.Ref)
	for index, result := range []SendResult{first, second} {
		if result.Outcome != SendOutcomeUnknown || !result.DraftRetained {
			t.Fatalf("result %d = %+v", index, result)
		}
	}
	if errorCode(firstErr) != "send_outcome_unknown" || errorCode(secondErr) != "send_outcome_unknown" {
		t.Fatalf("SendDraft() errors = %v, %v", firstErr, secondErr)
	}
	if gateway.calls.Load() != 1 || first.AttemptID != second.AttemptID {
		t.Fatalf("gateway calls = %d, attempts = %q / %q", gateway.calls.Load(), first.AttemptID, second.AttemptID)
	}
	inspected, err := service.GetDraft(draft.Ref)
	if err != nil || inspected.SendAttempt == nil || inspected.SendAttempt.ID != first.AttemptID {
		t.Fatalf("GetDraft() = %+v, error = %v", inspected, err)
	}
}

func TestSendDraftAcceptedWithoutStoreObservationRetainsDraft(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	gateway := &controlledSendGateway{
		evidence: SendEvidence{
			InvocationStarted: true, AcceptedByMail: true,
			Materialized: &SendMaterialization{
				From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
				Subject: "Materialized subject",
			},
		},
	}
	service := NewServiceWithDraftRoot(gateway, root)
	draft := createSendTestDraft(t, service)
	result, err := service.SendDraft(context.Background(), draft.Ref)
	if errorCode(err) != "send_not_observed" || result.Outcome != SendOutcomeAccepted || !result.DraftRetained || result.SentStoreObserved {
		t.Fatalf("SendDraft() = %+v, error = %v", result, err)
	}
	retained, err := service.GetDraft(draft.Ref)
	if err != nil || retained.SendAttempt == nil || retained.SendAttempt.ID != result.AttemptID ||
		retained.SendAttempt.Materialized == nil || retained.SendAttempt.Materialized.Subject != "Materialized subject" {
		t.Fatalf("GetDraft() = %+v, error = %v", retained, err)
	}
}

func TestSendDraftNotStartedClearsClaim(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	gateway := &controlledSendGateway{err: &OperationError{Code: "mail_busy", Message: "not started"}}
	service := NewServiceWithDraftRoot(gateway, root)
	draft := createSendTestDraft(t, service)
	if result, err := service.SendDraft(context.Background(), draft.Ref); err == nil || result.AttemptID != "" {
		t.Fatalf("first SendDraft() = %+v, %v", result, err)
	}
	gateway.err = nil
	gateway.evidence = SendEvidence{InvocationStarted: true, AcceptedByMail: true}
	result, err := service.SendDraft(context.Background(), draft.Ref)
	if errorCode(err) != "send_not_observed" || result.Outcome != SendOutcomeAccepted || gateway.calls.Load() != 2 {
		t.Fatalf("second SendDraft() = %+v, error = %v, calls = %d", result, err, gateway.calls.Load())
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

func TestOrphanedSendClaimNeverInvokesGateway(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	gateway := &controlledSendGateway{}
	service := NewServiceWithDraftRoot(gateway, root)
	draft := createSendTestDraft(t, service)
	if _, err := beginSendAttempt(root, draft.Ref); err != nil {
		t.Fatalf("beginSendAttempt() error = %v", err)
	}
	result, err := service.SendDraft(context.Background(), draft.Ref)
	if errorCode(err) != "send_outcome_unknown" || result.Outcome != SendOutcomeUnknown || gateway.calls.Load() != 0 {
		t.Fatalf("SendDraft() = %+v, error = %v, calls = %d", result, err, gateway.calls.Load())
	}
}

func TestClaimedDraftBlocksMutationUntilExplicitDiscard(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&controlledSendGateway{}, root)
	draft := createSendTestDraft(t, service)
	if _, err := beginSendAttempt(root, draft.Ref); err != nil {
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
