package imapclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"mailcli/internal/transport"
	"net"
	"strconv"
	"strings"
	"testing"
)

const excerptTestHeader = "Content-Type: text/plain\r\n\r\n"

func excerptTestResponse(uid int, section, text string) string {
	return fmt.Sprintf("* %d FETCH (UID %d BODY[HEADER.FIELDS (MIME-VERSION CONTENT-TYPE CONTENT-TRANSFER-ENCODING)] {%d}\r\n%s %s {%d}\r\n%s)\r\n",
		uid, uid, len(excerptTestHeader), excerptTestHeader, section, len(text), text)
}

func fetchExcerptsFromFake(t *testing.T, response string, uids []uint32, bound int64) (*fakeServer, map[uint32]transport.MessageExcerptSource, error) {
	t.Helper()
	server := newFakeServer(t, fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX"}, fetchResponse: []byte(response + "<tag> OK FETCH completed\r\n")})
	host, rawPort, err := net.SplitHostPort(server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	client := New()
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	results, err := client.FetchMessageExcerpts(context.Background(), transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}, "INBOX", 12345, uids, bound)
	return server, results, err
}

func TestFetchMessageExcerptsBatchesOneMailbox(t *testing.T) {
	full := strings.Repeat("x", 64)
	response := excerptTestResponse(42, "BODY[TEXT]<0>", full) +
		"* 7 FETCH (FLAGS (\\Seen))\r\n" +
		excerptTestResponse(43, "BODY[TEXT]<0>", "Hi")
	server, results, err := fetchExcerptsFromFake(t, response, []uint32{42, 43, 44, 43}, 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || string(results[42].Source) != excerptTestHeader+full || results[42].Complete ||
		string(results[43].Source) != excerptTestHeader+"Hi" || !results[43].Complete {
		t.Fatalf("results = %+v", results)
	}
	if _, found := results[44]; found {
		t.Fatal("a UID the server did not return has a result")
	}
	fetches, selects := 0, 0
	for _, command := range server.Commands() {
		fetches += strings.Count(command, "FETCH")
		selects += strings.Count(command, "SELECT") + strings.Count(command, "EXAMINE")
	}
	if fetches != 1 || selects != 1 {
		t.Fatalf("commands = %v, want one SELECT and one FETCH", server.Commands())
	}
}

func TestFetchMessageExcerptsRejectsUnboundedOrMalformedResponses(t *testing.T) {
	for _, test := range []struct {
		name, response, wantCode string
	}{
		{"text over cap", excerptTestResponse(42, "BODY[TEXT]<0>", strings.Repeat("x", 65)), transport.CodeIMAPRawSourceTooLarge},
		{"full body section", excerptTestResponse(42, "BODY[]<0>", "Hi"), transport.CodeIMAPResponseMalformed},
		{"text missing", fmt.Sprintf("* 1 FETCH (UID 42 BODY[HEADER.FIELDS (CONTENT-TYPE)] {%d}\r\n%s)\r\n", len(excerptTestHeader), excerptTestHeader), transport.CodeIMAPResponseMalformed},
		{"duplicate UID", excerptTestResponse(42, "BODY[TEXT]<0>", "Hi") + excerptTestResponse(42, "BODY[TEXT]<0>", "Hi"), transport.CodeIMAPResponseMalformed},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, results, err := fetchExcerptsFromFake(t, test.response, []uint32{42}, 64)
			if transport.ErrorCode(err) != test.wantCode {
				t.Fatalf("results=%+v err=%v, want %s", results, err, test.wantCode)
			}
		})
	}
}
