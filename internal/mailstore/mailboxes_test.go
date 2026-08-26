package mailstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

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

func TestParseMailboxCacheXML(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		source  string
		wantErr bool
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
		{name: "wrong integer type", source: `<?xml version="1.0"?><plist><dict><key>mboxes</key><dict>` +
			`<key>root</key><dict><key>MailboxUnreadCount</key><string>3</string></dict>` +
			`</dict></dict></plist>`, wantErr: true},
		{name: "duplicate root", source: `<?xml version="1.0"?><plist><dict>` +
			`<key>mboxes</key><dict><key>a</key><dict/></dict>` +
			`<key>mboxes</key><dict><key>b</key><dict/></dict>` +
			`</dict></plist>`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cache, err := parseMailboxCacheXML(strings.NewReader(test.source))
			if test.wantErr {
				if err == nil {
					t.Fatalf("parseMailboxCacheXML() = %+v, want error", cache)
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
