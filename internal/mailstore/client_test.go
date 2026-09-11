package mailstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	boxes                  []transport.MailboxInfo
	uid                    uint32
	uidvalidity            uint32
	searchMatches          int
	searchMatchesByMailbox map[string][]int
	raw                    []byte
	lastCommand            string
	lastUsername           string
	lastMailbox            string
	flags                  []string
	status                 transport.MailboxStatus
	err                    error
	// mutationErrs scripts per-call mutation results: each mutation op
	// consumes the head (nil head = success). mutationCalls counts every
	// mutation invocation, so retry tests can assert exactly-once retry.
	mutationErrs  []error
	mutationCalls int
	searchCalls   int
	// statusErr scripts per-mailbox CheckStatus failures for sync-check
	// tests; absent mailboxes report s.status.
	statusErr         map[string]error
	statusByMailbox   map[string]transport.MailboxStatus
	listErrByUsername map[string]error
	// fetchErr scripts an IMAP fetch failure for raw-source tests.
	fetchErr error
	// lastFetchMax records the bound the last FetchMessage carried.
	lastFetchMax int64
	listCalls    int
}

func (s *stubImapOperator) nextMutationErr() error {
	s.mutationCalls++
	if len(s.mutationErrs) > 0 {
		err := s.mutationErrs[0]
		s.mutationErrs = s.mutationErrs[1:]
		return err
	}
	return s.err
}

// stubValidity mirrors the SearchUID default: tests that never set
// uidvalidity resolve and observe 12345.
func (s *stubImapOperator) stubValidity() uint32 {
	if s.uidvalidity != 0 {
		return s.uidvalidity
	}
	return 12345
}

func (s *stubImapOperator) AppendToSent(ctx context.Context, cfg transport.ImapConfig, msg []byte, messageID string) (transport.AppendEvidence, error) {
	return transport.AppendEvidence{Mailbox: "Sent", Appended: true}, nil
}

func (s *stubImapOperator) ListMailboxes(ctx context.Context, cfg transport.ImapConfig) ([]transport.MailboxInfo, error) {
	s.listCalls++
	if err, ok := s.listErrByUsername[cfg.Username]; ok {
		return s.boxes, err
	}
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

func (s *stubImapOperator) SearchUID(ctx context.Context, cfg transport.ImapConfig, mailbox string, messageID string) (uint32, uint32, int, error) {
	s.searchCalls++
	if s.err != nil {
		return 0, 0, 0, s.err
	}
	uid := s.uid
	if uid == 0 {
		uid = 101
	}
	val := s.uidvalidity
	if val == 0 {
		val = 12345
	}
	matches := s.searchMatches
	if sequence, ok := s.searchMatchesByMailbox[mailbox]; ok && len(sequence) > 0 {
		matches = sequence[0]
		if len(sequence) > 1 {
			s.searchMatchesByMailbox[mailbox] = sequence[1:]
		}
	}
	if matches == 0 {
		if _, ok := s.searchMatchesByMailbox[mailbox]; ok {
			return 0, val, 0, &transport.TransportError{
				Code:    transport.CodeIMAPMessageNotFound,
				Message: "message not found in mailbox " + mailbox,
			}
		}
		matches = 1
	}
	return uid, val, matches, nil
}

func (s *stubImapOperator) SetFlags(ctx context.Context, cfg transport.ImapConfig, mailbox string, uid uint32, expectedUIDValidity uint32, addFlags, removeFlags []string) (transport.MutationEvidence, error) {
	if err := s.nextMutationErr(); err != nil {
		return transport.MutationEvidence{}, err
	}
	s.lastCommand = "STORE"
	s.lastUsername = cfg.Username
	s.lastMailbox = mailbox
	for _, removal := range removeFlags {
		for index := 0; index < len(s.flags); {
			if strings.EqualFold(s.flags[index], removal) {
				s.flags = append(s.flags[:index], s.flags[index+1:]...)
			} else {
				index++
			}
		}
	}
	for _, addition := range addFlags {
		present := false
		for _, flag := range s.flags {
			present = present || strings.EqualFold(flag, addition)
		}
		if !present {
			s.flags = append(s.flags, addition)
		}
	}
	return transport.MutationEvidence{
		Command:             "STORE",
		Outcome:             transport.MutationOutcomeCompleted,
		ActualFlags:         append([]string(nil), s.flags...),
		FlagsState:          transport.FlagObservationObserved,
		FlagsSource:         "STORE",
		ServerResponse:      "OK STORE completed",
		Mailbox:             mailbox,
		UID:                 uid,
		UIDValidity:         s.stubValidity(),
		ExpectedUIDValidity: expectedUIDValidity,
	}, nil
}

func (s *stubImapOperator) CopyMessage(ctx context.Context, cfg transport.ImapConfig, srcMailbox string, uid uint32, expectedUIDValidity uint32, dstMailbox string) (transport.MutationEvidence, error) {
	evidence := transport.MutationEvidence{
		OperationID:         transport.MutationOperationID("COPY", cfg.Username, srcMailbox, uid, expectedUIDValidity, dstMailbox),
		Outcome:             transport.MutationOutcomeAttempted,
		SourceAccount:       cfg.Username,
		Command:             "COPY",
		ServerResponse:      "OK COPY completed",
		Mailbox:             srcMailbox,
		TargetMailbox:       dstMailbox,
		UID:                 uid,
		UIDValidity:         s.stubValidity(),
		ExpectedUIDValidity: expectedUIDValidity,
	}
	if err := s.nextMutationErr(); err != nil {
		return evidence, err
	}
	s.lastCommand = "COPY"
	evidence.Outcome = transport.MutationOutcomeCompleted
	return evidence, nil
}

func (s *stubImapOperator) MoveMessage(ctx context.Context, cfg transport.ImapConfig, srcMailbox string, uid uint32, expectedUIDValidity uint32, dstMailbox string) (transport.MutationEvidence, error) {
	if err := s.nextMutationErr(); err != nil {
		return transport.MutationEvidence{}, err
	}
	s.lastCommand = "MOVE"
	s.lastUsername = cfg.Username
	s.lastMailbox = srcMailbox
	return transport.MutationEvidence{
		Command:             "MOVE",
		ServerResponse:      "OK MOVE completed",
		Mailbox:             srcMailbox,
		TargetMailbox:       dstMailbox,
		UID:                 uid,
		UIDValidity:         s.stubValidity(),
		ExpectedUIDValidity: expectedUIDValidity,
	}, nil
}

func (s *stubImapOperator) DeleteMessage(ctx context.Context, cfg transport.ImapConfig, srcMailbox string, uid uint32, expectedUIDValidity uint32) (transport.MutationEvidence, error) {
	if err := s.nextMutationErr(); err != nil {
		return transport.MutationEvidence{}, err
	}
	s.lastCommand = "DELETE"
	s.lastUsername = cfg.Username
	s.lastMailbox = srcMailbox
	return transport.MutationEvidence{
		Command:             "DELETE",
		ServerResponse:      "OK DELETE completed",
		Mailbox:             srcMailbox,
		TargetMailbox:       "Trash",
		UID:                 uid,
		UIDValidity:         s.stubValidity(),
		ExpectedUIDValidity: expectedUIDValidity,
	}, nil
}

func (s *stubImapOperator) FetchMessage(ctx context.Context, cfg transport.ImapConfig, mailbox string, uid uint32, expectedUIDValidity uint32, maxBytes int64) ([]byte, error) {
	s.lastFetchMax = maxBytes
	if s.err != nil {
		return nil, s.err
	}
	if s.fetchErr != nil {
		return nil, s.fetchErr
	}
	return s.raw, nil
}

func (s *stubImapOperator) CheckStatus(ctx context.Context, cfg transport.ImapConfig, mailbox string) (transport.MailboxStatus, error) {
	if s.err != nil {
		return transport.MailboxStatus{}, s.err
	}
	if err, ok := s.statusErr[mailbox]; ok {
		return transport.MailboxStatus{}, err
	}
	if status, ok := s.statusByMailbox[mailbox]; ok {
		return status, nil
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

func TestClientListAccountCatalogReportsMissingAccountCache(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	spy := &fallbackSpy{accounts: []mail.Account{{EmailAddresses: []string{"fallback@example.com"}}}}
	client := &Client{store: store, fallback: spy}
	catalog, err := client.ListAccountCatalog(context.Background())
	if err != nil {
		t.Fatalf("ListAccountCatalog() error = %v", err)
	}
	if catalog.Complete || len(catalog.Accounts) != 1 || catalog.Accounts[0].State != "degraded" ||
		catalog.Accounts[0].DegradedReason != "mailbox_cache_unreadable" {
		t.Fatalf("ListAccountCatalog() = %+v, want one degraded account", catalog)
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

func TestMutationUsesHealthyAccountWhenSiblingCatalogDegraded(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "identity@gmail.com")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 1,
	})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("ListMessages() = %+v, error = %v", page, err)
	}
	otherAccount, err := parseAccountRoot("imap://BBBBBBBB-CCCC-4DDD-8EEE-FFFFFFFFFFFF/")
	if err != nil {
		t.Fatalf("parseAccountRoot() error = %v", err)
	}
	store.activeAccounts = append(store.activeAccounts, otherAccount)
	accountRef, err := mailref.EncodeAccount(testAccountID)
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	fallback := &fallbackSpy{accounts: []mail.Account{{
		Ref: accountRef, EmailAddresses: []string{"identity@gmail.com"},
	}}}
	fakeImap := &stubImapOperator{boxes: []transport.MailboxInfo{{Name: "INBOX"}}, uid: 101}
	client := &Client{
		store: store, fallback: fallback,
		send: mail.SendTransport{
			Imap: fakeImap, Credentials: strictCredentials{"identity@gmail.com": "secret"},
		},
	}
	read := true
	_, err = client.MarkMessage(context.Background(), mail.MarkMessageRequest{
		Ref: page.Messages[0].Ref, Read: &read, AllowDraftMutation: true,
	})
	if err != nil {
		t.Fatalf("MarkMessage() error = %v, want healthy account mutation to proceed", err)
	}
	if fallback.accountCalls != 0 {
		t.Fatalf("fallback ListAccounts() calls = %d, want 0", fallback.accountCalls)
	}
	if fakeImap.mutationCalls != 1 {
		t.Fatalf("IMAP mutation calls = %d, want 1", fakeImap.mutationCalls)
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
	messageRef = messageRefWithExpectedID(t, messageRef, "<101@example.com>")
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

func TestClientGetMessageReportsCorruptAttachment(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 3,
	})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	messageRef := messageRefWithExpectedID(t, messageRefWithSubject(t, page.Messages, "Quarterly Report"), "<101@example.com>")
	writeFixtureEMLX(t, store, 101, "imap://"+testAccountID+"/%5BGmail%5D/All", []byte(
		"From: Alice <alice@example.com>\r\nTo: Christopher <christopher@example.com>\r\n"+
			"Subject: Quarterly Report\r\nMessage-ID: <101@example.com>\r\n"+
			"Content-Type: multipart/mixed; boundary=b\r\n\r\n"+
			"--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n"+
			"--b\r\nContent-Type: application/pdf\r\n"+
			"Content-Disposition: attachment; filename=broken.pdf\r\n"+
			"Content-Transfer-Encoding: base64\r\n\r\nnot-base64!\r\n--b--\r\n",
	))
	client := &Client{store: store}
	message, err := client.GetMessage(context.Background(), messageRef)
	if err != nil || message.ContentComplete || message.Content != "body" ||
		len(message.MissingParts) != 1 || message.MissingParts[0] != "2" ||
		len(message.Attachments) != 1 || message.Attachments[0].Downloaded ||
		message.Attachments[0].SizeKnown {
		t.Fatalf("GetMessage() = %+v, error = %v; want explicit incomplete attachment", message, err)
	}
}

func TestClientPartialHydrationFailureRetainsContentAndCauses(t *testing.T) {
	tests := []struct {
		name      string
		fetchErr  error
		wantCode  string
		wantState mail.HydrationState
	}{
		{
			name:      "authentication",
			fetchErr:  &transport.TransportError{Code: transport.CodeIMAPAuthFailed, Message: "AUTHENTICATIONFAILED secret-token"},
			wantCode:  transport.CodeIMAPAuthFailed,
			wantState: mail.HydrationStateFailed,
		},
		{
			name:      "size limit",
			fetchErr:  &transport.TransportError{Code: transport.CodeIMAPRawSourceTooLarge, Message: "announced 128 MiB"},
			wantCode:  transport.CodeIMAPRawSourceTooLarge,
			wantState: mail.HydrationStateFailed,
		},
		{
			name:      "canceled",
			fetchErr:  context.Canceled,
			wantCode:  operationCanceledCode,
			wantState: mail.HydrationStateCanceled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, inboxRef := newSearchFixture(t)
			closeTestResource(t, store, "test store")
			account := "partial-hydration-" + strings.ReplaceAll(test.name, " ", "-") + "@gmail.com"
			installImapIdentityFixture(t, store, account)
			page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inboxRef, Limit: 3})
			if err != nil {
				t.Fatalf("ListMessages() error = %v", err)
			}
			messageRef := messageRefWithExpectedID(t, messageRefWithSubject(t, page.Messages, "Quarterly Report"), "<101@example.com>")
			writeFixtureEMLX(t, store, 101, "imap://"+testAccountID+"/%5BGmail%5D/All", []byte(
				"From: Alice <alice@example.com>\r\nSubject: Quarterly Report\r\n"+
					"Message-ID: <101@example.com>\r\nContent-Type: text/plain; charset=x-mailcli-unknown\r\n\r\n"+
					"partial body\r\n",
			))
			location, err := parseMailboxURL("imap://" + testAccountID + "/%5BGmail%5D/All")
			if err != nil {
				t.Fatalf("parseMailboxURL() error = %v", err)
			}
			base, err := store.messageBasePath(location, 101)
			if err != nil {
				t.Fatalf("messageBasePath() error = %v", err)
			}
			if err := os.Rename(base+".emlx", base+".partial.emlx"); err != nil {
				t.Fatalf("rename partial source: %v", err)
			}
			fakeImap := &stubImapOperator{
				boxes: []transport.MailboxInfo{{Name: "INBOX"}}, fetchErr: test.fetchErr,
			}
			client := &Client{store: store, send: mail.SendTransport{
				Imap: fakeImap, Credentials: stubCredentials{account: "secret"},
			}}
			message, err := client.GetMessage(context.Background(), messageRef)
			if err == nil || message.Content != "partial body" || message.ContentComplete {
				t.Fatalf("GetMessage() = %+v, error = %v; want retained incomplete content and error", message, err)
			}
			if transport.ErrorCode(err) != test.wantCode {
				t.Fatalf("GetMessage() error code = %q, want %q: %v", transport.ErrorCode(err), test.wantCode, err)
			}
			var combined *hydrationError
			if !errors.As(err, &combined) {
				t.Fatalf("GetMessage() error type = %T, want hydrationError", err)
			}
			if message.Hydration == nil || message.Hydration.State != test.wantState ||
				message.Hydration.AttemptedSource != "imap" || message.Hydration.Local == nil ||
				message.Hydration.Local.Code != "raw_source_partial" || message.Hydration.Remote == nil ||
				message.Hydration.Remote.Code != test.wantCode || message.Hydration.Remediation == "" {
				t.Fatalf("hydration diagnostic = %+v", message.Hydration)
			}
			if strings.Contains(message.Hydration.Remote.Message, "secret-token") {
				t.Fatalf("hydration diagnostic leaked protocol authentication text: %+v", message.Hydration.Remote)
			}
			if test.name == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("GetMessage() error = %v, want context.Canceled cause", err)
			}
		})
	}
}

func TestClientCompleteLocalReadSkipsHydration(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inboxRef, Limit: 1})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("ListMessages() = %+v, error = %v", page, err)
	}
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{{Name: "INBOX"}},
		err:   &transport.TransportError{Code: transport.CodeIMAPAuthFailed, Message: "must not be called"},
	}
	client := &Client{store: store, send: mail.SendTransport{
		Imap: fakeImap, Credentials: stubCredentials{"test@gmail.com": "secret"},
	}}
	message, err := client.GetMessage(context.Background(), page.Messages[0].Ref)
	if err != nil || !message.ContentComplete || message.Hydration != nil {
		t.Fatalf("GetMessage() = %+v, error = %v; want complete local read", message, err)
	}
	if fakeImap.listCalls != 0 || fakeImap.searchCalls != 0 || fakeImap.lastFetchMax != 0 {
		t.Fatalf("complete local read contacted IMAP: list=%d search=%d fetch=%d", fakeImap.listCalls, fakeImap.searchCalls, fakeImap.lastFetchMax)
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
	messageRef = messageRefWithExpectedID(t, messageRef, "<101@example.com>")
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
	if result.ServerTruth.DuplicateMatches != 0 {
		t.Fatalf("single-match server truth duplicate_matches = %d, want omitted", result.ServerTruth.DuplicateMatches)
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

func uidValidityChangedErrorForTest() error {
	return &transport.TransportError{
		Code:    "mailbox_uidvalidity_changed",
		Message: "mailbox was rebuilt between resolution and mutation (UIDVALIDITY 12345 -> 99999); message moved or mailbox rebuilt; rerun the command",
	}
}

func TestMarkMessageReportsDuplicateMessageIDMatches(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "duplicate@gmail.com")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 1,
	})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("ListMessages() = %+v, error = %v", page, err)
	}
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{{Name: "INBOX"}}, uid: 101, searchMatches: 2,
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap: fakeImap, Credentials: strictCredentials{"duplicate@gmail.com": "secret"},
		},
	}
	read := true
	result, err := client.MarkMessage(context.Background(), mail.MarkMessageRequest{
		Ref: page.Messages[0].Ref, Read: &read, AllowDraftMutation: true,
	})
	if transport.ErrorCode(err) != transport.CodeIMAPAmbiguousMessageID {
		t.Fatalf("MarkMessage() error = %v, want %s", err, transport.CodeIMAPAmbiguousMessageID)
	}
	if result != (mail.MessageSummary{}) {
		t.Fatalf("MarkMessage() result = %+v, want empty result", result)
	}
	if fakeImap.lastCommand != "" {
		t.Fatalf("mutation command = %q, want no command for ambiguous target", fakeImap.lastCommand)
	}
}

func TestMarkMessageRejectsMissingMessageIDBeforeIMAP(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	updateFixtureMessage(t, store, `UPDATE messages SET message_id = NULL WHERE ROWID = 102`)
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 3,
	})
	if err != nil || len(page.Messages) != 3 {
		t.Fatalf("ListMessages() = %+v, error = %v", page, err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Status Update")
	_, source, err := store.openMessageSource(context.Background(), messageRef)
	if err != nil {
		t.Fatalf("openMessageSource() error = %v", err)
	}
	sourcePath := source.path
	if err := source.Close(); err != nil {
		t.Fatalf("closeMessageSource() error = %v", err)
	}
	raw := []byte("From: Alice <alice@example.com>\r\nSubject: no identity\r\n\r\nbody\r\n")
	framed := append([]byte(fmt.Sprintf("%-10d\n", len(raw))), raw...)
	framed = append(framed, validPlistTrailer()...)
	if err := os.WriteFile(sourcePath, framed, 0o600); err != nil {
		t.Fatalf("replace message source: %v", err)
	}

	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{{Name: "INBOX"}},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"identity@gmail.com": "secret"},
		},
	}
	read := true
	_, err = client.MarkMessage(context.Background(), mail.MarkMessageRequest{
		Ref: messageRef, Read: &read, AllowDraftMutation: true,
	})
	if transport.ErrorCode(err) != transport.CodeIMAPMessageUIDUnknown ||
		!strings.Contains(err.Error(), "message has no Message-ID") {
		t.Fatalf("MarkMessage() error = %v, want unresolved Message-ID", err)
	}
	if fakeImap.searchCalls != 0 {
		t.Fatalf("IMAP SearchUID calls = %d, want 0", fakeImap.searchCalls)
	}
}

// On a UIDVALIDITY mismatch the mutation wrapper re-resolves once and
// retries exactly once: first STORE fails, second succeeds.
func TestMarkMessageRetriesOnceAfterUIDValidityChange(t *testing.T) {
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
		boxes:        []transport.MailboxInfo{{Name: "INBOX"}},
		uid:          101,
		mutationErrs: []error{uidValidityChangedErrorForTest()},
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
	if result.ServerTruth.ExpectedUIDValidity != 12345 || result.ServerTruth.UIDValidity != 12345 {
		t.Fatalf("ServerTruth pair = (%d, %d), want (12345, 12345)",
			result.ServerTruth.ExpectedUIDValidity, result.ServerTruth.UIDValidity)
	}
	if fakeImap.mutationCalls != 2 {
		t.Fatalf("mutation calls = %d, want 2 (first attempt + exactly one retry)", fakeImap.mutationCalls)
	}
}

// A repeated mismatch fails closed after exactly one retry: no loop.
func TestMarkMessageFailsClosedOnRepeatedUIDValidityChange(t *testing.T) {
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
		mutationErrs: []error{
			uidValidityChangedErrorForTest(),
			uidValidityChangedErrorForTest(),
		},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"identity@gmail.com": "secret"},
		},
	}
	read := true
	_, err = client.MarkMessage(context.Background(), mail.MarkMessageRequest{
		Ref: page.Messages[0].Ref, Read: &read, AllowDraftMutation: true,
	})
	if errorCodeForTest(err) != "mailbox_uidvalidity_changed" {
		t.Fatalf("MarkMessage() error = %v, want mailbox_uidvalidity_changed", err)
	}
	if fakeImap.mutationCalls != 2 {
		t.Fatalf("mutation calls = %d, want 2 (first attempt + exactly one retry, no loop)", fakeImap.mutationCalls)
	}
}

func TestDeleteMessageRetriesOnceAfterUIDValidityChange(t *testing.T) {
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
		boxes:        []transport.MailboxInfo{{Name: "INBOX"}, {Name: "Trash", Flags: []string{"\\Trash"}}},
		uid:          101,
		mutationErrs: []error{uidValidityChangedErrorForTest()},
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
	if err != nil || !result.Deleted || result.ServerTruth == nil || result.ServerTruth.Command != "DELETE" {
		t.Fatalf("DeleteMessage() = %+v, error = %v", result, err)
	}
	if fakeImap.mutationCalls != 2 {
		t.Fatalf("mutation calls = %d, want 2 (first attempt + exactly one retry)", fakeImap.mutationCalls)
	}
}

func TestTransferMessageRetriesOnceAfterUIDValidityChange(t *testing.T) {
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
		uid:                    102,
		mutationErrs:           []error{uidValidityChangedErrorForTest()},
		searchMatchesByMailbox: map[string][]int{"Sent": []int{0, 0, 1}},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"identity@gmail.com": "secret"},
		},
	}
	result, err := client.TransferMessage(context.Background(), mail.TransferMessageRequest{
		Ref: messageRef, DestinationMailbox: destinationRef, Copy: true,
	})
	if err != nil || result.ServerTruth == nil || result.ServerTruth.Command != "COPY" {
		t.Fatalf("TransferMessage() = %+v, error = %v", result, err)
	}
	if fakeImap.mutationCalls != 2 {
		t.Fatalf("mutation calls = %d, want 2 (first attempt + exactly one retry)", fakeImap.mutationCalls)
	}
}

func TestTransferMessageRejectsAmbiguousDestinationMailbox(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "ambiguous-destination@gmail.com")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 1,
	})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("ListMessages() = %+v, error = %v", page, err)
	}
	destinationRef, err := mailref.EncodeMailbox(testAccountID, []string{"Sent"})
	if err != nil {
		t.Fatal(err)
	}
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{
			{Name: "INBOX"},
			{Name: "Sent A", Flags: []string{"\\Sent"}},
			{Name: "Sent B", Flags: []string{"\\Sent"}},
		},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"ambiguous-destination@gmail.com": "secret"},
		},
	}
	_, err = client.TransferMessage(context.Background(), mail.TransferMessageRequest{
		Ref: page.Messages[0].Ref, DestinationMailbox: destinationRef, Copy: true,
	})
	if transport.ErrorCode(err) != transport.CodeIMAPAmbiguousMailbox {
		t.Fatalf("TransferMessage() error = %v, want %s", err, transport.CodeIMAPAmbiguousMailbox)
	}
	if fakeImap.mutationCalls != 0 {
		t.Fatalf("mutation calls = %d, want 0", fakeImap.mutationCalls)
	}
	if !strings.Contains(err.Error(), "Sent A") || !strings.Contains(err.Error(), "Sent B") {
		t.Fatalf("TransferMessage() error = %v, want both candidates", err)
	}
}

func TestTransferMoveDoesNotReplayAfterUnknownOutcome(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "identity@gmail.com")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 1,
	})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("ListMessages() = %+v, error = %v", page, err)
	}
	destinationRef, err := mailref.EncodeMailbox(testAccountID, []string{"Archive"})
	if err != nil {
		t.Fatal(err)
	}
	fakeImap := &stubImapOperator{
		boxes:                  []transport.MailboxInfo{{Name: "INBOX"}, {Name: "Archive"}},
		mutationErrs:           []error{errors.New("move response lost")},
		searchMatchesByMailbox: map[string][]int{"Archive": []int{0, 1}},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"identity@gmail.com": "secret"},
		},
	}
	request := mail.TransferMessageRequest{
		Ref: page.Messages[0].Ref, DestinationMailbox: destinationRef,
	}

	_, err = client.TransferMessage(context.Background(), request)
	if transport.ErrorCode(err) != transport.CodeIMAPMoveOutcomeUnknown {
		t.Fatalf("first TransferMessage() error = %v, want %s", err, transport.CodeIMAPMoveOutcomeUnknown)
	}
	if fakeImap.mutationCalls != 1 {
		t.Fatalf("first mutation calls = %d, want 1", fakeImap.mutationCalls)
	}

	_, err = client.TransferMessage(context.Background(), request)
	if transport.ErrorCode(err) != transport.CodeIMAPMoveOutcomeUnknown {
		t.Fatalf("replayed TransferMessage() error = %v, want %s", err, transport.CodeIMAPMoveOutcomeUnknown)
	}
	if fakeImap.mutationCalls != 1 {
		t.Fatalf("replayed mutation calls = %d, want 1", fakeImap.mutationCalls)
	}
}

func TestTransferCopyDoesNotReplayAfterUnknownOutcome(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "identity@gmail.com")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 1,
	})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("ListMessages() = %+v, error = %v", page, err)
	}
	destinationRef, err := mailref.EncodeMailbox(testAccountID, []string{"Archive"})
	if err != nil {
		t.Fatal(err)
	}
	fakeImap := &stubImapOperator{
		boxes:                  []transport.MailboxInfo{{Name: "INBOX"}, {Name: "Archive"}},
		mutationErrs:           []error{errors.New("copy response lost")},
		searchMatchesByMailbox: map[string][]int{"Archive": []int{0, 1, 1}},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"identity@gmail.com": "secret"},
		},
	}
	request := mail.TransferMessageRequest{
		Ref: page.Messages[0].Ref, DestinationMailbox: destinationRef, Copy: true,
	}

	_, err = client.TransferMessage(context.Background(), request)
	if transport.ErrorCode(err) != transport.CodeIMAPCopyOutcomeUnknown {
		t.Fatalf("first TransferMessage() error = %v, want %s", err, transport.CodeIMAPCopyOutcomeUnknown)
	}
	var outcomeErr *transport.MutationOutcomeError
	if !errors.As(err, &outcomeErr) || outcomeErr.Evidence.OperationID == "" ||
		outcomeErr.Evidence.DestinationUID == 0 || outcomeErr.Evidence.DestinationUIDValidity == 0 {
		t.Fatalf("first COPY error evidence = %+v", outcomeErr)
	}
	if fakeImap.mutationCalls != 1 {
		t.Fatalf("first mutation calls = %d, want 1", fakeImap.mutationCalls)
	}

	_, err = client.TransferMessage(context.Background(), request)
	if transport.ErrorCode(err) != transport.CodeIMAPCopyOutcomeUnknown {
		t.Fatalf("replayed TransferMessage() error = %v, want %s", err, transport.CodeIMAPCopyOutcomeUnknown)
	}
	if fakeImap.mutationCalls != 1 {
		t.Fatalf("replayed mutation calls = %d, want 1", fakeImap.mutationCalls)
	}
}

func TestTransferCopyRejectsAmbiguousDestinationBeforeDispatch(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "identity@gmail.com")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 1,
	})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("ListMessages() = %+v, error = %v", page, err)
	}
	destinationRef, err := mailref.EncodeMailbox(testAccountID, []string{"Archive"})
	if err != nil {
		t.Fatal(err)
	}
	fakeImap := &stubImapOperator{
		boxes:                  []transport.MailboxInfo{{Name: "INBOX"}, {Name: "Archive"}},
		searchMatchesByMailbox: map[string][]int{"Archive": []int{2}},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"identity@gmail.com": "secret"},
		},
	}
	_, err = client.TransferMessage(context.Background(), mail.TransferMessageRequest{
		Ref: page.Messages[0].Ref, DestinationMailbox: destinationRef, Copy: true,
	})
	if transport.ErrorCode(err) != transport.CodeIMAPCopyOutcomeUnknown {
		t.Fatalf("TransferMessage() error = %v, want %s", err, transport.CodeIMAPCopyOutcomeUnknown)
	}
	if fakeImap.mutationCalls != 0 {
		t.Fatalf("mutation calls = %d, want 0", fakeImap.mutationCalls)
	}
}

func TestVerifyMoveDestinationFailsClosedAfterMutationError(t *testing.T) {
	fakeImap := &stubImapOperator{
		searchMatchesByMailbox: map[string][]int{"Archive": []int{0}},
	}
	err := verifyMoveDestination(
		context.Background(),
		fakeImap,
		transport.ImapConfig{},
		"Archive",
		"<message@example.com>",
		errors.New("move response lost"),
	)
	if transport.ErrorCode(err) != transport.CodeIMAPMoveOutcomeUnknown {
		t.Fatalf("verifyMoveDestination() error = %v, want %s", err, transport.CodeIMAPMoveOutcomeUnknown)
	}
}

// A failing mailbox degrades to a failure entry: the sibling mailbox is
// still checked, the typed server code is preserved, complete is false.
func TestSyncCheckReportsFailingMailbox(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "synccheck1@gmail.com")
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{
			{Name: "INBOX"}, {Name: "All"}, {Name: "Sent", Flags: []string{"\\Sent"}},
		},
		statusErr: map[string]error{
			"INBOX": &transport.TransportError{Code: transport.CodeIMAPTimeout, Message: "IMAP STATUS deadline"},
		},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"synccheck1@gmail.com": "secret"},
		},
	}
	result, err := client.SyncCheck(context.Background(), "")
	if err != nil {
		t.Fatalf("SyncCheck() error = %v", err)
	}
	if len(result.Failures) != 1 {
		t.Fatalf("Failures = %+v, want exactly 1", result.Failures)
	}
	failure := result.Failures[0]
	if failure.Mailbox != "INBOX" || failure.Code != transport.CodeIMAPTimeout || failure.Account != "synccheck1@gmail.com" {
		t.Fatalf("Failure = %+v, want INBOX/imap_timeout entry", failure)
	}
	if len(result.Mailboxes) == 0 {
		t.Fatal("sibling mailboxes were not checked despite one failure")
	}
	if result.Complete {
		t.Fatal("Complete = true despite a failing mailbox")
	}
}

// A mailbox returned only by IMAP still gets a server count and exact wire
// identity, but its missing local side keeps coverage incomplete.
func TestSyncCheckReportsServerOnlyMailbox(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "server-only-sync@gmail.com")
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{
			{Name: "INBOX"}, {Name: "All"}, {Name: "Sent", Flags: []string{"\\Sent"}},
			{
				Name: "Projects.2026", WireName: "Projects.2026", DisplayName: "Projects.2026",
				DisplayPath: []string{"Projects", "2026"}, Delimiter: ".",
			},
		},
		statusByMailbox: map[string]transport.MailboxStatus{
			"Projects.2026": {Messages: 7, Unseen: 2},
		},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"server-only-sync@gmail.com": "secret"},
		},
	}
	result, err := client.SyncCheck(context.Background(), "")
	if err != nil {
		t.Fatalf("SyncCheck() error = %v", err)
	}
	var found *mail.MailboxDelta
	for index := range result.Mailboxes {
		if result.Mailboxes[index].State == mail.MailboxDeltaStateServerOnly {
			found = &result.Mailboxes[index]
			break
		}
	}
	if found == nil {
		t.Fatalf("Mailboxes = %+v, want server-only entry", result.Mailboxes)
	}
	if found.Name != "2026" || strings.Join(found.Path, "/") != "Projects/2026" ||
		found.ServerName != "Projects.2026" || found.LocalMessagesAvailable ||
		!found.ServerMessagesAvailable || found.ServerMessages != 7 || found.Unseen != 2 ||
		found.MailboxRef == "" {
		t.Fatalf("server-only delta = %+v, want missing-local state with exact server identity", *found)
	}
	if result.Complete {
		t.Fatal("Complete = true despite a server-only mailbox")
	}
	missingLocal := false
	for _, failure := range result.Failures {
		if failure.Mailbox == "Projects.2026" && failure.Code == syncCheckMissingLocalMailboxCode {
			missingLocal = true
		}
	}
	if !missingLocal {
		t.Fatalf("Failures = %+v, want missing-local evidence", result.Failures)
	}
}

// A local mailbox absent from a complete server list remains visible as a
// local-only identity instead of being dropped with its local count.
func TestSyncCheckReportsLocalOnlyMailbox(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "local-only-sync@gmail.com")
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{
			{Name: "INBOX"}, {Name: "Sent", Flags: []string{"\\Sent"}},
		},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"local-only-sync@gmail.com": "secret"},
		},
	}
	result, err := client.SyncCheck(context.Background(), "")
	if err != nil {
		t.Fatalf("SyncCheck() error = %v", err)
	}
	found := false
	for _, delta := range result.Mailboxes {
		if delta.State == mail.MailboxDeltaStateLocalOnly && delta.Name == "All" {
			found = true
			if delta.LocalMessages != 1 || !delta.LocalMessagesAvailable || delta.ServerMessagesAvailable {
				t.Fatalf("local-only delta = %+v, want local evidence only", delta)
			}
		}
	}
	if !found {
		t.Fatalf("Mailboxes = %+v, want local-only All entry", result.Mailboxes)
	}
	if result.Complete {
		t.Fatal("Complete = true despite a local-only mailbox")
	}
}

// A cached local identity without an Envelope Index count is retained, but a
// server STATUS count cannot be presented as a comparable delta.
func TestSyncCheckReportsUnavailableLocalCount(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "unavailable-local-sync@gmail.com")
	cache := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>mboxes</key><dict><key>[Gmail]</key><dict>
<key>MailboxPathComponent</key><string>[Gmail]</string>
<key>IMAPMailboxChildren</key><dict><key>Archive</key><dict>
<key>MailboxPathComponent</key><string>Archive</string>
<key>IMAPMailboxChildren</key><dict/></dict><key>Sent</key><dict>
<key>MailboxPathComponent</key><string>Sent</string>
<key>IMAPMailboxAttributes</key><integer>32768</integer>
<key>IMAPMailboxChildren</key><dict/></dict></dict></dict></dict></dict></plist>`)
	accountRoot := filepath.Join(store.versionRoot, testAccountID)
	if err := os.WriteFile(filepath.Join(accountRoot, ".mboxCache.plist"), cache, 0o600); err != nil {
		t.Fatalf("write unavailable-count mailbox cache: %v", err)
	}
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{{Name: "INBOX"}, {Name: "All"}, {Name: "Sent"}, {Name: "Archive"}},
		statusByMailbox: map[string]transport.MailboxStatus{
			"Archive": {Messages: 9, Unseen: 3},
		},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"unavailable-local-sync@gmail.com": "secret"},
		},
	}
	result, err := client.SyncCheck(context.Background(), "")
	if err != nil {
		t.Fatalf("SyncCheck() error = %v", err)
	}
	var found bool
	for _, delta := range result.Mailboxes {
		if delta.Name != "Archive" {
			continue
		}
		found = true
		if delta.State != mail.MailboxDeltaStateUnresolved || delta.LocalMessagesAvailable ||
			!delta.ServerMessagesAvailable || delta.ServerMessages != 9 || delta.Unseen != 3 || delta.Delta != 0 {
			t.Fatalf("Archive delta = %+v, want unresolved server evidence without a delta", delta)
		}
	}
	if !found || result.Complete {
		t.Fatalf("SyncCheck() = %+v, want unresolved Archive and incomplete coverage", result)
	}
	for _, failure := range result.Failures {
		if failure.Mailbox == "Archive" && failure.Code == syncCheckLocalMessagesUnavailableCode {
			return
		}
	}
	t.Fatalf("Failures = %+v, want unavailable-local-count evidence", result.Failures)
}

// A server LIST failure makes the account catalog incomplete before any
// mailbox pairing is attempted.
func TestSyncCheckReportsServerCatalogFailure(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "list-failure-sync@gmail.com")
	fakeImap := &stubImapOperator{
		listErrByUsername: map[string]error{
			"list-failure-sync@gmail.com": &transport.TransportError{
				Code: transport.CodeIMAPTimeout, Message: "IMAP LIST deadline",
			},
		},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"list-failure-sync@gmail.com": "secret"},
		},
	}
	result, err := client.SyncCheck(context.Background(), "")
	if err != nil {
		t.Fatalf("SyncCheck() error = %v", err)
	}
	if len(result.Mailboxes) == 0 || len(result.Failures) != 1 ||
		result.Failures[0].Code != transport.CodeIMAPTimeout || result.Complete {
		t.Fatalf("SyncCheck() = %+v, want unresolved local evidence plus one server-list failure", result)
	}
	for _, delta := range result.Mailboxes {
		if delta.State != mail.MailboxDeltaStateUnresolved || !delta.LocalMessagesAvailable ||
			delta.ServerMessagesAvailable {
			t.Fatalf("mailbox delta = %+v, want unresolved local evidence only", delta)
		}
	}
}

// A transport may retain mailbox entries parsed before a LIST failure. Those
// known entries remain useful, while local identities missing from the partial
// catalog stay unresolved and the account remains incomplete.
func TestSyncCheckRetainsPartialServerCatalogOnFailure(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "partial-list-sync@gmail.com")
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{{Name: "INBOX"}},
		listErrByUsername: map[string]error{
			"partial-list-sync@gmail.com": &transport.TransportError{
				Code: transport.CodeIMAPTimeout, Message: "partial IMAP LIST deadline",
			},
		},
		statusByMailbox: map[string]transport.MailboxStatus{
			"INBOX": {Messages: 4, Unseen: 1},
		},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"partial-list-sync@gmail.com": "secret"},
		},
	}
	result, err := client.SyncCheck(context.Background(), "")
	if err != nil {
		t.Fatalf("SyncCheck() error = %v", err)
	}
	var matchedInbox, unresolvedLocal bool
	for _, delta := range result.Mailboxes {
		switch {
		case delta.State == mail.MailboxDeltaStateMatched && delta.Name == "INBOX":
			matchedInbox = delta.ServerMessages == 4 && delta.Unseen == 1 &&
				delta.ServerName == "INBOX"
		case delta.State == mail.MailboxDeltaStateUnresolved:
			unresolvedLocal = delta.LocalMessagesAvailable && !delta.ServerMessagesAvailable
		}
	}
	if !matchedInbox || !unresolvedLocal || result.Complete {
		t.Fatalf("SyncCheck() = %+v, want retained matched evidence, unresolved local evidence and incomplete coverage", result)
	}
	if len(result.Failures) == 0 || result.Failures[0].Code != transport.CodeIMAPTimeout {
		t.Fatalf("Failures = %+v, want partial LIST timeout evidence", result.Failures)
	}
}

// A healthy account remains useful when a later account cannot complete its
// server LIST. The result keeps the successful mailbox evidence and marks the
// global coverage incomplete.
func TestSyncCheckRetainsOtherAccountsWhenOneServerCatalogFails(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "multi-primary-sync@gmail.com")
	secondaryRef := addSyncImapAccount(t, store, "multi-secondary-sync@gmail.com")
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{
			{Name: "INBOX"}, {Name: "All"}, {Name: "Sent", Flags: []string{"\\Sent"}},
		},
		listErrByUsername: map[string]error{
			"multi-secondary-sync@gmail.com": &transport.TransportError{
				Code: transport.CodeIMAPTimeout, Message: "secondary IMAP LIST deadline",
			},
		},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap: fakeImap,
			Credentials: stubCredentials{
				"multi-primary-sync@gmail.com":   "secret",
				"multi-secondary-sync@gmail.com": "secret",
			},
		},
	}
	result, err := client.SyncCheck(context.Background(), "")
	if err != nil {
		t.Fatalf("SyncCheck() error = %v", err)
	}
	foundPrimary := false
	for _, delta := range result.Mailboxes {
		if delta.AccountRef != secondaryRef && delta.State == mail.MailboxDeltaStateMatched {
			foundPrimary = true
			break
		}
	}
	if !foundPrimary {
		t.Fatalf("Mailboxes = %+v, want successful primary-account evidence", result.Mailboxes)
	}
	foundSecondaryFailure := false
	for _, failure := range result.Failures {
		if failure.Account == "multi-secondary-sync@gmail.com" && failure.Code == transport.CodeIMAPTimeout {
			foundSecondaryFailure = true
		}
	}
	if !foundSecondaryFailure || result.Complete {
		t.Fatalf("SyncCheck() = %+v, want retained primary evidence and incomplete secondary coverage", result)
	}
}

func TestSyncCheckReportsAmbiguousSpecialMailboxMapping(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "ambiguous-sync@gmail.com")
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{
			{Name: "INBOX"}, {Name: "All"},
			{Name: "Sent A", Flags: []string{"\\Sent"}},
			{Name: "Sent B", Flags: []string{"\\Sent"}},
		},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"ambiguous-sync@gmail.com": "secret"},
		},
	}
	result, err := client.SyncCheck(context.Background(), "")
	if err != nil {
		t.Fatalf("SyncCheck() error = %v", err)
	}
	found := false
	for _, failure := range result.Failures {
		if failure.Mailbox == "Sent" && failure.Code == transport.CodeIMAPAmbiguousMailbox {
			found = true
		}
	}
	if !found {
		t.Fatalf("SyncCheck() failures = %+v, want ambiguous Sent mapping", result.Failures)
	}
	if result.Complete {
		t.Fatal("Complete = true despite an ambiguous mailbox mapping")
	}
	if len(result.Mailboxes) == 0 {
		t.Fatal("SyncCheck() did not retain unambiguous mailbox evidence")
	}
}

// A dead context maps to sync_check_timeout per unchecked mailbox.
func TestSyncCheckReportsTimeoutPerMailbox(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "synccheck2@gmail.com")
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{
			{Name: "INBOX"}, {Name: "All"}, {Name: "Sent", Flags: []string{"\\Sent"}},
		},
		statusErr: map[string]error{"INBOX": context.DeadlineExceeded},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"synccheck2@gmail.com": "secret"},
		},
	}
	result, err := client.SyncCheck(context.Background(), "")
	if err != nil {
		t.Fatalf("SyncCheck() error = %v", err)
	}
	found := false
	for _, failure := range result.Failures {
		if failure.Mailbox == "INBOX" && failure.Code == "sync_check_timeout" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Failures = %+v, want INBOX/sync_check_timeout entry", result.Failures)
	}
	if result.Complete {
		t.Fatal("Complete = true despite a timed-out mailbox")
	}
}

// Missing credentials surface as an account-level entry, never as silent
// empty mailboxes.
func TestSyncCheckReportsMissingCredentials(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "synccheck3@gmail.com")
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{{Name: "INBOX"}},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: strictCredentials{},
		},
	}
	result, err := client.SyncCheck(context.Background(), "")
	if err != nil {
		t.Fatalf("SyncCheck() error = %v", err)
	}
	if len(result.Failures) != 1 {
		t.Fatalf("Failures = %+v, want exactly 1", result.Failures)
	}
	failure := result.Failures[0]
	if failure.Mailbox != "" || failure.Code != "imap_credentials_missing" {
		t.Fatalf("Failure = %+v, want account-level imap_credentials_missing entry", failure)
	}
	if len(result.Mailboxes) != 0 {
		t.Fatalf("Mailboxes = %+v, want none checked without credentials", result.Mailboxes)
	}
	if result.Complete {
		t.Fatal("Complete = true despite missing credentials")
	}
}

// An unresolvable provider surfaces its typed code at account level.
func TestSyncCheckReportsUnsupportedProvider(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "synccheck4@unknown-provider.tld")
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{{Name: "INBOX"}},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"synccheck4@unknown-provider.tld": "secret"},
		},
	}
	result, err := client.SyncCheck(context.Background(), "")
	if err != nil {
		t.Fatalf("SyncCheck() error = %v", err)
	}
	if len(result.Failures) != 1 {
		t.Fatalf("Failures = %+v, want exactly 1", result.Failures)
	}
	if result.Failures[0].Code != transport.CodeUnsupportedProvider {
		t.Fatalf("Failure = %+v, want transport_unsupported_provider", result.Failures[0])
	}
	if result.Complete {
		t.Fatal("Complete = true despite an unresolvable provider")
	}
}

// A clean check reports complete with no failures.
func TestSyncCheckCompleteWhenAllChecked(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "synccheck5@gmail.com")
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{
			{Name: "INBOX"}, {Name: "All"}, {Name: "Sent", Flags: []string{"\\Sent"}},
		},
		status: transport.MailboxStatus{Messages: 3},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"synccheck5@gmail.com": "secret"},
		},
	}
	result, err := client.SyncCheck(context.Background(), "")
	if err != nil {
		t.Fatalf("SyncCheck() error = %v", err)
	}
	if len(result.Failures) != 0 {
		t.Fatalf("Failures = %+v, want none", result.Failures)
	}
	if !result.Complete {
		t.Fatal("Complete = false despite a clean check")
	}
	if len(result.Mailboxes) == 0 {
		t.Fatal("no mailboxes checked")
	}
}

// failureCode maps deadline/cancelation to sync_check_timeout even when the
// context is still alive, typed errors to their code, and anything else to
// sync_check_failed.
func TestFailureCodeMapping(t *testing.T) {
	ctx := context.Background()
	if got := failureCode(ctx, context.DeadlineExceeded); got != "sync_check_timeout" {
		t.Fatalf("deadline error = %q, want sync_check_timeout", got)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got := failureCode(canceled, nil); got != "sync_check_timeout" {
		t.Fatalf("canceled ctx = %q, want sync_check_timeout", got)
	}
	typed := &transport.TransportError{Code: transport.CodeIMAPAuthFailed, Message: "no"}
	if got := failureCode(ctx, typed); got != transport.CodeIMAPAuthFailed {
		t.Fatalf("typed error = %q, want the transport code", got)
	}
	if got := failureCode(ctx, errTestPlain{}); got != "sync_check_failed" {
		t.Fatalf("plain error = %q, want sync_check_failed", got)
	}
}

const secondarySyncAccountID = "CCCCCCCC-DDDD-4EEE-8FFF-000000000000"

func addSyncImapAccount(t *testing.T, store *Store, address string) string {
	t.Helper()
	location, err := parseAccountRoot("imap://" + secondarySyncAccountID + "/")
	if err != nil {
		t.Fatalf("parse secondary sync account: %v", err)
	}
	store.activeAccounts = append(store.activeAccounts, location)
	store.activeAccountKeys[location.rootKey()] = struct{}{}
	accountRoot := filepath.Join(store.versionRoot, secondarySyncAccountID)
	if err := os.MkdirAll(accountRoot, 0o700); err != nil {
		t.Fatalf("create secondary sync account root: %v", err)
	}
	cache := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>mboxes</key><dict><key>[Gmail]</key><dict>
<key>MailboxPathComponent</key><string>[Gmail]</string>
<key>IMAPMailboxChildren</key><dict><key>Sent</key><dict>
<key>MailboxPathComponent</key><string>Sent</string>
<key>IMAPMailboxAttributes</key><integer>32768</integer>
<key>IMAPMailboxChildren</key><dict/>
</dict></dict></dict></dict></dict></plist>`)
	if err := os.WriteFile(filepath.Join(accountRoot, ".mboxCache.plist"), cache, 0o600); err != nil {
		t.Fatalf("write secondary sync mailbox cache: %v", err)
	}
	databasePath := filepath.Join(store.versionRoot, "MailData", envelopeIndexName)
	writer := openTestWriter(t, databasePath)
	statements := []struct {
		query string
		args  []any
	}{
		{
			query: `INSERT INTO mailboxes(ROWID,url,total_count,unread_count,deleted_count,source)
				VALUES (5,'imap://` + secondarySyncAccountID + `/%5BGmail%5D/Sent',0,0,0,1)`,
		},
		{query: `INSERT INTO addresses(ROWID,address,comment) VALUES (4,?,?)`, args: []any{address, "Secondary"}},
		{query: `INSERT INTO subjects(ROWID,subject) VALUES (1901,'Secondary sent identity')`},
		{query: `INSERT INTO summaries(ROWID,summary) VALUES (2901,'secondary sent identity')`},
		{
			query: `INSERT INTO messages(
				ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,
				mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color
			) VALUES (901,3901,4901,4,1901,2901,50,50,5,0,1,0,0,100,901,0,50,0)`,
		},
	}
	for _, statement := range statements {
		if _, err := writer.Exec(statement.query, statement.args...); err != nil {
			closeTestResourceNow(t, writer, "secondary sync account writer")
			t.Fatalf("execute secondary sync account fixture: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close secondary sync account writer: %v", err)
	}
	ref, err := mailref.EncodeAccount(secondarySyncAccountID)
	if err != nil {
		t.Fatalf("encode secondary sync account ref: %v", err)
	}
	return ref
}

type errTestPlain struct{}

func (errTestPlain) Error() string { return "plain" }

// An oversized IMAP fetch on the raw-source path surfaces the typed error
// instead of being masked by the local missing-source error, and carries
// the shared cap as its bound.
func TestGetRawSourcePropagatesOversizedFetch(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "rawcap1@gmail.com")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Quarterly Report")
	messageRef = messageRefWithExpectedID(t, messageRef, "<101@example.com>")
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
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{{Name: "INBOX"}},
		uid:   101,
		fetchErr: &transport.TransportError{
			Code:    transport.CodeIMAPRawSourceTooLarge,
			Message: "IMAP FETCH announced 134217728 bytes exceeding the 67108864 byte raw-source cap",
		},
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"rawcap1@gmail.com": "secret"},
		},
	}
	_, err = client.GetRawSource(context.Background(), messageRef)
	if transport.ErrorCode(err) != transport.CodeIMAPRawSourceTooLarge {
		t.Fatalf("GetRawSource() error = %v, want raw_source_too_large (not the masked local error)", err)
	}
	var combined *hydrationError
	if !errors.As(err, &combined) {
		t.Fatalf("GetRawSource() error type = %T, want hydrationError", err)
	}
	var localErr *Error
	if !errors.As(err, &localErr) || localErr.Code != "message_source_missing" {
		t.Fatalf("GetRawSource() local cause = %v, want message_source_missing", err)
	}
	var remoteErr *transport.TransportError
	if !errors.As(err, &remoteErr) || remoteErr.Code != transport.CodeIMAPRawSourceTooLarge {
		t.Fatalf("GetRawSource() remote cause = %v, want raw_source_too_large", err)
	}
	if fakeImap.lastFetchMax != mail.MaximumRawSourceBytes {
		t.Fatalf("fetch bound = %d, want shared cap %d", fakeImap.lastFetchMax, mail.MaximumRawSourceBytes)
	}
}

func TestWriteRawSourcePreservesBothHydrationFailures(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "writecap@gmail.com")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Quarterly Report")
	messageRef = messageRefWithExpectedID(t, messageRef, "<101@example.com>")
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
	remoteErr := &transport.TransportError{
		Code:    transport.CodeIMAPTimeout,
		Message: "FETCH timed out",
	}
	fakeImap := &stubImapOperator{
		boxes:    []transport.MailboxInfo{{Name: "INBOX"}},
		uid:      101,
		fetchErr: remoteErr,
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"writecap@gmail.com": "secret"},
		},
	}
	var output strings.Builder
	err = client.WriteRawSource(context.Background(), messageRef, &output)
	if transport.ErrorCode(err) != transport.CodeIMAPTimeout {
		t.Fatalf("WriteRawSource() error = %v, want %s", err, transport.CodeIMAPTimeout)
	}
	var combined *hydrationError
	if !errors.As(err, &combined) {
		t.Fatalf("WriteRawSource() error type = %T, want hydrationError", err)
	}
	var localErr *Error
	if !errors.As(err, &localErr) || localErr.Code != "message_source_missing" {
		t.Fatalf("WriteRawSource() local cause = %v, want message_source_missing", err)
	}
	var fetchedErr *transport.TransportError
	if !errors.As(err, &fetchedErr) || fetchedErr.Code != transport.CodeIMAPTimeout {
		t.Fatalf("WriteRawSource() remote cause = %v, want %s", err, transport.CodeIMAPTimeout)
	}
}

// Content hydration uses the same bounded IMAP literal path as raw-source
// hydration so a remote response cannot bypass the local source limit.
func TestGetMessageHydrationFetchBounded(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "rawcap2@gmail.com")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Quarterly Report")
	messageRef = messageRefWithExpectedID(t, messageRef, "<101@example.com>")
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
			Credentials: stubCredentials{"rawcap2@gmail.com": "secret"},
		},
	}
	message, err := client.GetMessage(context.Background(), messageRef)
	if err != nil || message.Content != "Full body" {
		t.Fatalf("GetMessage() = %+v, error = %v", message, err)
	}
	if fakeImap.lastFetchMax != mail.MaximumRawSourceBytes {
		t.Fatalf("content fetch bound = %d, want shared cap %d", fakeImap.lastFetchMax, mail.MaximumRawSourceBytes)
	}
}

// A sent-empty IMAP account is listed degraded instead of aborting the whole
// catalog, and a healthy sibling account stays intact.
func TestListAccountsDegradesSentEmptyAccount(t *testing.T) {
	store, _ := newSearchFixture(t)
	// installSentMailboxFixture without identity messages: Sent exists but
	// holds no senders, so the IMAP account has no provable identity.
	installSentMailboxFixture(t, store)
	accounts, err := store.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if len(accounts) == 0 {
		t.Fatal("no accounts listed")
	}
	degraded := 0
	for _, account := range accounts {
		if account.State != "degraded" {
			continue
		}
		degraded++
		if account.DegradedReason != "no_provably_sent_identity" {
			t.Fatalf("degraded reason = %q, want no_provably_sent_identity", account.DegradedReason)
		}
		if account.DegradedRemediation == "" {
			t.Fatalf("degraded account has no remediation: %+v", account)
		}
		if len(account.EmailAddresses) != 0 {
			t.Fatalf("degraded account carries identities: %+v", account)
		}
	}
	if degraded == 0 {
		t.Fatalf("sent-empty IMAP account not degraded: %+v", accounts)
	}
}

// A corrupted mailbox cache for one account degrades only that account.
func TestListAccountsDegradesUnreadableCache(t *testing.T) {
	store, _ := newSearchFixture(t)
	installImapIdentityFixture(t, store, "healthy@gmail.com")
	accountRoot := filepath.Join(store.versionRoot, testAccountID)
	if err := os.WriteFile(filepath.Join(accountRoot, ".mboxCache.plist"), []byte("not a plist"), 0o600); err != nil {
		t.Fatalf("corrupt cache: %v", err)
	}
	accounts, err := store.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts() error = %v (one bad cache must not abort)", err)
	}
	if len(accounts) == 0 {
		t.Fatal("no accounts listed")
	}
	found := false
	for _, account := range accounts {
		if account.State == "degraded" && account.DegradedReason == "mailbox_cache_unreadable" {
			if account.DegradedRemediation == "" {
				t.Fatalf("degraded account has no remediation: %+v", account)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no mailbox_cache_unreadable entry: %+v", accounts)
	}
}

// A cache-declared special-use mailbox missing from the Envelope Index
// degrades the account instead of failing the catalog.
func TestListAccountsDegradesUnresolvedSpecialMailbox(t *testing.T) {
	store, _ := newSearchFixture(t)
	installSentMailboxFixture(t, store)
	writer := openTestWriter(t, filepath.Join(store.versionRoot, "MailData", envelopeIndexName))
	if _, err := writer.Exec(`DELETE FROM mailboxes WHERE ROWID = 4`); err != nil {
		closeTestResourceNow(t, writer, "sent row writer")
		t.Fatalf("delete Sent row: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	accounts, err := store.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	found := false
	for _, account := range accounts {
		if account.State == "degraded" && account.DegradedReason == "special_use_mailbox_unresolved" {
			if account.DegradedRemediation == "" {
				t.Fatalf("degraded account has no remediation: %+v", account)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no special_use_mailbox_unresolved entry: %+v", accounts)
	}
}

// A healthy account reports state ok with no reason.
func TestListAccountsHealthyStateOk(t *testing.T) {
	store, _ := newSearchFixture(t)
	installImapIdentityFixture(t, store, "healthy@gmail.com")
	accounts, err := store.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if len(accounts) != 1 || accounts[0].State != "ok" || accounts[0].DegradedReason != "" {
		t.Fatalf("accounts = %+v, want one ok account without reason", accounts)
	}
	if len(accounts[0].EmailAddresses) == 0 {
		t.Fatalf("healthy account lost identities: %+v", accounts[0])
	}
	coverage := accounts[0].IdentityCoverage
	if coverage.Source != mail.SenderIdentityCoverageSourceSentHistory ||
		coverage.State != mail.SenderIdentityCoverageStateComplete ||
		coverage.ObservedMessages != 1 || coverage.MoreAvailable {
		t.Fatalf("healthy account identity coverage = %+v", coverage)
	}
}

// A locally materialized (external-file) attachment satisfies the IMAP
// fallback without any hydration: no IMAP fetch runs even though the
// .emlx source is missing.
func TestSaveAttachmentSkipsHydrateWhenMaterialized(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "materialize1@gmail.com")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Quarterly Report")
	resolved, err := store.resolveMessage(context.Background(), messageRef)
	if err != nil {
		t.Fatalf("resolveMessage() error = %v", err)
	}
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
	// Materialize the catalog attachment (id 2, invoice.pdf) externally.
	externalDir, err := store.attachmentDirectory(resolved, "2")
	if err != nil {
		t.Fatalf("attachmentDirectory() error = %v", err)
	}
	if err := os.MkdirAll(externalDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(externalDir, "invoice.pdf"), []byte("materialized-bytes"), 0o600); err != nil {
		t.Fatalf("write external attachment: %v", err)
	}

	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{{Name: "INBOX"}},
		uid:   101,
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"materialize1@gmail.com": "secret"},
		},
	}
	output := filepath.Join(t.TempDir(), "invoice.pdf")
	if err := client.SaveAttachmentTo(context.Background(), messageRef, "2", output); err != nil {
		t.Fatalf("SaveAttachmentTo() error = %v", err)
	}
	content, err := os.ReadFile(output)
	if err != nil || string(content) != "materialized-bytes" {
		t.Fatalf("attachment bytes = %q, error = %v; want the external file content", content, err)
	}
	if fakeImap.lastFetchMax != 0 {
		t.Fatalf("IMAP fetch ran (bound %d) although the attachment was locally materialized", fakeImap.lastFetchMax)
	}
}

// The attachment hydration fetch honors the shared raw-source cap, and an
// oversized fetch surfaces the typed error instead of the masked local
// missing-source error.
func TestSaveAttachmentHydrationHonorsCapAndFailsTyped(t *testing.T) {
	for _, test := range []struct {
		name     string
		raw      []byte
		fetchErr error
		wantCode string
	}{
		{
			name: "capped fetch succeeds",
			raw: []byte("From: Alice <alice@example.com>\r\nSubject: Quarterly Report\r\n" +
				"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
				"--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
				"--b\r\nContent-Disposition: attachment; filename=invoice.pdf\r\n" +
				"Content-Transfer-Encoding: base64\r\n\r\naW52b2ljZS1ieXRlcw==\r\n--b--\r\n"),
			wantCode: "",
		},
		{
			name: "oversized fetch fails typed",
			fetchErr: &transport.TransportError{
				Code:    transport.CodeIMAPRawSourceTooLarge,
				Message: "IMAP FETCH announced 134217728 bytes exceeding the 67108864 byte raw-source cap",
			},
			wantCode: transport.CodeIMAPRawSourceTooLarge,
		},
		{
			name: "generic fetch failure remains typed",
			fetchErr: &transport.TransportError{
				Code:    transport.CodeIMAPTimeout,
				Message: "FETCH timed out",
			},
			wantCode: transport.CodeIMAPTimeout,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, inboxRef := newSearchFixture(t)
			closeTestResource(t, store, "test store")
			installImapIdentityFixture(t, store, "materialize2@gmail.com")
			page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
				MailboxRef: inboxRef, Limit: 3,
			})
			if err != nil {
				t.Fatal(err)
			}
			messageRef := messageRefWithSubject(t, page.Messages, "Quarterly Report")
			messageRef = messageRefWithExpectedID(t, messageRef, "<101@example.com>")
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
			fakeImap := &stubImapOperator{
				boxes:    []transport.MailboxInfo{{Name: "INBOX"}},
				uid:      101,
				raw:      test.raw,
				fetchErr: test.fetchErr,
			}
			client := &Client{
				store: store,
				send: mail.SendTransport{
					Imap:        fakeImap,
					Credentials: stubCredentials{"materialize2@gmail.com": "secret"},
				},
			}
			output := filepath.Join(t.TempDir(), "invoice.pdf")
			err = client.SaveAttachmentTo(context.Background(), messageRef, "2", output)
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("SaveAttachmentTo() error = %v", err)
				}
				content, readErr := os.ReadFile(output)
				if readErr != nil || string(content) != "invoice-bytes" {
					t.Fatalf("attachment bytes = %q, error = %v", content, readErr)
				}
			} else if transport.ErrorCode(err) != test.wantCode {
				t.Fatalf("SaveAttachmentTo() error = %v, want %s (not the masked local error)", err, test.wantCode)
			} else {
				var combined *hydrationError
				if !errors.As(err, &combined) {
					t.Fatalf("SaveAttachmentTo() error type = %T, want hydrationError", err)
				}
			}
			if fakeImap.lastFetchMax != mail.MaximumRawSourceBytes {
				t.Fatalf("fetch bound = %d, want the shared cap %d", fakeImap.lastFetchMax, mail.MaximumRawSourceBytes)
			}
		})
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
		uid:                    102,
		searchMatchesByMailbox: map[string][]int{"Sent": []int{0}},
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

func installTrashMessageFixture(t *testing.T, store *Store) string {
	t.Helper()
	updateFixtureMessage(t, store, `INSERT INTO mailboxes(ROWID,url,total_count,unread_count,deleted_count,source) VALUES (5,'imap://`+testAccountID+`/Trash',1,0,0,1)`)
	updateFixtureMessage(t, store, `UPDATE messages SET mailbox = 5 WHERE ROWID = 102`)
	ref, err := mailref.EncodeMailbox(testAccountID, []string{"Trash"})
	if err != nil {
		t.Fatalf("EncodeMailbox() error = %v", err)
	}
	return ref
}

func TestDeleteMessageRejectsTrashTarget(t *testing.T) {
	tests := []struct {
		name     string
		identity string
		boxes    []transport.MailboxInfo
	}{
		{
			name:     "special use",
			identity: "trash-special@gmail.com",
			boxes: []transport.MailboxInfo{
				{Name: "INBOX"}, {Name: "Bin", Flags: []string{"\\Trash"}},
			},
		},
		{
			name:     "name match",
			identity: "trash-name@gmail.com",
			boxes: []transport.MailboxInfo{
				{Name: "INBOX"}, {Name: "Trash"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, _ := newSearchFixture(t)
			closeTestResource(t, store, "test store")
			installImapIdentityFixture(t, store, tt.identity)
			trashRef := installTrashMessageFixture(t, store)
			page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
				MailboxRef: trashRef, Limit: 1,
			})
			if err != nil || len(page.Messages) != 1 {
				t.Fatalf("ListMessages() = %+v, error = %v", page, err)
			}
			messageRef := messageRefWithExpectedID(t, page.Messages[0].Ref, "<102@example.com>")
			fakeImap := &stubImapOperator{boxes: tt.boxes, uid: 102}
			client := &Client{
				store: store,
				send: mail.SendTransport{
					Imap: fakeImap, Credentials: strictCredentials{tt.identity: "secret"},
				},
			}
			_, err = client.DeleteMessage(context.Background(), mail.DeleteMessageRequest{
				Ref: messageRef, AllowDraftMutation: true,
			})
			if transport.ErrorCode(err) != transport.CodeMessageAlreadyTrashed {
				t.Fatalf("DeleteMessage() error = %v, want %s", err, transport.CodeMessageAlreadyTrashed)
			}
			if !strings.Contains(err.Error(), "restore it in Mail") ||
				!strings.Contains(err.Error(), "empty the trash") {
				t.Fatalf("DeleteMessage() error = %v, want remediation", err)
			}
			if fakeImap.mutationCalls != 0 {
				t.Fatalf("IMAP mutation calls = %d, want 0", fakeImap.mutationCalls)
			}
		})
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

func messageRefWithExpectedID(t *testing.T, value, messageID string) string {
	t.Helper()
	ref, err := mailref.DecodeMessage(value)
	if err != nil {
		t.Fatalf("DecodeMessage() error = %v", err)
	}
	ref.ExpectedMessageID = messageID
	encoded, err := mailref.EncodeMessage(ref)
	if err != nil {
		t.Fatalf("EncodeMessage() error = %v", err)
	}
	return encoded
}

func TestSyncIdentityUsesCredentialBackedAlias(t *testing.T) {
	account := mail.Account{EmailAddresses: []string{"primary@gmail.com", "alias@gmail.com"}}
	email, host, port, password, err := syncIdentity(account, strictCredentials{"alias@gmail.com": "secret"})
	if err != nil {
		t.Fatalf("syncIdentity() error = %v", err)
	}
	if email != "alias@gmail.com" || host == "" || port == 0 || password != "secret" {
		t.Fatalf("syncIdentity() = email:%q host:%q port:%d password:%q", email, host, port, password)
	}
}

func TestSyncIdentitySelectionIsStableAcrossAliasOrder(t *testing.T) {
	credentials := strictCredentials{
		"alpha@gmail.com": "alpha-secret",
		"zeta@gmail.com":  "zeta-secret",
	}
	first, host, port, password, err := syncIdentity(mail.Account{
		EmailAddresses: []string{"zeta@gmail.com", "alpha@gmail.com"},
	}, credentials)
	if err != nil {
		t.Fatalf("syncIdentity() error = %v", err)
	}
	second, _, _, _, err := syncIdentity(mail.Account{
		EmailAddresses: []string{"alpha@gmail.com", "zeta@gmail.com"},
	}, credentials)
	if err != nil {
		t.Fatalf("syncIdentity() reordered error = %v", err)
	}
	if first != "alpha@gmail.com" || second != first || host != "imap.gmail.com" || port != 993 || password != "alpha-secret" {
		t.Fatalf("identity = %q/%q host=%q port=%d password=%q", first, second, host, port, password)
	}
}

func TestMailboxCacheIsScopedClonedAndExpires(t *testing.T) {
	operator := &stubImapOperator{boxes: []transport.MailboxInfo{{Name: "Inbox", Flags: []string{"\\Inbox"}}}}
	client := &Client{}
	cfg := transport.ImapConfig{Host: "imap.example.com", Port: 993, Username: "alice@example.com"}
	first, err := client.getOrLoadMailboxes(context.Background(), operator, cfg, cfg.Username)
	if err != nil {
		t.Fatalf("first mailbox load: %v", err)
	}
	first[0].Flags[0] = "mutated"
	second, err := client.getOrLoadMailboxes(context.Background(), operator, cfg, cfg.Username)
	if err != nil {
		t.Fatalf("cached mailbox load: %v", err)
	}
	if operator.listCalls != 1 || second[0].Flags[0] != "\\Inbox" {
		t.Fatalf("cached boxes = %+v, list calls = %d", second, operator.listCalls)
	}
	otherCfg := cfg
	otherCfg.Username = "bob@example.com"
	if _, err := client.getOrLoadMailboxes(context.Background(), operator, otherCfg, otherCfg.Username); err != nil {
		t.Fatalf("isolated mailbox load: %v", err)
	}
	if operator.listCalls != 2 {
		t.Fatalf("list calls for isolated identity = %d, want 2", operator.listCalls)
	}
	key := mailboxCacheKey(cfg, cfg.Username)
	client.mailboxCacheMu.Lock()
	entry := client.mailboxCache[key]
	entry.expiresAt = time.Now().Add(-time.Second)
	client.mailboxCache[key] = entry
	client.mailboxCacheMu.Unlock()
	if _, err := client.getOrLoadMailboxes(context.Background(), operator, cfg, cfg.Username); err != nil {
		t.Fatalf("expired mailbox load: %v", err)
	}
	if operator.listCalls != 3 {
		t.Fatalf("list calls after expiry = %d, want 3", operator.listCalls)
	}
}

func TestMailboxCacheKeyIncludesEverySelectionIdentity(t *testing.T) {
	base := transport.ImapConfig{Host: "imap.example.com", Port: 993, Username: "alice@example.com"}
	baseKey := mailboxCacheKey(base, "alice@example.com")
	tests := []struct {
		name  string
		cfg   transport.ImapConfig
		email string
	}{
		{name: "host", cfg: transport.ImapConfig{Host: "imap.other.example", Port: 993, Username: base.Username}, email: "alice@example.com"},
		{name: "port", cfg: transport.ImapConfig{Host: base.Host, Port: 143, Username: base.Username}, email: "alice@example.com"},
		{name: "username", cfg: transport.ImapConfig{Host: base.Host, Port: base.Port, Username: "bob@example.com"}, email: "alice@example.com"},
		{name: "account", cfg: base, email: "alias@example.com"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := mailboxCacheKey(test.cfg, test.email); got == baseKey {
				t.Fatalf("mailboxCacheKey() = %q, collides with base key", got)
			}
		})
	}
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
			"Reply-To: Bob <bob@example.com>, Carol <carol@example.com>\r\n"+
			"Subject: Threaded\r\n"+
			"Message-ID: <reply-102@example.com>\r\n"+
			"References: <root-1@example.com> <reply-101@example.com>\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n\r\nBody\r\n",
	))
	source, err = client.MessageThreadSource(context.Background(), ref)
	if err != nil {
		t.Fatalf("MessageThreadSource() error = %v", err)
	}
	if source.Subject != "Threaded" || len(source.ReplyTo) != 2 ||
		source.ReplyTo[0].Address != "bob@example.com" || source.ReplyTo[1].Address != "carol@example.com" ||
		source.MessageID != "<reply-102@example.com>" ||
		source.References != "<root-1@example.com> <reply-101@example.com>" {
		t.Fatalf("thread source = %+v", source)
	}
	if len(source.CC) != 1 || source.CC[0].Address != "zoe@example.com" {
		t.Fatalf("cc = %+v", source.CC)
	}

	writeFixtureEMLX(t, store, 102, "imap://"+testAccountID+"/INBOX", []byte(
		"From: Alice <alice@example.com>\r\n"+
			"Reply-To: Bob <bob@example.com>, malformed\r\n"+
			"Subject: Malformed Reply-To\r\n"+
			"Message-ID: <reply-102@example.com>\r\n\r\nBody\r\n",
	))
	if _, err := client.MessageThreadSource(context.Background(), ref); err == nil ||
		errorCodeForTest(err) != "invalid_message_source" {
		t.Fatalf("malformed Reply-To error = %v", err)
	}

	writeFixtureEMLX(t, store, 102, "imap://"+testAccountID+"/INBOX", []byte(
		"From: Alice <alice@example.com>\r\nSubject: No ID\r\n\r\nBody\r\n",
	))
	if _, err := client.MessageThreadSource(context.Background(), ref); err == nil ||
		errorCodeForTest(err) != "invalid_message_source" {
		t.Fatalf("missing Message-ID error = %v", err)
	}
}
