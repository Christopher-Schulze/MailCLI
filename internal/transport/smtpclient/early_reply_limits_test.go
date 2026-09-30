package smtpclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestEarlySMTPRepliesHaveFiniteBudgets(t *testing.T) {
	for _, phase := range []struct{ name, code string }{
		{"greeting", "220"}, {"EHLO", "250"}, {"STARTTLS", "220"},
		{"EHLO_TLS", "250"}, {"AUTH", "235"}, {"MAIL", "250"},
		{"RCPT", "250"}, {"DATA", "354"},
	} {
		for _, form := range []string{"long line", "multiline", "mismatched multiline"} {
			t.Run(phase.name+"/"+form, func(t *testing.T) {
				reply := phase.code + "-" + strings.Repeat("x", 2<<20)
				if form != "long line" {
					lineCode := phase.code
					if form == "mismatched multiline" {
						lineCode = "199"
					}
					reply = phase.code + "-begin\r\n" + strings.Repeat(lineCode+"-"+strings.Repeat("x", 1024)+"\r\n", 2050)
				}
				reply += "\r\n" + phase.code + " done"
				if strings.HasPrefix(phase.name, "EHLO") {
					reply = strings.TrimSuffix(reply, phase.code+" done") + "250-STARTTLS\r\n250 AUTH PLAIN"
				}
				server := newFakeSMTPServer(t, func(s *fakeSMTPServer) {
					s.replyOverrides = map[string]string{phase.name: reply}
				})
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				cfg := transport.SubmitConfig{Host: server.host(), Port: server.port(), Username: "user", Password: "s3cret-app-pw"}
				evidence, err := testClient().Submit(ctx, cfg, "a@b.c", []string{"d@e.f"}, []byte(testMessage))
				var unknown *transport.SubmissionError
				if err == nil || !strings.Contains(err.Error(), "SMTP inbound reply budget exceeded") || errors.As(err, &unknown) || evidence != (transport.SubmitEvidence{}) || ctx.Err() != nil {
					t.Fatalf("unbounded early reply: evidence=%+v, error=%v, context=%v", evidence, err, ctx.Err())
				}
				server.mu.Lock()
				defer server.mu.Unlock()
				if len(server.data) != 0 {
					t.Fatal("early reply failure submitted a DATA payload")
				}
			})
		}
	}
}

func TestSMTPReplyBudgetsPreserveFragmentedTLSExchange(t *testing.T) {
	for _, size := range []int{1, 7, 128} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			server := newFakeSMTPServer(t, func(s *fakeSMTPServer) { s.fragmentBytes = size })
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			cfg := transport.SubmitConfig{Host: server.host(), Port: server.port(), Username: "user", Password: "s3cret-app-pw"}
			evidence, err := testClient().Submit(ctx, cfg, "a@b.c", []string{"d@e.f", "g@h.i"}, []byte(testMessage))
			if err != nil || evidence.MessageID != "<m1@a.b>" || !strings.HasPrefix(evidence.ServerResponse, "250") {
				t.Fatalf("fragmented TLS exchange = %+v, error = %v", evidence, err)
			}
			server.mu.Lock()
			defer server.mu.Unlock()
			if server.authCalls != 1 || server.dataCalls != 1 || len(server.rcpts) != 2 || !bytes.Equal(server.data, []byte(testMessage)) {
				t.Fatal("fragmented replies changed authentication, recipient or DATA behavior")
			}
		})
	}
}

func TestSMTPReplyBudgetResetsBetweenCommands(t *testing.T) {
	reply := func(code string) string {
		return strings.Repeat(code+"-"+strings.Repeat("x", 500)+"\r\n", 120) + code + " done"
	}
	server := newFakeSMTPServer(t, func(s *fakeSMTPServer) {
		s.replyOverrides = map[string]string{"AUTH": reply("235"), "MAIL": reply("250"), "RCPT": reply("250")}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cfg := transport.SubmitConfig{Host: server.host(), Port: server.port(), Username: "user", Password: "s3cret-app-pw"}
	evidence, err := testClient().Submit(ctx, cfg, "a@b.c", []string{"d@e.f", "g@h.i"}, []byte(testMessage))
	if err != nil || !strings.HasPrefix(evidence.ServerResponse, "250") {
		t.Fatalf("individually bounded replies were treated as one session budget: %+v, error=%v", evidence, err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.dataCalls != 1 || !bytes.Equal(server.data, []byte(testMessage)) {
		t.Fatal("resetting reply budgets changed DATA submission")
	}
}

type fragmentSMTPConn struct {
	net.Conn
	chunk int
}

func (c *fragmentSMTPConn) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n, err := c.Conn.Write(p[:min(len(p), c.chunk)])
		written += n
		p = p[n:]
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}
