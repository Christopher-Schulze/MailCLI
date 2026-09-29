package mailstore

import (
	"bytes"
	"context"
	"errors"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

const serverMessageRaw = "From: =?UTF-8?Q?J=C3=BCrgen?= <j@example.com>\r\nTo: Me <me@example.com>\r\n" +
	"Subject: =?UTF-8?B?w5xiZXJyYXNjaHVuZw==?=\r\nDate: Tue, 29 Sep 2026 10:15:00 +0200\r\nMessage-ID: <srv-1@example.com>\r\n" +
	"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
	"--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nFresh from the server\r\n" +
	"--b\r\nContent-Disposition: attachment; filename=invoice.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\naW52b2ljZS1ieXRlcw==\r\n--b--\r\n"

func serverRefFor(t *testing.T, uid uint32) string {
	t.Helper()
	ref, err := mailref.EncodeServer(mailref.Server{AccountID: testAccountID, MailboxPath: []string{"INBOX"}, UIDValidity: 900, UID: uid})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func serverReadFixture(t *testing.T, address string) (*Client, *stubImapOperator) {
	t.Helper()
	client, _ := newMessagesFixture(t, address, nil)
	operator := &stubImapOperator{
		raw:       []byte(serverMessageRaw),
		boxes:     []transport.MailboxInfo{{Name: "INBOX"}, {Name: "All"}, {Name: "Sent", Flags: []string{"\\Sent"}}},
		flagState: transport.FlagState{Flags: []string{`\Seen`}, UIDValidity: 900},
	}
	client.send.Imap = operator
	return client, operator
}

func TestServerRefReadsTheFullMessageOverIMAP(t *testing.T) {
	client, operator := serverReadFixture(t, "srv-full@gmail.com")
	ref := serverRefFor(t, 5003)
	message, err := client.GetMessage(context.Background(), ref)
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	if message.Content != "Fresh from the server" || !message.ContentComplete || message.ContentSource != "imap_raw" ||
		len(message.Attachments) != 1 || message.Attachments[0].ID != "2" {
		t.Fatalf("message = %+v", message)
	}
	summary := message.Summary
	if summary.Ref != ref || summary.Subject != "Überraschung" || summary.Sender != "Jürgen <j@example.com>" ||
		summary.DateSent != "2026-09-29T08:15:00Z" || summary.MessageID != "srv-1@example.com" || !summary.Read ||
		summary.LocalRef != "" || summary.StalenessNote == "" || summary.FlagsState != mail.MessageServerStateObserved {
		t.Fatalf("summary = %+v", summary)
	}
	if operator.fetchFlagsUID != 5003 {
		t.Fatalf("flags were read for UID %d, want 5003", operator.fetchFlagsUID)
	}
}

func TestServerRefFlagObservationQualifiesUsefulContent(t *testing.T) {
	for _, test := range []struct {
		name        string
		state       transport.FlagState
		failure     error
		unsupported bool
		wantState   string
		wantFlags   bool
	}{
		{name: "empty observed flags", state: transport.FlagState{UIDValidity: 900}, wantState: "observed"},
		{name: "observed set flags", state: transport.FlagState{UIDValidity: 900, Flags: []string{`\sEeN`, `\FLAGGED`, `\Deleted`, "$Junk"}}, wantState: "observed", wantFlags: true},
		{name: "missing UID", state: transport.FlagState{UIDValidity: 900, Missing: true}, wantState: "missing"},
		{name: "wrong generation", state: transport.FlagState{UIDValidity: 901, Flags: []string{`\Seen`}}, wantState: "unverified"},
		{name: "missing in wrong generation", state: transport.FlagState{UIDValidity: 901, Missing: true}, wantState: "unverified"},
		{name: "timeout", failure: &transport.TransportError{Code: transport.CodeIMAPTimeout, Message: "timed out"}, wantState: "unverified"},
		{name: "transport failure", failure: errors.New("flag observation failed"), wantState: "unverified"},
		{name: "unsupported reader", unsupported: true, wantState: "unverified"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, operator := serverReadFixture(t, "srv-flags@gmail.com")
			operator.flagState, operator.flagStateErr = test.state, test.failure
			if test.unsupported {
				client.send.Imap = struct{ transport.ImapOperator }{operator}
			}
			message, err := client.GetMessage(context.Background(), serverRefFor(t, 5003))
			if err != nil || message.Content != "Fresh from the server" || !message.ContentComplete {
				t.Fatalf("optional flags discarded useful content: message=%+v error=%v", message, err)
			}
			summary := message.Summary
			if summary.FlagsState != test.wantState || summary.Read != test.wantFlags || summary.Flagged != test.wantFlags ||
				summary.Deleted != test.wantFlags || summary.Junk || summary.StalenessNote == "" {
				t.Fatalf("summary=%+v; want state=%s flags=%t without a junk claim", summary, test.wantState, test.wantFlags)
			}
			if (summary.StalenessNote == serverRefStalenessNote) != (test.wantState == "observed") {
				t.Fatalf("state=%s has misleading note %q", test.wantState, summary.StalenessNote)
			}
		})
	}
}

func TestServerRefFailedIdentityResolutionKeepsUnverifiedContent(t *testing.T) {
	client, operator := serverReadFixture(t, "srv-resolve@gmail.com")
	client.send.Credentials = strictCredentials{}
	message := client.finishServerMessage(context.Background(), serverRefFor(t, 5003), mail.Message{
		Headers: serverMessageRaw, Content: "already fetched", ContentComplete: true,
	})
	if message.Summary.FlagsState != "unverified" || message.Content != "already fetched" ||
		!message.ContentComplete || operator.fetchFlagsCalls != 0 {
		t.Fatalf("failed optional resolution lost evidence: %+v, calls=%d", message, operator.fetchFlagsCalls)
	}
}

func TestServerRefDetailHeadersRemainCompleteBeyondTriageLimit(t *testing.T) {
	subject := strings.Repeat("Überraschung", 30) + " Ende"
	name := strings.Repeat("Jürgen", 40) + " Ende"
	raw := strings.Replace(serverMessageRaw, "=?UTF-8?B?w5xiZXJyYXNjaHVuZw==?=", mime.BEncoding.Encode("utf-8", subject), 1)
	raw = strings.Replace(raw, "=?UTF-8?Q?J=C3=BCrgen?=", mime.QEncoding.Encode("utf-8", name), 1)
	for _, intent := range []mail.MessageReadIntent{mail.MessageReadIntentFull, mail.MessageReadIntentHeaders, mail.MessageReadIntentAttachments} {
		t.Run(string(intent), func(t *testing.T) {
			client, operator := serverReadFixture(t, "srv-long@gmail.com")
			operator.raw = []byte(raw)
			message, err := client.GetMessageWithIntent(context.Background(), serverRefFor(t, 5003), intent)
			if err != nil || message.Summary.Subject != subject || message.Summary.Sender != name+" <j@example.com>" ||
				message.Summary.DateSent != "2026-09-29T08:15:00Z" || message.Summary.FlagsState != "observed" {
				t.Fatalf("detail headers shortened or changed: %+v error=%v", message.Summary, err)
			}
			if intent == mail.MessageReadIntentAttachments && (message.Headers != "" || message.Content != "" || len(message.Attachments) != 1) {
				t.Fatalf("attachment intent retained temporary headers/body or lost metadata: %+v", message)
			}
		})
	}
	triage := mail.NewMessageFromHeader("server", []byte(raw), true)
	if utf8.RuneCountInString(triage.Subject) != 200 || utf8.RuneCountInString(triage.Sender) != 200 ||
		!strings.HasSuffix(triage.Subject, "…") || !strings.HasSuffix(triage.Sender, "…") {
		t.Fatalf("triage lost its bounded contract: %+v", triage)
	}
}

func TestServerHeaderSummaryRetainsUndecodableBoundedValues(t *testing.T) {
	subject := "=?not-a-real-charset?Q?" + strings.Repeat("subject", 40) + "?="
	sender := strings.Repeat("invalid-address", 20)
	var summary mail.MessageSummary
	completeServerHeaderSummary(&summary, "From: "+sender+"\r\nSubject: "+subject+"\r\n\r\n")
	if summary.Subject != subject || summary.Sender != sender {
		t.Fatalf("optional decoders discarded useful raw headers: %+v", summary)
	}
	oversized := "Subject: " + strings.Repeat("s", maximumHeaderBytes) + "\r\n\r\n"
	summary = mail.MessageSummary{Subject: "retained"}
	completeServerHeaderSummary(&summary, oversized)
	if summary.Subject != "retained" {
		t.Fatalf("summary decoder bypassed the existing header bound: %+v", summary)
	}
}

func TestServerHeaderTextKeepsCompleteSafeSingleLines(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{input: " \tÜber\x00raschung\r\n\u2028Ende\x1b ", want: "Über raschung Ende"},
		{input: strings.Repeat("Ü", 300), want: strings.Repeat("Ü", 300)},
		{input: "\r\n\x00", want: ""},
	} {
		if got := serverHeaderText(test.input); got != test.want {
			t.Errorf("serverHeaderText(%q)=%q, want %q", test.input, got, test.want)
		}
	}
}

func TestServerRefMetadataViewReadsHeadersOnly(t *testing.T) {
	client, _ := serverReadFixture(t, "srv-headers@gmail.com")
	ref := serverRefFor(t, 5003)
	message, err := client.GetMessageWithIntent(context.Background(), ref, mail.MessageReadIntentHeaders)
	if err != nil || message.Summary.Subject != "Überraschung" || message.Summary.Ref != ref || message.Content != "" {
		t.Fatalf("headers message = %+v, error = %v", message, err)
	}
	attachments, err := client.GetMessageWithIntent(context.Background(), ref, mail.MessageReadIntentAttachments)
	if err != nil || len(attachments.Attachments) != 1 {
		t.Fatalf("attachments message = %+v, error = %v", attachments, err)
	}
	if _, err := client.GetMessageWithIntent(context.Background(), ref, mail.MessageReadIntentIndex); err == nil ||
		!strings.Contains(err.Error(), "server ref has no local index row") {
		t.Fatalf("index intent error = %v", err)
	}
}

func TestServerRefReportsTheLocalRefOnceTheStoreHoldsTheMessage(t *testing.T) {
	client, _ := serverReadFixture(t, "srv-local@gmail.com")
	ref := serverRefFor(t, 5001)
	message, err := client.GetMessage(context.Background(), ref)
	if err != nil || message.Summary.LocalRef == "" {
		t.Fatalf("message = %+v, error = %v", message.Summary, err)
	}
	local, err := mailref.DecodeMessage(message.Summary.LocalRef)
	if err != nil || !local.IsStoreBound() || local.ExpectedIMAPUID != 5001 {
		t.Fatalf("local ref = %+v, error = %v", local, err)
	}
}

func TestServerRefFindsTheLocalRefOfALabelBackedMailboxByHeaderIdentity(t *testing.T) {
	client, _ := serverReadFixture(t, "srv-labels@gmail.com")
	updateFixtureMessage(t, client.store, `UPDATE messages SET mailbox = 2, remote_id = ROWID + 77000 WHERE ROWID IN (102, 103)`)
	moveFixtureMessagesToAll(t, client.store, 102, 103)
	updateFixtureMessage(t, client.store, `INSERT INTO labels(message_id, mailbox_id) VALUES (102, 1), (103, 1)`)
	// The server UID 41002 is an INBOX UID; the local row carries an All Mail UID.
	server := mailref.Server{AccountID: testAccountID, MailboxPath: []string{"INBOX"}, UIDValidity: 900, UID: 41002}
	ref, err := client.store.localRefForServerMessage(context.Background(), server, localIdentityHeader(t, client.store, 103))
	if err != nil || ref == "" {
		t.Fatalf("local ref = %q, %v", ref, err)
	}
	local, err := mailref.DecodeMessage(ref)
	if err != nil || local.LibraryID != "103" || !local.IsStoreBound() {
		t.Fatalf("local ref = %+v, %v; want the row with the same sender, date and subject", local, err)
	}
	other := []byte("From: someone@example.com\r\nSubject: Not stored\r\nDate: Tue, 29 Sep 2026 10:15:00 +0200\r\n\r\n")
	if ref, err := client.store.localRefForServerMessage(context.Background(), server, other); err != nil || ref != "" {
		t.Fatalf("a header with no local counterpart returned %q, %v", ref, err)
	}
}

func TestServerRefDoesNotChooseAHeaderCollisionAsLocalRef(t *testing.T) {
	client, _ := serverReadFixture(t, "srv-collision@gmail.com")
	updateFixtureMessage(t, client.store, `UPDATE messages SET mailbox = 2, remote_id = ROWID + 77000 WHERE ROWID IN (102, 103)`)
	moveFixtureMessagesToAll(t, client.store, 102, 103)
	updateFixtureMessage(t, client.store, `UPDATE messages SET date_sent = 200, subject = 2 WHERE ROWID = 103`)
	updateFixtureMessage(t, client.store, `INSERT INTO labels(message_id, mailbox_id) VALUES (102, 1), (103, 1)`)
	server := mailref.Server{AccountID: testAccountID, MailboxPath: []string{"INBOX"}, UIDValidity: 900, UID: 41002}
	known := localIdentityHeader(t, client.store, 102)
	ref, err := client.store.localRefForServerMessage(context.Background(), server, known)
	if err != nil || ref == "" {
		t.Fatalf("known collision ref = %q, %v", ref, err)
	}
	local, err := mailref.DecodeMessage(ref)
	if err != nil || local.LibraryID != "102" {
		t.Fatalf("selected local row = %+v, %v; want 102", local, err)
	}
	other := []byte(strings.Replace(string(known), "<102@example.com>", "<different@example.com>", 1))
	if ref, err := client.store.localRefForServerMessage(context.Background(), server, other); err != nil || ref != "" {
		t.Fatalf("unmatched collision ref = %q, %v", ref, err)
	}
}

func TestServerRefLocalLookupUsesVisibleMembershipAndGeneration(t *testing.T) {
	for _, test := range []struct {
		name           string
		sql            string
		validity       uint32
		matchingHeader bool
		wantRef        bool
	}{
		{name: "deleted label cannot hide the physical UID match", sql: `UPDATE messages SET deleted = 1 WHERE ROWID = 101; INSERT INTO labels(message_id, mailbox_id) VALUES (101, 1), (999999, 1)`, validity: 900, wantRef: true},
		{name: "deleted physical UID cannot publish a local ref", sql: `UPDATE messages SET deleted = 1 WHERE ROWID = 102`, validity: 900},
		{name: "empty visible membership has no counterpart", sql: `UPDATE messages SET deleted = 1 WHERE mailbox = 1; INSERT INTO labels(message_id, mailbox_id) VALUES (102, 1)`, validity: 0},
		{name: "mixed membership needs physical generation", sql: `INSERT INTO labels(message_id, mailbox_id) VALUES (101, 1)`, validity: 0, matchingHeader: true},
		{name: "mixed membership rejects changed physical generation", sql: `INSERT INTO labels(message_id, mailbox_id) VALUES (101, 1)`, validity: 901, matchingHeader: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, _ := serverReadFixture(t, "srv-visible@gmail.com")
			updateFixtureMessage(t, client.store, test.sql)
			location, err := parseMailboxURL("imap://" + testAccountID + "/INBOX")
			if err != nil {
				t.Fatal(err)
			}
			if test.validity == 0 {
				path, err := mailboxInfoPath(client.store.versionRoot, location)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else if test.validity != 900 {
				writeFixtureMailboxInfo(t, client.store, location, test.validity)
			}
			server := mailref.Server{AccountID: testAccountID, MailboxPath: []string{"INBOX"}, UIDValidity: 900, UID: 5001}
			// A deleted label cannot override a physical UID. In mixed membership,
			// even a matching exact Message-ID cannot bypass physical generation.
			headers := []byte("Subject: unrelated\r\n\r\n")
			if test.matchingHeader {
				headers = localIdentityHeader(t, client.store, 102)
			}
			ref, err := client.store.localRefForServerMessage(context.Background(), server, headers)
			if err != nil || (ref != "") != test.wantRef {
				t.Fatalf("local ref=%q error=%v; want reference=%t", ref, err, test.wantRef)
			}
			if test.wantRef {
				local, err := mailref.DecodeMessage(ref)
				if err != nil || !local.IsStoreBound() || local.LibraryID != "102" || local.ExpectedIMAPUID != 5001 {
					t.Fatalf("local ref=%+v error=%v; want verified physical row 102 UID 5001", local, err)
				}
			}
		})
	}
}

func TestServerRefRawSourceAndAttachmentSave(t *testing.T) {
	client, _ := serverReadFixture(t, "srv-raw@gmail.com")
	ref := serverRefFor(t, 5003)
	raw, err := client.GetRawSource(context.Background(), ref)
	if err != nil || raw != serverMessageRaw {
		t.Fatalf("GetRawSource() = %q, %v", raw, err)
	}
	var buffer bytes.Buffer
	if err := client.WriteRawSource(context.Background(), ref, &buffer); err != nil || buffer.String() != serverMessageRaw {
		t.Fatalf("WriteRawSource() = %q, %v", buffer.String(), err)
	}
	output := filepath.Join(t.TempDir(), "invoice.pdf")
	evidence, err := client.SaveAttachmentToWithEvidence(context.Background(), ref, "2", output)
	saved, readErr := os.ReadFile(output)
	if err != nil || readErr != nil || string(saved) != "invoice-bytes" || evidence.SHA256 == "" {
		t.Fatalf("save = %+v, %v; file = %q, %v", evidence, err, saved, readErr)
	}
}

func TestServerRefIsRejectedByEveryWriteAndLocalOnlyCommand(t *testing.T) {
	client, _ := serverReadFixture(t, "srv-write@gmail.com")
	ref := serverRefFor(t, 5003)
	ctx := context.Background()
	checks := map[string]error{}
	_, checks["mark"] = client.MarkMessage(ctx, mail.MarkMessageRequest{Ref: ref})
	_, checks["copy"] = client.TransferMessage(ctx, mail.TransferMessageRequest{Ref: ref, DestinationMailbox: "mbx_x", Copy: true})
	_, checks["delete"] = client.DeleteMessage(ctx, mail.DeleteMessageRequest{Ref: ref})
	_, checks["state"] = client.MessageState(ctx, ref)
	_, checks["thread"] = client.MessageThread(ctx, mail.MessageThreadRequest{Ref: ref, Limit: 5})
	_, checks["open draft"] = client.OpenDraft(ctx, ref)
	for name, err := range checks {
		if err == nil || (!strings.Contains(err.Error(), "server ref") && !strings.Contains(err.Error(), "read-only server evidence")) {
			t.Errorf("%s accepted or mis-reported a server ref: %v", name, err)
		}
		if typed, ok := err.(interface{ ErrorCode() string }); ok && typed.ErrorCode() != "invalid_reference" {
			t.Errorf("%s error code = %s, want invalid_reference", name, typed.ErrorCode())
		}
	}
}

func TestServerRefWorksInBatchReadAndAttachmentSaveButNotInBatchMutations(t *testing.T) {
	client, _ := serverReadFixture(t, "srv-batch@gmail.com")
	service := mail.NewService(client)
	ref := serverRefFor(t, 5003)
	ctx := context.Background()
	read, err := service.ExecuteBatch(ctx, mail.BatchRequest{Operation: mail.BatchOperationRead, Items: []mail.BatchItem{
		{ID: "full", Ref: ref}, {ID: "meta", Ref: ref, ReadIntent: mail.MessageReadIntentHeaders},
	}})
	if err != nil || len(read.Items) != 2 {
		t.Fatalf("batch read = %+v, %v", read, err)
	}
	for _, item := range read.Items {
		if item.State != mail.BatchItemCompleted || item.Message == nil || item.Message.Summary.Subject != "Überraschung" ||
			item.Message.Summary.FlagsState != mail.MessageServerStateObserved {
			t.Fatalf("batch read item %s = %+v", item.ID, item)
		}
	}
	output := filepath.Join(t.TempDir(), "invoice.pdf")
	save, err := service.ExecuteBatch(ctx, mail.BatchRequest{Operation: mail.BatchOperationAttachmentSave, Items: []mail.BatchItem{
		{ID: "save", Ref: ref, AttachmentID: "2", OutputPath: output},
	}})
	saved, readErr := os.ReadFile(output)
	if err != nil || len(save.Items) != 1 || save.Items[0].State != mail.BatchItemCompleted || readErr != nil || string(saved) != "invoice-bytes" {
		t.Fatalf("batch attachment save = %+v, %v; file = %q, %v", save, err, saved, readErr)
	}
	for _, operation := range []mail.BatchOperation{mail.BatchOperationMark, mail.BatchOperationDelete} {
		item := mail.BatchItem{ID: "write", Ref: ref}
		if operation == mail.BatchOperationMark {
			read := true
			item.Read = &read
		}
		_, err := service.ExecuteBatch(ctx, mail.BatchRequest{Operation: operation, Items: []mail.BatchItem{item}})
		var validation *mail.ValidationError
		if err == nil || !errors.As(err, &validation) || validation.Code != "invalid_reference" {
			t.Errorf("batch %s with a server ref: error = %v, want invalid_reference", operation, err)
		}
	}
}

func TestServerRefFailuresKeepTheirTypedCodes(t *testing.T) {
	client, operator := serverReadFixture(t, "srv-fail@gmail.com")
	operator.fetchErr = &transport.TransportError{Code: "mailbox_uidvalidity_changed", Message: "UIDVALIDITY changed"}
	if _, err := client.GetMessage(context.Background(), serverRefFor(t, 5003)); err == nil ||
		transport.ErrorCode(err) != "mailbox_uidvalidity_changed" {
		t.Fatalf("UIDVALIDITY change error = %v", err)
	}
	operator.fetchErr = nil
	operator.raw = nil
	if _, err := client.GetRawSource(context.Background(), serverRefFor(t, 5003)); err == nil ||
		transport.ErrorCode(err) != transport.CodeIMAPMessageNotFound {
		t.Fatalf("empty source error = %v", err)
	}
	client.send.Credentials = strictCredentials{}
	operator.raw = []byte(serverMessageRaw)
	if _, err := client.GetMessage(context.Background(), serverRefFor(t, 5003)); err == nil {
		t.Fatal("a server ref was read without stored credentials")
	}
}
