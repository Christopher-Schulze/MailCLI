package imapclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"mailcli/internal/transport"
)

func TestFetchPublishesOnlyRequestedBodySection(t *testing.T) {
	body := "Message-ID: <section@example.com>\r\n\r\nBody\r\n"
	for _, test := range []struct {
		name    string
		section string
		mode    string
		valid   bool
	}{
		{name: "full exact", section: "BODY[]", valid: true},
		{name: "full normalized", section: "body.peek[]", valid: true},
		{name: "full header", section: "BODY[HEADER]"},
		{name: "full text", section: "BODY[TEXT]"},
		{name: "full part", section: "BODY[1]"},
		{name: "full zero partial", section: "BODY[]<0>"},
		{name: "full shifted partial", section: "BODY[]<5>"},
		{name: "reader header", section: "BODY[HEADER]", mode: "reader"},
		{name: "reader shifted", section: "BODY[]<5>", mode: "reader"},
		{name: "reader exact", section: "BODY[]", mode: "reader", valid: true},
		{name: "headers exact", section: "BODY[HEADER]", mode: "headers", valid: true},
		{name: "headers full", section: "BODY[]", mode: "headers"},
		{name: "headers text", section: "BODY[TEXT]", mode: "headers"},
		{name: "headers partial", section: "BODY[HEADER]<0>", mode: "headers"},
		{name: "identity exact", section: "BODY[HEADER.FIELDS (MESSAGE-ID)]", mode: "identity", valid: true},
		{name: "identity full", section: "BODY[]", mode: "identity"},
		{name: "identity other fields", section: "BODY[HEADER.FIELDS (SUBJECT)]", mode: "identity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := fmt.Sprintf("* 1 FETCH (%s {%d}\r\n%s UID 42)\r\n<tag> OK done\r\n", test.section, len(body), body)
			srv := newFakeServer(t, fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX"}, searchMatchID: "<section@example.com>", fetchResponse: []byte(response)})
			client, cfg := newFakeClient(t, srv)
			t.Cleanup(func() {
				if err := client.Close(); err != nil {
					t.Error(err)
				}
			})
			var payload []byte
			var err error
			switch test.mode {
			case "reader":
				var source io.ReadSeekCloser
				source, _, err = client.FetchMessageReader(context.Background(), cfg, "INBOX", 42, 12345, 1024)
				if source != nil {
					payload, err = io.ReadAll(source)
					err = errors.Join(err, source.Close())
				}
			case "headers":
				payload, err = client.FetchMessageHeaders(context.Background(), cfg, "INBOX", 42, 12345, 1024)
			case "identity":
				var uid uint32
				var matches int
				uid, _, matches, err = client.SearchUID(context.Background(), cfg, "INBOX", "<section@example.com>")
				if test.valid && (uid != 42 || matches != 1) {
					t.Fatalf("identity = %d/%d, %v", uid, matches, err)
				}
			default:
				payload, err = client.FetchMessage(context.Background(), cfg, "INBOX", 42, 12345, 1024)
			}
			if test.valid {
				if err != nil || (test.mode != "identity" && string(payload) != body) {
					t.Fatalf("FETCH = %q, %v", payload, err)
				}
				return
			}
			if transport.ErrorCode(err) != transport.CodeIMAPResponseMalformed || payload != nil {
				t.Fatalf("wrong section published: %q, %v", payload, err)
			}
			if _, err := client.ListMailboxes(context.Background(), cfg); err != nil || srv.ConnectionCount() != 2 {
				t.Fatalf("malformed FETCH session reused: connections=%d, %v", srv.ConnectionCount(), err)
			}
		})
	}
}

func TestExcerptRequiresRequestedHeaderFieldsAndUniqueText(t *testing.T) {
	headerName := strings.Replace(excerptHeaderFields, "BODY.PEEK[", "BODY[", 1)
	for _, test := range []struct {
		name   string
		fields string
		text   string
	}{
		{name: "wrong fields", fields: "BODY[HEADER.FIELDS (SUBJECT)]", text: "BODY[TEXT]<0> \"Body\""},
		{name: "excluded fields", fields: "BODY[HEADER.FIELDS.NOT (SUBJECT)]", text: "BODY[TEXT]<0> \"Body\""},
		{name: "partial headers", fields: headerName + "<0>", text: "BODY[TEXT]<0> \"Body\""},
		{name: "shifted text", fields: headerName, text: "BODY[TEXT]<1> \"Body\""},
		{name: "duplicate text variants", fields: headerName, text: "BODY[TEXT] \"Body\" BODY[TEXT]<0> \"Other\""},
		{name: "NIL text", fields: headerName, text: "BODY[TEXT]<0> NIL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := fmt.Sprintf("* 1 FETCH (UID 42 %s {%d}\r\n%s %s)\r\nA001 OK done\r\n", test.fields, len(excerptTestHeader), excerptTestHeader, test.text)
			sess := &session{br: bufio.NewReader(strings.NewReader(response))}
			results, err := New().readExcerptResponses(context.Background(), sess, "A001", 12345, map[uint32]bool{42: true}, 1024, make(map[uint32]transport.MessageExcerptSource))
			if results != nil || transport.ErrorCode(err) != transport.CodeIMAPResponseMalformed || !sess.dirty {
				t.Fatalf("wrong excerpt sections published: %+v, %v, dirty=%t", results, err, sess.dirty)
			}
		})
	}
}
