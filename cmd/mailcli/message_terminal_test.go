package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"mime"
	stdmail "net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

func TestRunMessageTerminalPresentationAndExactData(t *testing.T) {
	const subject = "Résumé \x1b]52;c;YQ==\x07末"
	const sender = "Jörg \x1b[2J\x07"
	const body = "日本語\n\t👩‍💻\n\n\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\\n" +
		"\x1b]52;c;YQ==\x07\roverwrite\b\u009b2J\u009d52;c;Yg==\u009c終"
	const visibleBody = "日本語\n\t👩‍💻\n\n ]8;;https://example.com \\link ]8;; \\\n" +
		" ]52;c;YQ==  overwrite  2J 52;c;Yg== 終"
	mailboxRef, raw, sourcePath := createTerminalMailStore(t, subject, sender, body)
	sourceBefore, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	listing := runTerminalCommand(t, "messages", "list", "--mailbox", mailboxRef, "--json")
	var page struct {
		Data struct {
			Page mail.MessagePage `json:"page"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(listing), &page); err != nil || len(page.Data.Page.Messages) != 1 {
		t.Fatalf("list JSON=%q error=%v", listing, err)
	}
	summary := page.Data.Page.Messages[0]
	parsedSender, err := stdmail.ParseAddress(summary.Sender)
	if err != nil || parsedSender.Name != sender || parsedSender.Address != "alice@example.com" || summary.Subject != subject {
		t.Fatalf("JSON listing changed source values: %+v", summary)
	}
	routes := []struct {
		name string
		args []string
	}{
		{"get", []string{"messages", "get", "--ref", summary.Ref}},
		{"open", []string{"drafts", "open", "--message", summary.Ref}},
		{"table", []string{"messages", "list", "--mailbox", mailboxRef}},
	}
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			visible := runTerminalCommand(t, route.args...)
			if strings.ContainsAny(visible, "\x1b\x07\r\b\u009b\u009d\u009c") || !strings.Contains(visible, "Résumé") {
				t.Fatalf("unsafe or lossy presentation = %q", visible)
			}
			if route.name == "table" {
				return
			}
			if !strings.HasSuffix(visible, visibleBody) || !strings.Contains(visible, "Subject: Résumé  ]52;c;YQ== 末\n") {
				t.Fatalf("human details = %q", visible)
			}
			encoded := runTerminalCommand(t, append(route.args, "--json", "--view", "full")...)
			var response struct {
				Data struct {
					Message mail.Message `json:"message"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(encoded), &response); err != nil {
				t.Fatal(err)
			}
			message := response.Data.Message
			if message.Content != body || message.Summary.Subject != subject || !message.ContentComplete {
				t.Fatalf("normalized JSON data changed or incomplete: %+v", message)
			}
		})
	}
	if got := runTerminalCommand(t, "messages", "raw", "--ref", summary.Ref); got != raw {
		t.Fatalf("raw output = %q, want %q", got, raw)
	}
	var rawResponse struct {
		Data struct {
			RawSource string `json:"raw_source"`
		} `json:"data"`
	}
	encodedRaw := runTerminalCommand(t, "messages", "raw", "--ref", summary.Ref, "--json")
	if err := json.Unmarshal([]byte(encodedRaw), &rawResponse); err != nil || rawResponse.Data.RawSource != raw {
		t.Fatalf("raw JSON=%q error=%v", encodedRaw, err)
	}
	for _, test := range []struct{ command, want string }{{"get", body}, {"raw", raw}} {
		t.Run(test.command+"-export", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "export")
			encoded := runTerminalCommand(t, "messages", test.command, "--ref", summary.Ref, "--view", "full", "--export", path, "--json")
			var response struct {
				Data struct {
					Export mail.ContentExport `json:"content_export"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(encoded), &response); err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(path)
			if err != nil || string(contents) != test.want {
				t.Fatalf("export=%q error=%v", contents, err)
			}
			proof := response.Data.Export
			if proof.Path != path || proof.Size != int64(len(contents)) || proof.SHA256 != fmt.Sprintf("%x", sha256.Sum256(contents)) {
				t.Fatalf("export proof=%+v", proof)
			}
		})
	}
	sourceAfter, err := os.ReadFile(sourcePath)
	if err != nil || !bytes.Equal(sourceAfter, sourceBefore) {
		t.Fatalf("stored EMLX changed: error=%v", err)
	}
	t.Logf("fixture: 1 WAL SQLite message, RFC bytes=%d EMLX bytes=%d; stored SHA256=%x unchanged", len(raw), len(sourceBefore), sha256.Sum256(sourceBefore))
}

func runTerminalCommand(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, code := runWithArgsAndStderr(t, append([]string{"mailcli"}, args...))
	if code != 0 || stderr != "" {
		t.Fatalf("run(%q): exit=%d stdout=%q stderr=%q", args, code, stdout, stderr)
	}
	t.Logf("%s %s: exit=%d stdout=%d bytes stderr=%d bytes", args[0], args[1], code, len(stdout), len(stderr))
	return stdout
}

func createTerminalMailStore(t *testing.T, subject, sender, body string) (string, string, string) {
	t.Helper()
	const accountID = "11111111-2222-4333-8444-555555555555"
	const storeID = "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE"
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "Library", "Mail", "V10")
	writeTerminalFixture(t, filepath.Join(home, "Library", "Containers", "com.apple.mail", "Data", "Library", "Preferences", "com.apple.mail.plist"),
		`<plist version="1.0"><dict><key>AccountOrdering</key><array><string>imap://`+accountID+`/</string></array></dict></plist>`)
	databasePath := filepath.Join(root, "MailData", "Envelope Index")
	if err := os.MkdirAll(filepath.Dir(databasePath), 0o700); err != nil {
		t.Fatal(err)
	}
	// The production mailstore import registers this existing SQLite driver.
	database, err := sql.Open("sqlite3", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close fixture SQLite: %v", err)
		}
	})
	if _, err := database.ExecContext(context.Background(), terminalStoreSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(context.Background(),
		`INSERT INTO mailboxes VALUES (1,?,1,1,0,1); INSERT INTO subjects VALUES (1,?); INSERT INTO addresses VALUES (1,'alice@example.com',?);`,
		"imap://"+accountID+"/INBOX", subject, sender); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	raw := "From: " + mime.BEncoding.Encode("utf-8", sender) + " <alice@example.com>\r\n" +
		"To: Reader <reader@example.com>\r\nSubject: " + mime.BEncoding.Encode("utf-8", subject) + "\r\n" +
		"Message-ID: <terminal@example.com>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body
	sourcePath := filepath.Join(root, accountID, "INBOX.mbox", storeID, "Data", "Messages", "101.emlx")
	writeTerminalFixture(t, sourcePath, fmt.Sprintf("%-10d\n%s<plist version=\"1.0\"><dict/></plist>", len(raw), raw))
	ref, err := mailref.EncodeMailbox(accountID, []string{"INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	return ref, raw, sourcePath
}

func writeTerminalFixture(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

// This is the supported Mail Envelope Index profile, populated with one real
// message row and its authoritative EMLX source in the temporary HOME.
const terminalStoreSchema = `
PRAGMA journal_mode=WAL;
CREATE TABLE properties (ROWID INTEGER PRIMARY KEY, key, value);
CREATE TABLE mailboxes (ROWID INTEGER PRIMARY KEY, url TEXT NOT NULL, total_count INTEGER, unread_count INTEGER, deleted_count INTEGER, source INTEGER);
CREATE TABLE messages (ROWID INTEGER PRIMARY KEY, message_id INTEGER, global_message_id INTEGER, remote_id INTEGER, remote_mailbox INTEGER, sender INTEGER, subject INTEGER, summary INTEGER, date_sent INTEGER, date_received INTEGER, mailbox INTEGER, flags INTEGER, read INTEGER, flagged INTEGER, deleted INTEGER, size INTEGER, conversation_id INTEGER, type INTEGER, display_date INTEGER, flag_color INTEGER);
CREATE TABLE addresses (ROWID INTEGER PRIMARY KEY, address TEXT, comment TEXT);
CREATE TABLE subjects (ROWID INTEGER PRIMARY KEY, subject TEXT);
CREATE TABLE summaries (ROWID INTEGER PRIMARY KEY, summary TEXT);
CREATE TABLE recipients (ROWID INTEGER PRIMARY KEY, message INTEGER, address INTEGER, type INTEGER, position INTEGER);
CREATE TABLE attachments (ROWID INTEGER PRIMARY KEY, message INTEGER, attachment_id TEXT, name TEXT);
CREATE TABLE labels (message_id INTEGER, mailbox_id INTEGER);
CREATE TABLE server_messages (message INTEGER, mailbox INTEGER, junk_level INTEGER, draft INTEGER, replied INTEGER, forwarded INTEGER);
CREATE INDEX messages_mailbox_date_received ON messages(mailbox, date_received);
CREATE INDEX messages_deleted_date_received ON messages(deleted, date_received);
CREATE INDEX labels_mailbox ON labels(mailbox_id);
CREATE INDEX recipients_message ON recipients(message, position, type, address);
CREATE INDEX attachments_message ON attachments(message, attachment_id);
INSERT INTO properties(key,value) VALUES ('UUID','AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE'),('version','4'),('minor_version','74003'),('last_write_framework_version','3826.700.81'),('WriteTransactionGeneration','1');
INSERT INTO summaries VALUES (1,'terminal fixture');
INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES (101,1001,2001,1,1,1,300,300,1,0,0,0,0,500,1,0,300,0);
`
