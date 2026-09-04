package mailstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
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
func (s *fallbackSpy) DeleteMessage(_ context.Context, request mail.DeleteMessageRequest) (mail.DeleteResult, error) {
	s.deleteRef = request.Ref
	if s.deleteHook != nil {
		s.deleteHook()
	}
	return mail.DeleteResult{MessageRef: request.Ref, Deleted: true}, nil
}
func (*fallbackSpy) Sync(context.Context, string) error { return nil }

type stubImapOperator struct {
	boxes        []transport.MailboxInfo
	uid          uint32
	uidvalidity  uint32
	raw          []byte
	lastCommand  string
	lastUsername string
	lastMailbox  string
	status       transport.MailboxStatus
	err          error
}

func (s *stubImapOperator) AppendToSent(ctx context.Context, cfg transport.ImapConfig, msg []byte, messageID string) (transport.AppendEvidence, error) {
	return transport.AppendEvidence{Mailbox: "Sent", Appended: true}, nil
}

func (s *stubImapOperator) ListMailboxes(ctx context.Context, cfg transport.ImapConfig) ([]transport.MailboxInfo, error) {
	if s.err != nil {
		return nil, s.err
	}
	if len(s.boxes) > 0 {
		return s.boxes, nil
	}
	return []transport.MailboxInfo{
		{Name: "INBOX"},
		{Name: "Sent", Flags: []string{"\\Sent"}},
		{Name: "Trash", Flags: []string{"\\Trash"}},
		{Name: "Archive", Flags: []string{"\\Archive"}},
	}, nil
}

func (s *stubImapOperator) SearchUID(ctx context.Context, cfg transport.ImapConfig, mailbox string, messageID string) (uint32, uint32, error) {
	if s.err != nil {
		return 0, 0, s.err
	}
	uid := s.uid
	if uid == 0 {
		uid = 101
	}
	val := s.uidvalidity
	if val == 0 {
		val = 12345
	}
	return uid, val, nil
}

func (s *stubImapOperator) SetFlags(ctx context.Context, cfg transport.ImapConfig, mailbox string, uid uint32, addFlags, removeFlags []string) (transport.MutationEvidence, error) {
	if s.err != nil {
		return transport.MutationEvidence{}, s.err
	}
	s.lastCommand = "STORE"
	s.lastUsername = cfg.Username
	s.lastMailbox = mailbox
	return transport.MutationEvidence{
		Command:        "STORE",
		ServerResponse: "OK STORE completed",
		Mailbox:        mailbox,
		UID:            uid,
	}, nil
}

func (s *stubImapOperator) CopyMessage(ctx context.Context, cfg transport.ImapConfig, srcMailbox string, uid uint32, dstMailbox string) (transport.MutationEvidence, error) {
	if s.err != nil {
		return transport.MutationEvidence{}, s.err
	}
	s.lastCommand = "COPY"
	return transport.MutationEvidence{
		Command:        "COPY",
		ServerResponse: "OK COPY completed",
		Mailbox:        srcMailbox,
		TargetMailbox:  dstMailbox,
		UID:            uid,
	}, nil
}

func (s *stubImapOperator) MoveMessage(ctx context.Context, cfg transport.ImapConfig, srcMailbox string, uid uint32, dstMailbox string) (transport.MutationEvidence, error) {
	if s.err != nil {
		return transport.MutationEvidence{}, s.err
	}
	s.lastCommand = "MOVE"
	s.lastUsername = cfg.Username
	s.lastMailbox = srcMailbox
	return transport.MutationEvidence{
		Command:        "MOVE",
		ServerResponse: "OK MOVE completed",
		Mailbox:        srcMailbox,
		TargetMailbox:  dstMailbox,
		UID:            uid,
	}, nil
}

func (s *stubImapOperator) DeleteMessage(ctx context.Context, cfg transport.ImapConfig, srcMailbox string, uid uint32) (transport.MutationEvidence, error) {
	if s.err != nil {
		return transport.MutationEvidence{}, s.err
	}
	s.lastCommand = "DELETE"
	s.lastUsername = cfg.Username
	s.lastMailbox = srcMailbox
	return transport.MutationEvidence{
		Command:        "DELETE",
		ServerResponse: "OK DELETE completed",
		Mailbox:        srcMailbox,
		TargetMailbox:  "Trash",
		UID:            uid,
	}, nil
}

func (s *stubImapOperator) FetchMessage(ctx context.Context, cfg transport.ImapConfig, mailbox string, uid uint32) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.raw, nil
}

func (s *stubImapOperator) CheckStatus(ctx context.Context, cfg transport.ImapConfig, mailbox string) (transport.MailboxStatus, error) {
	if s.err != nil {
		return transport.MailboxStatus{}, s.err
	}
	return s.status, nil
}

type stubCredentials map[string]string

func (s stubCredentials) Load(account string) (string, error) {
	if pw, ok := s[account]; ok {
		return pw, nil
	}
	return "test-password", nil
}
func (s stubCredentials) Store(account, password string) error { s[account] = password; return nil }
func (s stubCredentials) Delete(account string) error          { delete(s, account); return nil }

// strictCredentials returns "" for unknown accounts so tests can prove the
// mutation path rejects an identity without a stored credential.
type strictCredentials map[string]string

func (s strictCredentials) Load(account string) (string, error) {
	if pw, ok := s[account]; ok {
		return pw, nil
	}
	return "", nil
}
func (s strictCredentials) Store(account, password string) error { s[account] = password; return nil }
func (s strictCredentials) Delete(account string) error          { delete(s, account); return nil }
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
	installImapIdentityFixture(t, store, "identity@gmail.com")
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
	fakeImap := &stubImapOperator{
		raw:   []byte(raw),
		boxes: []transport.MailboxInfo{{Name: "INBOX"}},
		uid:   101,
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"identity@gmail.com": "secret"},
		},
	}
	message, err := client.GetMessage(context.Background(), messageRef)
	if err != nil || !message.ContentComplete || message.ContentSource != "imap_raw" ||
		message.Content != "Complete raw body" || len(message.Attachments) != 1 ||
		message.Summary.AttachmentCount != 1 || message.Attachments[0].ID != "2" ||
		!message.Attachments[0].SizeKnown {
		t.Fatalf("GetMessage() = %+v, error = %v", message, err)
	}
	if message.Summary.Ref == "" || message.Summary.Subject == "" ||
		message.Summary.Sender == "" || message.Summary.DateReceived == "" {
		t.Fatalf("hydrated message lost record summary: %+v", message.Summary)
	}
	output := filepath.Join(t.TempDir(), "invoice.pdf")
	if err := client.SaveAttachmentTo(context.Background(), messageRef, "2", output); err != nil {
		t.Fatalf("SaveAttachmentTo() error = %v", err)
	}
	bytes, err := os.ReadFile(output)
	if err != nil || string(bytes) != "invoice-bytes" {
		t.Fatalf("attachment bytes = %q, error = %v", bytes, err)
	}
}

// A missing local source hydrates over IMAP and the returned message keeps
// the store record's summary (ref, subject, sender, dates) so follow-up
// commands can still reference the message.
func TestClientHydrationFallbackKeepsRecordSummary(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "identity@gmail.com")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 3,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Quarterly Report")
	// Remove the .emlx source that newSearchFixture created so GetMessage
	// fails locally with message_source_missing and falls back to IMAP.
	location, err := parseMailboxURL("imap://" + testAccountID + "/%5BGmail%5D/All")
	if err != nil {
		t.Fatalf("parseMailboxURL() error = %v", err)
	}
	base, err := store.messageBasePath(location, 101)
	if err != nil {
		t.Fatalf("messageBasePath() error = %v", err)
	}
	if err := os.Remove(base + ".emlx"); err != nil {
		t.Fatalf("remove .emlx source: %v", err)
	}
	raw := "From: Alice <alice@example.com>\r\nSubject: Quarterly Report\r\n\r\nFull body\r\n"
	fakeImap := &stubImapOperator{
		raw:   []byte(raw),
		boxes: []transport.MailboxInfo{{Name: "INBOX"}},
		uid:   101,
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"identity@gmail.com": "secret"},
		},
	}
	message, err := client.GetMessage(context.Background(), messageRef)
	if err != nil || message.ContentSource != "imap_raw" || message.Content != "Full body" {
		t.Fatalf("GetMessage() = %+v, error = %v", message, err)
	}
	if message.Summary.Ref == "" || message.Summary.Subject != "Quarterly Report" ||
		message.Summary.Sender == "" || message.Summary.DateReceived == "" ||
		message.Summary.MailboxRef == "" {
		t.Fatalf("hydration fallback lost the record summary: %+v", message.Summary)
	}
}

func TestClientRevalidatesStoreRefBeforeMarkAndObservesState(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "identity@gmail.com")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 1,
	})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("ListMessages() = %+v, error = %v", page, err)
	}
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{{Name: "INBOX"}},
		uid:   101,
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"identity@gmail.com": "secret"},
		},
	}
	read := true
	result, err := client.MarkMessage(context.Background(), mail.MarkMessageRequest{
		Ref: page.Messages[0].Ref, Read: &read, AllowDraftMutation: true,
	})
	if err != nil || !result.Read || result.ServerTruth == nil || result.ServerTruth.Command != "STORE" {
		t.Fatalf("MarkMessage() = %+v, error = %v", result, err)
	}
	if fakeImap.lastCommand != "STORE" {
		t.Fatalf("expected IMAP STORE command, got %s", fakeImap.lastCommand)
	}
	if fakeImap.lastUsername != "identity@gmail.com" {
		t.Fatalf("IMAP username = %q, want the store-resolved identity identity@gmail.com", fakeImap.lastUsername)
	}
	if fakeImap.lastMailbox != "INBOX" {
		t.Fatalf("IMAP mailbox = %q, want INBOX", fakeImap.lastMailbox)
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

type stubSendSubmitter struct {
	calls    int
	lastFrom string
	lastTo   []string
	err      error
}

func (s *stubSendSubmitter) Submit(
	_ context.Context,
	_ transport.SubmitConfig,
	from string,
	rcpts []string,
	_ []byte,
) (transport.SubmitEvidence, error) {
	s.calls++
	s.lastFrom, s.lastTo = from, rcpts
	if s.err != nil {
		return transport.SubmitEvidence{}, s.err
	}
	return transport.SubmitEvidence{ServerResponse: "250 2.0.0 OK", MessageID: "<abc123@example.com>"}, nil
}

type stubSendMirror struct {
	calls int
	err   error
}

func (s *stubSendMirror) AppendToSent(
	_ context.Context,
	_ transport.ImapConfig,
	_ []byte,
	_ string,
) (transport.AppendEvidence, error) {
	s.calls++
	if s.err != nil {
		return transport.AppendEvidence{}, s.err
	}
	return transport.AppendEvidence{Mailbox: "Sent", Appended: true}, nil
}

type stubSendCredentials struct {
	password string
	loadErr  error
}

func (c *stubSendCredentials) Load(string) (string, error) {
	if c.loadErr != nil {
		return "", c.loadErr
	}
	return c.password, nil
}

func (c *stubSendCredentials) Store(string, string) error { return nil }

func (c *stubSendCredentials) Delete(string) error { return nil }

func TestClientSendDraftSubmitsAndMirrorsWithoutMailApp(t *testing.T) {
	submitter := &stubSendSubmitter{}
	mirror := &stubSendMirror{}
	client := &Client{send: mail.SendTransport{
		Submitter: submitter, Mirror: mirror, Credentials: &stubSendCredentials{password: "secret"},
	}}
	draft := mail.Draft{
		From: "alice@icloud.com", To: []mail.Recipient{{Address: "christopher@example.com"}},
		Subject: "Direct send", Body: "Body",
	}

	evidence, err := client.SendDraft(context.Background(), draft)
	if err != nil {
		t.Fatalf("SendDraft() error = %v", err)
	}
	if evidence.ServerResponse != "250 2.0.0 OK" || evidence.MessageID != "<abc123@example.com>" ||
		evidence.MirrorMailbox != "Sent" || !evidence.MirrorAppended {
		t.Fatalf("SendDraft() evidence = %+v", evidence)
	}
	if submitter.calls != 1 || submitter.lastFrom != "alice@icloud.com" ||
		len(submitter.lastTo) != 1 || submitter.lastTo[0] != "christopher@example.com" {
		t.Fatalf("submitter = %+v", submitter)
	}
	if mirror.calls != 1 {
		t.Fatalf("mirror calls = %d", mirror.calls)
	}
}

func TestClientSendDraftNeverResubmitsAfterMirrorFailure(t *testing.T) {
	submitter := &stubSendSubmitter{}
	mirror := &stubSendMirror{err: &transport.TransportError{
		Code: transport.CodeIMAPAppendFailed, Message: "NO mailbox",
	}}
	client := &Client{send: mail.SendTransport{
		Submitter: submitter, Mirror: mirror, Credentials: &stubSendCredentials{password: "secret"},
	}}
	draft := mail.Draft{
		From: "alice@icloud.com", To: []mail.Recipient{{Address: "christopher@example.com"}},
		Subject: "Direct send", Body: "Body",
	}

	evidence, err := client.SendDraft(context.Background(), draft)
	if errorCodeForTest(err) != "imap_append_failed" {
		t.Fatalf("SendDraft() error = %v", err)
	}
	if evidence.MessageID != "<abc123@example.com>" || evidence.ServerResponse != "250 2.0.0 OK" ||
		evidence.MirrorMailbox != "" {
		t.Fatalf("partial evidence = %+v", evidence)
	}
	if submitter.calls != 1 {
		t.Fatalf("submitter calls = %d, want exactly one submission", submitter.calls)
	}
}

func TestClientSendDraftWithoutTransportIsRejected(t *testing.T) {
	client := &Client{}
	_, err := client.SendDraft(context.Background(), mail.Draft{From: "alice@icloud.com"})
	if errorCodeForTest(err) != "send_transport_unavailable" {
		t.Fatalf("SendDraft() error = %v", err)
	}
}

func TestClientSendDraftMissingCredentialsBlocksSubmission(t *testing.T) {
	submitter := &stubSendSubmitter{}
	mirror := &stubSendMirror{}
	client := &Client{send: mail.SendTransport{
		Submitter: submitter, Mirror: mirror,
		Credentials: &stubSendCredentials{loadErr: os.ErrNotExist},
	}}
	_, err := client.SendDraft(context.Background(), mail.Draft{
		From: "alice@icloud.com", To: []mail.Recipient{{Address: "christopher@example.com"}},
	})
	if errorCodeForTest(err) != "smtp_credentials_missing" {
		t.Fatalf("SendDraft() error = %v", err)
	}
	if submitter.calls != 0 || mirror.calls != 0 {
		t.Fatalf("submitter calls = %d, mirror calls = %d", submitter.calls, mirror.calls)
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
	installImapIdentityFixture(t, store, "identity@gmail.com")
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
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{
			{Name: "INBOX"},
			{Name: "Sent", Flags: []string{"\\Sent"}},
		},
		uid: 102,
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"identity@gmail.com": "secret"},
		},
	}
	result, err := client.TransferMessage(context.Background(), mail.TransferMessageRequest{
		Ref: messageRef, DestinationMailbox: destinationRef,
	})
	if err != nil || result.MailboxRef != destinationRef || result.Subject != "Status Update" || result.ServerTruth == nil || result.ServerTruth.Command != "MOVE" {
		t.Fatalf("TransferMessage() = %+v, error = %v", result, err)
	}
	if fakeImap.lastCommand != "MOVE" {
		t.Fatalf("expected IMAP MOVE command, got %s", fakeImap.lastCommand)
	}
	if fakeImap.lastUsername != "identity@gmail.com" {
		t.Fatalf("IMAP username = %q, want the store-resolved identity identity@gmail.com", fakeImap.lastUsername)
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
	installImapIdentityFixture(t, store, "identity@gmail.com")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{
			{Name: "INBOX"},
			{Name: "Trash", Flags: []string{"\\Trash"}},
		},
		uid: 101,
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"identity@gmail.com": "secret"},
		},
	}
	result, err := client.DeleteMessage(context.Background(), mail.DeleteMessageRequest{
		Ref: page.Messages[0].Ref, AllowDraftMutation: true,
	})
	if err != nil {
		t.Fatalf("DeleteMessage() error = %v", err)
	}
	if result.ServerTruth == nil || result.ServerTruth.Command != "DELETE" ||
		result.ServerTruth.TargetMailbox != "Trash" || result.ServerTruth.UID == 0 {
		t.Fatalf("delete server truth = %+v", result.ServerTruth)
	}
	if fakeImap.lastCommand != "DELETE" {
		t.Fatalf("expected IMAP DELETE command, got %s", fakeImap.lastCommand)
	}
	if fakeImap.lastUsername != "identity@gmail.com" {
		t.Fatalf("IMAP username = %q, want the store-resolved identity identity@gmail.com", fakeImap.lastUsername)
	}
	strictClient := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: strictCredentials{"other@gmail.com": "secret"},
		},
	}
	if _, err := strictClient.DeleteMessage(context.Background(), mail.DeleteMessageRequest{
		Ref: page.Messages[0].Ref, AllowDraftMutation: true,
	}); err == nil {
		t.Fatal("DeleteMessage() succeeded without a stored credential for the account identity")
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
	_, deleteErr := client.DeleteMessage(context.Background(), mail.DeleteMessageRequest{Ref: ref})
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

func TestClientMessageThreadSourceReadsHeaderBlock(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	defer closeTestResource(t, store, "test store")
	client := &Client{store: store}
	page, listErr := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inboxRef, Limit: 10})
	if listErr != nil {
		t.Fatalf("ListMessages() error = %v", listErr)
	}
	ref := messageRefWithSubject(t, page.Messages, "Status Update")
	source, err := client.MessageThreadSource(context.Background(), ref)
	if err != nil {
		t.Fatalf("MessageThreadSource() error = %v", err)
	}
	if source.MessageID != "<102@example.com>" {
		t.Fatalf("message id = %q", source.MessageID)
	}
	if !strings.Contains(source.From, "alice@example.com") {
		t.Fatalf("from = %q", source.From)
	}
	if len(source.To) != 1 || source.To[0].Address != "christopher@example.com" {
		t.Fatalf("to = %+v", source.To)
	}

	writeFixtureEMLX(t, store, 102, "imap://"+testAccountID+"/INBOX", []byte(
		"From: Alice <alice@example.com>\r\n"+
			"To: Christopher <christopher@example.com>\r\n"+
			"Cc: Zoe <zoe@example.com>\r\n"+
			"Reply-To: Bob <bob@example.com>\r\n"+
			"Subject: Threaded\r\n"+
			"Message-ID: <reply-102@example.com>\r\n"+
			"References: <root-1@example.com> <reply-101@example.com>\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n\r\nBody\r\n",
	))
	source, err = client.MessageThreadSource(context.Background(), ref)
	if err != nil {
		t.Fatalf("MessageThreadSource() error = %v", err)
	}
	if source.Subject != "Threaded" ||
		!strings.Contains(source.ReplyTo, "bob@example.com") ||
		source.MessageID != "<reply-102@example.com>" ||
		source.References != "<root-1@example.com> <reply-101@example.com>" {
		t.Fatalf("thread source = %+v", source)
	}
	if len(source.CC) != 1 || source.CC[0].Address != "zoe@example.com" {
		t.Fatalf("cc = %+v", source.CC)
	}

	writeFixtureEMLX(t, store, 102, "imap://"+testAccountID+"/INBOX", []byte(
		"From: Alice <alice@example.com>\r\nSubject: No ID\r\n\r\nBody\r\n",
	))
	if _, err := client.MessageThreadSource(context.Background(), ref); err == nil ||
		errorCodeForTest(err) != "invalid_message_source" {
		t.Fatalf("missing Message-ID error = %v", err)
	}
}
