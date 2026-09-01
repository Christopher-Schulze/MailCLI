package mailstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

type fallbackSpy struct {
	accountCalls        int
	mailboxCalls        int
	accounts            []mail.Account
	markRequest         mail.MarkMessageRequest
	transferRequest     mail.TransferMessageRequest
	deleteRef           string
	markHook            func()
	transferHook        func()
	deleteHook          func()
	saveHook            func()
	sendHook            func()
	sendCalls           int
	sendEvidence        mail.SendEvidence
	rawSource           string
	rawCalls            int
	saveAttachmentCalls int
	saveErr             error
	sendErr             error
}

type materializedSaveFallback struct {
	fallbackSpy
	materialized *mail.SendMaterialization
}

func (s *materializedSaveFallback) SaveDraftWithMaterialization(
	ctx context.Context,
	draft mail.Draft,
) (mail.MessageSummary, *mail.SendMaterialization, error) {
	summary, err := s.SaveDraft(ctx, draft)
	return summary, s.materialized, err
}

func (*fallbackSpy) Probe(context.Context, bool) mail.DiagnosticReport {
	return mail.DiagnosticReport{}
}
func (s *fallbackSpy) ListAccounts(context.Context) ([]mail.Account, error) {
	s.accountCalls++
	return s.accounts, nil
}
func (s *fallbackSpy) ListMailboxes(context.Context, mail.ListMailboxesRequest) ([]mail.Mailbox, error) {
	s.mailboxCalls++
	return nil, nil
}
func (*fallbackSpy) ListMessages(context.Context, mail.ListMessagesRequest) (mail.MessagePage, error) {
	return mail.MessagePage{}, nil
}
func (*fallbackSpy) GetMessage(context.Context, string) (mail.Message, error) {
	return mail.Message{}, nil
}
func (*fallbackSpy) OpenDraft(context.Context, string) (mail.Message, error) {
	return mail.Message{}, nil
}
func (s *fallbackSpy) GetRawSource(context.Context, string) (string, error) {
	s.rawCalls++
	return s.rawSource, nil
}
func (s *fallbackSpy) SaveAttachmentTo(context.Context, string, string, string) error {
	s.saveAttachmentCalls++
	return nil
}
func (s *fallbackSpy) SaveDraft(context.Context, mail.Draft) (mail.MessageSummary, error) {
	if s.saveHook != nil {
		s.saveHook()
	}
	return mail.MessageSummary{}, s.saveErr
}
func (s *fallbackSpy) SaveDraftWithMaterialization(
	ctx context.Context,
	draft mail.Draft,
) (mail.MessageSummary, *mail.SendMaterialization, error) {
	summary, err := s.SaveDraft(ctx, draft)
	return summary, materializationForDraft(draft), err
}
func (s *fallbackSpy) SendDraft(_ context.Context, draft mail.Draft) (mail.SendEvidence, error) {
	s.sendCalls++
	if s.sendHook != nil {
		s.sendHook()
	}
	if s.sendEvidence.InvocationStarted {
		return s.sendEvidence, s.sendErr
	}
	return mail.SendEvidence{
		InvocationStarted: true, AcceptedByMail: true, Materialized: sentMaterializationForDraft(draft),
	}, s.sendErr
}
func (s *fallbackSpy) MarkMessage(
	_ context.Context,
	request mail.MarkMessageRequest,
) (mail.MessageSummary, error) {
	s.markRequest = request
	if s.markHook != nil {
		s.markHook()
	}
	return mail.MessageSummary{}, nil
}
func (s *fallbackSpy) TransferMessage(
	_ context.Context,
	request mail.TransferMessageRequest,
) (mail.MessageSummary, error) {
	s.transferRequest = request
	if s.transferHook != nil {
		s.transferHook()
	}
	return mail.MessageSummary{}, nil
}
func (s *fallbackSpy) DeleteMessage(_ context.Context, request mail.DeleteMessageRequest) error {
	s.deleteRef = request.Ref
	if s.deleteHook != nil {
		s.deleteHook()
	}
	return nil
}
func (*fallbackSpy) Sync(context.Context, string) error { return nil }

func materializationForDraft(draft mail.Draft) *mail.SendMaterialization {
	body := draft.Body
	return &mail.SendMaterialization{
		From: draft.From, To: append([]mail.Recipient(nil), draft.To...),
		CC: append([]mail.Recipient(nil), draft.CC...), BCC: append([]mail.Recipient(nil), draft.BCC...),
		Subject: draft.Subject, Body: &body, AttachmentCount: len(draft.Attachments),
	}
}

func sentMaterializationForDraft(draft mail.Draft) *mail.SendMaterialization {
	materialized := materializationForDraft(draft)
	body := draft.Body + "\n\n--\nMail signature"
	materialized.Body = &body
	return materialized
}

func TestClientListAccountsUsesStoreWithoutFallback(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installSentMailboxFixture(t, store)
	insertSentMessageFixture(t, store, 104)
	spy := &fallbackSpy{accounts: []mail.Account{{EmailAddresses: []string{"fallback@example.com"}}}}
	client := &Client{store: store, fallback: spy}
	accounts, err := client.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if spy.accountCalls != 0 {
		t.Fatalf("fallback ListAccounts() calls = %d, want 0", spy.accountCalls)
	}
	if len(accounts) != 1 || len(accounts[0].EmailAddresses) != 1 ||
		accounts[0].EmailAddresses[0] != "alice@example.com" {
		t.Fatalf("ListAccounts() = %+v", accounts)
	}
	ref, err := mailref.DecodeAccount(accounts[0].Ref)
	if err != nil || ref.AccountID != testAccountID {
		t.Fatalf("DecodeAccount() = %+v, error = %v", ref, err)
	}
}

func TestClientListAccountsFailsClosedWithoutStoreCatalog(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	spy := &fallbackSpy{accounts: []mail.Account{{EmailAddresses: []string{"fallback@example.com"}}}}
	client := &Client{store: store, fallback: spy}
	_, err := client.ListAccounts(context.Background())
	if errorCodeForTest(err) != "account_catalog_incomplete" {
		t.Fatalf("ListAccounts() error = %v, want account_catalog_incomplete", err)
	}
	if spy.accountCalls != 0 {
		t.Fatalf("fallback ListAccounts() calls = %d, want 0", spy.accountCalls)
	}
}

func TestClientListAccountsFallsBackOnlyWhenStoreUnavailable(t *testing.T) {
	want := []mail.Account{{Name: "Fallback", EmailAddresses: []string{"fallback@example.com"}}}
	spy := &fallbackSpy{accounts: want}
	client := &Client{storeErr: operationError("mail_store_unavailable", "unavailable"), fallback: spy}
	accounts, err := client.ListAccounts(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0].Name != want[0].Name {
		t.Fatalf("ListAccounts() = %+v, error = %v", accounts, err)
	}
	if spy.accountCalls != 1 {
		t.Fatalf("fallback ListAccounts() calls = %d, want 1", spy.accountCalls)
	}
}

func TestClientNeverFallsBackToRecursiveMailboxScan(t *testing.T) {
	spy := &fallbackSpy{}
	client := &Client{storeErr: operationError("unsupported_mail_store_schema", "unsupported"), fallback: spy}
	_, err := client.ListMailboxes(context.Background(), mail.ListMailboxesRequest{})
	if errorCodeForTest(err) != "safe_mailbox_listing_unavailable" || spy.mailboxCalls != 0 {
		t.Fatalf("ListMailboxes() error = %v, fallback calls = %d", err, spy.mailboxCalls)
	}
}

func TestClientUsesRawMIMEForIncompleteBodyAndAttachmentFallback(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 3,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Quarterly Report")
	writeFixtureEMLX(t, store, 101, "imap://"+testAccountID+"/%5BGmail%5D/All", []byte(
		"From: Alice <alice@example.com>\r\nTo: Christopher <christopher@example.com>\r\n"+
			"Subject: Quarterly Report\r\nContent-Type: text/plain; charset=x-mailcli-unknown\r\n\r\npartial",
	))
	raw := "From: Alice <alice@example.com>\r\nTo: Christopher <christopher@example.com>\r\n" +
		"Subject: Quarterly Report\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nComplete raw body\r\n" +
		"--b\r\nContent-Disposition: attachment; filename=invoice.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\naW52b2ljZS1ieXRlcw==\r\n--b--\r\n"
	spy := &fallbackSpy{rawSource: raw}
	client := &Client{store: store, fallback: spy}
	message, err := client.GetMessage(context.Background(), messageRef)
	if err != nil || !message.ContentComplete || message.ContentSource != "mail_app_raw" ||
		message.Content != "Complete raw body" || len(message.Attachments) != 1 ||
		message.Summary.AttachmentCount != 1 || message.Attachments[0].ID != "2" ||
		!message.Attachments[0].SizeKnown {
		t.Fatalf("GetMessage() = %+v, error = %v", message, err)
	}
	output := filepath.Join(t.TempDir(), "invoice.pdf")
	if err := client.SaveAttachmentTo(context.Background(), messageRef, "2", output); err != nil {
		t.Fatalf("SaveAttachmentTo() error = %v", err)
	}
	bytes, err := os.ReadFile(output)
	if err != nil || string(bytes) != "invoice-bytes" {
		t.Fatalf("attachment bytes = %q, error = %v", bytes, err)
	}
	if spy.saveAttachmentCalls != 0 || spy.rawCalls != 2 {
		t.Fatalf("fallback raw calls = %d, attachment-id calls = %d", spy.rawCalls, spy.saveAttachmentCalls)
	}
}

func TestStoreAccountIdentityValidatesNewDraftSend(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installSentMailboxFixture(t, store)
	insertSentMessageFixture(t, store, 104)
	spy := &fallbackSpy{}
	spy.sendHook = func() { insertSentMessageFixture(t, store, 105) }
	client := &Client{store: store, fallback: spy}
	service := mail.NewServiceWithDraftRoot(client, filepath.Join(t.TempDir(), "drafts"))
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		From: "Alice <alice@example.com>", To: []mail.Recipient{{Address: "christopher@example.com"}},
		Subject: "Observed send", Body: "Body",
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	result, err := service.SendDraft(context.Background(), draft.Ref)
	if err != nil || !result.SentStoreObserved {
		t.Fatalf("SendDraft() = %+v, error = %v", result, err)
	}
	if spy.accountCalls != 0 {
		t.Fatalf("fallback ListAccounts() calls = %d, want 0", spy.accountCalls)
	}
}

func TestClientRevalidatesStoreRefBeforeMarkAndObservesState(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 1,
	})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("ListMessages() = %+v, error = %v", page, err)
	}
	spy := &fallbackSpy{}
	spy.markHook = func() {
		updateFixtureMessage(t, store, `UPDATE messages SET read = 1 WHERE ROWID = 101`)
	}
	client := &Client{store: store, fallback: spy}
	read := true
	result, err := client.MarkMessage(context.Background(), mail.MarkMessageRequest{
		Ref: page.Messages[0].Ref, Read: &read, AllowDraftMutation: true,
	})
	if err != nil || !result.Read {
		t.Fatalf("MarkMessage() = %+v, error = %v", result, err)
	}
	automationRef, err := mailref.DecodeMessage(spy.markRequest.Ref)
	if err != nil {
		t.Fatalf("DecodeMessage(automation ref) error = %v", err)
	}
	if automationRef.Version != mailref.FormatVersion || automationRef.IsStoreBound() || automationRef.LibraryID != "101" ||
		automationRef.ExpectedMessageID != "101@example.com" ||
		len(automationRef.MailboxPath) != 1 || automationRef.MailboxPath[0] != "All" {
		t.Fatalf("automation ref = %+v", automationRef)
	}
}

func TestClientRejectsStaleStoreMailboxIdentityBeforeWrite(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := mailref.DecodeMessage(page.Messages[0].Ref)
	if err != nil {
		t.Fatal(err)
	}
	ref.ExpectedStoreMailboxID++
	stale, err := mailref.EncodeMessage(ref)
	if err != nil {
		t.Fatal(err)
	}
	spy := &fallbackSpy{}
	client := &Client{store: store, fallback: spy}
	read := true
	_, err = client.MarkMessage(context.Background(), mail.MarkMessageRequest{Ref: stale, Read: &read})
	if errorCodeForTest(err) != "stale_reference" || spy.markRequest.Ref != "" {
		t.Fatalf("MarkMessage() error = %v, bridge request = %+v", err, spy.markRequest)
	}
}

func TestClientPromotesAcceptedSendToSentStoreObserved(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installSentMailboxFixture(t, store)
	spy := &fallbackSpy{}
	spy.sendHook = func() { insertSentMessageFixture(t, store, 104) }
	client := &Client{store: store, fallback: spy}
	draft := mail.Draft{
		From:    "alice@example.com",
		To:      []mail.Recipient{{Address: "christopher@example.com"}},
		Subject: "Observed send", Body: "Body",
	}
	baseline, err := client.PrepareSend(context.Background(), draft)
	if err != nil {
		t.Fatalf("PrepareSend() error = %v", err)
	}
	draft.PreparedSendBaseline = &baseline
	evidence, err := client.SendDraft(context.Background(), draft)
	if err != nil || !evidence.AcceptedByMail || !evidence.SentStoreObserved ||
		evidence.ObservedMessageRef == "" || evidence.ObservationBaseline == nil {
		t.Fatalf("SendDraft() = %+v, error = %v", evidence, err)
	}
}

func TestClientRefusesSentObservationWithoutExactNativeBody(t *testing.T) {
	tests := []struct {
		name         string
		materialized *mail.SendMaterialization
		wantCode     string
	}{
		{name: "missing materialization", wantCode: "send_materialization_missing"},
		{
			name: "missing body",
			materialized: &mail.SendMaterialization{
				From: "alice@example.com", To: []mail.Recipient{{Address: "christopher@example.com"}},
				Subject: "Observed send",
			},
			wantCode: "send_materialization_invalid",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, _ := newSearchFixture(t)
			closeTestResource(t, store, "test store")
			installSentMailboxFixture(t, store)
			spy := &fallbackSpy{sendEvidence: mail.SendEvidence{
				InvocationStarted: true, AcceptedByMail: true, Materialized: test.materialized,
			}}
			spy.sendHook = func() { insertSentMessageFixture(t, store, 104) }
			client := &Client{store: store, fallback: spy}
			draft := mail.Draft{
				From: "alice@example.com", To: []mail.Recipient{{Address: "christopher@example.com"}},
				Subject: "Observed send", Body: "Body",
			}
			baseline, err := client.PrepareSend(context.Background(), draft)
			if err != nil {
				t.Fatalf("PrepareSend() error = %v", err)
			}
			draft.PreparedSendBaseline = &baseline
			evidence, err := client.SendDraft(context.Background(), draft)
			if evidence.SentStoreObserved || errorWithCode(err, test.wantCode) == nil {
				t.Fatalf("SendDraft() = %+v, error = %v, want %s", evidence, err, test.wantCode)
			}
		})
	}
}

func TestClientPropagatesPrivateCleanupFailureAfterObservedSend(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installSentMailboxFixture(t, store)
	spy := &fallbackSpy{sendErr: operationError(
		"attachment_snapshot_cleanup_failed", "private attachment snapshot remained",
	)}
	spy.sendHook = func() { insertSentMessageFixture(t, store, 104) }
	client := &Client{store: store, fallback: spy}
	draft := mail.Draft{
		From: "alice@example.com", To: []mail.Recipient{{Address: "christopher@example.com"}},
		Subject: "Observed send", Body: "Body",
	}
	baseline, err := client.PrepareSend(context.Background(), draft)
	if err != nil {
		t.Fatalf("PrepareSend() error = %v", err)
	}
	draft.PreparedSendBaseline = &baseline
	evidence, err := client.SendDraft(context.Background(), draft)
	if !evidence.SentStoreObserved || errorWithCode(err, "attachment_snapshot_cleanup_failed") == nil {
		t.Fatalf("SendDraft() = %+v, error = %v", evidence, err)
	}
}

func TestClientObservesBodyOnlyNativeReplyFromMaterializedHeaders(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installSentMailboxFixture(t, store)
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 3,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	materializedBody := "Body\n\n--\nMail signature"
	spy := &fallbackSpy{sendEvidence: mail.SendEvidence{
		InvocationStarted: true, AcceptedByMail: true,
		Materialized: &mail.SendMaterialization{
			From: "alice@example.com", To: []mail.Recipient{{Address: "christopher@example.com"}},
			Subject: "Observed send", Body: &materializedBody, AttachmentCount: 0,
		},
	}}
	spy.sendHook = func() { insertSentMessageFixture(t, store, 104) }
	client := &Client{store: store, fallback: spy}
	draft := mail.Draft{
		Kind: mail.DraftKindReply, SourceRef: messageRefWithSubject(t, page.Messages, "Quarterly Report"),
		Body: "Body",
	}
	baseline, err := client.PrepareSend(context.Background(), draft)
	if err != nil {
		t.Fatalf("PrepareSend() error = %v", err)
	}
	draft.PreparedSendBaseline = &baseline
	evidence, err := client.SendDraft(context.Background(), draft)
	if err != nil || !evidence.SentStoreObserved || evidence.ObservedMessageRef == "" {
		t.Fatalf("SendDraft() = %+v, error = %v", evidence, err)
	}
}

func TestClientObservesForwardWithOriginalAttachmentFingerprint(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installSentMailboxFixture(t, store)
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 3,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	attachmentBytes := []byte("forwarded source bytes")
	writeFixtureEMLX(t, store, 101, "imap://"+testAccountID+"/%5BGmail%5D/All", sentFixtureSource(101, sentMessageFixture{
		Body: "Source body", Attachments: []fixtureAttachment{{Name: "invoice.pdf", Content: attachmentBytes}},
	}))
	materializedBody := "Body\n\n--\nMail signature"
	spy := &fallbackSpy{sendEvidence: mail.SendEvidence{
		InvocationStarted: true, AcceptedByMail: true,
		Materialized: &mail.SendMaterialization{
			From: "alice@example.com", To: []mail.Recipient{{Address: "christopher@example.com"}},
			Subject: "Observed send", Body: &materializedBody, AttachmentCount: 1,
		},
	}}
	spy.sendHook = func() {
		insertSentMessageFixtureWithDetails(t, store, 104, sentMessageFixture{
			Body: "Body", RecipientTypes: []int{0},
			Attachments: []fixtureAttachment{{Name: "invoice.pdf", Content: attachmentBytes}},
		})
	}
	client := &Client{store: store, fallback: spy}
	draft := mail.Draft{
		Kind: mail.DraftKindForward, SourceRef: messageRefWithSubject(t, page.Messages, "Quarterly Report"),
		Body: "Body",
	}
	baseline, err := client.PrepareSend(context.Background(), draft)
	if err != nil {
		t.Fatalf("PrepareSend() error = %v", err)
	}
	draft.PreparedSendBaseline = &baseline
	evidence, err := client.SendDraft(context.Background(), draft)
	if err != nil || !evidence.SentStoreObserved {
		t.Fatalf("SendDraft() = %+v, error = %v", evidence, err)
	}
}

func TestClientRefusesUnpreparedSendBeforeAutomation(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installSentMailboxFixture(t, store)
	spy := &fallbackSpy{}
	client := &Client{store: store, fallback: spy}
	_, err := client.SendDraft(context.Background(), mail.Draft{
		From: "alice@example.com", To: []mail.Recipient{{Address: "christopher@example.com"}},
		Subject: "Unprepared", Body: "Body",
	})
	if errorCodeForTest(err) != "send_prepare_required" || spy.sendCalls != 0 {
		t.Fatalf("SendDraft() error = %v, automation calls = %d", err, spy.sendCalls)
	}
}

func TestClientReconcilesPreparedSendFromStoreOnly(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installSentMailboxFixture(t, store)
	client := &Client{store: store, fallback: &fallbackSpy{}}
	draft := mail.Draft{
		From: "alice@example.com", To: []mail.Recipient{{Address: "christopher@example.com"}},
		Subject: "Observed send", Body: "Body",
	}
	baseline, err := client.PrepareSend(context.Background(), draft)
	if err != nil {
		t.Fatalf("PrepareSend() error = %v", err)
	}
	insertSentMessageFixture(t, store, 104)
	evidence, err := client.ReconcileSend(context.Background(), draft, mail.SendAttempt{
		InvocationStarted: true, ObservationBaseline: &baseline,
		Materialized: sentMaterializationForDraft(draft),
	})
	if err != nil || !evidence.SentStoreObserved || evidence.ObservedMessageRef == "" {
		t.Fatalf("ReconcileSend() = %+v, error = %v", evidence, err)
	}
	baseline.StoreUUID = "different-store"
	if _, err := client.ReconcileSend(context.Background(), draft, mail.SendAttempt{
		ObservationBaseline: &baseline,
	}); errorCodeForTest(err) != "send_reconcile_unavailable" {
		t.Fatalf("ReconcileSend(wrong store) error = %v", err)
	}
}

func TestClientReturnsOnlyObservedNativeDraft(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installDraftsMailboxFixture(t, store)
	spy := &fallbackSpy{}
	spy.saveHook = func() { insertDraftMessageFixture(t, store, 104) }
	client := &Client{store: store, fallback: spy}
	message, err := client.SaveDraft(context.Background(), mail.Draft{
		From:    "alice@example.com",
		To:      []mail.Recipient{{Address: "christopher@example.com"}},
		Subject: "Observed draft", Body: "Body",
	})
	if err != nil || message.Ref == "" || message.Subject != "Observed draft" {
		t.Fatalf("SaveDraft() = %+v, error = %v", message, err)
	}
}

func TestClientPropagatesPrivateCleanupFailureAfterObservedDraftSave(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installDraftsMailboxFixture(t, store)
	spy := &fallbackSpy{saveErr: operationError(
		"bridge_cleanup_failed", "private bridge request remained",
	)}
	spy.saveHook = func() { insertDraftMessageFixture(t, store, 104) }
	client := &Client{store: store, fallback: spy}
	message, err := client.SaveDraft(context.Background(), mail.Draft{
		From: "alice@example.com", To: []mail.Recipient{{Address: "christopher@example.com"}},
		Subject: "Observed draft", Body: "Body",
	})
	if message.Ref == "" || errorWithCode(err, "bridge_cleanup_failed") == nil {
		t.Fatalf("SaveDraft() = %+v, error = %v", message, err)
	}
}

func TestClientRefusesDraftObservationWithoutExactNativeBody(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installDraftsMailboxFixture(t, store)
	fallback := &materializedSaveFallback{materialized: &mail.SendMaterialization{
		From: "alice@example.com", To: []mail.Recipient{{Address: "christopher@example.com"}},
		Subject: "Observed draft",
	}}
	fallback.saveHook = func() { insertDraftMessageFixture(t, store, 104) }
	client := &Client{store: store, fallback: fallback}
	evidence, err := client.SaveDraftWithEvidence(context.Background(), mail.Draft{
		From: "alice@example.com", To: []mail.Recipient{{Address: "christopher@example.com"}},
		Subject: "Observed draft", Body: "Body",
	})
	if evidence.ObservedMessage.Ref != "" || errorWithCode(err, "send_materialization_invalid") == nil {
		t.Fatalf("SaveDraftWithEvidence() = %+v, error = %v", evidence, err)
	}
}

func TestClientObservesBodyOnlyNativeReplyDraftFromMaterializedHeaders(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installDraftsMailboxFixture(t, store)
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 3,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	materializedBody := "Body"
	fallback := &materializedSaveFallback{materialized: &mail.SendMaterialization{
		From: "alice@example.com", To: []mail.Recipient{{Address: "christopher@example.com"}},
		Subject: "Observed draft", Body: &materializedBody, AttachmentCount: 0,
	}}
	fallback.saveHook = func() { insertDraftMessageFixture(t, store, 104) }
	client := &Client{store: store, fallback: fallback}
	message, err := client.SaveDraft(context.Background(), mail.Draft{
		Kind: mail.DraftKindReply, SourceRef: messageRefWithSubject(t, page.Messages, "Quarterly Report"),
		From: "alice@example.com", Body: "Body",
	})
	if err != nil || message.Ref == "" || message.Subject != "Observed draft" {
		t.Fatalf("SaveDraft() = %+v, error = %v", message, err)
	}
}

func TestClientObservesMoveInDestinationAndSource(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installSentMailboxFixture(t, store)
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Status Update")
	destinationRef, err := mailref.EncodeMailbox(testAccountID, []string{"Sent"})
	if err != nil {
		t.Fatal(err)
	}
	spy := &fallbackSpy{}
	spy.transferHook = func() {
		updateFixtureMessage(t, store, `UPDATE messages SET mailbox = 4 WHERE ROWID = 102`)
	}
	client := &Client{store: store, fallback: spy}
	result, err := client.TransferMessage(context.Background(), mail.TransferMessageRequest{
		Ref: messageRef, DestinationMailbox: destinationRef,
	})
	if err != nil || result.MailboxRef != destinationRef || result.Subject != "Status Update" {
		t.Fatalf("TransferMessage() = %+v, error = %v", result, err)
	}
	forwarded, err := mailref.DecodeMessage(spy.transferRequest.Ref)
	if err != nil || forwarded.Version != mailref.FormatVersion || forwarded.IsStoreBound() {
		t.Fatalf("forwarded transfer ref = %+v, error = %v", forwarded, err)
	}
}

func TestTransferCandidatesRequireStableMessageIdentity(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installSentMailboxFixture(t, store)
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	destinationRef, err := mailref.EncodeMailbox(testAccountID, []string{"Sent"})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := store.captureTransferBaseline(
		context.Background(), messageRefWithSubject(t, page.Messages, "Status Update"), destinationRef,
	)
	if err != nil {
		t.Fatalf("captureTransferBaseline() error = %v", err)
	}
	updateFixtureMessage(t, store, `
		INSERT INTO messages(
			ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,
			mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color
		) VALUES (104,9004,9005,1,2,2,400,400,4,0,1,0,0,100,4,0,400,0)
	`)
	candidates, err := store.transferCandidates(context.Background(), baseline)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("unrelated transfer candidates = %+v, error = %v", candidates, err)
	}
	updateFixtureMessage(t, store, `
		INSERT INTO messages(
			ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,
			mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color
		) VALUES (105,1002,2002,1,2,2,401,401,4,0,1,0,0,100,5,0,401,0)
	`)
	candidates, err = store.transferCandidates(context.Background(), baseline)
	if err != nil || len(candidates) != 1 || candidates[0].RowID != 105 {
		t.Fatalf("stable transfer candidates = %+v, error = %v", candidates, err)
	}
}

func TestCopyObservationRejectsPreexistingDestinationMembership(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installSentMailboxFixture(t, store)
	updateFixtureMessage(t, store, `INSERT INTO labels(message_id,mailbox_id) VALUES (102,4)`)
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	destinationRef, err := mailref.EncodeMailbox(testAccountID, []string{"Sent"})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := store.captureTransferBaseline(
		context.Background(), messageRefWithSubject(t, page.Messages, "Status Update"), destinationRef,
	)
	if err != nil || !baseline.DestinationHadSource {
		t.Fatalf("captureTransferBaseline() = %+v, error = %v", baseline, err)
	}
	candidates, err := store.transferCandidates(context.Background(), baseline)
	if err != nil {
		t.Fatalf("transferCandidates() error = %v", err)
	}
	if observed := observedTransferCandidates(candidates, baseline, true); len(observed) != 0 {
		t.Fatalf("copy observation accepted preexisting destination: %+v", observed)
	}
}

func TestClientObservesDeletionFromLogicalLabelMailbox(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	spy := &fallbackSpy{}
	spy.deleteHook = func() {
		updateFixtureMessage(t, store, `DELETE FROM labels WHERE message_id = 101 AND mailbox_id = 1`)
	}
	client := &Client{store: store, fallback: spy}
	if err := client.DeleteMessage(context.Background(), mail.DeleteMessageRequest{
		Ref: page.Messages[0].Ref, AllowDraftMutation: true,
	}); err != nil {
		t.Fatalf("DeleteMessage() error = %v", err)
	}
	forwarded, err := mailref.DecodeMessage(spy.deleteRef)
	if err != nil || forwarded.Version != mailref.FormatVersion || forwarded.IsStoreBound() {
		t.Fatalf("forwarded delete ref = %+v, error = %v", forwarded, err)
	}
}

func TestClientFailsClosedWhenDraftMailboxIdentityIsUnavailable(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 1,
	})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("ListMessages() = %+v, error = %v", page, err)
	}
	spy := &fallbackSpy{}
	client := &Client{store: store, fallback: spy}
	read := true
	_, err = client.MarkMessage(context.Background(), mail.MarkMessageRequest{
		Ref: page.Messages[0].Ref, Read: &read,
	})
	if err == nil || !strings.Contains(err.Error(), "inspect Drafts mailbox identity") {
		t.Fatalf("MarkMessage() error = %v", err)
	}
	if spy.markRequest.Ref != "" {
		t.Fatalf("fallback received mutation after incomplete Drafts identity: %+v", spy.markRequest)
	}
}

func TestClientBlocksUnconfirmedDraftMutations(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installDraftsMailboxFixture(t, store)
	insertDraftMessageFixture(t, store, 104)
	draftsRef, err := mailref.EncodeMailbox(testAccountID, []string{"Drafts"})
	if err != nil {
		t.Fatalf("EncodeMailbox() error = %v", err)
	}
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: draftsRef, Limit: 1,
	})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("ListMessages(Drafts) = %+v, error = %v", page, err)
	}
	ref := page.Messages[0].Ref
	spy := &fallbackSpy{}
	client := &Client{store: store, fallback: spy}
	read := false
	_, markErr := client.MarkMessage(context.Background(), mail.MarkMessageRequest{
		Ref: ref, Read: &read,
	})
	_, moveErr := client.TransferMessage(context.Background(), mail.TransferMessageRequest{
		Ref: ref, DestinationMailbox: inboxRef,
	})
	deleteErr := client.DeleteMessage(context.Background(), mail.DeleteMessageRequest{Ref: ref})
	for name, mutationErr := range map[string]error{
		"mark": markErr, "move": moveErr, "delete": deleteErr,
	} {
		if errorCodeForTest(mutationErr) != "draft_mutation_confirmation_required" {
			t.Errorf("%s error = %v", name, mutationErr)
		}
	}
	if spy.markRequest.Ref != "" || spy.transferRequest.Ref != "" || spy.deleteRef != "" {
		t.Fatalf("fallback received blocked draft mutation: %+v", spy)
	}
	if err := client.rejectUnconfirmedDraftMutation(context.Background(), ref, true); err != nil {
		t.Fatalf("explicit draft mutation confirmation error = %v", err)
	}
}

func updateFixtureMessage(t *testing.T, store *Store, query string) {
	t.Helper()
	path := filepath.Join(store.versionRoot, "MailData", envelopeIndexName)
	writer := openTestWriter(t, path)
	if _, err := writer.Exec(query); err != nil {
		closeTestResourceNow(t, writer, "fixture writer")
		t.Fatalf("update fixture message: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close fixture writer: %v", err)
	}
}

func errorCodeForTest(err error) string {
	if err == nil {
		return ""
	}
	if typed, ok := err.(interface{ ErrorCode() string }); ok {
		return typed.ErrorCode()
	}
	return ""
}

func messageRefWithSubject(t *testing.T, messages []mail.MessageSummary, subject string) string {
	t.Helper()
	for _, message := range messages {
		if message.Subject == subject {
			return message.Ref
		}
	}
	t.Fatalf("message subject %q not found", subject)
	return ""
}
