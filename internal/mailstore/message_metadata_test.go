package mailstore

import (
	"context"
	"fmt"
	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
	"os"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestExcerptMIMEFixtures(t *testing.T) {
	for _, test := range []struct {
		name, source, want string
		complete           bool
	}{
		{"plain signature", "Content-Type: text/plain\r\n\r\nHello\r\n> quoted\r\n-- \r\nSignature", "Hello", true},
		{"html only", "Content-Type: text/html; charset=utf-8\r\n\r\n<p>Hello <b>world</b></p>", "Hello world", true},
		{"plain preferred", "Content-Type: multipart/alternative; boundary=x\r\n\r\n--x\r\nContent-Type: text/html\r\n\r\n<p>HTML</p>\r\n--x\r\nContent-Type: text/plain\r\n\r\nPlain\r\n--x--\r\n", "Plain", true},
		{"invalid MIME", "no header boundary", "", false},
		{"iso-8859-1", "Content-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nGr=FC=DFe aus M=FCnchen\r\n", "Grüße aus München", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			text, complete := excerptText(context.Background(), []byte(test.source))
			got := mail.BuildExcerpt(text, 240)
			if got != test.want || complete != test.complete {
				t.Fatalf("excerpt=%q complete=%t", got, complete)
			}
		})
	}
}

func enrichOne(client *Client, ref string, request mail.MessageEnrichmentRequest) (mail.MessageSummary, error) {
	summaries, err := client.EnrichMessages(context.Background(), []string{ref}, request)
	if err != nil {
		return mail.MessageSummary{}, err
	}
	return summaries[0], nil
}

// excerptBatchOperator resolves UIDs by subject and serves excerpt sources for
// the UIDs in sources; every other requested UID is not returned.
type excerptBatchOperator struct {
	*metadataResolverStub
	uidsBySubject map[string]uint32
	sources       map[uint32]string
	fetchErr      error
	fetchUIDs     [][]uint32
	fetchMailbox  []string
	fetchLimit    int64
	fetchValidity []uint32
}

func (operator *excerptBatchOperator) ResolveMessageIdentity(
	_ context.Context, _ transport.ImapConfig, _ string, hint transport.MessageIdentityHint,
) (transport.MessageIdentity, error) {
	operator.calls++
	uid := operator.uidsBySubject[hint.Subject]
	return transport.MessageIdentity{UID: uid, UIDValidity: 12345, MessageID: fmt.Sprintf("<%d@example.com>", uid)}, nil
}

func (operator *excerptBatchOperator) FetchMessageExcerpts(
	_ context.Context, _ transport.ImapConfig, mailbox string, expectedUIDValidity uint32, uids []uint32, maxTextBytes int64,
) (map[uint32]transport.MessageExcerptSource, error) {
	operator.fetchValidity = append(operator.fetchValidity, expectedUIDValidity)
	operator.fetchUIDs = append(operator.fetchUIDs, append([]uint32(nil), uids...))
	operator.fetchMailbox = append(operator.fetchMailbox, mailbox)
	operator.fetchLimit = maxTextBytes
	if operator.fetchErr != nil {
		return nil, operator.fetchErr
	}
	results := make(map[uint32]transport.MessageExcerptSource)
	for _, uid := range uids {
		if source, found := operator.sources[uid]; found {
			results[uid] = transport.MessageExcerptSource{Source: []byte(source), Complete: true}
		}
	}
	return results, nil
}

// newExcerptBatchFixture removes the local sources of the two INBOX messages
// so their excerpts need IMAP, and returns refs for the INBOX rows plus the
// locally complete archive row.
func newExcerptBatchFixture(t *testing.T, operator *excerptBatchOperator) (*Client, []string) {
	t.Helper()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "excerpt batch fixture store")
	installImapIdentityFixture(t, store, "metadata@gmail.com")
	updateFixtureMessage(t, store, `UPDATE messages SET message_id = NULL, remote_id = NULL, remote_mailbox = NULL WHERE ROWID IN (102, 103)`)
	location, err := parseMailboxURL("imap://" + testAccountID + "/INBOX")
	if err != nil {
		t.Fatal(err)
	}
	for _, rowID := range []int64{102, 103} {
		base, err := store.messageBasePath(location, rowID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(base + ".emlx"); err != nil {
			t.Fatalf("remove local source %d: %v", rowID, err)
		}
	}
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inboxRef, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	archiveRef, err := mailref.EncodeMailbox(testAccountID, []string{"All"})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: archiveRef, Limit: 1})
	if err != nil || len(archive.Messages) != 1 {
		t.Fatalf("archive rows=%d err=%v", len(archive.Messages), err)
	}
	operator.metadataResolverStub = &metadataResolverStub{stubImapOperator: stubImapOperator{boxes: []transport.MailboxInfo{{Name: "INBOX"}, {Name: "[Gmail]/All"}}}}
	client := &Client{store: store, send: mail.SendTransport{Imap: operator, Credentials: stubCredentials{"metadata@gmail.com": "secret"}}}
	refs := []string{
		messageRefWithSubject(t, page.Messages, "Status Update"),
		archive.Messages[0].Ref,
		messageRefWithSubject(t, page.Messages, "Noise"),
	}
	return client, refs
}

func TestEnrichMessagesFetchesIMAPExcerptsOncePerMailbox(t *testing.T) {
	operator := &excerptBatchOperator{
		uidsBySubject: map[string]uint32{"Status Update": 202, "Noise": 203},
		sources:       map[uint32]string{202: "Content-Type: text/plain\r\n\r\nRemote status text"},
	}
	client, refs := newExcerptBatchFixture(t, operator)
	summaries, err := client.EnrichMessages(context.Background(), refs, mail.MessageEnrichmentRequest{Excerpt: true, ExcerptLength: 240})
	if err != nil {
		t.Fatal(err)
	}
	if len(operator.fetchUIDs) != 1 || !slices.Equal(operator.fetchUIDs[0], []uint32{202, 203}) ||
		operator.fetchMailbox[0] != "INBOX" || operator.fetchLimit != mail.IMAPExcerptTextBytes {
		t.Fatalf("fetches=%v mailboxes=%v limit=%d, want one INBOX fetch of 202,203", operator.fetchUIDs, operator.fetchMailbox, operator.fetchLimit)
	}
	remote, local, missing := summaries[0], summaries[1], summaries[2]
	if remote.ExcerptSource != mail.ExcerptSourceIMAPPartial || remote.Excerpt != "Remote status text" || !remote.ExcerptComplete || remote.EnrichmentError != "" {
		t.Fatalf("remote row = %+v", remote)
	}
	if local.ExcerptSource != mail.ExcerptSourceLocal || local.Excerpt == "" || local.EnrichmentError != "" {
		t.Fatalf("local row = %+v", local)
	}
	if missing.ExcerptSource != mail.ExcerptSourceUnavailable || missing.Excerpt != "" || missing.EnrichmentError != transport.CodeIMAPMessageNotFound {
		t.Fatalf("row the server did not return = %+v", missing)
	}
}

// TestEnrichMessagesUsesLocalUIDsWithoutServerSearch covers mailboxes whose
// local Info.plist has no UIDVALIDITY: the local UID is fetched directly, the
// Message-ID is verified instead of searched, and LIST runs once per account.
func TestEnrichMessagesUsesLocalUIDsWithoutServerSearch(t *testing.T) {
	operator := &excerptBatchOperator{sources: map[uint32]string{
		202: "Message-ID: <local-102@example.com>\r\nContent-Type: text/plain\r\n\r\nVerified text",
		203: "Message-ID: <other@example.com>\r\nContent-Type: text/plain\r\n\r\nWrong message",
	}}
	client, refs := newExcerptBatchFixture(t, operator)
	updateFixtureMessage(t, client.store, `UPDATE messages SET remote_id = ROWID + 100 WHERE ROWID IN (102, 103)`)
	location, err := parseMailboxURL("imap://" + testAccountID + "/INBOX")
	if err != nil {
		t.Fatal(err)
	}
	for _, rowID := range []int64{102, 103} {
		base, err := client.store.messageBasePath(location, rowID)
		if err != nil {
			t.Fatal(err)
		}
		headers := []byte(fmt.Sprintf("Message-ID: <local-%d@example.com>\r\nSubject: partial\r\n\r\n", rowID))
		framed := append([]byte(fmt.Sprintf("%-10d\n", len(headers))), headers...)
		if err := os.WriteFile(base+".partial.emlx", append(framed, validPlistTrailer()...), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inboxRef, err := mailref.EncodeMailbox(testAccountID, []string{"INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inboxRef, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	refs[0], refs[2] = messageRefWithSubject(t, page.Messages, "Status Update"), messageRefWithSubject(t, page.Messages, "Noise")
	summaries, err := client.EnrichMessages(context.Background(), refs, mail.MessageEnrichmentRequest{Excerpt: true, ExcerptLength: 240})
	if err != nil {
		t.Fatal(err)
	}
	if operator.searchCalls != 0 || operator.calls != 0 || operator.listCalls != 1 {
		t.Fatalf("server searches=%d resolver calls=%d LIST calls=%d, want 0, 0, 1", operator.searchCalls, operator.calls, operator.listCalls)
	}
	if len(operator.fetchUIDs) != 1 || !slices.Equal(operator.fetchUIDs[0], []uint32{202, 203}) || operator.fetchValidity[0] != 0 {
		t.Fatalf("fetches=%v validity=%v, want one unverified fetch of 202,203", operator.fetchUIDs, operator.fetchValidity)
	}
	if summaries[0].ExcerptSource != mail.ExcerptSourceIMAPPartial || summaries[0].Excerpt != "Verified text" || summaries[0].EnrichmentError != "" {
		t.Fatalf("verified row = %+v", summaries[0])
	}
	if summaries[2].ExcerptSource != mail.ExcerptSourceLocal || strings.Contains(summaries[2].Excerpt, "Wrong") || summaries[2].EnrichmentError != transport.CodeIMAPMessageUIDMismatch {
		t.Fatalf("mismatched row = %+v", summaries[2])
	}
}

func TestEnrichMessagesReportsAFailedExcerptFetchPerRow(t *testing.T) {
	operator := &excerptBatchOperator{
		uidsBySubject: map[string]uint32{"Status Update": 202, "Noise": 203},
		fetchErr:      &transport.TransportError{Code: transport.CodeIMAPTimeout, Message: "timeout"},
	}
	client, refs := newExcerptBatchFixture(t, operator)
	summaries, err := client.EnrichMessages(context.Background(), refs, mail.MessageEnrichmentRequest{Excerpt: true, ExcerptLength: 240})
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{0, 2} {
		if summaries[index].ExcerptSource != mail.ExcerptSourceUnavailable || summaries[index].EnrichmentError != transport.CodeIMAPTimeout {
			t.Fatalf("row %d = %+v", index, summaries[index])
		}
	}
	if summaries[1].ExcerptSource != mail.ExcerptSourceLocal || summaries[1].EnrichmentError != "" {
		t.Fatalf("local row = %+v", summaries[1])
	}
}

func TestExcerptSourceCapAndThreadingRead(t *testing.T) {
	store, ref, _ := newLargeReadIntentFixture(t, 512<<10)
	metrics := &readMetrics{}
	store.readMetrics = metrics
	client := &Client{store: store}
	result, err := enrichOne(client, ref, mail.MessageEnrichmentRequest{Excerpt: true, ExcerptLength: 13})
	if err != nil || result.ExcerptSource != mail.ExcerptSourceLocal || result.ExcerptComplete || utf8.RuneCountInString(result.Excerpt) > 13 || metrics.sourceBytes.Load() > mail.MaximumExcerptSourceBytes {
		t.Fatalf("metadata=%+v bytes=%d err=%v", result, metrics.sourceBytes.Load(), err)
	}
	metrics.sourceBytes.Store(0)
	result, err = enrichOne(client, ref, mail.MessageEnrichmentRequest{Threading: true, ExcerptLength: 240})
	if err != nil || !result.ThreadingComplete || metrics.sourceBytes.Load() > maximumHeaderBytes || result.Excerpt != "" {
		t.Fatalf("threading=%+v bytes=%d err=%v", result, metrics.sourceBytes.Load(), err)
	}
}

func TestExcerptUsesCompleteLargeLocalSourceWithoutIMAP(t *testing.T) {
	store, ref, _ := newLargeReadIntentFixture(t, 512<<10)
	data, complete, localPartial, err := store.readExcerptSource(context.Background(), ref)
	if err != nil || complete || localPartial || int64(len(data)) != mail.MaximumExcerptSourceBytes {
		t.Fatalf("bytes=%d complete=%t partial=%t err=%v", len(data), complete, localPartial, err)
	}
	if excerptNeedsRemote(err, localPartial) {
		t.Fatal("a complete local source larger than the cap requested an IMAP partial fetch")
	}
	if !excerptNeedsRemote(nil, true) {
		t.Fatal("a partial local source did not allow the bounded IMAP prefix")
	}
	if excerptNeedsRemote(context.Canceled, false) {
		t.Fatal("an unsafe local error allowed an IMAP fetch")
	}
}

func TestEnrichmentReportsTheFailureInsteadOfHidingIt(t *testing.T) {
	store, _, _ := newLargeReadIntentFixture(t, 1024)
	client := &Client{store: store}
	for _, request := range []mail.MessageEnrichmentRequest{
		{Threading: true, ExcerptLength: 240},
		{Excerpt: true, ExcerptLength: 240},
	} {
		result, err := enrichOne(client, "msg_not-a-reference", request)
		if err != nil || result.EnrichmentError == "" || result.ThreadingComplete || result.Excerpt != "" {
			t.Fatalf("request=%+v result=%+v err=%v", request, result, err)
		}
	}
}

func TestExcerptDecodedTextCap(t *testing.T) {
	text, complete := excerptText(context.Background(), []byte("Content-Type: text/plain\r\n\r\n"+strings.Repeat("x", int(mail.MaximumExcerptSourceBytes))))
	if complete || int64(len(text)) > mail.MaximumExcerptSourceBytes {
		t.Fatalf("text bytes=%d complete=%t", len(text), complete)
	}
}
