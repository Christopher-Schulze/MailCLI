package imapclient

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestFetchCompletionStatusContract(t *testing.T) {
	for _, test := range []struct {
		name       string
		completion string
		status     string
		malformed  bool
	}{
		{name: "lowercase", completion: "T1 ok done", status: "OK"},
		{name: "mixed case", completion: "T1 oK done", status: "OK"},
		{name: "NO", completion: "T1 no [UNAVAILABLE] unavailable", status: "NO"},
		{name: "BAD", completion: "T1 bAd invalid command", status: "BAD"},
		{name: "unknown", completion: "T1 WHAT invalid", malformed: true},
		{name: "missing", completion: "T1", malformed: true},
		{name: "empty", completion: "T1 ", malformed: true},
	} {
		for _, mode := range []string{"source", "excerpt", "flags"} {
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				response := "* 1 FETCH (UID 42 BODY[] \"Body\")\r\n"
				if mode == "excerpt" {
					response = excerptTestResponse(42, "BODY[TEXT]<0>", "Body")
				}
				if mode == "flags" {
					response = "* 1 FETCH (UID 42 FLAGS (\\Seen))\r\n"
				}
				sess := &session{br: bufio.NewReader(strings.NewReader(response + test.completion + "\r\n"))}
				var err error
				switch mode {
				case "source":
					var source *fetchSource
					source, err = New().readFetchSource(context.Background(), sess, "T1", 42, 12345, 1024, false, "BODY[]")
					if source != nil {
						if test.status != "OK" || string(source.data) != "Body" {
							t.Fatalf("unproven source published: %+v", source)
						}
						err = errors.Join(err, source.Close())
					}
				case "excerpt":
					var results map[uint32]transport.MessageExcerptSource
					results, err = New().readExcerptResponses(context.Background(), sess, "T1", 12345, map[uint32]bool{42: true}, 1024, make(map[uint32]transport.MessageExcerptSource))
					if err == nil && (test.status != "OK" || string(results[42].Source) != excerptTestHeader+"Body") {
						t.Fatalf("unproven excerpt published: %+v", results)
					}
				case "flags":
					var result flagCommandResult
					result, err = New().readFlagResult(context.Background(), sess, "T1", 42, 12345, &flagPermissions{})
					if err == nil && (result.status != "OK" || !result.observation.observed) {
						t.Fatalf("unproven flags published: %+v", result)
					}
				}
				wantCode := ""
				if test.status == "NO" || test.status == "BAD" {
					wantCode = transport.CodeIMAPFetchFailed
					if mode == "flags" {
						wantCode = transport.CodeIMAPMutationFailed
					}
				}
				if test.malformed {
					wantCode = transport.CodeIMAPResponseMalformed
				}
				if (err != nil) != (wantCode != "") || transport.ErrorCode(err) != wantCode || sess.dirty != test.malformed {
					t.Fatalf("completion = %v, dirty=%t; want code %q, dirty=%t", err, sess.dirty, wantCode, test.malformed)
				}
				if (test.status == "NO" || test.status == "BAD") && mode != "flags" {
					rejection, ok := transport.TaggedIMAPRejection(err)
					if !ok || rejection.Command != "FETCH" || rejection.Status != test.status || rejection.Text == "" {
						t.Fatalf("FETCH lost rejection evidence: %+v, present=%t", rejection, ok)
					}
				}
			})
		}
	}
}

func TestEnableCompletionStatusContract(t *testing.T) {
	for _, test := range []struct {
		name       string
		completion string
		enabled    bool
		utf8Only   bool
		utf8       bool
		malformed  bool
	}{
		{name: "lowercase", completion: "T1 ok done", enabled: true, utf8: true},
		{name: "mixed mandatory", completion: "T1 oK done", enabled: true, utf8Only: true, utf8: true},
		{name: "optional NO", completion: "T1 no unavailable"},
		{name: "optional BAD", completion: "T1 bAd unavailable"},
		{name: "optional no enabled evidence", completion: "T1 OK done"},
		{name: "unknown optional", completion: "T1 WHAT invalid", malformed: true},
		{name: "missing optional", completion: "T1", malformed: true},
		{name: "empty optional", completion: "T1 ", malformed: true},
		{name: "mandatory refusal", completion: "T1 NO unavailable", utf8Only: true, malformed: true},
		{name: "mandatory no enabled evidence", completion: "T1 OK done", utf8Only: true, malformed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			peer, conn := net.Pipe()
			defer func() {
				if err := conn.Close(); err != nil {
					t.Error(err)
				}
			}()
			done := make(chan error, 1)
			go func() {
				var err error
				defer func() { done <- errors.Join(err, peer.Close()) }()
				command, err := bufio.NewReader(peer).ReadString('\n')
				if err != nil {
					return
				}
				if command != "T1 ENABLE UTF8=ACCEPT\r\n" {
					err = errors.New("unexpected ENABLE command")
					return
				}
				response := test.completion + "\r\n"
				if test.enabled {
					response = "* enabled utf8=accept\r\n" + response
				}
				_, err = io.WriteString(peer, response)
			}()
			sess := &session{conn: conn, br: bufio.NewReader(conn), bw: bufio.NewWriter(conn), utf8Accept: true, utf8Only: test.utf8Only,
				mailboxEncoding: transport.MailboxEncodingModifiedUTF7, nextTag: func() string { return "T1" }}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := New().enableUTF8(ctx, sess)
			if peerErr := <-done; peerErr != nil {
				t.Fatal(peerErr)
			}
			wantCode := ""
			if test.malformed {
				wantCode = transport.CodeIMAPResponseMalformed
			}
			if (err != nil) != test.malformed || transport.ErrorCode(err) != wantCode || sess.dirty != test.malformed ||
				(sess.mailboxEncoding == transport.MailboxEncodingUTF8) != test.utf8 {
				t.Fatalf("ENABLE = %v, dirty=%t, encoding=%s", err, sess.dirty, sess.mailboxEncoding)
			}
		})
	}
}
