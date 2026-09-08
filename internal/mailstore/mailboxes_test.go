package mailstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

// The mailbox catalog is memoized per Store: repeated catalog consumers
// (ListMailboxes, ListMessages, membership checks, accounts) share one
// mailboxes-table scan.
func TestMailboxRecordsMemoizedPerStore(t *testing.T) {
	t.Parallel()
	store, _ := newSearchFixture(t)
	defer closeTestResource(t, store, "test store")
	ctx := context.Background()

	records, err := store.mailboxRecords(ctx)
	if err != nil {
		t.Fatalf("mailboxRecords: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("mailboxRecords returned no records")
	}
	if store.mailboxCatalogQueries != 1 {
		t.Fatalf("after first call: queries = %d, want 1", store.mailboxCatalogQueries)
	}
	again, err := store.mailboxRecords(ctx)
	if err != nil {
		t.Fatalf("second mailboxRecords: %v", err)
	}
	if len(again) != len(records) {
		t.Fatalf("memoized records = %d, want %d", len(again), len(records))
	}
	if store.mailboxCatalogQueries != 1 {
		t.Fatalf("after second call: queries = %d, want 1 (memoized)", store.mailboxCatalogQueries)
	}
}
func TestListMailboxesRejectsIncompleteCatalog(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content []byte
	}{
		{name: "missing cache"},
		{name: "invalid cache", content: []byte("not a plist")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store, _ := newSearchFixture(t)
			closeTestResource(t, store, "test store")
			if test.content != nil {
				accountRoot := filepath.Join(store.versionRoot, testAccountID)
				if err := os.MkdirAll(accountRoot, 0o700); err != nil {
					t.Fatalf("MkdirAll() error = %v", err)
				}
				if err := os.WriteFile(
					filepath.Join(accountRoot, ".mboxCache.plist"), test.content, 0o600,
				); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
			}
			_, err := store.ListMailboxes(context.Background(), mail.ListMailboxesRequest{})
			if errorCodeForTest(err) != "mailbox_catalog_incomplete" {
				t.Fatalf("ListMailboxes() error = %v, want mailbox_catalog_incomplete", err)
			}
		})
	}
}

func TestLoadMailboxCacheBoundsUseTypedMalformedError(t *testing.T) {
	store, _ := newSearchFixture(t)
	defer closeTestResource(t, store, "test store")
	path := filepath.Join(store.versionRoot, testAccountID, ".mboxCache.plist")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	for _, size := range []int64{0, maximumMailboxCacheBytes + 1} {
		if err := os.Truncate(path, size); err != nil {
			t.Fatalf("Truncate(%d) error = %v", size, err)
		}
		_, err := store.loadMailboxCache(context.Background(), testAccountID)
		if errorCodeForTest(err) != "mailbox_cache_malformed" {
			t.Fatalf("loadMailboxCache(size=%d) error = %v, want mailbox_cache_malformed", size, err)
		}
	}
}

func TestParseMailboxCacheXML(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		source   string
		wantErr  bool
		wantCode string
	}{
		{
			name: "nested catalog",
			source: `<?xml version="1.0"?><plist version="1.0"><dict><key>mboxes</key><dict>` +
				`<key>root</key><dict><key>MailboxPathComponent</key><string>Inbox</string>` +
				`<key>MailboxUnreadCount</key><integer>3</integer><key>IMAPMailboxAttributes</key><integer>8</integer>` +
				`<key>IMAPMailboxChildren</key><dict><key>child</key><dict>` +
				`<key>MailboxPathComponent</key><string>Nested</string><key>IMAPMailboxChildren</key><dict/>` +
				`</dict></dict></dict></dict></dict></plist>`,
		},
		{
			name: "whitespace around document",
			source: " \n\t" + `<?xml version="1.0"?><plist><dict><key>mboxes</key><dict>` +
				`<key>root</key><dict><key>MailboxPathComponent</key><string>Inbox</string>` +
				`<key>MailboxUnreadCount</key><integer>3</integer><key>IMAPMailboxAttributes</key><integer>8</integer>` +
				`<key>IMAPMailboxChildren</key><dict><key>child</key><dict>` +
				`<key>MailboxPathComponent</key><string>Nested</string><key>IMAPMailboxChildren</key><dict/>` +
				`</dict></dict></dict></dict></dict></plist>` + "\n \t",
		},
		{name: "wrong integer type", source: `<?xml version="1.0"?><plist><dict><key>mboxes</key><dict>` +
			`<key>root</key><dict><key>MailboxUnreadCount</key><string>3</string></dict>` +
			`</dict></dict></plist>`, wantErr: true, wantCode: "mailbox_cache_malformed"},
		{name: "duplicate root", source: `<?xml version="1.0"?><plist><dict>` +
			`<key>mboxes</key><dict><key>a</key><dict/></dict>` +
			`<key>mboxes</key><dict><key>b</key><dict/></dict>` +
			`</dict></plist>`, wantErr: true, wantCode: "mailbox_cache_malformed"},
		{name: "trailing text", source: `<?xml version="1.0"?><plist><dict><key>mboxes</key><dict>` +
			`<key>root</key><dict/></dict></dict></plist>trailing`, wantErr: true, wantCode: "mailbox_cache_malformed"},
		{name: "trailing element", source: `<?xml version="1.0"?><plist><dict><key>mboxes</key><dict>` +
			`<key>root</key><dict/></dict></dict></plist><plist/>`, wantErr: true, wantCode: "mailbox_cache_malformed"},
		{name: "malformed nesting", source: `<?xml version="1.0"?><plist><dict><key>mboxes</key><dict>` +
			`<key>root</key><dict/></dict></plist>`, wantErr: true, wantCode: "mailbox_cache_malformed"},
		{name: "invalid encoding", source: `<?xml version="1.0" encoding="ISO-8859-1"?><plist><dict>` +
			`<key>mboxes</key><dict><key>root</key><dict/></dict></dict></plist>`, wantErr: true, wantCode: "mailbox_cache_malformed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cache, err := parseMailboxCacheXML(strings.NewReader(test.source))
			if test.wantErr {
				if err == nil {
					t.Fatalf("parseMailboxCacheXML() = %+v, want error", cache)
				}
				if test.wantCode != "" && errorCodeForTest(err) != test.wantCode {
					t.Fatalf("parseMailboxCacheXML() error = %v, want %s", err, test.wantCode)
				}
				return
			}
			root := cache.Mailboxes["root"]
			if err != nil || root.PathComponent != "Inbox" || root.UnreadCount != 3 ||
				root.Attributes != 8 || root.Children["child"].PathComponent != "Nested" {
				t.Fatalf("parseMailboxCacheXML() = %+v, error = %v", cache, err)
			}
		})
	}
}

func TestParseMailboxCacheXMLSizeLimit(t *testing.T) {
	t.Parallel()
	source := nestedMailboxCacheXML(1) + strings.Repeat(" ", maximumMailboxCacheBytes)
	_, err := parseMailboxCacheXML(strings.NewReader(source))
	if errorCodeForTest(err) != "mailbox_cache_malformed" {
		t.Fatalf("parseMailboxCacheXML() error = %v, want mailbox_cache_malformed", err)
	}
}

func FuzzParseMailboxCacheXML(f *testing.F) {
	f.Add(nestedMailboxCacheXML(1))
	f.Add(nestedMailboxCacheXML(1) + " trailing")
	f.Add("<plist><dict>")
	f.Fuzz(func(t *testing.T, source string) {
		cache, err := parseMailboxCacheXML(strings.NewReader(source))
		if err == nil {
			if len(cache.Mailboxes) == 0 {
				t.Fatal("successful parse returned an empty mailbox catalog")
			}
			return
		}
		if errorCodeForTest(err) != "mailbox_cache_malformed" {
			t.Fatalf("parseMailboxCacheXML() error = %v, want mailbox_cache_malformed", err)
		}
	})
}

func TestMailboxCacheDepthLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		depth int
		want  string
	}{
		{name: "legitimate depth", depth: 10},
		{name: "exceeds limit", depth: maxMailboxCacheDepth + 8, want: "mailbox_cache_malformed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cache, err := parseMailboxCacheXML(strings.NewReader(nestedMailboxCacheXML(test.depth)))
			if test.want == "" {
				if err != nil || len(cache.Mailboxes) != 1 {
					t.Fatalf("parseMailboxCacheXML(depth=%d) = %+v, error = %v", test.depth, cache, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("parseMailboxCacheXML(depth=%d) = %+v, want typed error", test.depth, cache)
			}
			var typed *Error
			if !errors.As(err, &typed) || typed.Code != test.want {
				t.Fatalf("parseMailboxCacheXML(depth=%d) error = %v, want %s", test.depth, err, test.want)
			}
		})
	}
}

func TestCollectMailboxIDsDepthLimit(t *testing.T) {
	t.Parallel()
	records := make(map[string]mailboxRecord)
	output := make(map[int64]struct{})
	if err := collectMailboxIDs(
		nestedMailboxCacheNodes(10), nil, testAccountID, "imap", mailboxAttributeSent,
		records, output, 1,
	); err != nil {
		t.Fatalf("collectMailboxIDs(depth=10) error = %v", err)
	}
	if err := collectMailboxIDs(
		nestedMailboxCacheNodes(maxMailboxCacheDepth+8), nil, testAccountID, "imap", mailboxAttributeSent,
		records, output, 1,
	); err == nil {
		t.Fatal("collectMailboxIDs() error = nil, want typed depth error")
	} else {
		var typed *Error
		if !errors.As(err, &typed) || typed.Code != "mailbox_cache_malformed" {
			t.Fatalf("collectMailboxIDs() error = %v, want mailbox_cache_malformed", err)
		}
	}
}

func nestedMailboxCacheXML(depth int) string {
	var source strings.Builder
	source.WriteString(`<?xml version="1.0"?><plist><dict><key>mboxes</key><dict>`)
	for range depth {
		source.WriteString(`<key>node</key><dict><key>MailboxPathComponent</key><string>node</string><key>IMAPMailboxChildren</key><dict>`)
	}
	for range depth {
		source.WriteString(`</dict></dict>`)
	}
	source.WriteString(`</dict></dict></plist>`)
	return source.String()
}

func nestedMailboxCacheNodes(depth int) map[string]mailboxCacheNode {
	root := make(map[string]mailboxCacheNode)
	current := root
	for range depth {
		children := make(map[string]mailboxCacheNode)
		current["node"] = mailboxCacheNode{PathComponent: "node", Children: children}
		current = children
	}
	return root
}

func BenchmarkParseMailboxCacheXML(b *testing.B) {
	source := `<?xml version="1.0"?><plist version="1.0"><dict><key>mboxes</key><dict>` +
		`<key>mailbox</key><dict><key>MailboxPathComponent</key><string>Inbox</string>` +
		`<key>MailboxUnreadCount</key><integer>3</integer><key>IMAPMailboxAttributes</key><integer>8</integer>` +
		`<key>IMAPMailboxChildren</key><dict/></dict>` + `</dict></dict></plist>`
	b.ReportAllocs()
	b.SetBytes(int64(len(source)))
	for b.Loop() {
		if _, err := parseMailboxCacheXML(strings.NewReader(source)); err != nil {
			b.Fatal(err)
		}
	}
}

func TestListMailboxesRejectsUnsafeOrAmbiguousCache(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "empty catalog",
			content: `<?xml version="1.0"?><plist version="1.0"><dict><key>mboxes</key><dict/></dict></plist>`,
		},
		{
			name: "unsafe path",
			content: `<?xml version="1.0"?><plist version="1.0"><dict><key>mboxes</key><dict><key>bad</key><dict>` +
				`<key>MailboxPathComponent</key><string>..</string><key>IMAPMailboxChildren</key><dict/>` +
				`</dict></dict></dict></plist>`,
		},
		{
			name: "duplicate visible path",
			content: `<?xml version="1.0"?><plist version="1.0"><dict><key>mboxes</key><dict>` +
				`<key>one</key><dict><key>MailboxPathComponent</key><string>INBOX</string><key>IMAPMailboxChildren</key><dict/></dict>` +
				`<key>two</key><dict><key>MailboxPathComponent</key><string>INBOX</string><key>IMAPMailboxChildren</key><dict/></dict>` +
				`</dict></dict></plist>`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store, _ := newSearchFixture(t)
			closeTestResource(t, store, "test store")
			accountRoot := filepath.Join(store.versionRoot, testAccountID)
			if err := os.MkdirAll(accountRoot, 0o700); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			if err := os.WriteFile(
				filepath.Join(accountRoot, ".mboxCache.plist"), []byte(test.content), 0o600,
			); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			_, err := store.ListMailboxes(context.Background(), mail.ListMailboxesRequest{})
			if errorCodeForTest(err) != "mailbox_catalog_incomplete" {
				t.Fatalf("ListMailboxes() error = %v, want mailbox_catalog_incomplete", err)
			}
		})
	}
}

func TestInactiveAccountReferencesAreRejected(t *testing.T) {
	t.Parallel()
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	inactiveID := "BBBBBBBB-CCCC-4DDD-8EEE-FFFFFFFFFFFF"
	accountRef, err := mailref.EncodeAccount(inactiveID)
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	if _, err := store.ListMailboxes(context.Background(), mail.ListMailboxesRequest{
		AccountRef: accountRef,
	}); errorCodeForTest(err) != "stale_reference" {
		t.Fatalf("ListMailboxes() error = %v, want stale_reference", err)
	}
	mailboxRef, err := mailref.EncodeMailbox(inactiveID, []string{"INBOX"})
	if err != nil {
		t.Fatalf("EncodeMailbox() error = %v", err)
	}
	if _, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: mailboxRef, Limit: 10,
	}); errorCodeForTest(err) != "stale_reference" {
		t.Fatalf("ListMessages() error = %v, want stale_reference", err)
	}
	query, err := mail.PrepareQuery(mail.Query{AccountRef: accountRef, Limit: 10})
	if err != nil {
		t.Fatalf("PrepareQuery() error = %v", err)
	}
	if _, err := store.SearchMessages(context.Background(), query); errorCodeForTest(err) != "stale_reference" {
		t.Fatalf("SearchMessages() error = %v, want stale_reference", err)
	}
}

func TestMailboxPathKeyCacheMatchesUncached(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		accountID string
		path      []string
	}{
		{name: "simple", accountID: "ABC-123", path: []string{"INBOX"}},
		{name: "nested", accountID: "abc-456", path: []string{"Parent", "Child"}},
		{name: "gmail", accountID: "USER@ICLOUD", path: []string{"Sent"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := mailboxRecord{
				Location: mailboxLocation{
					AccountID:   test.accountID,
					VisiblePath: test.path,
				},
			}
			record.pathKey = mailboxPathKey(record.Location.AccountID, record.Location.VisiblePath)
			uncached := mailboxPathKey(test.accountID, test.path)
			if record.pathKey != uncached {
				t.Fatalf("cached pathKey = %q, uncached = %q", record.pathKey, uncached)
			}
		})
	}
}

func TestFindMailboxRecordUsesCachedPathKey(t *testing.T) {
	t.Parallel()
	records := []mailboxRecord{
		{
			Location: mailboxLocation{AccountID: "ACC-1", VisiblePath: []string{"INBOX"}},
			pathKey:  mailboxPathKey("ACC-1", []string{"INBOX"}),
		},
		{
			Location: mailboxLocation{AccountID: "ACC-2", VisiblePath: []string{"Sent"}},
			pathKey:  mailboxPathKey("ACC-2", []string{"Sent"}),
		},
	}
	record, found := findMailboxRecord(records, "acc-1", []string{"INBOX"})
	if !found || record.Location.AccountID != "ACC-1" {
		t.Fatalf("findMailboxRecord() = %+v, found %v", record, found)
	}
	_, found = findMailboxRecord(records, "acc-1", []string{"Drafts"})
	if found {
		t.Fatal("findMailboxRecord() found non-existent record")
	}
}

func TestListMailboxesSortKeepsValuesWithKeys(t *testing.T) {
	t.Parallel()
	store, _ := newSearchFixture(t)
	defer closeTestResource(t, store, "test store")
	otherAccountID := "BBBBBBBB-CCCC-4DDD-8EEE-FFFFFFFFFFFF"
	otherAccount, err := parseAccountRoot("imap://" + otherAccountID + "/")
	if err != nil {
		t.Fatalf("parseAccountRoot() error = %v", err)
	}
	store.activeAccountKeys[otherAccount.rootKey()] = struct{}{}
	writer := openTestWriter(t, store.databasePath)
	if _, err := writer.Exec(
		`INSERT INTO mailboxes(ROWID,url,total_count,unread_count,deleted_count,source) VALUES
			(3,'imap://` + otherAccountID + `/INBOX',7,1,0,1),
			(4,'imap://` + otherAccountID + `/Parent/Child',9,2,0,1)`,
	); err != nil {
		closeTestResourceNow(t, writer, "mailbox sort fixture writer")
		t.Fatalf("insert mailbox sort fixture rows: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close mailbox sort fixture writer: %v", err)
	}

	writeCache := func(accountID, cache string) {
		t.Helper()
		accountRoot := filepath.Join(store.versionRoot, accountID)
		if err := os.MkdirAll(accountRoot, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", accountID, err)
		}
		if err := os.WriteFile(filepath.Join(accountRoot, ".mboxCache.plist"), []byte(cache), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", accountID, err)
		}
	}
	firstCache := `<?xml version="1.0"?><plist version="1.0"><dict><key>mboxes</key><dict>` +
		`<key>zulu</key><dict><key>MailboxPathComponent</key><string>Zulu</string><key>IMAPMailboxChildren</key><dict/></dict>` +
		`<key>alpha</key><dict><key>MailboxPathComponent</key><string>Alpha</string><key>MailboxUnreadCount</key><integer>2</integer>` +
		`<key>IMAPMailboxChildren</key><dict><key>child</key><dict><key>MailboxPathComponent</key><string>Child</string>` +
		`<key>MailboxUnreadCount</key><integer>5</integer><key>IMAPMailboxChildren</key><dict/></dict></dict></dict>` +
		`<key>middle</key><dict><key>MailboxPathComponent</key><string>Middle</string><key>IMAPMailboxChildren</key><dict/></dict>` +
		`</dict></dict></plist>`
	secondCache := `<?xml version="1.0"?><plist version="1.0"><dict><key>mboxes</key><dict>` +
		`<key>shared</key><dict><key>MailboxPathComponent</key><string>Shared</string><key>MailboxUnreadCount</key><integer>6</integer>` +
		`<key>IMAPMailboxChildren</key><dict/></dict>` +
		`<key>parent</key><dict><key>MailboxPathComponent</key><string>Parent</string><key>IMAPMailboxChildren</key><dict>` +
		`<key>child</key><dict><key>MailboxPathComponent</key><string>Child</string><key>IMAPMailboxChildren</key><dict/></dict>` +
		`</dict></dict>` +
		`<key>inbox</key><dict><key>MailboxPathComponent</key><string>INBOX</string><key>IMAPMailboxChildren</key><dict/></dict>` +
		`</dict></dict></plist>`
	writeCache(testAccountID, firstCache)
	writeCache(otherAccountID, secondCache)

	mailboxes, err := store.ListMailboxes(context.Background(), mail.ListMailboxesRequest{})
	if err != nil {
		t.Fatalf("ListMailboxes() error = %v", err)
	}
	again, err := store.ListMailboxes(context.Background(), mail.ListMailboxesRequest{})
	if err != nil {
		t.Fatalf("second ListMailboxes() error = %v", err)
	}
	if !reflect.DeepEqual(mailboxes, again) {
		t.Fatalf("repeated ListMailboxes() changed output: first=%+v second=%+v", mailboxes, again)
	}
	type expectedMailbox struct {
		name           string
		unreadCount    int
		messageCount   int
		localAvailable bool
	}
	expected := make(map[string]expectedMailbox)
	addExpected := func(accountID string, path []string, value expectedMailbox) {
		t.Helper()
		accountRef, err := mailref.EncodeAccount(accountID)
		if err != nil {
			t.Fatalf("EncodeAccount(%s) error = %v", accountID, err)
		}
		expected[accountRef+"\x00"+strings.Join(path, "\x00")] = value
	}
	addExpected(testAccountID, []string{"All"}, expectedMailbox{name: "All", messageCount: 1, localAvailable: true})
	addExpected(testAccountID, []string{"Alpha"}, expectedMailbox{name: "Alpha", unreadCount: 2})
	addExpected(testAccountID, []string{"Alpha", "Child"}, expectedMailbox{name: "Child", unreadCount: 5})
	addExpected(testAccountID, []string{"INBOX"}, expectedMailbox{name: "INBOX", messageCount: 3, localAvailable: true})
	addExpected(testAccountID, []string{"Middle"}, expectedMailbox{name: "Middle"})
	addExpected(testAccountID, []string{"Zulu"}, expectedMailbox{name: "Zulu"})
	addExpected(otherAccountID, []string{"INBOX"}, expectedMailbox{name: "INBOX", unreadCount: 1, messageCount: 7, localAvailable: true})
	addExpected(otherAccountID, []string{"Parent"}, expectedMailbox{name: "Parent"})
	addExpected(otherAccountID, []string{"Parent", "Child"}, expectedMailbox{name: "Child", unreadCount: 2, messageCount: 9, localAvailable: true})
	addExpected(otherAccountID, []string{"Shared"}, expectedMailbox{name: "Shared", unreadCount: 6})
	if len(mailboxes) != len(expected) {
		t.Fatalf("ListMailboxes() returned %d entries, want %d: %+v", len(mailboxes), len(expected), mailboxes)
	}
	var previousKey string
	for index, mailbox := range mailboxes {
		key := mailbox.AccountRef + "\x00" + strings.Join(mailbox.Path, "\x00")
		if index > 0 && key <= previousKey {
			t.Fatalf("mailboxes[%d] sort key = %q, previous = %q", index, key, previousKey)
		}
		previousKey = key
		want, ok := expected[key]
		if !ok {
			t.Fatalf("mailboxes[%d] = %+v, unexpected identity %q", index, mailbox, key)
		}
		if mailbox.Name != want.name || mailbox.UnreadCount != want.unreadCount ||
			mailbox.MessageCount != want.messageCount || mailbox.LocalMessagesAvailable != want.localAvailable {
			t.Fatalf("mailboxes[%d] = %+v, want %+v", index, mailbox, want)
		}
		delete(expected, key)
	}
	if len(expected) != 0 {
		t.Fatalf("ListMailboxes() omitted identities: %v", expected)
	}
}
