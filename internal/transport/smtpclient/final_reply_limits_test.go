package smtpclient

import (
	"bufio"
	"context"
	"errors"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestFinalSMTPReplyAggregateLimits(t *testing.T) {
	for _, test := range []struct {
		name, response string
		overflow       bool
	}{
		{name: "line boundary", response: strings.Repeat("250-more\r\n", 127) + "250 accepted\r\n"},
		{name: "line overflow", response: strings.Repeat("250-more\r\n", 128) + "250 accepted\r\n", overflow: true},
		{name: "byte boundary", response: strings.Repeat("250-"+strings.Repeat("x", 3994)+"\r\n", 16) + "250 " + strings.Repeat("x", 1530) + "\r\n"},
		{name: "byte overflow", response: strings.Repeat("250-"+strings.Repeat("x", 3994)+"\r\n", 16) + "250 " + strings.Repeat("x", 1531) + "\r\n", overflow: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, text, err := readFinalSMTPResponse(bufio.NewReader(strings.NewReader(test.response)))
			if !test.overflow {
				if err != nil || code != 250 || text == "" {
					t.Fatalf("valid boundary: code=%d, text size=%d, err=%v", code, len(text), err)
				}
				return
			}
			var protocolErr textproto.ProtocolError
			if !errors.As(err, &protocolErr) || code != 0 || text != "" || !strings.Contains(err.Error(), "exceeds") {
				t.Fatalf("flood accepted: code=%d, text size=%d, err=%v", code, len(text), err)
			}
		})
	}
}

func TestSMTPReplyFloodKeepsSubmissionUnknown(t *testing.T) {
	for _, test := range []struct {
		name, response string
	}{
		{name: "success line flood", response: strings.Repeat("250-more\r\n", 128) + "250 accepted\r\n"},
		{name: "negative byte flood", response: strings.Repeat("451-"+strings.Repeat("x", 3994)+"\r\n", 17) + "451 rejected\r\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newFakeSMTPServer(t, func(s *fakeSMTPServer) { s.finalReplyBytes = []byte(test.response) })
			cfg := transport.SubmitConfig{Host: server.host(), Port: server.port(), Username: "user", Password: "s3cret-app-pw"}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			evidence, err := testClient().Submit(ctx, cfg, "a@b.c", []string{"d@e.f"}, []byte(testMessage))
			var unknown *transport.SubmissionError
			var protocolErr textproto.ProtocolError
			var rejection *textproto.Error
			if !errors.As(err, &unknown) || !errors.As(err, &protocolErr) || errors.As(err, &rejection) ||
				transport.ErrorCode(err) != transport.CodeSMTPSubmissionUnknown || evidence != (transport.SubmitEvidence{}) || ctx.Err() != nil {
				t.Fatalf("flood became acceptance/rejection: response bytes=%d, error code=%q, error type=%T", len(evidence.ServerResponse), transport.ErrorCode(err), err)
			}
			server.mu.Lock()
			defer server.mu.Unlock()
			if server.dataCalls != 1 || server.authCalls != 1 || string(server.data) != testMessage {
				t.Fatalf("DATA replayed or changed: calls=%d, auth=%d", server.dataCalls, server.authCalls)
			}
		})
	}
}
