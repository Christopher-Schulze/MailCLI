package mail

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/transport"
)

type stubSubmitter struct {
	calls    int
	lastHost string
	lastPort int
	lastFrom string
	lastTo   []string
	evidence transport.SubmitEvidence
	err      error
}

func (s *stubSubmitter) Submit(
	_ context.Context,
	cfg transport.SubmitConfig,
	from string,
	rcpts []string,
	_ []byte,
) (transport.SubmitEvidence, error) {
	s.calls++
	s.lastHost, s.lastPort, s.lastFrom, s.lastTo = cfg.Host, cfg.Port, from, rcpts
	return s.evidence, s.err
}

type stubMirror struct {
	calls    int
	lastID   string
	evidence transport.AppendEvidence
	err      error
}

func (s *stubMirror) AppendToSent(
	_ context.Context,
	_ transport.ImapConfig,
	_ []byte,
	messageID string,
) (transport.AppendEvidence, error) {
	s.calls++
	s.lastID = messageID
	return s.evidence, s.err
}

type stubCredentials struct {
	password string
	loadErr  error
}

func (c *stubCredentials) Load(string) (string, error) {
	if c.loadErr != nil {
		return "", c.loadErr
	}
	return c.password, nil
}

func (c *stubCredentials) Store(string, string) error { return nil }

func (c *stubCredentials) Delete(string) error { return nil }

func newTransportService(
	root string,
	submitter *stubSubmitter,
	mirror *stubMirror,
	credentials transport.CredentialStore,
) *Service {
	return NewServiceWithTransport(nil, root, SendTransport{
		Submitter: submitter, Mirror: mirror, Credentials: credentials,
	})
}

func createTransportDraft(t *testing.T, service *Service) Draft {
	t.Helper()
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		From: "sender@icloud.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Send test", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	return draft
}

func sendTransportStubs() (*stubSubmitter, *stubMirror) {
	return &stubSubmitter{
		evidence: transport.SubmitEvidence{ServerResponse: "250 2.0.0 OK", MessageID: "<abc123@icloud.com>"},
	}, &stubMirror{
		evidence: transport.AppendEvidence{Mailbox: "Sent", Appended: true},
	}
}

func assertNoSendClaim(t *testing.T, root string, ref string) {
	t.Helper()
	path, err := sendClaimPath(root, ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("send claim still exists: %v", err)
	}
}

func TestSendDraftDeliversViaTransportAndMirrors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)

	result, err := service.SendDraft(context.Background(), draft.Ref)
	if err != nil {
		t.Fatalf("SendDraft() error = %v", err)
	}
	if result.Outcome != SendOutcomeSent || !result.Accepted || result.DraftRetained || result.AttemptID == "" {
		t.Fatalf("SendDraft() = %+v", result)
	}
	if submitter.calls != 1 || submitter.lastHost != "smtp.mail.me.com" || submitter.lastPort != 587 ||
		submitter.lastFrom != "sender@icloud.com" ||
		len(submitter.lastTo) != 1 || submitter.lastTo[0] != "recipient@example.com" {
		t.Fatalf("submitter = %+v", submitter)
	}
	if mirror.calls != 1 || mirror.lastID != submitter.evidence.MessageID {
		t.Fatalf("mirror = %+v", mirror)
	}
	if _, err := service.GetDraft(draft.Ref); err == nil {
		t.Fatal("sent draft still exists")
	}
	assertNoSendClaim(t, root, draft.Ref)
}

func TestSendDraftWithoutTransportIsRejected(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(nil, root)
	draft := createTransportDraft(t, service)

	result, err := service.SendDraft(context.Background(), draft.Ref)
	if errorCode(err) != "send_transport_unavailable" || result.AttemptID != "" {
		t.Fatalf("SendDraft() = %+v, error = %v", result, err)
	}
	inspected, err := service.GetDraft(draft.Ref)
	if err != nil || inspected.SendAttempt != nil {
		t.Fatalf("GetDraft() = %+v, error = %v", inspected, err)
	}
}

func TestSendDraftMissingCredentialsBlocksSubmission(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	service := newTransportService(root, submitter, mirror, &stubCredentials{
		loadErr: &OperationError{Code: "keychain_item_not_found", Message: "missing"},
	})
	draft := createTransportDraft(t, service)

	result, err := service.SendDraft(context.Background(), draft.Ref)
	if errorCode(err) != "smtp_credentials_missing" || result.AttemptID != "" {
		t.Fatalf("SendDraft() = %+v, error = %v", result, err)
	}
	if !strings.Contains(err.Error(), "mailcli send setup --from sender@icloud.com") {
		t.Fatalf("SendDraft() error lacks setup remediation: %v", err)
	}
	if submitter.calls != 0 || mirror.calls != 0 {
		t.Fatalf("submitter calls = %d, mirror calls = %d", submitter.calls, mirror.calls)
	}
	assertNoSendClaim(t, root, draft.Ref)
}

func TestSendDraftRejectedSubmissionClearsClaimAndAllowsRetry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	submitter.err = &transport.TransportError{Code: transport.CodeSMTPRejected, Message: "550 rejected"}
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)

	result, err := service.SendDraft(context.Background(), draft.Ref)
	if errorCode(err) != "smtp_rejected" || result.AttemptID != "" {
		t.Fatalf("SendDraft() = %+v, error = %v", result, err)
	}
	assertNoSendClaim(t, root, draft.Ref)

	submitter.err = nil
	retry, err := service.SendDraft(context.Background(), draft.Ref)
	if err != nil || retry.Outcome != SendOutcomeSent || submitter.calls != 2 {
		t.Fatalf("retry SendDraft() = %+v, error = %v, submits = %d", retry, err, submitter.calls)
	}
}

func TestSendDraftUnsupportedProviderIsRejected(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		From: "sender@unknown.example", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Send test", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}

	if _, err := service.SendDraft(context.Background(), draft.Ref); errorCode(err) != "transport_unsupported_provider" {
		t.Fatalf("SendDraft() error = %v", err)
	}
	if submitter.calls != 0 {
		t.Fatalf("submitter calls = %d", submitter.calls)
	}
	assertNoSendClaim(t, root, draft.Ref)
}

func TestSendDraftMirrorPendingKeepsClaimReconcilable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	mirror.err = &transport.TransportError{Code: transport.CodeIMAPAppendFailed, Message: "NO mailbox"}
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)

	result, err := service.SendDraft(context.Background(), draft.Ref)
	if errorCode(err) != "imap_append_failed" || result.Outcome != SendOutcomeMirrorPending ||
		!result.DraftRetained || result.AttemptID == "" {
		t.Fatalf("SendDraft() = %+v, error = %v", result, err)
	}
	if submitter.calls != 1 {
		t.Fatalf("submitter calls = %d", submitter.calls)
	}
	retained, err := service.GetDraft(draft.Ref)
	if err != nil || retained.SendAttempt == nil ||
		retained.SendAttempt.Outcome != SendOutcomeMirrorPending ||
		retained.SendAttempt.Transport == nil ||
		retained.SendAttempt.Transport.MessageID != submitter.evidence.MessageID ||
		retained.SendAttempt.Transport.ServerResponse != submitter.evidence.ServerResponse {
		t.Fatalf("GetDraft() = %+v, error = %v", retained, err)
	}

	replayed, replayErr := service.SendDraft(context.Background(), draft.Ref)
	if errorCode(replayErr) != "send_mirror_pending" || !replayed.Replayed ||
		replayed.AttemptID != result.AttemptID || submitter.calls != 1 {
		t.Fatalf("replay SendDraft() = %+v, error = %v, submits = %d", replayed, replayErr, submitter.calls)
	}

	mirror.err = nil
	reconciled, reconcileErr := service.ReconcileDraft(context.Background(), draft.Ref)
	if reconcileErr != nil || reconciled.Outcome != SendOutcomeSent || !reconciled.Reconciled ||
		reconciled.DraftRetained || reconciled.ObservedMessageRef != "" {
		t.Fatalf("ReconcileDraft() = %+v, error = %v", reconciled, reconcileErr)
	}
	if submitter.calls != 1 {
		t.Fatalf("reconcile resent the submission %d times", submitter.calls-1)
	}
	if mirror.calls != 2 || mirror.lastID != submitter.evidence.MessageID {
		t.Fatalf("mirror = %+v", mirror)
	}
	if _, err := service.GetDraft(draft.Ref); err == nil {
		t.Fatal("reconciled draft still exists")
	}
	assertNoSendClaim(t, root, draft.Ref)
}

func TestReconcileDraftMirrorFailureKeepsClaim(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	mirror.err = &transport.TransportError{Code: transport.CodeIMAPAuthFailed, Message: "AUTH failed"}
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)
	if _, err := service.SendDraft(context.Background(), draft.Ref); err == nil {
		t.Fatal("SendDraft() error = nil, want mirror pending")
	}

	result, err := service.ReconcileDraft(context.Background(), draft.Ref)
	if errorCode(err) != "imap_auth_failed" || !result.Reconciled ||
		result.Outcome != SendOutcomeMirrorPending || !result.DraftRetained {
		t.Fatalf("ReconcileDraft() = %+v, error = %v", result, err)
	}
	retained, err := service.GetDraft(draft.Ref)
	if err != nil || retained.SendAttempt == nil || retained.SendAttempt.Outcome != SendOutcomeMirrorPending {
		t.Fatalf("GetDraft() = %+v, error = %v", retained, err)
	}
}

func TestReconcileDraftObservesGatewaySentStoreWithoutSendingAgain(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	gateway := &reconcileOnlyGateway{
		reconcileEvidence: SendEvidence{
			InvocationStarted: true, AcceptedByMail: true, SentStoreObserved: true,
			ObservedMessageRef: "msg_reconciled",
		},
	}
	service := NewServiceWithDraftRoot(gateway, root)
	draft := createTransportDraft(t, service)
	baseline := SendObservationBaseline{
		StoreUUID: "test-store", MaximumRowID: 10, CapturedUnix: 1, SentMailboxIDs: []int64{20},
	}
	attempt, err := beginSendAttemptWithBaseline(root, draft.Ref, &baseline)
	if err != nil {
		t.Fatalf("beginSendAttemptWithBaseline() error = %v", err)
	}
	attempt.InvocationStarted = true
	attempt.AcceptedByMail = true
	attempt.Outcome = SendOutcomeAccepted
	if err := replaceSendAttempt(root, draft.Ref, attempt); err != nil {
		t.Fatalf("replaceSendAttempt() error = %v", err)
	}

	result, err := service.ReconcileDraft(context.Background(), draft.Ref)
	if err != nil || result.Outcome != SendOutcomeObserved || !result.Reconciled ||
		result.ObservedMessageRef != "msg_reconciled" || result.DraftRetained {
		t.Fatalf("ReconcileDraft() = %+v, error = %v", result, err)
	}
	if gateway.reconcileCalls != 1 {
		t.Fatalf("reconcile calls = %d", gateway.reconcileCalls)
	}
	if _, err := service.GetDraft(draft.Ref); err == nil {
		t.Fatal("reconciled draft still exists")
	}
}

func TestOrphanedSendClaimReplaysWithoutSubmitting(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)
	if _, err := beginSendAttempt(root, draft.Ref); err != nil {
		t.Fatalf("beginSendAttempt() error = %v", err)
	}

	result, err := service.SendDraft(context.Background(), draft.Ref)
	if errorCode(err) != "send_outcome_unknown" || result.Outcome != SendOutcomeUnknown ||
		!result.Replayed || submitter.calls != 0 {
		t.Fatalf("SendDraft() = %+v, error = %v, submits = %d", result, err, submitter.calls)
	}
}

type reconcileOnlyGateway struct {
	gatewayStub
	reconcileCalls    int
	reconcileEvidence SendEvidence
}

func (g *reconcileOnlyGateway) ReconcileSend(
	_ context.Context,
	_ Draft,
	attempt SendAttempt,
) (SendEvidence, error) {
	g.reconcileCalls++
	if attempt.ObservationBaseline == nil {
		return SendEvidence{}, errors.New("missing observation baseline")
	}
	return g.reconcileEvidence, nil
}
