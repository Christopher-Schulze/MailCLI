package imapclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"

	"mailcli/internal/transport"
)

func recentTestResponse(sequence int, uid int, flags, header string) string {
	return fmt.Sprintf("* %d FETCH (UID %d FLAGS (%s) BODY[HEADER.FIELDS (FROM SUBJECT DATE MESSAGE-ID)] {%d}\r\n%s)\r\n",
		sequence, uid, flags, len(header), header)
}

func listRecentFromFake(t *testing.T, exists int, response string, count int) (*fakeServer, transport.RecentMailbox, error) {
	t.Helper()
	selectResponse := fmt.Sprintf("* OK [UIDVALIDITY 777] ok\r\n* %d EXISTS\r\n<tag> OK [READ-WRITE] SELECT completed\r\n", exists)
	if !strings.Contains(response, "<tag> ") {
		response += "<tag> OK FETCH completed\r\n"
	}
	server := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX"}, selectResponse: selectResponse,
		fetchResponse: []byte(response),
	})
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
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	result, err := client.ListRecentMessages(context.Background(),
		transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}, "INBOX", count)
	return server, result, err
}

func TestListRecentMessagesFetchesTheNewestHeadersOnly(t *testing.T) {
	const first = "From: a@example.com\r\nSubject: One\r\nMessage-ID: <1@example.com>\r\n\r\n"
	const second = "From: b@example.com\r\nSubject: Two\r\nMessage-ID: <2@example.com>\r\n\r\n"
	response := recentTestResponse(9, 1042, `\Seen`, second) + recentTestResponse(8, 1041, "", first)
	server, result, err := listRecentFromFake(t, 9, response, 2)
	if err != nil {
		t.Fatal(err)
	}
	if result.UIDValidity != 777 || result.Exists != 9 || len(result.Messages) != 2 {
		t.Fatalf("result = %+v", result)
	}
	if result.Messages[0].UID != 1041 || result.Messages[0].Seen || string(result.Messages[0].Header) != first ||
		result.Messages[1].UID != 1042 || !result.Messages[1].Seen || string(result.Messages[1].Header) != second {
		t.Fatalf("messages are not in ascending UID order with their flags and headers: %+v", result.Messages)
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

func TestListRecentMessagesOfAnEmptyMailboxSendsNoFetch(t *testing.T) {
	server, result, err := listRecentFromFake(t, 0, "", 5)
	if err != nil || result.Exists != 0 || len(result.Messages) != 0 || result.UIDValidity != 777 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	for _, command := range server.Commands() {
		if strings.Contains(command, "FETCH") {
			t.Fatalf("empty mailbox sent %q", command)
		}
	}
}

func TestListRecentMessagesRejectsBadCountsAndMalformedResponses(t *testing.T) {
	if _, _, err := listRecentFromFake(t, 3, "", 0); transport.ErrorCode(err) != transport.CodeIMAPInvalidValue {
		t.Fatalf("count 0 error = %v", err)
	}
	if _, _, err := listRecentFromFake(t, 3, "", transport.MaximumRecentMessages+1); transport.ErrorCode(err) != transport.CodeIMAPInvalidValue {
		t.Fatalf("oversized count error = %v", err)
	}
	header := "Subject: x\r\n\r\n"
	duplicate := recentTestResponse(1, 5, "", header) + recentTestResponse(2, 5, "", header)
	if _, _, err := listRecentFromFake(t, 2, duplicate, 2); transport.ErrorCode(err) != transport.CodeIMAPResponseMalformed {
		t.Fatalf("duplicate UID error = %v", err)
	}
}

func TestListRecentMessagesRequiresTheCompleteRequestedWindow(t *testing.T) {
	first := recentTestResponse(1, 40, "", "Subject: First\r\n\r\n")
	second := recentTestResponse(2, 41, `\Seen`, "Subject: Second\r\n\r\n")
	for _, test := range []struct {
		name     string
		response string
		count    int
		code     string
		detail   string
	}{
		{name: "complete window", response: second + first},
		{name: "mailbox shorter than requested count", response: first + second, count: 5},
		{name: "empty FLAGS are unseen", response: first + second},
		{name: "empty header literal is valid", response: recentTestResponse(1, 40, "", "") + second},
		{name: "FLAGS-only update is not a message row", response: "* 1 FETCH (UID 40 FLAGS (\\Seen))\r\n" + first + second},
		{name: "EXPUNGE text in a status is not a removal", response: "* OK EXPUNGE\r\n" + first + second},
		{name: "mailbox growth preserves the requested window", response: "* 3 EXISTS\r\n" + first + second},
		{name: "ignored response bound permits its limit", response: strings.Repeat("* OK working\r\n", maxRecentUnsolicitedResponses) + first + second},
		{name: "all rows missing", code: transport.CodeIMAPResponseMalformed, detail: "missing recent FETCH sequence 1"},
		{name: "last row missing", response: first, code: transport.CodeIMAPResponseMalformed, detail: "sequence 2"},
		{name: "FLAGS-only update cannot fill a hole", response: first + "* 2 FETCH (UID 41 FLAGS ())\r\n", code: transport.CodeIMAPResponseMalformed, detail: "sequence 2"},
		{name: "missing FLAGS is not unseen", response: strings.Replace(first, "FLAGS () ", "", 1) + second, code: transport.CodeIMAPResponseMalformed, detail: "UID/FLAGS"},
		{name: "missing UID", response: strings.Replace(first, "UID 40 ", "", 1) + second, code: transport.CodeIMAPResponseMalformed, detail: "UID/FLAGS"},
		{name: "zero UID", response: strings.Replace(first, "UID 40 ", "UID 0 ", 1) + second, code: transport.CodeIMAPResponseMalformed},
		{name: "zero sequence", response: strings.Replace(first, "* 1 FETCH", "* 0 FETCH", 1) + second, code: transport.CodeIMAPResponseMalformed},
		{name: "duplicate sequence", response: first + strings.Replace(second, "* 2 FETCH", "* 1 FETCH", 1), code: transport.CodeIMAPResponseMalformed, detail: "duplicate"},
		{name: "duplicate UID", response: first + strings.Replace(second, "UID 41 ", "UID 40 ", 1), code: transport.CodeIMAPResponseMalformed, detail: "duplicate"},
		{name: "out-of-window row", response: first + strings.Replace(second, "* 2 FETCH", "* 3 FETCH", 1), code: transport.CodeIMAPResponseMalformed, detail: "outside requested window"},
		{name: "extra row after complete window", response: first + second + recentTestResponse(3, 42, "", "Subject: Extra\r\n\r\n"), code: transport.CodeIMAPResponseMalformed, detail: "outside requested window"},
		{name: "wrong header fields", response: strings.Replace(first, "FROM SUBJECT DATE MESSAGE-ID", "FROM SUBJECT", 1) + second, code: transport.CodeIMAPResponseMalformed, detail: "requested header section"},
		{name: "additional BODY section is not requested", response: strings.TrimSuffix(first, ")\r\n") + " BODY[TEXT] {1}\r\nx)\r\n" + second, code: transport.CodeIMAPResponseMalformed, detail: "requested header section"},
		{name: "HEADER.FIELDS.NOT cannot match requested fields", response: strings.Replace(first, "HEADER.FIELDS", "HEADER.FIELDS.NOT", 1) + second, code: transport.CodeIMAPResponseMalformed, detail: "requested header section"},
		{name: "partial section is not the requested header", response: strings.Replace(first, "] {", "]<0> {", 1) + second, code: transport.CodeIMAPResponseMalformed, detail: "requested header section"},
		{name: "NIL header is unavailable", response: "* 1 FETCH (UID 40 FLAGS () BODY[HEADER.FIELDS (FROM SUBJECT DATE MESSAGE-ID)] NIL)\r\n" + second, code: transport.CodeIMAPResponseMalformed, detail: "non-NIL"},
		{name: "quoted header obeys the header bound", response: "* 1 FETCH (UID 40 FLAGS () BODY[HEADER.FIELDS (FROM SUBJECT DATE MESSAGE-ID)] \"" + strings.Repeat("x", maxRecentHeaderBytes+1) + "\")\r\n" + second, code: transport.CodeIMAPResponseMalformed, detail: "header exceeds"},
		{name: "literal header obeys the header bound", response: recentTestResponse(1, 40, "", strings.Repeat("x", maxRecentHeaderBytes+1)) + second, code: transport.CodeIMAPRawSourceTooLarge},
		{name: "EXPUNGE invalidates the sequence window", response: first + "* 1 EXPUNGE\r\n" + second, code: transport.CodeIMAPResponseMalformed, detail: "EXPUNGE"},
		{name: "EXISTS shrink invalidates the window", response: first + "* 1 EXISTS\r\n" + second, code: transport.CodeIMAPResponseMalformed, detail: "EXISTS"},
		{name: "untagged generation changes", response: "* OK [UIDVALIDITY 778] changed\r\n" + first + second, code: "mailbox_uidvalidity_changed"},
		{name: "tagged generation changes", response: first + second + "<tag> OK [UIDVALIDITY 778] completed\r\n", code: "mailbox_uidvalidity_changed"},
		{name: "unchanged generation is valid", response: first + second + "<tag> OK [UIDVALIDITY 777] completed\r\n"},
		{name: "ignored-response work is bounded", response: strings.Repeat("* OK working\r\n", maxRecentUnsolicitedResponses+1) + first + second, code: transport.CodeIMAPResponseMalformed, detail: "ignored-response count"},
		{name: "aggregate response bytes are bounded", response: strings.Repeat("* OK "+strings.Repeat("x", 128*1024)+"\r\n", 40) + first + second, code: transport.CodeIMAPResponseMalformed, detail: "aggregate wire bytes remaining"},
		{name: "invalid tagged status", response: first + second + "<tag> SUCCESS completed\r\n", code: transport.CodeIMAPResponseMalformed, detail: "invalid status"},
		{name: "tagged NO remains failure", response: first + second + "<tag> NO failed\r\n", code: transport.CodeIMAPFetchFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			count := test.count
			if count == 0 {
				count = 2
			}
			server, result, err := listRecentFromFake(t, 2, test.response, count)
			if transport.ErrorCode(err) != test.code || (err == nil) != (test.code == "") || result.Exists != 2 || result.UIDValidity != 777 {
				t.Fatalf("result=%+v error=%v; want code %s", result, err, test.code)
			}
			if test.code != "" {
				if len(result.Messages) != 0 || !strings.Contains(err.Error(), test.detail) {
					t.Fatalf("failed coverage retained usable rows or lost cause: %+v, %v", result, err)
				}
			} else if len(result.Messages) != 2 || result.Messages[0].UID != 40 || result.Messages[0].Seen || result.Messages[1].UID != 41 || !result.Messages[1].Seen {
				t.Fatalf("complete window lost UID or FLAGS evidence: %+v", result)
			}
			fetches := 0
			for _, command := range server.Commands() {
				if command == "FETCH" {
					fetches++
				}
			}
			if fetches != 1 {
				t.Fatalf("coverage added round trips: %v", server.Commands())
			}
		})
	}
}

func TestListRecentMessagesRequiresProvenMailboxCounts(t *testing.T) {
	for _, test := range []struct {
		name           string
		selectResponse string
		code           string
	}{
		{name: "missing EXISTS", selectResponse: "* OK [UIDVALIDITY 777] valid\r\n", code: transport.CodeIMAPResponseMalformed},
		{name: "negative EXISTS", selectResponse: "* OK [UIDVALIDITY 777] valid\r\n* -1 EXISTS\r\n", code: transport.CodeIMAPResponseMalformed},
		{name: "overflowing EXISTS", selectResponse: "* OK [UIDVALIDITY 777] valid\r\n* 4294967296 EXISTS\r\n", code: transport.CodeIMAPResponseMalformed},
		{name: "invalid EXISTS", selectResponse: "* OK [UIDVALIDITY 777] valid\r\n* one EXISTS\r\n", code: transport.CodeIMAPResponseMalformed},
		{name: "signed EXISTS", selectResponse: "* OK [UIDVALIDITY 777] valid\r\n* +1 EXISTS\r\n", code: transport.CodeIMAPResponseMalformed},
		{name: "trailing EXISTS data", selectResponse: "* OK [UIDVALIDITY 777] valid\r\n* 1 EXISTS unexpected\r\n", code: transport.CodeIMAPResponseMalformed},
		{name: "unknown generation for a nonempty mailbox", selectResponse: "* 1 EXISTS\r\n", code: transport.CodeIMAPUIDValidityUnknown},
		{name: "explicit empty mailbox", selectResponse: "* OK [UIDVALIDITY 777] valid\r\n* 0 exists\r\n"},
		{name: "EXISTS text in a status is not a count", selectResponse: "* OK [UIDVALIDITY 777] valid\r\n* 0 EXISTS\r\n* OK EXISTS\r\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newFakeServer(t, fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX"}, selectResponse: test.selectResponse + "<tag> OK SELECT complete\r\n"})
			client, cfg := newFakeClient(t, server)
			t.Cleanup(func() {
				if err := client.Close(); err != nil {
					t.Error(err)
				}
			})
			result, err := client.ListRecentMessages(context.Background(), cfg, "INBOX", 2)
			if transport.ErrorCode(err) != test.code || (err == nil) != (test.code == "") || len(result.Messages) != 0 {
				t.Fatalf("unproven SELECT became usable discovery: %+v, %v", result, err)
			}
			for _, command := range server.Commands() {
				if command == "FETCH" {
					t.Fatalf("unproven or empty mailbox dispatched FETCH: %v", server.Commands())
				}
			}
		})
	}
}

func TestRecentCoverageFailureDirtiesTheSession(t *testing.T) {
	response := recentTestResponse(1, 40, "", "Subject: First\r\n\r\n") + "A1 OK completed\r\n"
	sess := &session{br: bufio.NewReader(strings.NewReader(response))}
	messages, err := (&Client{}).readRecentResponses(context.Background(), sess, "A1", 1, 2, 777)
	if transport.ErrorCode(err) != transport.CodeIMAPResponseMalformed || len(messages) != 0 || !sess.dirty {
		t.Fatalf("incomplete FETCH remained reusable: messages=%+v error=%v dirty=%t", messages, err, sess.dirty)
	}
}
