package mail

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"mailcli/internal/transport"
)

type stubSubmitter struct {
	calls       int
	lastHost    string
	lastPort    int
	lastFrom    string
	lastTo      []string
	lastMessage []byte
	evidence    transport.SubmitEvidence
	err         error
	submitHook  func(context.Context)
}

func (s *stubSubmitter) Submit(
	ctx context.Context,
	cfg transport.SubmitConfig,
	from string,
	rcpts []string,
	message []byte,
) (transport.SubmitEvidence, error) {
	s.calls++
	s.lastHost, s.lastPort, s.lastFrom, s.lastTo = cfg.Host, cfg.Port, from, rcpts
	s.lastMessage = append(s.lastMessage[:0], message...)
	if start := strings.Index(string(message), "Message-ID: "); start >= 0 {
		line := string(message[start+len("Message-ID: "):])
		if end := strings.IndexByte(line, '\r'); end >= 0 {
			s.evidence.MessageID = strings.TrimSpace(line[:end])
		}
	}
	if s.submitHook != nil {
		s.submitHook(ctx)
	}
	return s.evidence, s.err
}

type streamingSubmitter struct {
	stubSubmitter
	readerCalls int
	readerSize  int64
}

func (s *streamingSubmitter) SubmitReader(
	_ context.Context, _ transport.SubmitConfig, _ string, _ []string, messageID string, reader io.Reader, size int64,
) (transport.SubmitEvidence, error) {
	s.readerCalls++
	s.readerSize = size
	payload, err := io.ReadAll(reader)
	if err != nil {
		return transport.SubmitEvidence{}, err
	}
	if int64(len(payload)) != size {
		return transport.SubmitEvidence{}, errors.New("stream size mismatch")
	}
	s.evidence.MessageID = messageID
	return s.evidence, s.err
}

type stubMirror struct {
	calls      int
	lastID     string
	evidence   transport.AppendEvidence
	err        error
	appendHook func(context.Context)
}

func (s *stubMirror) AppendToSent(
	ctx context.Context,
	_ transport.ImapConfig,
	_ []byte,
	messageID string,
) (transport.AppendEvidence, error) {
	s.calls++
	s.lastID = messageID
	if s.appendHook != nil {
		s.appendHook(ctx)
	}
	return s.evidence, s.err
}

type streamingMirror struct {
	stubMirror
	readerCalls int
	readerSize  int64
}

func (s *streamingMirror) AppendToSentReader(
	_ context.Context, _ transport.ImapConfig, reader io.Reader, size int64, messageID string,
) (transport.AppendEvidence, error) {
	s.readerCalls++
	s.readerSize = size
	payload, err := io.ReadAll(reader)
	if err != nil {
		return transport.AppendEvidence{}, err
	}
	if int64(len(payload)) != size {
		return transport.AppendEvidence{}, errors.New("stream size mismatch")
	}
	s.lastID = messageID
	return s.evidence, s.err
}

type stubCredentials struct {
	password string
	loadErr  error
	loadHook func()
}

func (c *stubCredentials) Load(string) (string, error) {
	if c.loadErr != nil {
		return "", c.loadErr
	}
	if c.loadHook != nil {
		c.loadHook()
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

func TestDeliverViaTransportExposesDirectSendBoundary(t *testing.T) {
	submitter, mirror := sendTransportStubs()
	mirror.evidence.UIDValidity = 77
	mirror.evidence.UID = 42
	evidence, err := DeliverViaTransport(context.Background(), SendTransport{
		Submitter: submitter, Mirror: mirror, Credentials: &stubCredentials{password: "secret"},
	}, Draft{
		From: "sender@icloud.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Boundary", Body: "Body",
	})
	if err != nil {
		t.Fatalf("DeliverViaTransport() error = %v", err)
	}
	if submitter.calls != 1 || mirror.calls != 1 ||
		evidence.ServerResponse != "250 2.0.0 OK" ||
		evidence.MessageID == "" || evidence.MirrorMailbox != "Sent" || !evidence.MirrorAppended ||
		evidence.MirrorUIDValidity != 77 || evidence.MirrorUID != 42 {
		t.Fatalf("DeliverViaTransport() evidence = %+v, submitter = %+v, mirror = %+v", evidence, submitter, mirror)
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

func TestSendDraftRejectsInvalidStoredRecipient(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)
	draft.To[0].Address = "not an address"
	if err := writeDraftFile(root, draft); err != nil {
		t.Fatalf("writeDraftFile() error = %v", err)
	}
	if _, err := service.SendDraft(context.Background(), draft.Ref); errorCode(err) != "invalid_argument" {
		t.Fatalf("SendDraft() error = %v, want invalid_argument", err)
	}
	if submitter.calls != 0 || mirror.calls != 0 {
		t.Fatalf("submission calls = %d, mirror calls = %d", submitter.calls, mirror.calls)
	}
}

func TestSendDraftRejectsDuplicateStoredRecipient(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)
	draft.CC = []Recipient{{Address: draft.To[0].Address}}
	if err := writeDraftFile(root, draft); err != nil {
		t.Fatalf("writeDraftFile() error = %v", err)
	}
	if _, err := service.SendDraft(context.Background(), draft.Ref); errorCode(err) != "invalid_argument" {
		t.Fatalf("SendDraft() error = %v, want invalid_argument", err)
	}
	if submitter.calls != 0 || mirror.calls != 0 {
		t.Fatalf("submission calls = %d, mirror calls = %d", submitter.calls, mirror.calls)
	}
}

func TestSendDraftRetainsUnknownSMTPOutcome(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	submitter.err = &transport.SubmissionError{
		Stage: "final_reply",
		Err:   &transport.TransportError{Code: transport.CodeSMTPTimeout, Message: "SMTP final reply timed out"},
	}
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)

	result, err := service.SendDraft(context.Background(), draft.Ref)
	if errorCode(err) != transport.CodeSMTPSubmissionUnknown || result.Outcome != SendOutcomeUnknown ||
		!result.DraftRetained || submitter.calls != 1 || mirror.calls != 0 {
		t.Fatalf("SendDraft() = %+v, error = %v, submitter calls = %d, mirror calls = %d", result, err, submitter.calls, mirror.calls)
	}
	retained, getErr := service.GetDraft(draft.Ref)
	if getErr != nil || retained.SendAttempt == nil || retained.SendAttempt.Transport == nil ||
		retained.SendAttempt.Transport.SubmissionStage != "final_reply" {
		t.Fatalf("retained attempt = %+v, error = %v", retained.SendAttempt, getErr)
	}
}

func TestSendDraftRetainsClaimWhenContextIsCanceledDuringUnknownSubmission(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	ctx, cancel := context.WithCancel(context.Background())
	submitter.submitHook = func(context.Context) {
		cancel()
	}
	submitter.err = &transport.SubmissionError{
		Stage: "final_reply",
		Err:   &transport.TransportError{Code: transport.CodeSMTPTimeout, Message: "SMTP final reply timed out"},
	}
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)

	result, err := service.SendDraft(ctx, draft.Ref)
	if errorCode(err) != transport.CodeSMTPSubmissionUnknown ||
		result.Outcome != SendOutcomeUnknown || !result.DraftRetained {
		t.Fatalf("SendDraft() = %+v, error = %v, want retained unknown outcome", result, err)
	}
	retained, getErr := service.GetDraft(draft.Ref)
	if getErr != nil || retained.SendAttempt == nil {
		t.Fatalf("retained draft = %+v, error = %v", retained, getErr)
	}
}

func TestReconcileDoesNotReplayUnknownSentAppend(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	mirror.err = &transport.TransportError{
		Code:    transport.CodeIMAPAppendOutcomeUnknown,
		Message: "IMAP APPEND final response timed out",
	}
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)

	result, err := service.SendDraft(context.Background(), draft.Ref)
	if errorCode(err) != transport.CodeIMAPAppendOutcomeUnknown ||
		result.Outcome != SendOutcomeMirrorPending || mirror.calls != 1 {
		t.Fatalf("SendDraft() = %+v, error = %v, mirror calls = %d", result, err, mirror.calls)
	}
	retained, err := service.GetDraft(draft.Ref)
	if err != nil || retained.SendAttempt == nil || retained.SendAttempt.Transport == nil ||
		!retained.SendAttempt.Transport.MirrorOutcomeUnknown {
		t.Fatalf("retained attempt = %+v, error = %v", retained.SendAttempt, err)
	}

	reconciled, err := service.ReconcileDraft(context.Background(), draft.Ref)
	if errorCode(err) != "send_mirror_outcome_unknown" ||
		reconciled.Outcome != SendOutcomeMirrorPending || mirror.calls != 1 {
		t.Fatalf("ReconcileDraft() = %+v, error = %v, mirror calls = %d", reconciled, err, mirror.calls)
	}
	reconciled, err = service.ReconcileDraft(context.Background(), draft.Ref)
	if errorCode(err) != "send_mirror_outcome_unknown" ||
		reconciled.Outcome != SendOutcomeMirrorPending || mirror.calls != 1 {
		t.Fatalf("second ReconcileDraft() = %+v, error = %v, mirror calls = %d", reconciled, err, mirror.calls)
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

func TestSendDraftReplaysImmutableReceiptWithoutTransport(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	mirror.evidence.UIDValidity = 77
	mirror.evidence.UID = 42
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)

	first, err := service.SendDraft(context.Background(), draft.Ref)
	if err != nil || first.Receipt == nil {
		t.Fatalf("first SendDraft() = %+v, error = %v", first, err)
	}
	receiptPath, err := sendReceiptPath(root, draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatalf("ReadFile(receipt) error = %v", err)
	}
	if strings.Contains(string(payload), "Body") || strings.Contains(string(payload), "recipient@example.com") {
		t.Fatalf("receipt contains draft content: %s", payload)
	}

	second, err := service.SendDraft(context.Background(), draft.Ref)
	if err != nil || !second.Replayed || second.Receipt == nil || submitter.calls != 1 || mirror.calls != 1 {
		t.Fatalf("replayed SendDraft() = %+v, error = %v, submitter = %d, mirror = %d", second, err, submitter.calls, mirror.calls)
	}
	if !sendReceiptsEqual(*first.Receipt, *second.Receipt) || second.Receipt.UIDValidity != 77 || second.Receipt.UID != 42 {
		t.Fatalf("receipt changed across replay: first = %+v, second = %+v", first.Receipt, second.Receipt)
	}
	inspected, err := service.GetSendReceipt(draft.Ref)
	if err != nil || !sendReceiptsEqual(inspected, *first.Receipt) {
		t.Fatalf("GetSendReceipt() = %+v, error = %v", inspected, err)
	}
}

func TestSendDraftRecoversReceiptPersistedBeforeDraftCleanup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := newTransportService(root, nil, nil, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)
	attempt, err := beginSendAttempt(root, draft.Ref, "<recovered@example.com>", envelopeFingerprint(draft, "<recovered@example.com>"))
	if err != nil {
		t.Fatalf("beginSendAttempt() error = %v", err)
	}
	attempt.InvocationStarted = true
	attempt.AcceptedByMail = true
	attempt.SentStoreObserved = true
	attempt.Outcome = SendOutcomeSent
	attempt.Transport = &TransportEvidence{
		MessageID: "<recovered@example.com>", ServerResponse: "250 2.0.0 OK",
		MirrorMailbox: "Sent", MirrorUIDValidity: 77, MirrorUID: 42,
	}
	attempt.UpdatedAt = time.Now().UTC()
	if err := replaceSendAttempt(root, draft.Ref, attempt); err != nil {
		t.Fatalf("replaceSendAttempt() error = %v", err)
	}
	written, err := ensureSendReceipt(root, draft.Ref, attempt)
	if err != nil {
		t.Fatalf("ensureSendReceipt() error = %v", err)
	}
	if err := os.Remove(filepath.Join(root, draft.Ref+".json")); err != nil {
		t.Fatalf("Remove(draft) error = %v", err)
	}

	result, err := service.SendDraft(context.Background(), draft.Ref)
	if err != nil || !result.Replayed || result.Receipt == nil || !sendReceiptsEqual(*result.Receipt, *written) {
		t.Fatalf("recovery SendDraft() = %+v, error = %v", result, err)
	}
	if _, err := service.GetDraft(draft.Ref); errorCode(err) != "not_found" {
		t.Fatalf("recovered draft error = %v, want not_found", err)
	}
	assertNoSendClaim(t, root, draft.Ref)
}

func TestSendDraftRejectsMalformedReceiptWithoutSubmission(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)
	path, err := sendReceiptPath(root, draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"draft_ref":"broken"}`), 0o600); err != nil {
		t.Fatalf("WriteFile(receipt) error = %v", err)
	}
	if _, err := service.SendDraft(context.Background(), draft.Ref); errorCode(err) != "send_receipt_invalid" {
		t.Fatalf("SendDraft() error = %v, want send_receipt_invalid", err)
	}
	if submitter.calls != 0 || mirror.calls != 0 {
		t.Fatalf("submission calls = %d, mirror calls = %d", submitter.calls, mirror.calls)
	}
}

func TestExpiredSendReceiptIsBlockedAndPruned(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := newTransportService(root, nil, nil, &stubCredentials{password: "secret"})
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		From: "sender@icloud.com", To: []Recipient{{Address: "recipient@example.com"}}, Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	completed := time.Now().UTC().Add(-SendReceiptRetention - time.Hour)
	receipt := SendReceipt{
		DraftRef: draft.Ref, AttemptID: "send_expired", StartedAt: completed.Add(-time.Minute),
		CompletedAt: completed, ExpiresAt: completed.Add(SendReceiptRetention), Outcome: SendOutcomeSent,
		Accepted: true, MessageID: "<expired@example.com>",
	}
	if err := os.Remove(filepath.Join(root, draft.Ref+".json")); err != nil {
		t.Fatalf("Remove(draft) error = %v", err)
	}
	if err := persistSendReceipt(root, draft.Ref, receipt); err != nil {
		t.Fatalf("persistSendReceipt() error = %v", err)
	}
	if _, err := service.GetSendReceipt(draft.Ref); errorCode(err) != "not_found" {
		t.Fatalf("GetSendReceipt() error = %v, want not_found", err)
	}
	if _, err := service.SendDraft(context.Background(), draft.Ref); errorCode(err) != "send_receipt_expired" {
		t.Fatalf("SendDraft() error = %v, want send_receipt_expired", err)
	}
	result, err := service.PruneDraftsContext(context.Background(), PruneDraftsRequest{OlderThan: 24 * time.Hour, Confirm: true})
	if err != nil || len(result.ExpiredReceipts) != 1 || result.ExpiredReceipts[0] != draft.Ref {
		t.Fatalf("PruneDraftsContext() = %+v, error = %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(root, draft.Ref+".send-receipt")); !os.IsNotExist(err) {
		t.Fatalf("expired receipt still exists: %v", err)
	}
}

func TestSendDraftRetainsClaimWhenContextIsCanceledDuringMirror(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	ctx, cancel := context.WithCancel(context.Background())
	mirror.appendHook = func(context.Context) {
		cancel()
	}
	mirror.err = errors.New("mirror canceled")
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)

	result, err := service.SendDraft(ctx, draft.Ref)
	if errorCode(err) != "send_mirror_pending" ||
		result.Outcome != SendOutcomeMirrorPending || !result.DraftRetained {
		t.Fatalf("SendDraft() = %+v, error = %v, want retained mirror-pending outcome", result, err)
	}
	retained, getErr := service.GetDraft(draft.Ref)
	if getErr != nil || retained.SendAttempt == nil ||
		retained.SendAttempt.Outcome != SendOutcomeMirrorPending {
		t.Fatalf("retained draft = %+v, error = %v", retained, getErr)
	}
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
	cases := []struct {
		name     string
		message  string
		guidance string
	}{
		{name: "transient", message: "final reply rejected: 451 4.3.0 Greylisted; transient final rejection", guidance: "transient final rejection"},
		{name: "permanent", message: "final reply rejected: 550 5.7.1 Policy rejection; permanent final rejection", guidance: "permanent final rejection"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "drafts")
			submitter, mirror := sendTransportStubs()
			submitter.err = &transport.TransportError{Code: transport.CodeSMTPRejected, Message: tc.message}
			service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
			draft := createTransportDraft(t, service)

			result, err := service.SendDraft(context.Background(), draft.Ref)
			if errorCode(err) != transport.CodeSMTPRejected || result.AttemptID != "" || !strings.Contains(err.Error(), tc.guidance) {
				t.Fatalf("SendDraft() = %+v, error = %v", result, err)
			}
			assertNoSendClaim(t, root, draft.Ref)

			submitter.err = nil
			retry, err := service.SendDraft(context.Background(), draft.Ref)
			if err != nil || retry.Outcome != SendOutcomeSent || submitter.calls != 2 {
				t.Fatalf("retry SendDraft() = %+v, error = %v, submits = %d", retry, err, submitter.calls)
			}
		})
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

func TestSendDraftUsesReplayableStreamingTransport(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	attachment := filepath.Join(t.TempDir(), "invoice.txt")
	if err := os.WriteFile(attachment, []byte("attachment bytes"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	submitter := &streamingSubmitter{stubSubmitter: stubSubmitter{
		evidence: transport.SubmitEvidence{ServerResponse: "250 2.0.0 OK"},
	}}
	mirror := &streamingMirror{stubMirror: stubMirror{
		evidence: transport.AppendEvidence{Mailbox: "Sent", Appended: true},
	}}
	service := NewServiceWithTransport(nil, root, SendTransport{
		Submitter: submitter, Mirror: mirror, Credentials: &stubCredentials{password: "secret"},
	})
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		From: "sender@icloud.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Stream", Body: "Body", Attachments: []string{attachment},
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	if _, err := service.SendDraft(context.Background(), draft.Ref); err != nil {
		t.Fatalf("SendDraft() error = %v", err)
	}
	if submitter.readerCalls != 1 || mirror.readerCalls != 1 || submitter.readerSize <= 0 || submitter.readerSize != mirror.readerSize {
		t.Fatalf("stream calls = submitter:%d mirror:%d sizes:%d/%d", submitter.readerCalls, mirror.readerCalls, submitter.readerSize, mirror.readerSize)
	}
	if submitter.calls != 0 || mirror.calls != 0 {
		t.Fatalf("legacy calls = submitter:%d mirror:%d", submitter.calls, mirror.calls)
	}
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

func TestReconcileMirrorRetryArmsUnknownOutcomeBeforeAppend(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	mirror.err = &transport.TransportError{Code: transport.CodeIMAPAppendFailed, Message: "NO mailbox"}
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)

	if _, err := service.SendDraft(context.Background(), draft.Ref); errorCode(err) != transport.CodeIMAPAppendFailed {
		t.Fatalf("SendDraft() error = %v, want known APPEND failure", err)
	}
	first, err := service.GetDraft(draft.Ref)
	if err != nil || first.SendAttempt == nil || first.SendAttempt.Transport == nil {
		t.Fatalf("GetDraft() = %+v, error = %v", first, err)
	}
	firstMirrorID := first.SendAttempt.Transport.MirrorAttemptID
	if firstMirrorID == "" || first.SendAttempt.Transport.MirrorOutcomeUnknown {
		t.Fatalf("initial mirror evidence = %+v, want armed ID and known failure", first.SendAttempt.Transport)
	}

	mirror.err = &transport.TransportError{Code: transport.CodeIMAPAppendOutcomeUnknown, Message: "final response lost"}
	if _, err := service.ReconcileDraft(context.Background(), draft.Ref); errorCode(err) != transport.CodeIMAPAppendOutcomeUnknown {
		t.Fatalf("ReconcileDraft() error = %v, want unknown APPEND outcome", err)
	}
	retained, err := service.GetDraft(draft.Ref)
	if err != nil || retained.SendAttempt == nil || retained.SendAttempt.Transport == nil {
		t.Fatalf("retained draft = %+v, error = %v", retained, err)
	}
	if retained.SendAttempt.Transport.MirrorAttemptID == "" ||
		retained.SendAttempt.Transport.MirrorAttemptID == firstMirrorID ||
		!retained.SendAttempt.Transport.MirrorOutcomeUnknown {
		t.Fatalf("retry mirror evidence = %+v, want a new durable unknown attempt", retained.SendAttempt.Transport)
	}
	if mirror.calls != 2 {
		t.Fatalf("mirror calls = %d, want initial plus one retry", mirror.calls)
	}

	if _, err := service.ReconcileDraft(context.Background(), draft.Ref); errorCode(err) != "send_mirror_outcome_unknown" {
		t.Fatalf("second ReconcileDraft() error = %v, want replay block", err)
	}
	if mirror.calls != 2 {
		t.Fatalf("mirror calls after replay block = %d, want 2", mirror.calls)
	}
}

func TestReconcileMirrorPendingVerifiesExistingSentIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	mirror.err = &transport.TransportError{Code: transport.CodeIMAPAppendFailed, Message: "NO mailbox"}
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	imap := &reconcileImapStub{
		mailboxes: []transport.MailboxInfo{{Name: "Sent", Flags: []string{"\\Sent"}}},
	}
	service.send.Imap = imap
	draft := createTransportDraft(t, service)
	if _, err := service.SendDraft(context.Background(), draft.Ref); errorCode(err) != "imap_append_failed" {
		t.Fatalf("SendDraft() error = %v", err)
	}
	retained, err := service.GetDraft(draft.Ref)
	if err != nil || retained.SendAttempt == nil || retained.SendAttempt.Transport == nil {
		t.Fatalf("GetDraft() = %+v, error = %v", retained, err)
	}
	messageID := retained.SendAttempt.MessageID
	imap.uid = 42
	imap.fetchRaw = []byte("From: sender@icloud.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Message-ID: " + messageID + "\r\n" +
		"Subject: Send test\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nBody\r\n")
	mirror.err = nil
	calls := mirror.calls
	result, err := service.ReconcileDraft(context.Background(), draft.Ref)
	if err != nil || result.Outcome != SendOutcomeSent || result.DraftRetained || mirror.calls != calls {
		t.Fatalf("ReconcileDraft() = %+v, error = %v, mirror calls = %d want %d", result, err, mirror.calls, calls)
	}
	if imap.fetchCalls != 1 {
		t.Fatalf("FetchMessage() calls = %d, want 1", imap.fetchCalls)
	}
}

func TestReconcileMirrorRetryUsesResolvedSentMailbox(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	submitter, mirror := sendTransportStubs()
	mirror.err = &transport.TransportError{Code: transport.CodeIMAPAppendFailed, Message: "NO mailbox"}
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	imap := &reconcileImapStub{
		mailboxes:     []transport.MailboxInfo{{Name: "Sent Messages", Flags: []string{"\\Sent"}}},
		uid:           42,
		searchResults: []int{0, 1},
	}
	service.send.Imap = imap
	draft := createTransportDraft(t, service)
	if _, err := service.SendDraft(context.Background(), draft.Ref); errorCode(err) != transport.CodeIMAPAppendFailed {
		t.Fatalf("SendDraft() error = %v", err)
	}
	retained, err := service.GetDraft(draft.Ref)
	if err != nil || retained.SendAttempt == nil {
		t.Fatalf("GetDraft() = %+v, error = %v", retained, err)
	}
	messageID := retained.SendAttempt.MessageID
	imap.fetchRaw = []byte("From: sender@icloud.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Message-ID: " + messageID + "\r\n" +
		"Subject: Send test\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nBody\r\n")
	mirror.err = nil

	result, err := service.ReconcileDraft(context.Background(), draft.Ref)
	if err != nil || result.Outcome != SendOutcomeSent || result.DraftRetained {
		t.Fatalf("ReconcileDraft() = %+v, error = %v", result, err)
	}
	if imap.searchCalls != 2 || imap.searchBox != "Sent Messages" || imap.fetchBox != "Sent Messages" {
		t.Fatalf("IMAP mailbox flow = searches:%d search:%q fetch:%q", imap.searchCalls, imap.searchBox, imap.fetchBox)
	}
	if mirror.calls != 2 {
		t.Fatalf("mirror calls = %d, want initial failure plus one retry", mirror.calls)
	}
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
	attempt, err := beginSendAttemptWithBaseline(root, draft.Ref, &baseline, "", "")
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
	if _, err := beginSendAttempt(root, draft.Ref, "", ""); err != nil {
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

type reconcileImapStub struct {
	transport.ImapOperator
	mailboxes     []transport.MailboxInfo
	uid           uint32
	searchErr     error
	searchID      string
	searchBox     string
	searchCalls   int
	searchResults []int
	searchIndex   int
	matchCount    int
	fetchRaw      []byte
	fetchErr      error
	fetchBox      string
	fetchCalls    int
}

func (s *reconcileImapStub) ListMailboxes(context.Context, transport.ImapConfig) ([]transport.MailboxInfo, error) {
	return s.mailboxes, nil
}

func (s *reconcileImapStub) SearchUID(
	_ context.Context, _ transport.ImapConfig, mailbox string, messageID string,
) (uint32, uint32, int, error) {
	s.searchCalls++
	s.searchBox, s.searchID = mailbox, messageID
	matchCount := s.matchCount
	if s.searchIndex < len(s.searchResults) {
		matchCount = s.searchResults[s.searchIndex]
		s.searchIndex++
	} else if matchCount == 0 {
		matchCount = 1
	}
	return s.uid, 77, matchCount, s.searchErr
}

func (s *reconcileImapStub) FetchMessage(
	_ context.Context, _ transport.ImapConfig, mailbox string, _ uint32, _ uint32, _ int64,
) ([]byte, error) {
	s.fetchCalls++
	s.fetchBox = mailbox
	return s.fetchRaw, s.fetchErr
}

func beginUnknownClaim(t *testing.T, root string, draft Draft) (string, string) {
	t.Helper()
	fingerprint := envelopeFingerprint(draft, "<claim@example.com>")
	mimeFingerprint, err := draftMIMEFingerprint(draft)
	if err != nil {
		t.Fatalf("draftMIMEFingerprint() error = %v", err)
	}
	if _, err := beginSendAttemptWithMIMEFingerprint(root, draft.Ref, nil, "<claim@example.com>", fingerprint, mimeFingerprint); err != nil {
		t.Fatalf("beginSendAttempt() error = %v", err)
	}
	return "<claim@example.com>", fingerprint
}

func TestReconcileUnknownClaimFindsSentMessage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := newTransportService(root, nil, nil, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)
	beginUnknownClaim(t, root, draft)
	imap := &reconcileImapStub{
		mailboxes: []transport.MailboxInfo{{Name: "Sent Messages", Flags: []string{"\\Sent"}}},
		uid:       42,
		fetchRaw: []byte("From: sender@icloud.com\r\n" +
			"To: recipient@example.com\r\n" +
			"Message-ID: <claim@example.com>\r\n" +
			"Subject: Send test\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n\r\nBody\r\n"),
	}
	service.send.Imap = imap

	result, err := service.ReconcileDraft(context.Background(), draft.Ref)
	if err != nil || result.Outcome != SendOutcomeSent || !result.Reconciled || result.DraftRetained || result.Receipt == nil {
		t.Fatalf("ReconcileDraft() = %+v, error = %v", result, err)
	}
	if imap.searchCalls != 1 || imap.searchBox != "Sent Messages" || imap.searchID != "<claim@example.com>" {
		t.Fatalf("IMAP search = %q in %q after %d calls", imap.searchID, imap.searchBox, imap.searchCalls)
	}
	if _, err := service.GetDraft(draft.Ref); errorCode(err) != "not_found" {
		t.Fatalf("reconciled draft error = %v, want not_found", err)
	}
	receipt, err := service.GetSendReceipt(draft.Ref)
	if err != nil || receipt.MessageID != "<claim@example.com>" || receipt.SentMailbox != "Sent Messages" {
		t.Fatalf("receipt after reconcile = %+v, error = %v", receipt, err)
	}
}

func TestReconcileUnknownClaimRejectsDuplicateMessageID(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := newTransportService(root, nil, nil, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)
	beginUnknownClaim(t, root, draft)
	imap := &reconcileImapStub{
		mailboxes:  []transport.MailboxInfo{{Name: "Sent", Flags: []string{"\\Sent"}}},
		uid:        42,
		matchCount: 2,
	}
	service.send.Imap = imap

	result, err := service.ReconcileDraft(context.Background(), draft.Ref)
	if errorCode(err) != "send_outcome_unverifiable" || result.Outcome != SendOutcomeUnknown {
		t.Fatalf("ReconcileDraft() = %+v, error = %v", result, err)
	}
	if imap.fetchCalls != 0 {
		t.Fatalf("FetchMessage() calls = %d, want 0 for duplicate candidates", imap.fetchCalls)
	}
}

func TestReconcileUnknownClaimRejectsIdentityMismatch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := newTransportService(root, nil, nil, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)
	beginUnknownClaim(t, root, draft)
	service.send.Imap = &reconcileImapStub{
		mailboxes: []transport.MailboxInfo{{Name: "Sent", Flags: []string{"\\Sent"}}},
		uid:       42,
		fetchRaw: []byte("From: sender@icloud.com\r\n" +
			"To: recipient@example.com\r\n" +
			"Message-ID: <claim@example.com>\r\n" +
			"Subject: Changed\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n\r\nBody\r\n"),
	}

	result, err := service.ReconcileDraft(context.Background(), draft.Ref)
	if errorCode(err) != "send_identity_mismatch" || result.Outcome != SendOutcomeUnknown {
		t.Fatalf("ReconcileDraft() = %+v, error = %v", result, err)
	}
}

func TestReconcileUnknownClaimNotFoundStaysUnknown(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := newTransportService(root, nil, nil, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)
	beginUnknownClaim(t, root, draft)
	service.send.Imap = &reconcileImapStub{
		mailboxes: []transport.MailboxInfo{{Name: "Sent", Flags: []string{"\\Sent"}}},
		searchErr: &transport.TransportError{Code: transport.CodeIMAPMessageNotFound, Message: "not found"},
	}

	result, err := service.ReconcileDraft(context.Background(), draft.Ref)
	if err == nil || errorCode(err) != "send_outcome_unverifiable" || result.Outcome != SendOutcomeUnknown {
		t.Fatalf("ReconcileDraft() = %+v, error = %v", result, err)
	}
	if !strings.Contains(err.Error(), "<claim@example.com>") ||
		!strings.Contains(err.Error(), "discard the draft") {
		t.Fatalf("unverifiable error lacks actionable context: %v", err)
	}
	retained, getErr := service.GetDraft(draft.Ref)
	if getErr != nil || retained.SendAttempt == nil || retained.SendAttempt.Outcome != SendOutcomeUnknown {
		t.Fatalf("claim after not-found = %+v, error = %v", retained.SendAttempt, getErr)
	}
}

func TestUnverifiableSendErrorPreservesInvalidRecipientEvidence(t *testing.T) {
	attempt := SendAttempt{
		MessageID: "<claim@example.com>",
		StartedAt: time.Date(2026, time.September, 7, 10, 30, 0, 0, time.UTC),
	}
	draft := Draft{
		To:  []Recipient{{Address: "not an address"}},
		CC:  []Recipient{{Address: "cc@example.com"}},
		BCC: []Recipient{{Address: "hidden@example.com"}},
	}

	err := unverifiableSendError(attempt, draft, "no matching Sent message")
	if errorCode(err) != "send_outcome_unverifiable" {
		t.Fatalf("unverifiableSendError() code = %q, want send_outcome_unverifiable", errorCode(err))
	}
	for _, value := range []string{
		"<claim@example.com>",
		"2026-09-07T10:30:00Z",
		"not an address, cc@example.com, hidden@example.com",
		"discard the draft",
	} {
		if !strings.Contains(err.Error(), value) {
			t.Fatalf("unverifiableSendError() = %q, want %q", err, value)
		}
	}
}

func TestReconcileUnknownClaimRejectsFingerprintMismatch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := newTransportService(root, nil, nil, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)
	if _, err := beginSendAttempt(root, draft.Ref, "<claim@example.com>", "stale-fingerprint"); err != nil {
		t.Fatalf("beginSendAttempt() error = %v", err)
	}
	service.send.Imap = &reconcileImapStub{}

	if _, err := service.ReconcileDraft(context.Background(), draft.Ref); err == nil ||
		errorCode(err) != "send_fingerprint_mismatch" {
		t.Fatalf("fingerprint mismatch error = %v", err)
	}
}

func TestReconcileLegacyUnknownClaimStaysBlocked(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := newTransportService(root, nil, nil, &stubCredentials{password: "secret"})
	draft := createTransportDraft(t, service)
	if _, err := beginSendAttempt(root, draft.Ref, "", ""); err != nil {
		t.Fatalf("beginSendAttempt() error = %v", err)
	}

	_, err := service.ReconcileDraft(context.Background(), draft.Ref)
	if err == nil || errorCode(err) != "send_reconcile_unavailable" {
		t.Fatalf("legacy claim error = %v", err)
	}
	if !strings.Contains(err.Error(), "no Message-ID") {
		t.Fatalf("blocked message lacks claim context: %v", err)
	}
}

type submitClaimSpec struct {
	ref         string
	messageID   string
	fingerprint string
	outcome     SendOutcome
}

// claimReadingSubmitter wraps a successful submission and records the send
// claim exactly as it exists when the submission starts (the crash window
// that TASK 046 closes).
type claimReadingSubmitter struct {
	t      *testing.T
	root   string
	spec   *submitClaimSpec
	result transport.SubmitEvidence
}

func (s *claimReadingSubmitter) Submit(
	_ context.Context, _ transport.SubmitConfig, _ string, _ []string, _ []byte,
) (transport.SubmitEvidence, error) {
	claim, err := readSendAttempt(s.root, s.spec.ref)
	if err != nil {
		s.t.Fatalf("read claim during submit: %v", err)
	}
	s.spec.messageID, s.spec.fingerprint, s.spec.outcome =
		claim.MessageID, claim.EnvelopeFingerprint, claim.Outcome
	return s.result, nil
}

func TestSendDraftClaimCarriesMessageIDBeforeSubmit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	spec := &submitClaimSpec{}
	service := NewServiceWithTransport(nil, root, SendTransport{
		Submitter: &claimReadingSubmitter{
			t: t, root: root, spec: spec,
			result: transport.SubmitEvidence{ServerResponse: "250 2.0.0 OK", MessageID: "<abc123@icloud.com>"},
		},
		Mirror:      &stubMirror{evidence: transport.AppendEvidence{Mailbox: "Sent", Appended: true}},
		Credentials: &stubCredentials{password: "secret"},
	})
	draft := createTransportDraft(t, service)
	spec.ref = draft.Ref

	if _, err := service.SendDraft(context.Background(), draft.Ref); err != nil {
		t.Fatalf("SendDraft() error = %v", err)
	}
	if spec.messageID == "" || spec.fingerprint == "" || spec.outcome != SendOutcomeUnknown {
		t.Fatalf("claim at submit time = %+v", spec)
	}
	if spec.fingerprint != envelopeFingerprint(draft, spec.messageID) {
		t.Fatalf("claim fingerprint %q does not match the draft envelope", spec.fingerprint)
	}
}

func TestVerifySentMessageIdentityRequiresMIMEProof(t *testing.T) {
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Subject", Body: "Body",
	}
	raw := []byte("From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Subject: Subject\r\n" +
		"Message-ID: <identity@example.com>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nBody\r\n")
	if err := verifySentMessageIdentity(raw, draft, "<identity@example.com>"); errorCode(err) != "send_identity_unverifiable" {
		t.Fatalf("verifySentMessageIdentity() error = %v, want send_identity_unverifiable", err)
	}
}

func TestVerifySentMessageIdentityChecksHTMLAttachmentsAndOrder(t *testing.T) {
	directory := t.TempDir()
	firstPath := filepath.Join(directory, "first.txt")
	secondPath := filepath.Join(directory, "second.bin")
	if err := os.WriteFile(firstPath, []byte("first"), 0o600); err != nil {
		t.Fatalf("WriteFile(first) error = %v", err)
	}
	if err := os.WriteFile(secondPath, []byte{0, 1, 2, 3}, 0o600); err != nil {
		t.Fatalf("WriteFile(second) error = %v", err)
	}
	firstDigest := sha256.Sum256([]byte("first"))
	secondDigest := sha256.Sum256([]byte{0, 1, 2, 3})
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Subject", Body: "Body", BodyHTML: "<p>HTML</p>",
		Attachments: []DraftAttachment{
			{Path: firstPath, Size: 5, SHA256: hex.EncodeToString(firstDigest[:])},
			{Path: secondPath, Size: 4, SHA256: hex.EncodeToString(secondDigest[:])},
		},
	}
	messageID := "<identity@example.com>"
	expected, err := draftMIMEFingerprint(draft)
	if err != nil {
		t.Fatalf("draftMIMEFingerprint() error = %v", err)
	}
	raw, err := BuildMessage(draft, messageID)
	if err != nil {
		t.Fatalf("BuildMessage() error = %v", err)
	}
	if err := verifySentMessageIdentity(raw, draft, messageID, expected); err != nil {
		t.Fatalf("verifySentMessageIdentity(equal) error = %v", err)
	}

	withoutHTML := draft
	withoutHTML.BodyHTML = ""
	missingHTML, err := BuildMessage(withoutHTML, messageID)
	if err != nil {
		t.Fatalf("BuildMessage(withoutHTML) error = %v", err)
	}
	if err := verifySentMessageIdentity(missingHTML, draft, messageID, expected); errorCode(err) != "send_identity_mismatch" {
		t.Fatalf("verifySentMessageIdentity(missing HTML) error = %v, want send_identity_mismatch", err)
	}

	changedAttachment := strings.Replace(string(raw), base64.StdEncoding.EncodeToString([]byte("first")), base64.StdEncoding.EncodeToString([]byte("other")), 1)
	if err := verifySentMessageIdentity([]byte(changedAttachment), draft, messageID, expected); errorCode(err) != "send_identity_mismatch" {
		t.Fatalf("verifySentMessageIdentity(changed attachment) error = %v, want send_identity_mismatch", err)
	}

	reordered := draft
	reordered.Attachments = []DraftAttachment{draft.Attachments[1], draft.Attachments[0]}
	reorderedRaw, err := BuildMessage(reordered, messageID)
	if err != nil {
		t.Fatalf("BuildMessage(reordered) error = %v", err)
	}
	if err := verifySentMessageIdentity(reorderedRaw, draft, messageID, expected); errorCode(err) != "send_identity_mismatch" {
		t.Fatalf("verifySentMessageIdentity(reordered) error = %v, want send_identity_mismatch", err)
	}
}

func TestVerifySentMessageIdentityAllowsBoundaryAndTransferReencoding(t *testing.T) {
	draft := Draft{
		From: "sender@example.com", To: []Recipient{{Address: "recipient@example.com"}},
		Subject: "Subject", Body: "Body", BodyHTML: "<p>HTML</p>",
	}
	messageID := "<identity@example.com>"
	expected, err := draftMIMEFingerprint(draft)
	if err != nil {
		t.Fatalf("draftMIMEFingerprint() error = %v", err)
	}
	raw, err := BuildMessage(draft, messageID)
	if err != nil {
		t.Fatalf("BuildMessage() error = %v", err)
	}
	boundaryPattern := regexp.MustCompile(`=_[0-9a-f]{32}`)
	seen := make(map[string]string)
	mutated := boundaryPattern.ReplaceAllStringFunc(string(raw), func(value string) string {
		if replacement, ok := seen[value]; ok {
			return replacement
		}
		replacement := "=_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		if len(seen) > 0 {
			replacement = "=_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}
		seen[value] = replacement
		return replacement
	})
	mutated = strings.Replace(mutated,
		"Content-Transfer-Encoding: quoted-printable\r\n\r\nBody",
		"Content-Transfer-Encoding: 8bit\r\n\r\nBody", 1)
	if err := verifySentMessageIdentity([]byte(mutated), draft, messageID, expected); err != nil {
		t.Fatalf("verifySentMessageIdentity(reencoded) error = %v", err)
	}
}
