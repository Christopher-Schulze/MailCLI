package imapclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"testing"
	"time"

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

	ev, err := client.SetFlags(context.Background(), cfg, "INBOX", 42, []string{"\\Seen", "\\Flagged"}, []string{"\\Draft"})
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

	ev, err := client.CopyMessage(context.Background(), cfg, "INBOX", 42, "Archive")
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

	ev, err := client.MoveMessage(context.Background(), cfg, "INBOX", 42, "Archive")
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
		moveSupported: false, // forces fallback COPY + STORE \Deleted + EXPUNGE
	})

	host, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	ev, err := client.MoveMessage(context.Background(), cfg, "INBOX", 42, "Archive")
	if err != nil {
		t.Fatalf("MoveMessage fallback: %v", err)
	}
	if ev.Command != "MOVE" || ev.UID != 42 || ev.TargetMailbox != "Archive" {
		t.Fatalf("unexpected evidence: %+v", ev)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.copyCalled || !srv.storeCalled || !srv.expungeCalled {
		t.Fatalf("fallback chain incomplete: copy=%v, store=%v, expunge=%v", srv.copyCalled, srv.storeCalled, srv.expungeCalled)
	}
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

	ev, err := client.DeleteMessage(context.Background(), cfg, "INBOX", 42)
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

	payload, err := client.FetchMessage(context.Background(), cfg, "INBOX", 42)
	if err != nil {
		t.Fatalf("FetchMessage: %v", err)
	}
	if !bytes.Equal(payload, expectedBody) {
		t.Fatalf("expected payload %q, got %q", expectedBody, payload)
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
