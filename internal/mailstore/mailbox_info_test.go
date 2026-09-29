package mailstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

func TestParseMailboxInfoXML(t *testing.T) {
	t.Parallel()
	source := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>UIDNEXT</key><integer>88</integer><key>UIDVALIDITY</key><integer>12345</integer></dict></plist>`
	validity, err := parseMailboxInfoXML(strings.NewReader(source))
	if err != nil || validity != 12345 {
		t.Fatalf("parseMailboxInfoXML() = %d, error = %v", validity, err)
	}
}

func TestParseMailboxInfoAcceptsDecimalStringUIDValidity(t *testing.T) {
	t.Parallel()
	source := `<plist><dict><key>UIDVALIDITY</key><string> 1469693657 </string></dict></plist>`
	validity, err := parseMailboxInfoXML(strings.NewReader(source))
	if err != nil || validity != 1469693657 {
		t.Fatalf("parseMailboxInfoXML() = %d, error = %v", validity, err)
	}
}

func TestParseMailboxInfoRejectsMissingOrInvalidUIDValidity(t *testing.T) {
	t.Parallel()
	for _, source := range []string{
		`<plist><dict><key>UIDNEXT</key><integer>4</integer></dict></plist>`,
		`<plist><dict><key>UIDVALIDITY</key><integer>0</integer></dict></plist>`,
		`<plist><dict><key>UIDVALIDITY</key><string>0</string></dict></plist>`,
		`<plist><dict><key>UIDVALIDITY</key><string>not-a-number</string></dict></plist>`,
		`<plist><dict><key>UIDVALIDITY</key><string>4294967296</string></dict></plist>`,
	} {
		if _, err := parseMailboxInfoXML(strings.NewReader(source)); err == nil {
			t.Fatalf("parseMailboxInfoXML(%q) error = nil", source)
		}
	}
	_, err := parseMailboxInfoXML(strings.NewReader(`<plist><dict><key>UIDVALIDITY</key><dict></dict></dict></plist>`))
	if err == nil || !strings.Contains(err.Error(), "expected plist integer or string, got dict") {
		t.Fatalf("malformed UIDVALIDITY error = %v, want the parse cause", err)
	}
}

func TestClientHydratesMissingSourceFromMappedIMAPIdentity(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "mapped@gmail.com")
	updateFixtureMessage(t, store, `UPDATE messages SET remote_id = 101, remote_mailbox = 7001 WHERE ROWID = 101`)
	location, err := parseMailboxURL("imap://" + testAccountID + "/%5BGmail%5D/All")
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureMailboxInfo(t, store, location, 12345)
	mailboxRef, err := mailref.EncodeMailbox(testAccountID, []string{"All"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: mailboxRef, Limit: 3})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Quarterly Report")
	decoded, err := mailref.DecodeMessage(messageRef)
	if err != nil || decoded.ExpectedIMAPUID != 101 || decoded.ExpectedIMAPMailboxID != 7001 {
		t.Fatalf("mapped ref = %+v, error = %v", decoded, err)
	}
	decoded.ExpectedMessageID = "<101@example.com>"
	messageRef, err = mailref.EncodeMessage(decoded)
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.messageBasePath(location, 101)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(base + ".emlx"); err != nil {
		t.Fatalf("remove local source: %v", err)
	}
	fakeImap := &stubImapOperator{
		raw:   []byte("From: Alice <alice@example.com>\r\nSubject: Quarterly Report\r\nMessage-ID: <101@example.com>\r\n\r\nMapped body\r\n"),
		boxes: []transport.MailboxInfo{{Name: "INBOX"}, {Name: "All"}},
	}
	client := &Client{store: store, send: mail.SendTransport{
		Imap: fakeImap, Credentials: stubCredentials{"mapped@gmail.com": "secret"},
	}}
	message, err := client.GetMessage(context.Background(), messageRef)
	if err != nil || message.Content != "Mapped body" || message.ContentSource != "imap_raw" {
		t.Fatalf("GetMessage() = %+v, error = %v", message, err)
	}
	if fakeImap.searchCalls != 0 {
		t.Fatalf("SearchUID calls = %d, want 0 for mapped identity", fakeImap.searchCalls)
	}
	resolvedRef, err := mailref.DecodeMessage(message.Summary.Ref)
	if err != nil || resolvedRef.ExpectedIMAPUID != 101 || resolvedRef.ExpectedIMAPUIDValidity != 12345 {
		t.Fatalf("hydrated summary ref = %+v, error = %v", resolvedRef, err)
	}
}

func TestClientRejectsStaleMappedUIDValidity(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "mapped-stale@gmail.com")
	updateFixtureMessage(t, store, `UPDATE messages SET remote_id = 101, remote_mailbox = 7001 WHERE ROWID = 101`)
	location, err := parseMailboxURL("imap://" + testAccountID + "/%5BGmail%5D/All")
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureMailboxInfo(t, store, location, 99999)
	mailboxRef, err := mailref.EncodeMailbox(testAccountID, []string{"All"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: mailboxRef, Limit: 3})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Quarterly Report")
	ref, err := mailref.DecodeMessage(messageRef)
	if err != nil {
		t.Fatal(err)
	}
	ref.ExpectedIMAPUIDValidity = 12345
	messageRef, err = mailref.EncodeMessage(ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientForIdentityTest(store, "mapped-stale@gmail.com").resolveImapTarget(context.Background(), messageRef); errorCodeForTest(err) != "stale_reference" {
		t.Fatalf("resolveImapTarget() error = %v, want stale_reference", err)
	}
}

func TestClientResolvesLabelMailboxUIDByMessageID(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "label-identity@gmail.com")
	updateFixtureMessage(t, store, `UPDATE messages SET remote_id = 77960, remote_mailbox = 7001 WHERE ROWID = 101`)
	location, err := parseMailboxURL("imap://" + testAccountID + "/%5BGmail%5D/All")
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureMailboxInfo(t, store, location, 12345)
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inboxRef, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	ref := messageRefWithSubject(t, page.Messages, "Quarterly Report")
	operator := &stubImapOperator{uid: 41751, uidvalidity: 12345, boxes: []transport.MailboxInfo{{Name: "INBOX"}}}
	client := &Client{store: store, send: mail.SendTransport{
		Imap: operator, Credentials: stubCredentials{"label-identity@gmail.com": "secret"},
	}}
	target, err := client.resolveImapTargetForMutation(context.Background(), ref)
	if err != nil || target.imapMailbox != "INBOX" || target.uid != 41751 || operator.searchCalls != 1 {
		t.Fatalf("label target = %+v, search calls = %d, error = %v", target, operator.searchCalls, err)
	}
	resolvedRef, err := mailref.DecodeMessage(target.summary.Ref)
	if err != nil || resolvedRef.ExpectedIMAPUID != 0 || resolvedRef.ExpectedMessageID != "101@example.com" {
		t.Fatalf("returned label ref = %+v, error = %v", resolvedRef, err)
	}
	reused, err := client.resolveImapTargetForMutation(context.Background(), target.summary.Ref)
	if err != nil || reused.uid != 41751 || operator.searchCalls != 2 {
		t.Fatalf("reused label ref target = %+v, search calls = %d, error = %v", reused, operator.searchCalls, err)
	}
	base, err := store.messageBasePath(location, 101)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(base + ".emlx"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.resolveImapTargetForMutation(context.Background(), ref); transport.ErrorCode(err) != transport.CodeIMAPMessageUIDUnknown {
		t.Fatalf("missing label source error = %v, want unresolved identity", err)
	}
	if operator.searchCalls != 2 {
		t.Fatalf("unexpected search after local identity loss: %d", operator.searchCalls)
	}
}

func TestClientRejectsRenamedIMAPMailboxWithoutGuessing(t *testing.T) {
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "renamed-folder@gmail.com")
	updateFixtureMessage(t, store, `UPDATE messages SET remote_id = 101, remote_mailbox = 7001 WHERE ROWID = 101`)
	location, err := parseMailboxURL("imap://" + testAccountID + "/%5BGmail%5D/All")
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureMailboxInfo(t, store, location, 12345)
	mailboxRef, err := mailref.EncodeMailbox(testAccountID, []string{"All"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: mailboxRef, Limit: 3})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Quarterly Report")
	base, err := store.messageBasePath(location, 101)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(base + ".emlx"); err != nil {
		t.Fatalf("remove local source: %v", err)
	}
	fakeImap := &stubImapOperator{
		raw:   []byte("Subject: Quarterly Report\r\n\r\nRenamed body\r\n"),
		boxes: []transport.MailboxInfo{{Name: "Renamed"}},
	}
	client := &Client{store: store, send: mail.SendTransport{
		Imap: fakeImap, Credentials: stubCredentials{"renamed-folder@gmail.com": "secret"},
	}}
	if _, err := client.GetMessage(context.Background(), messageRef); transport.ErrorCode(err) != transport.CodeIMAPMailboxNotFound {
		t.Fatalf("GetMessage() error = %v, want %s", err, transport.CodeIMAPMailboxNotFound)
	}
	if fakeImap.lastFetchMax != 0 {
		t.Fatalf("FetchMessage max bytes = %d, want no fetch after mailbox rename", fakeImap.lastFetchMax)
	}
}

func TestClientRejectsMetadataOnlyTargetsBeforeRemoteDispatch(t *testing.T) {
	for _, candidateCount := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d matching server candidates", candidateCount), func(t *testing.T) {
			store, inboxRef := newSearchFixture(t)
			closeTestResource(t, store, "identity fixture store")
			installImapIdentityFixture(t, store, "metadata@gmail.com")
			updateFixtureMessage(t, store, `UPDATE messages SET message_id = NULL, remote_id = NULL, remote_mailbox = NULL WHERE ROWID = 102`)
			page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inboxRef, Limit: 3})
			if err != nil {
				t.Fatal(err)
			}
			ref := messageRefWithSubject(t, page.Messages, "Status Update")
			location := mustMailboxLocation(t, "imap://"+testAccountID+"/INBOX")
			base, err := store.messageBasePath(location, 102)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(base + ".emlx"); err != nil {
				t.Fatal(err)
			}
			operator := &countingFetchOperator{stubImapOperator: stubImapOperator{
				raw:           []byte("From: Alice <alice@example.com>\r\nSubject: Status Update\r\nMessage-ID: <other@example.com>\r\n\r\nWrong message"),
				searchMatches: candidateCount,
			}}
			client := &Client{store: store, send: mail.SendTransport{
				Imap: operator, Credentials: stubCredentials{"metadata@gmail.com": "secret"},
			}}
			read := true
			for _, operation := range []struct {
				name string
				call func() error
			}{
				{"read", func() error { _, err := client.GetMessage(context.Background(), ref); return err }},
				{"mark", func() error {
					_, err := client.MarkMessage(context.Background(), mail.MarkMessageRequest{Ref: ref, Read: &read})
					return err
				}},
				{"move", func() error {
					_, err := client.TransferMessage(context.Background(), mail.TransferMessageRequest{Ref: ref, DestinationMailbox: inboxRef})
					return err
				}},
				{"copy", func() error {
					_, err := client.TransferMessage(context.Background(), mail.TransferMessageRequest{Ref: ref, DestinationMailbox: inboxRef, Copy: true})
					return err
				}},
				{"delete", func() error {
					_, err := client.DeleteMessage(context.Background(), mail.DeleteMessageRequest{Ref: ref})
					return err
				}},
			} {
				t.Run(operation.name, func(t *testing.T) {
					if err := operation.call(); transport.ErrorCode(err) != transport.CodeIMAPMessageUIDUnknown {
						t.Fatalf("operation error = %v, want unresolved independent identity", err)
					}
					if operator.fetchCalls != 0 || operator.searchCalls != 0 || operator.listCalls != 0 || operator.mutationCalls != 0 {
						t.Fatalf("remote dispatch: fetch=%d search=%d list=%d mutation=%d",
							operator.fetchCalls, operator.searchCalls, operator.listCalls, operator.mutationCalls)
					}
				})
			}
		})
	}
}

func TestClientRejectsAmbiguousMessageIDDuringReadIdentityResolution(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "search-ambiguous@gmail.com")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inboxRef, Limit: 3})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	messageRef := messageRefWithExpectedID(t,
		messageRefWithSubject(t, page.Messages, "Status Update"), "<status@example.com>")
	fakeImap := &stubImapOperator{
		boxes: []transport.MailboxInfo{{Name: "INBOX"}}, searchMatches: 2,
	}
	client := &Client{store: store, send: mail.SendTransport{
		Imap: fakeImap, Credentials: stubCredentials{"search-ambiguous@gmail.com": "secret"},
	}}
	if _, err := client.resolveImapTarget(context.Background(), messageRef); transport.ErrorCode(err) != transport.CodeIMAPAmbiguousMessageID ||
		!strings.Contains(err.Error(), "refusing identity resolution") || strings.Contains(err.Error(), "refusing mutation") {
		t.Fatalf("resolveImapTarget() error = %v, want read-safe %s wording", err, transport.CodeIMAPAmbiguousMessageID)
	}
	if fakeImap.searchCalls != 1 {
		t.Fatalf("SearchUID calls = %d, want one metadata identity search", fakeImap.searchCalls)
	}
}

type countingFetchOperator struct {
	stubImapOperator
	fetchCalls int
}

func (s *countingFetchOperator) FetchMessage(
	ctx context.Context, cfg transport.ImapConfig, mailbox string, uid uint32, expectedUIDValidity uint32, maxBytes int64,
) ([]byte, error) {
	s.fetchCalls++
	return s.stubImapOperator.FetchMessage(ctx, cfg, mailbox, uid, expectedUIDValidity, maxBytes)
}

func clientForIdentityTest(store *Store, email string) *Client {
	return &Client{store: store, send: mail.SendTransport{
		Imap:        &stubImapOperator{boxes: []transport.MailboxInfo{{Name: "INBOX"}}},
		Credentials: stubCredentials(map[string]string{email: "secret"}),
	}}
}

func writeFixtureMailboxInfo(t *testing.T, store *Store, location mailboxLocation, validity uint32) {
	t.Helper()
	path, err := mailboxInfoPath(store.versionRoot, location)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create mailbox info directory: %v", err)
	}
	source := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>UIDVALIDITY</key><integer>%d</integer></dict></plist>`, validity)
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("write mailbox Info.plist: %v", err)
	}
}
