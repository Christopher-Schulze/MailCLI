package smtpclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"reflect"
	"strings"
	"testing"
	"time"

	"mailcli/internal/transport"
)

const testMessage = "From: a@b.c\r\nTo: d@e.f\r\nMessage-ID: <m1@a.b>\r\nSubject: t\r\n\r\nline1\r\n.hidden\r\n\r\n"

func testClient() *Client {
	return New(WithTLSConfig(&tls.Config{InsecureSkipVerify: true})) // test-only: fake's self-signed cert
}

func TestSubmitSuccess(t *testing.T) {
	srv := newFakeSMTPServer(t)
	cfg := transport.SubmitConfig{Host: srv.host(), Port: srv.port(), Username: "user", Password: "s3cret-app-pw"}

	ev, err := testClient().Submit(context.Background(), cfg, "a@b.c",
		[]string{"d@e.f", "d@e.f", "g@h.i"}, []byte(testMessage))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !strings.HasPrefix(ev.ServerResponse, "250") {
		t.Errorf("ServerResponse = %q, want 250 prefix", ev.ServerResponse)
	}
	if ev.MessageID != "<m1@a.b>" {
		t.Errorf("MessageID = %q, want <m1@a.b>", ev.MessageID)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.mailFrom != "a@b.c" {
		t.Errorf("mailFrom = %q, want a@b.c", srv.mailFrom)
	}
	if want := []string{"d@e.f", "g@h.i"}; !reflect.DeepEqual(srv.rcpts, want) {
		t.Errorf("rcpts = %v, want %v (deduplicated, order-stable)", srv.rcpts, want)
	}
	if !bytes.Equal(srv.data, []byte(testMessage)) {
		t.Errorf("payload = %q, want %q", srv.data, testMessage)
	}
	if srv.authCalls != 1 {
		t.Errorf("authCalls = %d, want 1", srv.authCalls)
	}
}

func TestSubmitErrors(t *testing.T) {
	cfg := func(srv *fakeSMTPServer) transport.SubmitConfig {
		return transport.SubmitConfig{Host: srv.host(), Port: srv.port(), Username: "user", Password: "s3cret-app-pw"}
	}

	cases := []struct {
		name        string
		setup       func(*fakeSMTPServer)
		wantCode    string
		wantContain string
	}{
		{
			name:     "auth failure",
			setup:    func(s *fakeSMTPServer) { s.authFail = true },
			wantCode: transport.CodeSMTPAuthFailed,
		},
		{
			name:        "rcpt rejection",
			setup:       func(s *fakeSMTPServer) { s.rejectRcpt = "bad@x.y" },
			wantCode:    transport.CodeSMTPRejected,
			wantContain: "User unknown",
		},
		{
			name:     "no STARTTLS advertised",
			setup:    func(s *fakeSMTPServer) { s.noStartTLS = true },
			wantCode: transport.CodeSMTPTLSFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeSMTPServer(t, tc.setup)

			_, err := testClient().Submit(context.Background(), cfg(srv), "a@b.c",
				[]string{"d@e.f", "bad@x.y"}, []byte(testMessage))
			if err == nil {
				t.Fatal("Submit: want error, got nil")
			}
			if got := transport.ErrorCode(err); got != tc.wantCode {
				t.Errorf("code = %q, want %q (err: %v)", got, tc.wantCode, err)
			}
			if tc.wantContain != "" && !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("error %q does not contain %q", err, tc.wantContain)
			}
			if strings.Contains(err.Error(), "s3cret-app-pw") {
				t.Errorf("error %q leaks the password", err)
			}
		})
	}
}

func TestSubmitDefaultTLSRejectsSelfSigned(t *testing.T) {
	srv := newFakeSMTPServer(t)
	cfg := transport.SubmitConfig{Host: srv.host(), Port: srv.port(), Username: "user", Password: "s3cret-app-pw"}

	// Production path: no injected TLS config, default verification must fail
	// against the fake's self-signed certificate.
	_, err := New().Submit(context.Background(), cfg, "a@b.c", []string{"d@e.f"}, []byte(testMessage))
	if err == nil {
		t.Fatal("Submit: want TLS failure, got nil")
	}
	if got := transport.ErrorCode(err); got != transport.CodeSMTPTLSFailed {
		t.Errorf("code = %q, want %q (err: %v)", got, transport.CodeSMTPTLSFailed, err)
	}
}

func TestSubmitContextCanceledBeforeStart(t *testing.T) {
	srv := newFakeSMTPServer(t)
	cfg := transport.SubmitConfig{Host: srv.host(), Port: srv.port(), Username: "user", Password: "s3cret-app-pw"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := testClient().Submit(ctx, cfg, "a@b.c", []string{"d@e.f"}, []byte(testMessage))
	if err == nil {
		t.Fatal("Submit: want error, got nil")
	}
	if got := transport.ErrorCode(err); got != transport.CodeSMTPTimeout {
		t.Errorf("code = %q, want %q (err: %v)", got, transport.CodeSMTPTimeout, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Submit took %v after pre-canceled ctx", elapsed)
	}
}

func TestSubmitContextDeadlineDuringGreeting(t *testing.T) {
	srv := newFakeSMTPServer(t, func(s *fakeSMTPServer) { s.stallGreeting = true })
	cfg := transport.SubmitConfig{Host: srv.host(), Port: srv.port(), Username: "user", Password: "s3cret-app-pw"}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := testClient().Submit(ctx, cfg, "a@b.c", []string{"d@e.f"}, []byte(testMessage))
	if err == nil {
		t.Fatal("Submit: want error, got nil")
	}
	if got := transport.ErrorCode(err); got != transport.CodeSMTPTimeout {
		t.Errorf("code = %q, want %q (err: %v)", got, transport.CodeSMTPTimeout, err)
	}
	// Must abort well under the 10s dial / 30s command budgets.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Submit took %v, want abort within ~150ms", elapsed)
	}
}
