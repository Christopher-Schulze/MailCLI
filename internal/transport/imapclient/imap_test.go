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
