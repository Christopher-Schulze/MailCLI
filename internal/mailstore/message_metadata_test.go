package mailstore

import (
	"context"
	"fmt"
	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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
	client, refs := newLocalUIDExcerptFixture(t, operator)
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

// newLocalUIDExcerptFixture gives the two INBOX rows local server UIDs 202
// and 203 and header-only partial sources, so their excerpts need IMAP while
// their identity resolves locally.
func newLocalUIDExcerptFixture(t *testing.T, operator *excerptBatchOperator) (*Client, []string) {
	t.Helper()
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
	return client, refs
}

func TestEnrichMessagesReusesCachedIMAPExcerpts(t *testing.T) {
	operator := &excerptBatchOperator{sources: map[uint32]string{
		202: "Message-ID: <local-102@example.com>\r\nContent-Type: text/plain\r\n\r\nCached remote text",
		203: "Message-ID: <local-103@example.com>\r\nContent-Type: text/plain\r\n\r\nSecond remote text",
	}}
	client, refs := newLocalUIDExcerptFixture(t, operator)
	client.excerpts = excerptCache{dir: t.TempDir()}
	first, err := client.EnrichMessages(context.Background(), refs, mail.MessageEnrichmentRequest{Excerpt: true, ExcerptLength: 240})
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.EnrichMessages(context.Background(), refs, mail.MessageEnrichmentRequest{Excerpt: true, ExcerptLength: 6})
	if err != nil {
		t.Fatal(err)
	}
	if len(operator.fetchUIDs) != 1 || operator.listCalls != 1 {
		t.Fatalf("fetches=%v LIST calls=%d, want the second page served without server contact", operator.fetchUIDs, operator.listCalls)
	}
	for _, index := range []int{0, 2} {
		want := mail.BuildExcerpt(first[index].Excerpt, 6)
		if first[index].ExcerptSource != mail.ExcerptSourceIMAPPartial || second[index].ExcerptSource != mail.ExcerptSourceIMAPPartial ||
			second[index].Excerpt != want || second[index].ExcerptComplete != first[index].ExcerptComplete || second[index].EnrichmentError != "" {
			t.Fatalf("row %d first=%+v second=%+v want excerpt %q", index, first[index], second[index], want)
		}
	}
	if second[1].ExcerptSource != mail.ExcerptSourceLocal {
		t.Fatalf("local row = %+v", second[1])
	}
}

func TestExcerptCacheIgnoresExpiredAndOversizedEntries(t *testing.T) {
	cache := excerptCache{dir: t.TempDir()}
	if err := cache.store("fresh", cachedExcerpt{Excerpt: "text", Complete: true}); err != nil {
		t.Fatal(err)
	}
	if entry, hit := cache.load("fresh"); !hit || entry.Excerpt != "text" || !entry.Complete {
		t.Fatalf("fresh entry = %+v hit=%t", entry, hit)
	}
	expired := time.Now().Add(-excerptCacheTTL - time.Hour)
	if err := os.Chtimes(cache.path("fresh"), expired, expired); err != nil {
		t.Fatal(err)
	}
	if _, hit := cache.load("fresh"); hit {
		t.Fatal("an expired entry was served")
	}
	if err := os.WriteFile(cache.path("large"), []byte(`{"excerpt":"`+strings.Repeat("x", maximumExcerptCacheEntryBytes)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, hit := cache.load("large"); hit {
		t.Fatal("an oversized entry was served")
	}
	if _, hit := (excerptCache{}).load("fresh"); hit {
		t.Fatal("a disabled cache served an entry")
	}
}

func TestExcerptCacheRejectsForeignAndUnversionedEntries(t *testing.T) {
	cache := excerptCache{dir: t.TempDir()}
	for name, content := range map[string]string{
		"empty object":  `{}`,
		"unversioned":   `{"excerpt":"legacy text","complete":true}`,
		"other version": `{"v":2,"excerpt":"future text","complete":true}`,
	} {
		if err := os.WriteFile(cache.path(name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if entry, hit := cache.load(name); hit {
			t.Errorf("%s was served as %+v", name, entry)
		}
	}
	if err := cache.store("legacy", cachedExcerpt{Excerpt: "new", Complete: true}); err != nil {
		t.Fatal(err)
	}
	if entry, hit := cache.load("legacy"); !hit || entry.Excerpt != "new" || !entry.Complete {
		t.Fatalf("stored entry = %+v hit=%t", entry, hit)
	}
}

func TestExcerptCachePruneMarkerNeverFollowsASymlink(t *testing.T) {
	cache := excerptCache{dir: t.TempDir()}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("victim content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(cache.dir, excerptCachePruneMarker)); err != nil {
		t.Fatal(err)
	}
	if err := cache.store("key", cachedExcerpt{Excerpt: "text", Complete: true}); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(victim); err != nil || string(content) != "victim content" {
		t.Fatalf("victim = %q, %v; the marker write followed the symlink", content, err)
	}
	info, err := os.Lstat(filepath.Join(cache.dir, excerptCachePruneMarker))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("marker = %v, %v; want a regular file", info, err)
	}
}

func TestExcerptCacheKeyBindsTheServerMailboxGeneration(t *testing.T) {
	base := excerptCacheIdentity{
		storeUUID: "store", accountID: "account", mailboxPath: []string{"INBOX"}, rowID: 7, storeGlobalID: 70, remoteID: 5, uidValidity: 111,
	}
	same, other := base, base
	other.uidValidity = 222
	unknown := base
	unknown.uidValidity = 0
	if base.key() != same.key() || base.key() == other.key() || base.key() == unknown.key() {
		t.Fatalf("keys equal: same=%t other=%t unknown=%t; want true, false, false",
			same.key() == base.key(), other.key() == base.key(), unknown.key() == base.key())
	}
}

func TestCutExcerptMatchesBuildExcerpt(t *testing.T) {
	text := "Grüße  aus\r\n> quoted\r\nMünchen und   mehr Text"
	full := mail.BuildExcerpt(text, mail.MaximumExcerptLength)
	for length := 0; length <= utf8.RuneCountInString(full)+1; length++ {
		if got, want := cutExcerpt(full, length), mail.BuildExcerpt(text, length); got != want {
			t.Fatalf("length %d: cut=%q build=%q", length, got, want)
		}
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

func TestEnrichMessagesStopsExcerptsAtTheByteBudgetButKeepsThreading(t *testing.T) {
	store, ref, _ := newLargeReadIntentFixture(t, 512<<10)
	metrics := &readMetrics{}
	store.readMetrics = metrics
	client := &Client{store: store}
	refs := slices.Repeat([]string{ref}, 10)
	request := mail.MessageEnrichmentRequest{
		Threading: true, Excerpt: true, ExcerptLength: 40, ExcerptSourceBudget: 2 * mail.MaximumExcerptSourceBytes,
	}
	summaries, err := client.EnrichMessages(context.Background(), refs, request)
	if err != nil {
		t.Fatal(err)
	}
	for index, summary := range summaries {
		if !summary.ThreadingComplete {
			t.Fatalf("row %d lost its threading data: %+v", index, summary)
		}
		excerpted := summary.ExcerptSource == mail.ExcerptSourceLocal && summary.EnrichmentError == ""
		skipped := summary.ExcerptSource != mail.ExcerptSourceLocal && summary.Excerpt == "" &&
			summary.EnrichmentError == mail.EnrichmentBudgetExhausted
		if (index < enrichmentConcurrency && !excerpted) || (index >= enrichmentConcurrency && !skipped) {
			t.Fatalf("row %d = %+v; the first chunk is read, later rows stop at the budget", index, summary)
		}
	}
	limit := int64(enrichmentConcurrency)*mail.MaximumExcerptSourceBytes + int64(len(refs))*maximumHeaderBytes
	if metrics.sourceBytes.Load() > limit {
		t.Fatalf("read %d source bytes, want at most %d", metrics.sourceBytes.Load(), limit)
	}
}

func TestExcerptSourceChargeCountsRealBytes(t *testing.T) {
	local := excerptInput{data: make([]byte, 1000), source: mail.ExcerptSourceLocal}
	cached := excerptInput{cached: true, excerpt: "cached", source: mail.ExcerptSourceIMAPPartial}
	for _, test := range []struct {
		name        string
		input       excerptInput
		needsRemote bool
		want        int64
	}{
		{"local source counts its bytes", local, false, 1000},
		{"cache hit costs nothing", cached, false, 0},
		{"planned IMAP fetch counts its bound", excerptInput{}, true, mail.IMAPExcerptTextBytes},
		{"unavailable source costs nothing", excerptInput{source: mail.ExcerptSourceUnavailable}, false, 0},
	} {
		if got := excerptSourceCharge(test.input, test.needsRemote); got != test.want {
			t.Errorf("%s: charge = %d, want %d", test.name, got, test.want)
		}
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
