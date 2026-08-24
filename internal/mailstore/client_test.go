package mailstore

import (
	"context"
	"path/filepath"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

type fallbackSpy struct {
	accountCalls    int
	mailboxCalls    int
	accounts        []mail.Account
	markRequest     mail.MarkMessageRequest
	transferRequest mail.TransferMessageRequest
	deleteRef       string
	markHook        func()
	transferHook    func()
	deleteHook      func()
	saveHook        func()
	sendHook        func()
	sendCalls       int
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
func (*fallbackSpy) GetRawSource(context.Context, string) (string, error)           { return "", nil }
func (*fallbackSpy) SaveAttachmentTo(context.Context, string, string, string) error { return nil }
func (s *fallbackSpy) SaveDraft(context.Context, mail.Draft) (mail.MessageSummary, error) {
	if s.saveHook != nil {
		s.saveHook()
	}
	return mail.MessageSummary{}, nil
}
func (s *fallbackSpy) SendDraft(context.Context, mail.Draft) (mail.SendEvidence, error) {
	s.sendCalls++
	if s.sendHook != nil {
		s.sendHook()
	}
	return mail.SendEvidence{InvocationStarted: true, AcceptedByMail: true}, nil
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
func (s *fallbackSpy) DeleteMessage(_ context.Context, ref string) error {
	s.deleteRef = ref
	if s.deleteHook != nil {
		s.deleteHook()
	}
	return nil
}
func (*fallbackSpy) Sync(context.Context, string) error { return nil }

func TestClientListAccountsUsesStoreWithoutFallback(t *testing.T) {
	store, _ := newSearchFixture(t)
	defer store.Close()
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
	defer store.Close()
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

func TestStoreAccountIdentityValidatesNewDraftSend(t *testing.T) {
	store, _ := newSearchFixture(t)
	defer store.Close()
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
	defer store.Close()
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
		Ref: page.Messages[0].Ref, Read: &read,
	})
	if err != nil || !result.Read {
		t.Fatalf("MarkMessage() = %+v, error = %v", result, err)
	}
	automationRef, err := mailref.DecodeMessage(spy.markRequest.Ref)
	if err != nil {
		t.Fatalf("DecodeMessage(automation ref) error = %v", err)
	}
	if automationRef.Version != mailref.FormatVersion || automationRef.IsStoreBound() || automationRef.LibraryID != "101" ||
		len(automationRef.MailboxPath) != 1 || automationRef.MailboxPath[0] != "All" {
		t.Fatalf("automation ref = %+v", automationRef)
	}
}

func TestClientRejectsStaleStoreMailboxIdentityBeforeWrite(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	defer store.Close()
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
	defer store.Close()
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

func TestClientRefusesUnpreparedSendBeforeAutomation(t *testing.T) {
	store, _ := newSearchFixture(t)
	defer store.Close()
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
	defer store.Close()
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
	defer store.Close()
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

func TestClientObservesMoveInDestinationAndSource(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	defer store.Close()
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
	defer store.Close()
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
	defer store.Close()
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
	defer store.Close()
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
	if err := client.DeleteMessage(context.Background(), page.Messages[0].Ref); err != nil {
		t.Fatalf("DeleteMessage() error = %v", err)
	}
	forwarded, err := mailref.DecodeMessage(spy.deleteRef)
	if err != nil || forwarded.Version != mailref.FormatVersion || forwarded.IsStoreBound() {
		t.Fatalf("forwarded delete ref = %+v, error = %v", forwarded, err)
	}
}

func updateFixtureMessage(t *testing.T, store *Store, query string) {
	t.Helper()
	path := filepath.Join(store.versionRoot, "MailData", envelopeIndexName)
	writer := openTestWriter(t, path)
	if _, err := writer.Exec(query); err != nil {
		writer.Close()
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
