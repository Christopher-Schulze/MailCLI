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

func TestParseMailboxInfoRejectsMissingOrInvalidUIDValidity(t *testing.T) {
	t.Parallel()
	for _, source := range []string{
		`<plist><dict><key>UIDNEXT</key><integer>4</integer></dict></plist>`,
		`<plist><dict><key>UIDVALIDITY</key><integer>0</integer></dict></plist>`,
	} {
		if _, err := parseMailboxInfoXML(strings.NewReader(source)); err == nil {
			t.Fatalf("parseMailboxInfoXML(%q) error = nil", source)
		}
	}
}

func TestClientHydratesMissingSourceFromMappedIMAPIdentity(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "mapped@gmail.com")
	updateFixtureMessage(t, store, `UPDATE messages SET remote_id = 101, remote_mailbox = 7001 WHERE ROWID = 101`)
	location, err := parseMailboxURL("imap://" + testAccountID + "/%5BGmail%5D/All")
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureMailboxInfo(t, store, location, 12345)
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inboxRef, Limit: 3})
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
		boxes: []transport.MailboxInfo{{Name: "INBOX"}},
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
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "mapped-stale@gmail.com")
	updateFixtureMessage(t, store, `UPDATE messages SET remote_id = 101, remote_mailbox = 7001 WHERE ROWID = 101`)
	location, err := parseMailboxURL("imap://" + testAccountID + "/%5BGmail%5D/All")
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureMailboxInfo(t, store, location, 99999)
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inboxRef, Limit: 3})
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

func TestClientHydratesMissingSourceThroughBoundedMetadataResolver(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "metadata@gmail.com")
	updateFixtureMessage(t, store, `UPDATE messages SET message_id = NULL, remote_id = NULL, remote_mailbox = NULL WHERE ROWID = 102`)
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inboxRef, Limit: 3})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Status Update")
	location, err := parseMailboxURL("imap://" + testAccountID + "/INBOX")
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.messageBasePath(location, 102)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(base + ".emlx"); err != nil {
		t.Fatalf("remove local source: %v", err)
	}
	resolver := &metadataResolverStub{
		stubImapOperator: stubImapOperator{
			raw:   []byte("From: Alice <alice@example.com>\r\nSubject: Status Update\r\nMessage-ID: <remote-102@example.com>\r\n\r\nMetadata body\r\n"),
			boxes: []transport.MailboxInfo{{Name: "INBOX"}},
		},
		identity: transport.MessageIdentity{UID: 202, UIDValidity: 12345, MessageID: "<remote-102@example.com>"},
	}
	client := &Client{store: store, send: mail.SendTransport{
		Imap: resolver, Credentials: stubCredentials{"metadata@gmail.com": "secret"},
	}}
	message, err := client.GetMessage(context.Background(), messageRef)
	if err != nil || message.Content != "Metadata body" {
		t.Fatalf("GetMessage() = %+v, error = %v", message, err)
	}
	if resolver.calls != 1 || resolver.searchCalls != 0 {
		t.Fatalf("metadata resolver calls = %d, Message-ID searches = %d", resolver.calls, resolver.searchCalls)
	}
	ref, err := mailref.DecodeMessage(message.Summary.Ref)
	if err != nil || ref.ExpectedIMAPUID != 202 || ref.ExpectedIMAPUIDValidity != 12345 || ref.ExpectedMessageID != "<remote-102@example.com>" {
		t.Fatalf("resolved summary ref = %+v, error = %v", ref, err)
	}
}

func TestClientRejectsAmbiguousMetadataIdentity(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "metadata-ambiguous@gmail.com")
	updateFixtureMessage(t, store, `UPDATE messages SET message_id = NULL, remote_id = NULL, remote_mailbox = NULL WHERE ROWID = 102`)
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inboxRef, Limit: 3})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Status Update")
	location, err := parseMailboxURL("imap://" + testAccountID + "/INBOX")
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.messageBasePath(location, 102)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(base + ".emlx"); err != nil {
		t.Fatalf("remove local source: %v", err)
	}
	resolver := &metadataResolverStub{
		stubImapOperator: stubImapOperator{boxes: []transport.MailboxInfo{{Name: "INBOX"}}},
		err: &transport.TransportError{
			Code:    transport.CodeIMAPAmbiguousMessageID,
			Message: "metadata matched 2 IMAP messages",
		},
	}
	client := &Client{store: store, send: mail.SendTransport{
		Imap: resolver, Credentials: stubCredentials{"metadata-ambiguous@gmail.com": "secret"},
	}}
	if _, err := client.resolveImapTarget(context.Background(), messageRef); transport.ErrorCode(err) != transport.CodeIMAPAmbiguousMessageID {
		t.Fatalf("resolveImapTarget() error = %v, want %s", err, transport.CodeIMAPAmbiguousMessageID)
	}
	if resolver.fetchCalls != 0 {
		t.Fatalf("FetchMessage calls = %d, want 0 for ambiguous metadata", resolver.fetchCalls)
	}
}

type metadataResolverStub struct {
	stubImapOperator
	identity   transport.MessageIdentity
	err        error
	calls      int
	fetchCalls int
}

func (s *metadataResolverStub) ResolveMessageIdentity(
	_ context.Context, _ transport.ImapConfig, _ string, _ transport.MessageIdentityHint,
) (transport.MessageIdentity, error) {
	s.calls++
	return s.identity, s.err
}

func (s *metadataResolverStub) FetchMessage(
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
