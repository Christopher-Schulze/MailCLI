package imapclient

import (
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
	server := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX"}, selectResponse: selectResponse,
		fetchResponse: []byte(response + "<tag> OK FETCH completed\r\n"),
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
