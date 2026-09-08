package imapclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
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
			name:        "ambiguous special-use mailboxes",
			cfg:         fakeServerConfig{authOK: true, sentMboxes: []string{"Sent A", "Sent B"}, appendOK: true},
			wantErrCode: transport.CodeIMAPAmbiguousMailbox,
		},
		{
			name: "special-use discovered, exact message found, no append",
			cfg: fakeServerConfig{
				authOK: true, sentMboxes: []string{"Sent"}, otherMboxes: []string{"INBOX"},
				searchMatchID: messageID,
				fetchPayload:  []byte("Message-ID: <abc@example.com>\r\n\r\nbody\r\n"),
				appendOK:      true,
			},
			wantMailbox:  "Sent",
			wantAppended: false,
		},
		{
			name: "multiple existing Message-ID candidates",
			cfg: fakeServerConfig{
				authOK: true, sentMboxes: []string{"Sent"}, searchMatchID: messageID,
				searchUIDs: []uint32{41, 42}, appendOK: true,
			},
			wantErrCode: transport.CodeIMAPAmbiguousMessageID,
		},
		{
			name: "substring candidate with mismatched Message-ID",
			cfg: fakeServerConfig{
				authOK: true, sentMboxes: []string{"Sent"}, searchMatchID: messageID,
				fetchPayload: []byte("Message-ID: <different@example.com>\r\n\r\nbody\r\n"), appendOK: true,
			},
			wantErrCode: transport.CodeIMAPMessageNotFound,
		},
		{
			name: "appended candidate with mismatched Message-ID",
			cfg: fakeServerConfig{
				authOK: true, sentMboxes: []string{"Sent"},
				fetchPayload: []byte("Message-ID: <different@example.com>\r\n\r\nbody\r\n"), appendOK: true,
			},
			wantErrCode:      transport.CodeIMAPAppendOutcomeUnknown,
			wantAppendCalled: true,
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
			srv.mu.Lock()
			if len(srv.config.fetchPayload) == 0 {
				srv.config.fetchPayload = []byte("Message-ID: " + messageID + "\r\n\r\nbody\r\n")
			}
			srv.mu.Unlock()
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
				called, _, _, _ := srv.AppendRecord()
				if called != tt.wantAppendCalled {
					t.Fatalf("append called: got %v, want %v", called, tt.wantAppendCalled)
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

func TestAppendToSentConfirmsPostAppendSearch(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, sentMboxes: []string{"Sent"}, appendOK: true,
		fetchPayload: []byte("Message-ID: <confirm@example.com>\r\n\r\nmessage\r\n"),
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

	evidence, err := client.AppendToSent(context.Background(), cfg, []byte("message"), "<confirm@example.com>")
	if err != nil {
		t.Fatalf("AppendToSent() error = %v", err)
	}
	if !evidence.Appended || evidence.MatchCount != 1 {
		t.Fatalf("AppendToSent() evidence = %+v, want one confirmed append", evidence)
	}
	if got := srv.SearchCalls(); got != 2 {
		t.Fatalf("SEARCH calls = %d, want pre- and post-append confirmation", got)
	}
}

func TestAppendToSentRejectsDeliveryRaceAfterNegativeSearch(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, sentMboxes: []string{"Sent"}, appendOK: true,
		deliverAfterFirstSearch: true,
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

	_, err = client.AppendToSent(
		context.Background(),
		cfg,
		[]byte("Message-ID: <race@example.com>\r\n\r\nmessage"),
		"<race@example.com>",
	)
	if transport.ErrorCode(err) != transport.CodeIMAPAmbiguousMessageID {
		t.Fatalf("AppendToSent() error = %v, want duplicate evidence", err)
	}
	if got := srv.SearchCalls(); got != 2 {
		t.Fatalf("SEARCH calls = %d, want pre- and post-append confirmation", got)
	}
}

func TestAppendToSentClassifiesLostAppendResponseAsUnknown(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, sentMboxes: []string{"Sent"}, appendOK: true,
		dropAppendResponse: true,
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

	_, err = client.AppendToSent(
		context.Background(),
		cfg,
		[]byte("Message-ID: <lost@example.com>\r\n\r\nmessage"),
		"<lost@example.com>",
	)
	if transport.ErrorCode(err) != transport.CodeIMAPAppendOutcomeUnknown {
		t.Fatalf("AppendToSent() error = %v, want %s", err, transport.CodeIMAPAppendOutcomeUnknown)
	}
}

func TestAppendToSentReaderRejectsNegativeSize(t *testing.T) {
	client := New()
	_, err := client.AppendToSentReader(context.Background(), transport.ImapConfig{}, strings.NewReader("message"), -1, "<x@example.com>")
	if transport.ErrorCode(err) != transport.CodeIMAPInvalidValue {
		t.Fatalf("error = %v, want invalid_imap_value", err)
	}
}

type imapDeadlineRecorder struct {
	net.Conn
	deadlines []time.Time
}

func (d *imapDeadlineRecorder) SetDeadline(deadline time.Time) error {
	d.deadlines = append(d.deadlines, deadline)
	return d.Conn.SetDeadline(deadline)
}

// A 10 MiB literal earns a transfer budget above the short command budget.
// The recorder proves APPEND uses that budget only while writing the literal
// and restores the short budget before reading the final tagged response.
func TestDoAppendSetsSizeAwareTransferDeadline(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()
	recorder := &imapDeadlineRecorder{Conn: clientConn}
	sess := &session{
		conn: recorder,
		br:   bufio.NewReader(recorder),
		bw:   bufio.NewWriter(recorder),
		nextTag: func() string {
			return "A001"
		},
	}
	message := bytes.Repeat([]byte{'x'}, 10<<20)
	serverErr := make(chan error, 1)
	go func() {
		defer func() { _ = serverConn.Close() }()
		br := bufio.NewReader(serverConn)
		line, err := br.ReadString('\n')
		if err != nil {
			serverErr <- err
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			serverErr <- errors.New("APPEND command has no tag")
			return
		}
		if _, err := io.WriteString(serverConn, "+ go ahead\r\n"); err != nil {
			serverErr <- err
			return
		}
		payload := make([]byte, len(message))
		if _, err := io.ReadFull(br, payload); err != nil {
			serverErr <- err
			return
		}
		trailer := make([]byte, 2)
		if _, err := io.ReadFull(br, trailer); err != nil {
			serverErr <- err
			return
		}
		if !bytes.Equal(payload, message) || trailer[0] != '\r' || trailer[1] != '\n' {
			serverErr <- errors.New("APPEND literal mismatch")
			return
		}
		_, err = io.WriteString(serverConn, fields[0]+" OK APPEND completed\r\n")
		serverErr <- err
	}()

	started := time.Now()
	if err := (&Client{}).doAppend(
		context.Background(), sess, "A001", "Sent", bytes.NewReader(message), int64(len(message)),
	); err != nil {
		t.Fatalf("doAppend() error = %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("fake APPEND peer: %v", err)
	}
	if len(recorder.deadlines) != 3 {
		t.Fatalf("SetDeadline calls = %d, want command, transfer, final reply", len(recorder.deadlines))
	}
	if !recorder.deadlines[1].After(recorder.deadlines[0]) ||
		recorder.deadlines[1].Sub(recorder.deadlines[0]) < 5*time.Second {
		t.Fatalf("transfer deadline = %v, command deadline = %v, want a materially larger transfer budget", recorder.deadlines[1].Sub(started), recorder.deadlines[0].Sub(started))
	}
	if !recorder.deadlines[2].Before(recorder.deadlines[1]) {
		t.Fatalf("final reply deadline = %v, transfer deadline = %v, want restored command budget", recorder.deadlines[2], recorder.deadlines[1])
	}
}

func TestSetTransferDeadlineHonorsContextDeadline(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()
	recorder := &imapDeadlineRecorder{Conn: clientConn}
	sess := &session{conn: recorder}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Second))
	defer cancel()

	if err := (&Client{}).setTransferDeadline(ctx, sess, int64(1<<62)); err != nil {
		t.Fatalf("setTransferDeadline() error = %v", err)
	}
	if len(recorder.deadlines) != 1 {
		t.Fatalf("SetDeadline calls = %d, want 1", len(recorder.deadlines))
	}
	contextDeadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("context has no deadline")
	}
	if deadline := recorder.deadlines[0]; deadline.After(contextDeadline) {
		t.Fatalf("transfer deadline = %v, exceeds context deadline %v", deadline, contextDeadline)
	}
}

func TestMessageOperationsRejectZeroUIDBeforeConnection(t *testing.T) {
	client := New()
	cfg := transport.ImapConfig{}
	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "flags",
			call: func() error {
				_, err := client.SetFlags(context.Background(), cfg, "INBOX", 0, 12345, []string{"\\Seen"}, nil)
				return err
			},
		},
		{
			name: "copy",
			call: func() error {
				_, err := client.CopyMessage(context.Background(), cfg, "INBOX", 0, 12345, "Archive")
				return err
			},
		},
		{
			name: "move",
			call: func() error {
				_, err := client.MoveMessage(context.Background(), cfg, "INBOX", 0, 12345, "Archive")
				return err
			},
		},
		{
			name: "delete",
			call: func() error {
				_, err := client.DeleteMessage(context.Background(), cfg, "INBOX", 0, 12345)
				return err
			},
		},
		{
			name: "fetch",
			call: func() error {
				_, err := client.FetchMessage(context.Background(), cfg, "INBOX", 0, 12345, 1024)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); transport.ErrorCode(err) != transport.CodeIMAPMessageUIDUnknown {
				t.Fatalf("zero-UID operation error = %v, want %s", err, transport.CodeIMAPMessageUIDUnknown)
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

func TestAppendToSentCancellationDuringLiteralRetainsUnknown(t *testing.T) {
	started := make(chan struct{}, 1)
	continueReading := make(chan struct{})
	defer close(continueReading)
	srv := newFakeServer(t, fakeServerConfig{
		authOK:                  true,
		sentMboxes:              []string{"Sent"},
		appendOK:                true,
		appendReadStartedEvents: started,
		appendReadContinue:      continueReading,
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
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, callErr := client.AppendToSent(ctx, cfg, bytes.Repeat([]byte{'x'}, 1<<20), "<cancel@example.com>")
		result <- callErr
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("APPEND literal did not start")
	}
	startedAt := time.Now()
	cancel()
	select {
	case err := <-result:
		if transport.ErrorCode(err) != transport.CodeIMAPAppendOutcomeUnknown {
			t.Fatalf("canceled APPEND error = %v, want %s", err, transport.CodeIMAPAppendOutcomeUnknown)
		}
		if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
			t.Fatalf("canceled APPEND took %v after cancellation", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled APPEND did not return promptly")
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

func TestSafeQuoteIMAPRejectsControlCharacters(t *testing.T) {
	for _, value := range []string{"line\nfeed", "carriage\rreturn", "nul\x00value", "escape\x1bvalue"} {
		if _, err := safeQuoteIMAP(value); transport.ErrorCode(err) != transport.CodeIMAPInvalidValue {
			t.Fatalf("safeQuoteIMAP(%q) error = %v, want invalid_imap_value", value, err)
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
			name: "localized fallback",
			mboxes: []mailbox{
				{name: "INBOX", flags: []string{"\\HasNoChildren"}},
				{name: "Gesendet", flags: []string{"\\HasNoChildren"}},
			},
			want: "Gesendet",
		},
		{
			name:   "none",
			mboxes: []mailbox{{name: "INBOX"}},
			want:   "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := pickSent(c.mboxes)
			if got != c.want {
				t.Fatalf("pickSent: got %q, want %q", got, c.want)
			}
			if c.want == "" && transport.ErrorCode(err) != transport.CodeIMAPSentMailboxNotFound {
				t.Fatalf("pickSent() error = %v, want %s", err, transport.CodeIMAPSentMailboxNotFound)
			}
			if c.want != "" && err != nil {
				t.Fatalf("pickSent() error = %v", err)
			}
		})
	}
}

// Ensure Client implements transport.SentMirror.
var _ transport.SentMirror = (*Client)(nil)

func TestAppendToSentMidCommandCancel(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:       true,
		sentMboxes:   []string{"Sent"},
		appendOK:     true,
		searchDelay:  500 * time.Millisecond,
		fetchPayload: []byte("Message-ID: <x@example.com>\r\n\r\nmessage\r\n"),
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
		fetchPayload:  []byte("Message-ID: <literal@example.com>\r\n\r\nmessage\r\n"),
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

func TestListLiteralSizeLimit(t *testing.T) {
	sess := &session{br: bufio.NewReader(strings.NewReader(
		"* LIST (\\Sent) \".\" {" + strconv.Itoa(maxListLiteralBytes+1) + "}\r\n",
	))}
	_, _, err := New().readLineWithLiteral(sess)
	var malformed *malformedResponseError
	if !errors.As(err, &malformed) {
		t.Fatalf("readLineWithLiteral() error = %v, want malformed response", err)
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

	uid, uidval, matchCount, err := client.SearchUID(context.Background(), cfg, "INBOX", "found@example.com")
	if err != nil {
		t.Fatalf("SearchUID: %v", err)
	}
	if uid != 77 {
		t.Fatalf("expected UID 77, got %d", uid)
	}
	if uidval != 12345 {
		t.Fatalf("expected UIDVALIDITY 12345, got %d", uidval)
	}
	if matchCount != 1 {
		t.Fatalf("expected one Message-ID match, got %d", matchCount)
	}

	_, _, _, err = client.SearchUID(context.Background(), cfg, "INBOX", "<absent@example.com>")
	if err == nil {
		t.Fatalf("expected error for absent message")
	}
	if code := transport.ErrorCode(err); code != transport.CodeIMAPMessageNotFound {
		t.Fatalf("expected code %s, got %s", transport.CodeIMAPMessageNotFound, code)
	}
}

func TestSearchUIDRejectsSubstringCandidate(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:      true,
		otherMboxes: []string{"INBOX"},
		uidSearchResponse: []string{
			"* SEARCH 77",
			"<tag> OK SEARCH completed",
		},
		fetchPayload: []byte("Message-ID: <requested-extra@example.com>\r\n\r\nBody\r\n"),
	})
	client, cfg := newFakeClient(t, srv)
	_, _, _, err := client.SearchUID(context.Background(), cfg, "INBOX", "<requested@example.com>")
	if code := transport.ErrorCode(err); code != transport.CodeIMAPMessageNotFound {
		t.Fatalf("SearchUID() code = %s, want %s: %v", code, transport.CodeIMAPMessageNotFound, err)
	}
}

func TestSearchUIDCountsOnlyExactCandidates(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX"}, searchMatchID: "<requested@example.com>",
		searchUIDs: []uint32{41, 77},
		fetchPayloadByUID: map[uint32][]byte{
			41: []byte("Message-ID: <requested-extra@example.com>\r\n\r\nBody\r\n"),
			77: []byte("Message-ID: <requested@example.com>\r\n\r\nBody\r\n"),
		},
	})
	client, cfg := newFakeClient(t, srv)
	uid, _, matchCount, err := client.SearchUID(context.Background(), cfg, "INBOX", "<requested@example.com>")
	if err != nil {
		t.Fatalf("SearchUID() error = %v", err)
	}
	if uid != 77 || matchCount != 1 {
		t.Fatalf("SearchUID() = uid:%d matches:%d, want 77/1", uid, matchCount)
	}
}

func TestSearchUIDRejectsDuplicateMessageIDHeader(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX"}, searchMatchID: "<duplicate-header@example.com>",
		fetchPayload: []byte("Message-ID: <duplicate-header@example.com>\r\nMessage-ID: <duplicate-header@example.com>\r\n\r\nBody\r\n"),
	})
	client, cfg := newFakeClient(t, srv)
	_, _, _, err := client.SearchUID(context.Background(), cfg, "INBOX", "<duplicate-header@example.com>")
	if code := transport.ErrorCode(err); code != transport.CodeIMAPMessageNotFound {
		t.Fatalf("SearchUID() code = %s, want %s: %v", code, transport.CodeIMAPMessageNotFound, err)
	}
}

func TestNormalizeMessageID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "unbracketed", input: "abc@example.com", want: "<abc@example.com>"},
		{name: "bracketed", input: "<abc@example.com>", want: "<abc@example.com>"},
		{name: "unicode", input: "über@例え.テスト", want: "<über@例え.テスト>"},
		{name: "quoted and escaped", input: `abc"quoted"\id@example.com`, want: `<abc"quoted"\id@example.com>`},
		{name: "opening delimiter only", input: "<abc@example.com", wantErr: true},
		{name: "closing delimiter only", input: "abc@example.com>", wantErr: true},
		{name: "empty", input: "", wantErr: true},
		{name: "empty brackets", input: "<>", wantErr: true},
		{name: "leading whitespace", input: " <abc@example.com>", wantErr: true},
		{name: "trailing whitespace", input: "<abc@example.com> ", wantErr: true},
		{name: "unquoted whitespace", input: "abc def@example.com", wantErr: true},
		{name: "embedded delimiters", input: "<abc><example.com>", wantErr: true},
		{name: "control byte", input: "abc@example.com\r\nX", wantErr: true},
		{name: "unterminated quote", input: `abc"example.com`, wantErr: true},
		{name: "unterminated escape", input: `abc@example.com\`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeMessageID(test.input)
			if test.wantErr {
				if transport.ErrorCode(err) != transport.CodeIMAPInvalidValue {
					t.Fatalf("normalizeMessageID() error = %v, want %s", err, transport.CodeIMAPInvalidValue)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeMessageID() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("normalizeMessageID() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSearchUIDRejectsMalformedMessageIDBeforeConnection(t *testing.T) {
	client := New()
	for _, messageID := range []string{"<abc@example.com", "abc@example.com>", "", "abc\r\nX"} {
		t.Run(messageID, func(t *testing.T) {
			_, _, _, err := client.SearchUID(
				context.Background(), transport.ImapConfig{}, "INBOX", messageID,
			)
			if code := transport.ErrorCode(err); code != transport.CodeIMAPInvalidValue {
				t.Fatalf("SearchUID(%q) code = %s, want %s: %v",
					messageID, code, transport.CodeIMAPInvalidValue, err)
			}
		})
	}
}

func TestSearchUIDReportsDuplicateMatches(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX"},
		searchMatchID: "<duplicate@example.com>", searchUIDs: []uint32{41, 77},
	})
	client, cfg := newFakeClient(t, srv)

	uid, uidvalidity, matchCount, err := client.SearchUID(
		context.Background(), cfg, "INBOX", "<duplicate@example.com>",
	)
	if err != nil {
		t.Fatalf("SearchUID: %v", err)
	}
	if uid != 77 || uidvalidity != 12345 || matchCount != 2 {
		t.Fatalf("SearchUID() = uid:%d uidvalidity:%d matches:%d, want 77/12345/2", uid, uidvalidity, matchCount)
	}
}

func TestSearchUIDRejectsMalformedResponses(t *testing.T) {
	tests := []struct {
		name     string
		response []string
		wantCode string
		dirty    bool
	}{
		{
			name:     "mixed invalid token",
			response: []string{"* SEARCH 41 invalid", "<tag> OK completed"},
			wantCode: transport.CodeIMAPResponseMalformed,
			dirty:    true,
		},
		{
			name:     "overflow",
			response: []string{"* SEARCH 18446744073709551616", "<tag> OK completed"},
			wantCode: transport.CodeIMAPResponseMalformed,
			dirty:    true,
		},
		{
			name:     "zero UID",
			response: []string{"* SEARCH 0", "<tag> OK completed"},
			wantCode: transport.CodeIMAPResponseMalformed,
			dirty:    true,
		},
		{
			name:     "duplicate UID",
			response: []string{"* SEARCH 41 41", "<tag> OK completed"},
			wantCode: transport.CodeIMAPResponseMalformed,
			dirty:    true,
		},
		{
			name:     "multiple SEARCH responses",
			response: []string{"* SEARCH 41", "* SEARCH 77", "<tag> OK completed"},
			wantCode: transport.CodeIMAPResponseMalformed,
			dirty:    true,
		},
		{
			name:     "invalid SEARCH prefix",
			response: []string{"* SEARCHING 41", "<tag> OK completed"},
			wantCode: transport.CodeIMAPResponseMalformed,
			dirty:    true,
		},
		{
			name:     "missing SEARCH response",
			response: []string{"<tag> OK completed"},
			wantCode: transport.CodeIMAPResponseMalformed,
			dirty:    true,
		},
		{
			name:     "tagged failure",
			response: []string{"<tag> BAD invalid search"},
			wantCode: transport.CodeIMAPMutationFailed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := newFakeServer(t, fakeServerConfig{
				authOK:            true,
				otherMboxes:       []string{"INBOX"},
				uidSearchResponse: test.response,
			})
			client, cfg := newFakeClient(t, srv)
			_, _, _, err := client.SearchUID(
				context.Background(), cfg, "INBOX", "<malformed@example.com>",
			)
			if code := transport.ErrorCode(err); code != test.wantCode {
				t.Fatalf("SearchUID() code = %s, want %s: %v", code, test.wantCode, err)
			}
			if test.dirty {
				if _, err := client.ListMailboxes(context.Background(), cfg); err != nil {
					t.Fatalf("ListMailboxes after malformed SEARCH: %v", err)
				}
				if got := srv.ConnectionCount(); got < 2 {
					t.Fatalf("connection count after malformed SEARCH = %d, want reconnect", got)
				}
			}
		})
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
	if ev.Command != "COPY" || ev.UID != 42 || ev.Mailbox != "INBOX" || ev.TargetMailbox != "Archive" ||
		ev.Outcome != transport.MutationOutcomeCompleted || ev.CopyUIDValidity != 12345 ||
		ev.CopySourceUID != 42 || ev.CopyDestinationUID != 100 || ev.DestinationUIDValidity != 12345 ||
		ev.DestinationUID != 100 {
		t.Fatalf("unexpected evidence: %+v", ev)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.copyCalled || srv.copyUID != 42 || srv.copyDst != "Archive" {
		t.Fatalf("copy not recorded on server: called=%v, uid=%d, dst=%s", srv.copyCalled, srv.copyUID, srv.copyDst)
	}
}

func TestCopyMessageResponseLossReturnsRecoverableEvidence(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX", "Archive"}, dropCopyResponse: true,
	})
	client, cfg := newFakeClient(t, srv)

	ev, err := client.CopyMessage(context.Background(), cfg, "INBOX", 42, 12345, "Archive")
	if transport.ErrorCode(err) != transport.CodeIMAPCopyOutcomeUnknown {
		t.Fatalf("CopyMessage() error = %v, want %s", err, transport.CodeIMAPCopyOutcomeUnknown)
	}
	if ev.OperationID == "" || ev.SourceAccount != "user" || ev.Outcome != transport.MutationOutcomeUnknown ||
		ev.Command != "COPY" || ev.Mailbox != "INBOX" || ev.TargetMailbox != "Archive" || ev.UID != 42 ||
		ev.UIDValidity != 12345 || ev.ExpectedUIDValidity != 12345 {
		t.Fatalf("unexpected recoverable evidence: %+v", ev)
	}
	var outcomeErr *transport.MutationOutcomeError
	if !errors.As(err, &outcomeErr) || outcomeErr.Evidence.OperationID != ev.OperationID {
		t.Fatalf("error evidence = %+v, want operation %q", outcomeErr, ev.OperationID)
	}
	srv.mu.Lock()
	copyCalled := srv.copyCalled
	srv.mu.Unlock()
	if !copyCalled {
		t.Fatal("server did not record COPY before response loss")
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
	if ev.Command != "MOVE" || ev.UID != 42 || ev.TargetMailbox != "Archive" ||
		ev.OperationID == "" || ev.Outcome != transport.MutationOutcomeCompleted ||
		len(ev.CompletedEffects) != 1 || ev.CompletedEffects[0] != "move" {
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
	if ev.Command != "MOVE" || ev.UID != 42 || ev.TargetMailbox != "Archive" ||
		ev.Outcome != transport.MutationOutcomeCompleted || ev.ExpungeBranch != "deferred" ||
		ev.ForeignDeletedCount != 0 || len(ev.CompletedEffects) != 3 ||
		ev.CompletedEffects[0] != "copy" || ev.CompletedEffects[1] != "source_flag" ||
		ev.CompletedEffects[2] != "cleanup_deferred" {
		t.Fatalf("unexpected evidence: %+v", ev)
	}
	srv.mu.Lock()
	copyCalled, storeCalled, expungeCalled := srv.copyCalled, srv.storeCalled, srv.expungeCalled
	srv.mu.Unlock()
	if !copyCalled || !storeCalled || expungeCalled {
		t.Fatalf("fallback chain incomplete: copy=%v, store=%v, expunge=%v", copyCalled, storeCalled, expungeCalled)
	}
	if deleted := srv.DeletedUIDs(); len(deleted) != 1 || deleted[0] != 42 {
		t.Fatalf("deferred cleanup lost deleted UID state: %v", deleted)
	}
}

func TestMoveMessageFallbackReportsCopyWhenStoreRejected(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX", "Archive"},
		moveSupported: false, rejectStore: true,
	})
	client, cfg := newFakeClient(t, srv)

	ev, err := client.MoveMessage(context.Background(), cfg, "INBOX", 42, 12345, "Archive")
	if transport.ErrorCode(err) != transport.CodeIMAPMoveOutcomeUnknown {
		t.Fatalf("MoveMessage() error = %v, want %s", err, transport.CodeIMAPMoveOutcomeUnknown)
	}
	if ev.Outcome != transport.MutationOutcomePartial || len(ev.CompletedEffects) != 1 || ev.CompletedEffects[0] != "copy" ||
		ev.CopyDestinationUID != 100 || ev.CopyUIDValidity != 12345 {
		t.Fatalf("unexpected partial evidence: %+v", ev)
	}
	var outcomeErr *transport.MutationOutcomeError
	if !errors.As(err, &outcomeErr) || outcomeErr.Evidence.CompletedEffects[0] != "copy" {
		t.Fatalf("error evidence = %+v", outcomeErr)
	}
	if deleted := srv.DeletedUIDs(); len(deleted) != 0 {
		t.Fatalf("rejected STORE changed source flags: %v", deleted)
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
	if ev.Outcome != transport.MutationOutcomeCompleted || ev.ExpungeBranch != "uid_expunge" ||
		ev.ForeignDeletedCount != 0 || len(ev.CompletedEffects) != 3 ||
		ev.CompletedEffects[2] != "uid_expunge" {
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
	if !strings.Contains(ev.ServerResponse, "expunge deferred because UID EXPUNGE is unsupported") {
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

	payload, err := client.FetchMessage(context.Background(), cfg, "INBOX", 42, 12345, int64(len(expectedBody)))
	if err != nil {
		t.Fatalf("FetchMessage: %v", err)
	}
	if !bytes.Equal(payload, expectedBody) {
		t.Fatalf("expected payload %q, got %q", expectedBody, payload)
	}
}

func TestFetchMessageAcceptsUIDAfterBody(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:      true,
		otherMboxes: []string{"INBOX"},
		fetchResponse: []byte(
			"* 1 FETCH (BODY[] {4}\r\nBody UID 42)\r\n" +
				"<tag> OK FETCH completed\r\n",
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
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	payload, err := client.FetchMessage(context.Background(), cfg, "INBOX", 42, 12345, 1024)
	if err != nil {
		t.Fatalf("FetchMessage() error = %v", err)
	}
	if string(payload) != "Body" {
		t.Fatalf("FetchMessage() payload = %q, want Body", payload)
	}
}

func TestReadFetchLiteralAcceptsUIDAfterBodyAndUnsolicitedFlags(t *testing.T) {
	response := "* 1 FETCH (UID 99 FLAGS (\\Seen))\r\n" +
		"* 1 FETCH (UID 99 BODY[] {3}\r\nBad)\r\n" +
		"* 2 FETCH (BODY[] {4}\r\nBody UID 42)\r\n" +
		"A001 OK FETCH completed\r\n"
	sess := &session{br: bufio.NewReader(strings.NewReader(response))}

	payload, err := New().readFetchLiteral(context.Background(), sess, "A001", 42, 1024)
	if err != nil {
		t.Fatalf("readFetchLiteral() error = %v", err)
	}
	if string(payload) != "Body" {
		t.Fatalf("readFetchLiteral() payload = %q, want Body", payload)
	}
}

func TestReadFetchLiteralRejectsDuplicateTargetBodies(t *testing.T) {
	response := "* 1 FETCH (UID 42 BODY[] {4}\r\nBody)\r\n" +
		"* 1 FETCH (BODY.PEEK[] {4}\r\nCopy UID 42)\r\n" +
		"A001 OK FETCH completed\r\n"
	sess := &session{br: bufio.NewReader(strings.NewReader(response))}

	_, err := New().readFetchLiteral(context.Background(), sess, "A001", 42, 1024)
	if code := transport.ErrorCode(err); code != transport.CodeIMAPResponseMalformed {
		t.Fatalf("readFetchLiteral() code = %s, want %s: %v",
			code, transport.CodeIMAPResponseMalformed, err)
	}
	if !sess.dirty {
		t.Fatal("duplicate target bodies did not dirty the session")
	}
}

func TestParseFetchResponseRejectsDuplicateBodiesInOneResponse(t *testing.T) {
	line := "* 1 FETCH (UID 42 BODY[] \x00 BODY.PEEK[] \x00)"
	_, err := parseFetchResponse(line, [][]byte{[]byte("one"), []byte("two")})
	if err == nil || !strings.Contains(err.Error(), "duplicate FETCH BODY") {
		t.Fatalf("parseFetchResponse() error = %v, want duplicate BODY error", err)
	}
}

func TestParseFetchResponseSkipsGrammarAwareUnknownAttributes(t *testing.T) {
	line := "* 1 FETCH (BODY.PEEK[HEADER.FIELDS (MESSAGE-ID SUBJECT)] \x00 " +
		"X-CUSTOM (one (two three)) UID 42)"
	parsed, err := parseFetchResponse(line, [][]byte{[]byte("headers")})
	if err != nil {
		t.Fatalf("parseFetchResponse() error = %v", err)
	}
	if !parsed.uidPresent || parsed.uid != 42 || !parsed.bodyLiteral || string(parsed.body) != "headers" {
		t.Fatalf("parseFetchResponse() = %+v, want UID 42 and headers", parsed)
	}
}

func TestParseFetchUIDUsesCompleteFetchGrammar(t *testing.T) {
	uid, ok := parseFetchUID("* 1 FETCH (FLAGS (\\Seen) UID 42)")
	if !ok || uid != 42 {
		t.Fatalf("parseFetchUID() = %d, %v, want 42, true", uid, ok)
	}
}

func FuzzParseFetchResponse(f *testing.F) {
	f.Add("* 1 FETCH (UID 42 BODY[] ")
	f.Add("* 1 FETCH (BODY[] \x00 UID 42)")
	f.Add("* 1 FETCH (UID 42 FLAGS (\\Seen))")
	f.Fuzz(func(t *testing.T, line string) {
		_, _ = parseFetchResponse(line, nil)
	})
}

func TestFetchRejectsNonPositiveLimitBeforeConnection(t *testing.T) {
	client := New()
	for _, limit := range []int64{0, -1} {
		t.Run(strconv.FormatInt(limit, 10), func(t *testing.T) {
			_, err := client.FetchMessage(
				context.Background(), transport.ImapConfig{}, "INBOX", 42, 12345, limit,
			)
			if code := transport.ErrorCode(err); code != transport.CodeIMAPInvalidValue {
				t.Fatalf("FetchMessage(%d) error code = %s, want %s: %v",
					limit, code, transport.CodeIMAPInvalidValue, err)
			}
		})
	}
}

func TestReadFetchLiteralRejectsInvalidAnnouncedLength(t *testing.T) {
	tests := []struct {
		name   string
		length string
	}{
		{name: "negative", length: "-1"},
		{name: "integer overflow", length: "9223372036854775808"},
		{name: "unusually large", length: "999999999999999999999999999999"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := "* 1 FETCH (UID 42 BODY[] {" + test.length + "}\r\n"
			sess := &session{br: bufio.NewReader(strings.NewReader(response))}
			_, err := New().readFetchLiteral(
				context.Background(), sess, "A001", 42, 1024,
			)
			if code := transport.ErrorCode(err); code != transport.CodeIMAPResponseMalformed {
				t.Fatalf("readFetchLiteral() code = %s, want %s: %v",
					code, transport.CodeIMAPResponseMalformed, err)
			}
			if !sess.dirty {
				t.Fatal("malformed literal did not dirty the session")
			}
		})
	}
}

func TestReadFetchLiteralRejectsTruncatedPayload(t *testing.T) {
	response := "* 1 FETCH (UID 42 BODY[] {5}\r\nabc"
	sess := &session{br: bufio.NewReader(strings.NewReader(response))}

	_, err := New().readFetchLiteral(context.Background(), sess, "A001", 42, 1024)
	if code := transport.ErrorCode(err); code != transport.CodeIMAPFetchFailed {
		t.Fatalf("readFetchLiteral() code = %s, want %s: %v",
			code, transport.CodeIMAPFetchFailed, err)
	}
	if !sess.dirty {
		t.Fatal("truncated literal did not dirty the session")
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

	_, err := client.FetchMessage(context.Background(), cfg, "INBOX", 42, 12345, mail.MaximumRawSourceBytes)
	var typed *transport.TransportError
	if !errors.As(err, &typed) || typed.Code != transport.CodeIMAPRawSourceTooLarge {
		t.Fatalf("FetchMessage error = %v, want raw_source_too_large", err)
	}
	if !strings.Contains(typed.Message, "268435456") || !strings.Contains(typed.Message, "67108864") {
		t.Fatalf("error message = %q, want announced size and cap", typed.Message)
	}
	payload, err := client.FetchMessage(context.Background(), cfg, "INBOX", 42, 12345, mail.MaximumRawSourceBytes)
	if err != nil {
		t.Fatalf("follow-up FetchMessage after discard: %v", err)
	}
	if len(payload) == 0 {
		t.Fatal("follow-up fetch returned no payload")
	}
}

func TestFetchFailsClosedOnUIDValidityChangeBeforeLiteral(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX"}, searchMatchID: "<x@example.com>",
		changedUIDValidityAfter: 1, changedUIDValidityValue: 99999,
		fetchPayload: []byte("Message-ID: <x@example.com>\r\n\r\nheaders only\r\n"),
	})
	host, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}
	_, _, _, err := client.SearchUID(context.Background(), cfg, "INBOX", "<x@example.com>")
	if err != nil {
		t.Fatalf("SearchUID: %v", err)
	}
	_, err = client.FetchMessage(context.Background(), cfg, "INBOX", 42, 12345, mail.MaximumRawSourceBytes)
	var typed *transport.TransportError
	if !errors.As(err, &typed) || typed.Code != "mailbox_uidvalidity_changed" {
		t.Fatalf("FetchMessage error = %v, want mailbox_uidvalidity_changed", err)
	}
	srv.mu.Lock()
	selectCalls, storeCalled := srv.selectCalls, srv.storeCalled
	srv.mu.Unlock()
	if selectCalls != 2 {
		t.Fatalf("SELECT calls = %d, want 2", selectCalls)
	}
	if storeCalled {
		t.Fatal("FETCH payload path was reached after UIDVALIDITY mismatch")
	}
}

func TestFetchRejectsResponseUIDMismatch(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX"}, fetchResponseUID: 99,
	})
	host, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}
	_, err := client.FetchMessage(context.Background(), cfg, "INBOX", 42, 12345, mail.MaximumRawSourceBytes)
	var typed *transport.TransportError
	if !errors.As(err, &typed) || typed.Code != transport.CodeIMAPMessageUIDMismatch {
		t.Fatalf("FetchMessage error = %v, want %s", err, transport.CodeIMAPMessageUIDMismatch)
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
	_, _, _, err := client.SearchUID(context.Background(), cfg, "INBOX", "<x@example.com>")
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

func TestCheckUIDValidityRejectsUnknown(t *testing.T) {
	tests := []struct {
		name     string
		expected uint32
		observed uint32
	}{
		{name: "both unknown", expected: 0, observed: 0},
		{name: "expected unknown", expected: 0, observed: 12345},
		{name: "observed unknown", expected: 12345, observed: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := checkUIDValidity(test.expected, test.observed)
			if transport.ErrorCode(err) != transport.CodeIMAPUIDValidityUnknown {
				t.Fatalf("checkUIDValidity(%d, %d) = %v, want %s", test.expected, test.observed, err, transport.CodeIMAPUIDValidityUnknown)
			}
		})
	}
}

func TestMutationFailsClosedOnUnknownUIDValidity(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK:          true,
		otherMboxes:     []string{"INBOX"},
		omitUIDValidity: true,
	})
	host, portStr, _ := net.SplitHostPort(srv.Addr())
	port, _ := strconv.Atoi(portStr)
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}

	_, err := client.SetFlags(context.Background(), cfg, "INBOX", 42, 12345, []string{"\\Seen"}, nil)
	if transport.ErrorCode(err) != transport.CodeIMAPUIDValidityUnknown {
		t.Fatalf("SetFlags error = %v, want %s", err, transport.CodeIMAPUIDValidityUnknown)
	}
	srv.mu.Lock()
	storeCalled := srv.storeCalled
	srv.mu.Unlock()
	if storeCalled {
		t.Fatal("STORE ran despite unknown UIDVALIDITY")
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

// Mutations refresh SELECT state before acting so UIDVALIDITY is current even
// when SearchUID previously selected the same mailbox.
func TestMutationRefreshesSelectedSessionBeforeMutation(t *testing.T) {
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

	uid, validity, _, err := client.SearchUID(context.Background(), cfg, "INBOX", "<x@example.com>")
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
	if selectCalls != 2 {
		t.Fatalf("SELECT calls = %d, want 2 (fresh mutation selection)", selectCalls)
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

func TestCheckStatusStrictResponses(t *testing.T) {
	tests := []struct {
		name      string
		mailbox   string
		response  string
		wantError string
		dirty     bool
	}{
		{
			name:    "reordered escaped mailbox",
			mailbox: `IN\BOX`,
			response: "* STATUS \"IN\\\\BOX\" (UIDVALIDITY 12345 MESSAGES 42 UIDNEXT 100 UNSEEN 3)\r\n" +
				"<tag> OK STATUS completed\r\n",
		},
		{
			name: "literal mailbox",
			response: "* STATUS {5}\r\nINBOX (UIDNEXT 100 UIDVALIDITY 12345 MESSAGES 42 UNSEEN 3)\r\n" +
				"<tag> OK STATUS completed\r\n",
		},
		{
			name:      "missing field",
			response:  "* STATUS INBOX (MESSAGES 42 UNSEEN 3 UIDNEXT 100)\r\n<tag> OK STATUS completed\r\n",
			wantError: transport.CodeIMAPResponseMalformed,
			dirty:     true,
		},
		{
			name: "duplicate field",
			response: "* STATUS INBOX (MESSAGES 42 MESSAGES 42 UNSEEN 3 UIDNEXT 100 UIDVALIDITY 12345)\r\n" +
				"<tag> OK STATUS completed\r\n",
			wantError: transport.CodeIMAPResponseMalformed,
			dirty:     true,
		},
		{
			name: "unknown field",
			response: "* STATUS INBOX (MESSAGES 42 UNSEEN 3 UIDNEXT 100 UIDVALIDITY 12345 HIGHESTMODSEQ 9)\r\n" +
				"<tag> OK STATUS completed\r\n",
			wantError: transport.CodeIMAPResponseMalformed,
			dirty:     true,
		},
		{
			name:      "overflow",
			response:  "* STATUS INBOX (MESSAGES 18446744073709551616 UNSEEN 3 UIDNEXT 100 UIDVALIDITY 12345)\r\n<tag> OK STATUS completed\r\n",
			wantError: transport.CodeIMAPResponseMalformed,
			dirty:     true,
		},
		{
			name:      "malformed item list",
			response:  "* STATUS INBOX MESSAGES 42\r\n<tag> OK STATUS completed\r\n",
			wantError: transport.CodeIMAPResponseMalformed,
			dirty:     true,
		},
		{
			name: "mailbox mismatch",
			response: "* STATUS Archive (MESSAGES 42 UNSEEN 3 UIDNEXT 100 UIDVALIDITY 12345)\r\n" +
				"<tag> OK STATUS completed\r\n",
			wantError: transport.CodeIMAPResponseMalformed,
			dirty:     true,
		},
		{
			name:      "missing status response",
			response:  "<tag> OK STATUS completed\r\n",
			wantError: transport.CodeIMAPResponseMalformed,
			dirty:     true,
		},
		{
			name:      "tagged failure",
			response:  "<tag> NO STATUS failed\r\n",
			wantError: transport.CodeIMAPMailboxNotFound,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := newFakeServer(t, fakeServerConfig{
				authOK:         true,
				otherMboxes:    []string{"INBOX"},
				statusResponse: test.response,
			})
			client, cfg := newFakeClient(t, srv)
			mailbox := test.mailbox
			if mailbox == "" {
				mailbox = "INBOX"
			}
			status, err := client.CheckStatus(context.Background(), cfg, mailbox)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("CheckStatus() error = %v", err)
				}
				if status.Messages != 42 || status.Unseen != 3 ||
					status.UIDNext != 100 || status.UIDValidity != 12345 {
					t.Fatalf("CheckStatus() = %+v", status)
				}
			} else if code := transport.ErrorCode(err); code != test.wantError {
				t.Fatalf("CheckStatus() code = %s, want %s: %v", code, test.wantError, err)
			}
			if test.dirty {
				if _, err := client.ListMailboxes(context.Background(), cfg); err != nil {
					t.Fatalf("ListMailboxes after malformed STATUS: %v", err)
				}
				if got := srv.ConnectionCount(); got < 2 {
					t.Fatalf("connection count after malformed STATUS = %d, want reconnect", got)
				}
			}
		})
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
