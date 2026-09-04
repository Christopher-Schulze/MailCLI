package imapclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

func TestAppendToSent(t *testing.T) {
	msg := []byte("From: me@example.com\r\n" +
		"Message-ID: <abc@example.com>\r\n" +
		"Subject: hello\r\n" +
		"\r\n" +
		"body\r\n")
	messageID := "<abc@example.com>"

	tests := []struct {
		name             string
		cfg              fakeServerConfig
		wantMailbox      string
		wantAppended     bool
		wantErrCode      string
		wantAppendCalled bool
	}{
		{
			name:             "special-use discovered, message absent, append",
			cfg:              fakeServerConfig{authOK: true, sentMboxes: []string{"Sent"}, otherMboxes: []string{"INBOX"}, appendOK: true},
			wantMailbox:      "Sent",
			wantAppended:     true,
			wantAppendCalled: true,
		},
		{
			name:         "special-use discovered, message found, no append",
			cfg:          fakeServerConfig{authOK: true, sentMboxes: []string{"Sent"}, otherMboxes: []string{"INBOX"}, searchMatchID: messageID, appendOK: true},
			wantMailbox:  "Sent",
			wantAppended: false,
		},
		{
			name:             "fallback Sent Messages",
			cfg:              fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX", "Sent Messages"}, appendOK: true},
			wantMailbox:      "Sent Messages",
			wantAppended:     true,
			wantAppendCalled: true,
		},
		{
			name:             "fallback Gesendet",
			cfg:              fakeServerConfig{authOK: true, otherMboxes: []string{"Gesendet", "INBOX"}, appendOK: true},
			wantMailbox:      "Gesendet",
			wantAppended:     true,
			wantAppendCalled: true,
		},
		{
			name:             "fallback Gmail",
			cfg:              fakeServerConfig{authOK: true, otherMboxes: []string{"[Gmail]/Sent Mail"}, appendOK: true},
			wantMailbox:      "[Gmail]/Sent Mail",
			wantAppended:     true,
			wantAppendCalled: true,
		},
		{
			name:        "auth failure",
			cfg:         fakeServerConfig{authOK: false, appendOK: true},
			wantErrCode: transport.CodeIMAPAuthFailed,
		},
		{
			name:        "missing Sent mailbox",
			cfg:         fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX", "Drafts"}, appendOK: true},
			wantErrCode: transport.CodeIMAPSentMailboxNotFound,
		},
		{
			name:             "append fails",
			cfg:              fakeServerConfig{authOK: true, sentMboxes: []string{"Sent"}, appendOK: false},
			wantErrCode:      transport.CodeIMAPAppendFailed,
			wantAppendCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFakeServer(t, tt.cfg)
			host, portStr, err := net.SplitHostPort(srv.Addr())
			if err != nil {
				t.Fatalf("split host port: %v", err)
			}
			port, err := strconv.Atoi(portStr)
			if err != nil {
				t.Fatalf("atoi port: %v", err)
			}

			client := New()
			client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
			cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			got, err := client.AppendToSent(ctx, cfg, msg, messageID)
			if tt.wantErrCode != "" {
				if err == nil {
					t.Fatalf("expected error code %s, got nil", tt.wantErrCode)
				}
				if code := transport.ErrorCode(err); code != tt.wantErrCode {
					t.Fatalf("expected error code %s, got %s: %v", tt.wantErrCode, code, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Mailbox != tt.wantMailbox {
				t.Fatalf("mailbox: got %q, want %q", got.Mailbox, tt.wantMailbox)
			}
			if got.Appended != tt.wantAppended {
				t.Fatalf("Appended: got %v, want %v", got.Appended, tt.wantAppended)
			}

			called, mbox, flags, data := srv.AppendRecord()
			if called != tt.wantAppendCalled {
				t.Fatalf("append called: got %v, want %v", called, tt.wantAppendCalled)
			}
			if tt.wantAppendCalled {
				if mbox != tt.wantMailbox {
					t.Fatalf("append mailbox: got %q, want %q", mbox, tt.wantMailbox)
				}
				if !bytes.Equal(data, msg) {
					t.Fatalf("append payload mismatch\n got %q\nwant %q", data, msg)
				}
				hasSeen := false
				for _, f := range flags {
					if f == "\\Seen" {
						hasSeen = true
						break
					}
				}
				if !hasSeen {
					t.Fatalf("append flags missing \\Seen: %v", flags)
				}
			}
		})
	}
}

func TestAppendToSentContextCancel(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:     true,
		sentMboxes: []string{"Sent"},
		appendOK:   true,
	})
	host, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi port: %v", err)
	}

	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = client.AppendToSent(ctx, cfg, []byte("message"), "<x@example.com>")
	if err == nil {
		t.Fatalf("expected error on cancelled context")
	}
	if code := transport.ErrorCode(err); code != transport.CodeIMAPTimeout {
		t.Fatalf("expected code %s, got %s: %v", transport.CodeIMAPTimeout, code, err)
	}
}

func TestAppendToSentTimeout(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:     true,
		sentMboxes: []string{"Sent"},
		appendOK:   true,
	})

	host, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi port: %v", err)
	}

	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	// Wait for the deadline to be in the past.
	time.Sleep(5 * time.Millisecond)

	_, err = client.AppendToSent(ctx, cfg, []byte("message"), "<x@example.com>")
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if code := transport.ErrorCode(err); code != transport.CodeIMAPTimeout {
		t.Fatalf("expected code %s, got %s: %v", transport.CodeIMAPTimeout, code, err)
	}
}

func TestQuoteIMAP(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Sent", `"Sent"`},
		{`a"b`, `"a\"b"`},
		{`a\b`, `"a\\b"`},
		{`a{9}`, `"a{9}"`},
	}
	for _, c := range cases {
		if got := quoteIMAP(c.in); got != c.want {
			t.Fatalf("quoteIMAP(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseListLine(t *testing.T) {
	cases := []struct {
		line, wantName string
		wantFlags      []string
	}{
		{`* LIST (\Sent \HasNoChildren) "/" "Sent"`, "Sent", []string{"\\Sent", "\\HasNoChildren"}},
		{`* LIST (\HasNoChildren) "/" "INBOX"`, "INBOX", []string{"\\HasNoChildren"}},
		{`* LIST (\Sent) "/" "[Gmail]/Sent Mail"`, "[Gmail]/Sent Mail", []string{"\\Sent"}},
		{`* LIST (\Sent) NIL "Sent"`, "Sent", []string{"\\Sent"}},
	}
	for _, c := range cases {
		name, flags, err := parseListLine(c.line)
		if err != nil {
			t.Fatalf("parseListLine(%q): %v", c.line, err)
		}
		if name != c.wantName {
			t.Fatalf("name: got %q, want %q", name, c.wantName)
		}
		if len(flags) != len(c.wantFlags) {
			t.Fatalf("flags: got %v, want %v", flags, c.wantFlags)
		}
		for i := range flags {
			if flags[i] != c.wantFlags[i] {
				t.Fatalf("flags[%d]: got %q, want %q", i, flags[i], c.wantFlags[i])
			}
		}
	}
}

func TestParseListLineLiteralValuesRemainRaw(t *testing.T) {
	name, flags, err := parseListLine(
		"* LIST (\\Sent) "+imapLiteralMarker+" "+imapLiteralMarker,
		[]byte("."),
		[]byte(`a"b\c`),
	)
	if err != nil {
		t.Fatalf("parseListLine literal: %v", err)
	}
	if name != `a"b\c` {
		t.Fatalf("literal name = %q, want raw quote and slash", name)
	}
	if len(flags) != 1 || flags[0] != "\\Sent" {
		t.Fatalf("literal flags = %v, want [\\Sent]", flags)
	}
}

func TestPickSent(t *testing.T) {
	cases := []struct {
		name   string
		mboxes []mailbox
		want   string
	}{
		{
			name: "by flag",
			mboxes: []mailbox{
				{name: "INBOX", flags: []string{"\\HasNoChildren"}},
				{name: "Sent", flags: []string{"\\Sent"}},
			},
			want: "Sent",
		},
		{
			name: "fallback order",
			mboxes: []mailbox{
				{name: "INBOX", flags: []string{"\\HasNoChildren"}},
				{name: "Sent Messages", flags: []string{"\\HasNoChildren"}},
				{name: "Gesendet", flags: []string{"\\HasNoChildren"}},
			},
			want: "Sent Messages",
		},
		{
			name:   "none",
			mboxes: []mailbox{{name: "INBOX"}},
			want:   "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pickSent(c.mboxes); got != c.want {
				t.Fatalf("pickSent: got %q, want %q", got, c.want)
			}
		})
	}
}

// Ensure Client implements transport.SentMirror.
var _ transport.SentMirror = (*Client)(nil)

func TestAppendToSentMidCommandCancel(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:      true,
		sentMboxes:  []string{"Sent"},
		appendOK:    true,
		searchDelay: 500 * time.Millisecond,
	})

	host, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi port: %v", err)
	}

	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err = client.AppendToSent(ctx, cfg, []byte("message"), "<x@example.com>")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected error on cancelled context")
	}
	if code := transport.ErrorCode(err); code != transport.CodeIMAPTimeout {
		t.Fatalf("expected code %s, got %s: %v", transport.CodeIMAPTimeout, code, err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("mid-command cancel took too long: %v", elapsed)
	}
}

func TestListMailboxes(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:      true,
		sentMboxes:  []string{"Sent"},
		trashMboxes: []string{"Trash"},
		otherMboxes: []string{"INBOX", "Archive"},
	})

	host, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	mboxes, err := client.ListMailboxes(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}
	if len(mboxes) != 4 {
		t.Fatalf("expected 4 mailboxes, got %d", len(mboxes))
	}
	names := make(map[string]bool)
	for _, m := range mboxes {
		names[m.Name] = true
	}
	for _, expected := range []string{"Sent", "Trash", "INBOX", "Archive"} {
		if !names[expected] {
			t.Errorf("expected mailbox %s in list", expected)
		}
	}
}

func TestListLiteralMailboxesAndMutations(t *testing.T) {
	const sentName = "Entwürfe"
	const trashName = "Papierkorb"
	srv := newFakeServer(t, fakeServerConfig{
		authOK:        true,
		appendOK:      true,
		moveSupported: true,
		listResponse: []byte(
			"* LIST (\\Sent) \".\" {9}\r\nEntw\xc3\xbcrfe\r\n" +
				"* LIST (\\Trash) {1}\r\n.{10}\r\nPapierkorb\r\n",
		),
	})
	host, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi port: %v", err)
	}
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	defer func() { _ = client.Close() }()
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	appended, err := client.AppendToSent(context.Background(), cfg, []byte("message"), "<literal@example.com>")
	if err != nil {
		t.Fatalf("AppendToSent: %v", err)
	}
	if !appended.Appended || appended.Mailbox != sentName {
		t.Fatalf("literal Sent append = %+v, want mailbox %q", appended, sentName)
	}
	called, appendMailbox, _, _ := srv.AppendRecord()
	if !called || appendMailbox != sentName {
		t.Fatalf("literal append record = called:%v mailbox:%q", called, appendMailbox)
	}

	mboxes, err := client.ListMailboxes(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}
	flagsByName := make(map[string][]string, len(mboxes))
	for _, mbox := range mboxes {
		flagsByName[mbox.Name] = mbox.Flags
	}
	if !hasMailboxFlag(flagsByName[sentName], "\\Sent") || !hasMailboxFlag(flagsByName[trashName], "\\Trash") {
		t.Fatalf("literal mailboxes = %+v, want Sent=%q and Trash=%q", flagsByName, sentName, trashName)
	}

	if _, err := client.SetFlags(context.Background(), cfg, sentName, 42, 12345, []string{"\\Seen"}, nil); err != nil {
		t.Fatalf("SetFlags(%q): %v", sentName, err)
	}
	deletion, err := client.DeleteMessage(context.Background(), cfg, sentName, 42, 12345)
	if err != nil {
		t.Fatalf("DeleteMessage(%q): %v", sentName, err)
	}
	if deletion.TargetMailbox != trashName {
		t.Fatalf("delete target = %q, want %q", deletion.TargetMailbox, trashName)
	}
	srv.mu.Lock()
	storeCalled, moveCalled, moveDst := srv.storeCalled, srv.moveCalled, srv.moveDst
	srv.mu.Unlock()
	if !storeCalled || !moveCalled || moveDst != trashName {
		t.Fatalf("literal mutations = store:%v move:%v target:%q", storeCalled, moveCalled, moveDst)
	}
}

func hasMailboxFlag(flags []string, want string) bool {
	for _, flag := range flags {
		if strings.EqualFold(flag, want) {
			return true
		}
	}
	return false
}

func TestListMalformedResponseFailsLoudly(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:       true,
		listResponse: []byte("* LIST (\\Sent) \".\"\r\n"),
	})
	host, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi port: %v", err)
	}
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	_, err = client.ListMailboxes(context.Background(), cfg)
	var typed *transport.TransportError
	if !errors.As(err, &typed) || typed.Code != transport.CodeIMAPResponseMalformed {
		t.Fatalf("ListMailboxes error = %v, want imap_response_malformed", err)
	}
}

func TestSearchUID(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:        true,
		otherMboxes:   []string{"INBOX"},
		searchMatchID: "<found@example.com>",
		searchUID:     77,
	})

	host, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	uid, uidval, err := client.SearchUID(context.Background(), cfg, "INBOX", "<found@example.com>")
	if err != nil {
		t.Fatalf("SearchUID: %v", err)
	}
	if uid != 77 {
		t.Fatalf("expected UID 77, got %d", uid)
	}
	if uidval != 12345 {
		t.Fatalf("expected UIDVALIDITY 12345, got %d", uidval)
	}

	_, _, err = client.SearchUID(context.Background(), cfg, "INBOX", "<absent@example.com>")
	if err == nil {
		t.Fatalf("expected error for absent message")
	}
	if code := transport.ErrorCode(err); code != transport.CodeIMAPMessageNotFound {
		t.Fatalf("expected code %s, got %s", transport.CodeIMAPMessageNotFound, code)
	}
}

func TestSetFlags(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:      true,
		otherMboxes: []string{"INBOX"},
	})

	host, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	ev, err := client.SetFlags(context.Background(), cfg, "INBOX", 42, 12345, []string{"\\Seen", "\\Flagged"}, []string{"\\Draft"})
	if err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	if ev.Command != "STORE" || ev.UID != 42 || ev.Mailbox != "INBOX" {
		t.Fatalf("unexpected evidence: %+v", ev)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.storeCalled || srv.storeUID != 42 {
		t.Fatalf("store not recorded on server: called=%v, uid=%d", srv.storeCalled, srv.storeUID)
	}
}

func TestCopyMessage(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:      true,
		otherMboxes: []string{"INBOX", "Archive"},
	})

	host, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	ev, err := client.CopyMessage(context.Background(), cfg, "INBOX", 42, 12345, "Archive")
	if err != nil {
		t.Fatalf("CopyMessage: %v", err)
	}
	if ev.Command != "COPY" || ev.UID != 42 || ev.Mailbox != "INBOX" || ev.TargetMailbox != "Archive" {
		t.Fatalf("unexpected evidence: %+v", ev)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.copyCalled || srv.copyUID != 42 || srv.copyDst != "Archive" {
		t.Fatalf("copy not recorded on server: called=%v, uid=%d, dst=%s", srv.copyCalled, srv.copyUID, srv.copyDst)
	}
}

func TestMoveMessageNative(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:        true,
		otherMboxes:   []string{"INBOX", "Archive"},
		moveSupported: true,
	})

	host, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	ev, err := client.MoveMessage(context.Background(), cfg, "INBOX", 42, 12345, "Archive")
	if err != nil {
		t.Fatalf("MoveMessage native: %v", err)
	}
	if ev.Command != "MOVE" || ev.UID != 42 || ev.TargetMailbox != "Archive" {
		t.Fatalf("unexpected evidence: %+v", ev)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.moveCalled || srv.moveUID != 42 || srv.moveDst != "Archive" {
		t.Fatalf("move not recorded on server: called=%v, uid=%d, dst=%s", srv.moveCalled, srv.moveUID, srv.moveDst)
	}
}

func TestMoveMessageFallback(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:        true,
		otherMboxes:   []string{"INBOX", "Archive"},
		moveSupported: false, // forces fallback COPY + STORE \\Deleted + scoped cleanup
	})

	host, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	ev, err := client.MoveMessage(context.Background(), cfg, "INBOX", 42, 12345, "Archive")
	if err != nil {
		t.Fatalf("MoveMessage fallback: %v", err)
	}
	if ev.Command != "MOVE" || ev.UID != 42 || ev.TargetMailbox != "Archive" || ev.ExpungeBranch != "plain_expunge" {
		t.Fatalf("unexpected evidence: %+v", ev)
	}
	srv.mu.Lock()
	copyCalled, storeCalled, expungeCalled := srv.copyCalled, srv.storeCalled, srv.expungeCalled
	srv.mu.Unlock()
	if !copyCalled || !storeCalled || !expungeCalled {
		t.Fatalf("fallback chain incomplete: copy=%v, store=%v, expunge=%v", copyCalled, storeCalled, expungeCalled)
	}
	if deleted := srv.DeletedUIDs(); len(deleted) != 0 {
		t.Fatalf("plain EXPUNGE left deleted UIDs: %v", deleted)
	}
}

func TestMoveMessageFallbackUsesUIDExpunge(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX", "Archive"},
		moveSupported: false, uidExpungeSupported: true, initialDeletedUIDs: []uint32{99},
	})
	client, cfg := newFakeClient(t, srv)

	ev, err := client.MoveMessage(context.Background(), cfg, "INBOX", 42, 12345, "Archive")
	if err != nil {
		t.Fatalf("MoveMessage UID EXPUNGE fallback: %v", err)
	}
	if ev.ExpungeBranch != "uid_expunge" || ev.ForeignDeletedCount != 0 {
		t.Fatalf("unexpected UID EXPUNGE evidence: %+v", ev)
	}
	srv.mu.Lock()
	uidExpungeCalled, uidExpungeUID, expungeCalled := srv.uidExpungeCalled, srv.uidExpungeUID, srv.expungeCalled
	srv.mu.Unlock()
	if !uidExpungeCalled || uidExpungeUID != 42 || expungeCalled {
		t.Fatalf("UID EXPUNGE state: called=%v uid=%d plain=%v", uidExpungeCalled, uidExpungeUID, expungeCalled)
	}
	if deleted := srv.DeletedUIDs(); len(deleted) != 1 || deleted[0] != 99 {
		t.Fatalf("UID EXPUNGE removed foreign deleted UID: %v", deleted)
	}
}

func TestMoveMessageFallbackDefersWithForeignDeleted(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX", "Archive"},
		moveSupported: false, initialDeletedUIDs: []uint32{99},
	})
	client, cfg := newFakeClient(t, srv)

	ev, err := client.MoveMessage(context.Background(), cfg, "INBOX", 42, 12345, "Archive")
	if err != nil {
		t.Fatalf("MoveMessage deferred fallback: %v", err)
	}
	if ev.ExpungeBranch != "deferred" || ev.ForeignDeletedCount != 1 {
		t.Fatalf("unexpected deferred evidence: %+v", ev)
	}
	if !strings.Contains(ev.ServerResponse, "expunge deferred (other deleted messages present") {
		t.Fatalf("deferred response = %q", ev.ServerResponse)
	}
	srv.mu.Lock()
	uidExpungeCalled, expungeCalled := srv.uidExpungeCalled, srv.expungeCalled
	srv.mu.Unlock()
	if uidExpungeCalled || expungeCalled {
		t.Fatalf("deferred cleanup issued expunge: UID=%v plain=%v", uidExpungeCalled, expungeCalled)
	}
	if deleted := srv.DeletedUIDs(); len(deleted) != 2 || deleted[0] != 42 || deleted[1] != 99 {
		t.Fatalf("deferred move lost deleted UID state: %v", deleted)
	}
}

func newFakeClient(t *testing.T, srv *fakeServer) (*Client, transport.ImapConfig) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	return client, transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}
}

func TestDeleteMessage(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:        true,
		trashMboxes:   []string{"Trash"},
		otherMboxes:   []string{"INBOX"},
		moveSupported: true,
	})

	host, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	ev, err := client.DeleteMessage(context.Background(), cfg, "INBOX", 42, 12345)
	if err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}
	if ev.Command != "DELETE" || ev.TargetMailbox != "Trash" || ev.UID != 42 {
		t.Fatalf("unexpected delete evidence: %+v", ev)
	}
}

func TestFetchMessage(t *testing.T) {
	expectedBody := []byte("From: me@test.com\r\nSubject: Hi\r\n\r\nHello body!\r\n")
	srv := newFakeServer(t, fakeServerConfig{
		authOK:       true,
		otherMboxes:  []string{"INBOX"},
		fetchPayload: expectedBody,
	})

	host, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	payload, err := client.FetchMessage(context.Background(), cfg, "INBOX", 42, mail.MaximumRawSourceBytes)
	if err != nil {
		t.Fatalf("FetchMessage: %v", err)
	}
	if !bytes.Equal(payload, expectedBody) {
		t.Fatalf("expected payload %q, got %q", expectedBody, payload)
	}
}

// An announced literal above the cap fails closed before buffering: the
// fake sends no payload bytes at all, so a buffering implementation could
// not produce this typed code. The poisoned session is discarded and the
// next op re-establishes transparently.
func TestFetchRejectsOversizedLiteral(t *testing.T) {
	const announced = 256 << 20
	srv := newFakeServer(t, fakeServerConfig{
		authOK:         true,
		otherMboxes:    []string{"INBOX"},
		fetchPayload:   []byte("From: me@test.com\r\nSubject: Hi\r\n\r\nHello body!\r\n"),
		hugeFetchBytes: announced,
	})

	host, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	_, err := client.FetchMessage(context.Background(), cfg, "INBOX", 42, mail.MaximumRawSourceBytes)
	var typed *transport.TransportError
	if !errors.As(err, &typed) || typed.Code != transport.CodeIMAPRawSourceTooLarge {
		t.Fatalf("FetchMessage error = %v, want raw_source_too_large", err)
	}
	if !strings.Contains(typed.Message, "268435456") || !strings.Contains(typed.Message, "67108864") {
		t.Fatalf("error message = %q, want announced size and cap", typed.Message)
	}
	payload, err := client.FetchMessage(context.Background(), cfg, "INBOX", 42, mail.MaximumRawSourceBytes)
	if err != nil {
		t.Fatalf("follow-up FetchMessage after discard: %v", err)
	}
	if len(payload) == 0 {
		t.Fatal("follow-up fetch returned no payload")
	}
}

func TestMutationFailsClosedOnUIDValidityChange(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:        true,
		otherMboxes:   []string{"INBOX"},
		searchMatchID: "<x@example.com>",
		// First SELECT reports 12345; every later SELECT reports 99999.
		changedUIDValidityAfter: 1,
		changedUIDValidityValue: 99999,
	})

	host, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	// The client resolves the UID with UIDVALIDITY 12345 (first SELECT),
	// then the mailbox is rebuilt on the server: the mutation runs on a
	// fresh connection (session re-establishment mid-command, the
	// defense-in-depth case from the 048 notes), observes 99999 on its own
	// SELECT, and must fail BEFORE any STORE runs.
	_, _, err := client.SearchUID(context.Background(), cfg, "INBOX", "<x@example.com>")
	if err != nil {
		t.Fatalf("SearchUID: %v", err)
	}
	mutClient := New()
	mutClient.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	_, err = mutClient.SetFlags(context.Background(), cfg, "INBOX", 42, 12345, []string{"\\Seen"}, nil)
	var typed *transport.TransportError
	if !errors.As(err, &typed) || typed.Code != "mailbox_uidvalidity_changed" {
		t.Fatalf("SetFlags error = %v, want mailbox_uidvalidity_changed", err)
	}
	srv.mu.Lock()
	selectCalls := srv.selectCalls
	storeCalled := srv.storeCalled
	srv.mu.Unlock()
	if selectCalls != 2 {
		t.Fatalf("SELECT calls = %d, want 2 (resolve + mutation on a fresh connection)", selectCalls)
	}
	if storeCalled {
		t.Fatal("STORE ran despite the UIDVALIDITY mismatch")
	}
}

func TestMutationMatchingUIDValidityProceeds(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:      true,
		otherMboxes: []string{"INBOX"},
	})

	host, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	ev, err := client.SetFlags(context.Background(), cfg, "INBOX", 42, 12345, []string{"\\Seen"}, nil)
	if err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	if ev.UIDValidity != 12345 {
		t.Fatalf("evidence UIDValidity = %d, want 12345", ev.UIDValidity)
	}
	if ev.ExpectedUIDValidity != 12345 {
		t.Fatalf("evidence ExpectedUIDValidity = %d, want 12345", ev.ExpectedUIDValidity)
	}
}

// Same mailbox, same client, same session: the mutation reuses the SELECT
// state SearchUID established instead of issuing another SELECT (048
// acceptance: no extra IMAP round trip in the common case).
func TestMutationReusesSelectedSessionWithoutExtraSelect(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:        true,
		otherMboxes:   []string{"INBOX"},
		searchMatchID: "<x@example.com>",
	})

	host, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	uid, validity, err := client.SearchUID(context.Background(), cfg, "INBOX", "<x@example.com>")
	if err != nil {
		t.Fatalf("SearchUID: %v", err)
	}
	if _, err := client.SetFlags(context.Background(), cfg, "INBOX", uid, validity, []string{"\\Seen"}, nil); err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	srv.mu.Lock()
	selectCalls := srv.selectCalls
	storeCalled := srv.storeCalled
	srv.mu.Unlock()
	if selectCalls != 1 {
		t.Fatalf("SELECT calls = %d, want 1 (shared with SearchUID)", selectCalls)
	}
	if !storeCalled {
		t.Fatal("STORE did not run despite matching UIDVALIDITY")
	}
}

func TestCheckStatus(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:      true,
		otherMboxes: []string{"INBOX"},
	})

	host, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	st, err := client.CheckStatus(context.Background(), cfg, "INBOX")
	if err != nil {
		t.Fatalf("CheckStatus: %v", err)
	}
	if st.Messages != 42 || st.Unseen != 3 || st.UIDNext != 100 || st.UIDValidity != 12345 {
		t.Fatalf("unexpected status: %+v", st)
	}
}

func TestAppendToSentNoDoubleCloseOnCancel(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:     true,
		sentMboxes: []string{"Sent"},
		appendOK:   true,
	})
	host, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi port: %v", err)
	}
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}
	// Cancel before the call; the sync.Once close guard must prevent
	// a double-close panic when both the context goroutine and the
	// deferred cleanup run.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = client.AppendToSent(ctx, cfg, []byte("message"), "<x@example.com>")
	// A second call on a fresh context must also work without panic.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	_, _ = client.AppendToSent(ctx2, cfg, []byte("message"), "<y@example.com>")
}
