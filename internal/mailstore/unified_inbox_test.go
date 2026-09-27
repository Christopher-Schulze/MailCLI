package mailstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/cli"
	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

func TestUnifiedInboxCLIUsesRealStoreInOneCall(t *testing.T) {
	fixture := createSearchFixtureData(t, 0, true)
	store, err := Open(context.Background(), Config{MailRoot: fixture.mailRoot, ActiveAccountURLs: fixture.activeAccountURLs})
	if err != nil {
		t.Fatal(err)
	}
	closeTestResource(t, store, "CLI inbox fixture")
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), mail.NewService(&Client{store: store}), []string{"messages", "list", "--json", "--fields", "subject"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var response struct {
		OK   bool `json:"ok"`
		Data struct {
			Page mail.MessagePage `json:"page"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || len(response.Data.Page.Messages) != 3 || store.mailboxCatalogQueries != 1 {
		t.Fatalf("response=%+v catalog queries=%d", response, store.mailboxCatalogQueries)
	}
	for _, item := range response.Data.Page.Messages {
		if item.Account != fixture.accountRef || item.MailboxRef != fixture.inboxRef || item.Ref == "" || item.Subject == "" {
			t.Fatalf("real projected identity incomplete: %+v", item)
		}
	}
}

func TestMailboxAmbiguityEvidenceExcludesUnrelatedRefs(t *testing.T) {
	for _, test := range []struct {
		path  string
		infos []transport.MailboxInfo
	}{
		{path: "archive", infos: []transport.MailboxInfo{{Name: "Archive", WireName: "Archive"}, {Name: "Archiv", WireName: "Archiv"}, {Name: "INBOX", WireName: "INBOX"}}},
		{path: "sent", infos: []transport.MailboxInfo{{Name: "Outbound", WireName: "Outbound", Flags: []string{"\\Sent"}}, {Name: "Outgoing", WireName: "Outgoing", Flags: []string{"\\Sent"}}, {Name: "Gesendet", WireName: "Gesendet"}}},
	} {
		if _, err := transport.ResolveMailboxPath(test.infos, []string{test.path}); transport.ErrorCode(err) != transport.CodeIMAPAmbiguousMailbox {
			t.Fatalf("resolver did not find genuine ambiguity: %v", err)
		}
		mailboxes := []mail.Mailbox{{Ref: "first"}, {Ref: "second"}, {Ref: "unrelated"}}
		candidates := ambiguousListMailboxCandidates(test.infos, mailboxes, []string{test.path})
		if !reflect.DeepEqual(candidates, mailboxes[:2]) {
			t.Fatalf("path=%s candidates=%+v", test.path, candidates)
		}
	}
}

func TestMailboxRoleSelectorsUseMetadataAndExistingAliases(t *testing.T) {
	fixture := createSearchFixtureData(t, 0, true)
	var cache strings.Builder
	cache.WriteString(`<?xml version="1.0"?><plist version="1.0"><dict><key>mboxes</key><dict>`)
	for _, node := range []struct {
		name       string
		attributes int
	}{{"INBOX", 0}, {"Outgoing", mailboxAttributeSent}, {"Work in progress", mailboxAttributeDrafts}, {"Papierkorb", 0}, {"Spam", 0}, {"Projects", 0}} {
		if _, err := fmt.Fprintf(&cache, `<key>%s</key><dict><key>MailboxPathComponent</key><string>%s</string><key>IMAPMailboxAttributes</key><integer>%d</integer><key>IMAPMailboxChildren</key><dict/></dict>`, node.name, node.name, node.attributes); err != nil {
			t.Fatal(err)
		}
	}
	cache.WriteString(`</dict></dict></plist>`)
	if err := os.WriteFile(filepath.Join(fixture.mailRoot, "V10", testAccountID, ".mboxCache.plist"), []byte(cache.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), Config{MailRoot: fixture.mailRoot, ActiveAccountURLs: fixture.activeAccountURLs})
	if err != nil {
		t.Fatal(err)
	}
	closeTestResource(t, store, "role metadata fixture")
	for _, test := range []struct{ selector, want string }{
		{"InBoX", "INBOX"}, {"SeNt", "Outgoing"}, {"DrAfTs", "Work in progress"},
		{"TrAsH", "Papierkorb"}, {"JuNk", "Spam"}, {"ArChIvE", "Archive"}, {"Projects", "Projects"},
	} {
		selected, err := store.selectedListMailboxes(context.Background(), mail.ListMessagesRequest{MailboxRef: test.selector, AccountRef: fixture.accountRef})
		if err != nil || len(selected) != 1 || !reflect.DeepEqual(selected[0].Path, []string{test.want}) {
			t.Fatalf("selector=%s selected=%+v error=%v", test.selector, selected, err)
		}
	}
}

func TestUnifiedInboxPaginationAcrossAccounts(t *testing.T) {
	fixture := createSearchFixtureData(t, 0, true)
	store, err := Open(context.Background(), Config{MailRoot: fixture.mailRoot, ActiveAccountURLs: fixture.activeAccountURLs})
	if err != nil {
		t.Fatal(err)
	}
	closeTestResource(t, store, "unified inbox fixture")
	updateFixtureMessage(t, store, "UPDATE messages SET mailbox=3,date_received=300 WHERE ROWID=102")
	updateFixtureMessage(t, store, "UPDATE messages SET date_received=NULL WHERE ROWID=103")
	updateFixtureMessage(t, store, "INSERT INTO labels(message_id,mailbox_id) VALUES(101,1)")
	var rows []int64
	cursor := ""
	for range 4 {
		page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{Limit: 1, Cursor: cursor})
		if err != nil || len(page.Messages) != 1 {
			t.Fatalf("page=%+v error=%v", page, err)
		}
		item := page.Messages[0]
		identity, err := mailref.DecodeMessage(item.Ref)
		if err != nil {
			t.Fatal(err)
		}
		account, err := mailref.DecodeAccount(item.Account)
		if err != nil || account.AccountID != identity.AccountID {
			t.Fatalf("account=%s identity=%+v error=%v", item.Account, identity, err)
		}
		mailbox, err := mailref.DecodeMailbox(item.MailboxRef)
		if err != nil || mailbox.AccountID != identity.AccountID || !reflect.DeepEqual(mailbox.Path, []string{"INBOX"}) {
			t.Fatalf("mailbox=%+v identity=%+v error=%v", mailbox, identity, err)
		}
		rows = append(rows, identity.ExpectedStoreMessageID)
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if !reflect.DeepEqual(rows, []int64{1002, 1001, 1003}) || cursor != "" {
		t.Fatalf("rows=%v cursor=%s", rows, cursor)
	}
}

func TestMailboxSelectorsAmbiguityScopeAndCursorBinding(t *testing.T) {
	fixture := createSearchFixtureData(t, 0, true)
	store, err := Open(context.Background(), Config{MailRoot: fixture.mailRoot, ActiveAccountURLs: fixture.activeAccountURLs})
	if err != nil {
		t.Fatal(err)
	}
	closeTestResource(t, store, "mailbox selection fixture")
	secondRef, err := mailref.EncodeMailbox(secondaryCatalogAccountID, []string{"INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{"inbox", "INBOX", "InBoX"} {
		_, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: selector, Limit: 20})
		if errorCodeForTest(err) != "ambiguous_mailbox" || !strings.Contains(err.Error(), fixture.inboxRef) || !strings.Contains(err.Error(), secondRef) {
			t.Fatalf("selector=%s error=%v", selector, err)
		}
		page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: selector, AccountRef: fixture.accountRef, Limit: 20})
		if err != nil || len(page.Messages) != 3 {
			t.Fatalf("scoped selector=%s page=%+v error=%v", selector, page, err)
		}
	}
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{Limit: 1})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("page=%+v error=%v", page, err)
	}
	for _, request := range []mail.ListMessagesRequest{
		{AccountRef: fixture.accountRef, Cursor: page.NextCursor, Limit: 1},
		{MailboxRef: fixture.inboxRef, Cursor: page.NextCursor, Limit: 1},
	} {
		if _, err := store.ListMessages(context.Background(), request); errorCodeForTest(err) != "invalid_cursor" {
			t.Fatalf("request=%+v error=%v", request, err)
		}
	}
}

func TestUnifiedInboxEmptyAndExactPath(t *testing.T) {
	fixture := createSearchFixtureData(t, 0, true)
	store, err := Open(context.Background(), Config{MailRoot: fixture.mailRoot, ActiveAccountURLs: fixture.activeAccountURLs})
	if err != nil {
		t.Fatal(err)
	}
	closeTestResource(t, store, "empty inbox fixture")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: "Archive", Limit: 20})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Subject != "Quarterly Report" {
		t.Fatalf("exact path page=%+v error=%v", page, err)
	}
	secondAccount, err := mailref.EncodeAccount(secondaryCatalogAccountID)
	if err != nil {
		t.Fatal(err)
	}
	page, err = store.ListMessages(context.Background(), mail.ListMessagesRequest{AccountRef: secondAccount, Limit: 20})
	if err != nil || page.Messages == nil || len(page.Messages) != 0 || page.NextCursor != "" {
		t.Fatalf("empty inbox page=%+v error=%v", page, err)
	}
	if _, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: fixture.inboxRef, AccountRef: secondAccount, Limit: 20}); errorCodeForTest(err) != "invalid_argument" {
		t.Fatalf("cross-account ref error=%v", err)
	}
}

type inboxFallbackSpy struct {
	fallbackSpy
	listCalls int
}

func (s *inboxFallbackSpy) ListMessages(context.Context, mail.ListMessagesRequest) (mail.MessagePage, error) {
	s.listCalls++
	return mail.MessagePage{}, nil
}

func TestUnifiedInboxUnavailableNeverScansFallback(t *testing.T) {
	for _, request := range []mail.ListMessagesRequest{
		{Limit: 20}, {MailboxRef: "inbox", Limit: 20}, {MailboxRef: "Projects/Reports", Limit: 20},
		{MailboxRef: "mbx_reference", AccountRef: "account-reference", Limit: 20},
	} {
		fallback := &inboxFallbackSpy{}
		client := &Client{fallback: fallback, storeErr: operationError("mail_store_unavailable", "fixture unavailable")}
		_, err := client.ListMessages(context.Background(), request)
		if errorCodeForTest(err) != "safe_message_listing_unavailable" || fallback.listCalls != 0 {
			t.Fatalf("request=%+v calls=%d error=%v", request, fallback.listCalls, err)
		}
	}
}

func TestUnifiedInboxCursorRejectsChangedActiveAccounts(t *testing.T) {
	fixture := createSearchFixtureData(t, 0, true)
	store, err := Open(context.Background(), Config{MailRoot: fixture.mailRoot, ActiveAccountURLs: fixture.activeAccountURLs})
	if err != nil {
		t.Fatal(err)
	}
	closeTestResource(t, store, "complete inbox fixture")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{Limit: 1})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("page=%+v error=%v", page, err)
	}
	narrowed, err := Open(context.Background(), Config{MailRoot: fixture.mailRoot, ActiveAccountURLs: fixture.activeAccountURLs[:1]})
	if err != nil {
		t.Fatal(err)
	}
	closeTestResource(t, narrowed, "narrowed inbox fixture")
	if _, err := narrowed.ListMessages(context.Background(), mail.ListMessagesRequest{Limit: 1, Cursor: page.NextCursor}); errorCodeForTest(err) != "invalid_cursor" {
		t.Fatalf("changed active-account cursor error=%v", err)
	}
}
