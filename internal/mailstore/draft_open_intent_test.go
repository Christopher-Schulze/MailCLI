package mailstore

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

func TestDraftOpenReadsOnlySelectedLocalRepresentation(t *testing.T) {
	store, ref, raw := newLargeReadIntentFixture(t, 128<<10)
	metrics := &readMetrics{}
	store.readMetrics = metrics
	operator := &countingReadIntentIMAP{}
	client := &Client{store: store, send: mail.SendTransport{Imap: operator}}
	for _, test := range []struct {
		name    string
		options []string
		headers bool
		body    bool
		human   bool
	}{
		{"metadata", []string{"--json"}, true, false, false},
		{"summary", []string{"--fields", "summary", "--json"}, true, false, false},
		{"headers", []string{"--fields", "header_fields", "--json"}, true, false, false},
		{"attachments", []string{"--fields", "attachments", "--json"}, false, false, false},
		{"plain", []string{"--view", "plain", "--json"}, false, true, false},
		{"full", []string{"--view", "full", "--json"}, false, true, false},
		{"human", nil, false, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			metrics.sourceBytes.Store(0)
			metrics.retainedBodyBytes.Store(0)
			args := append([]string{"drafts", "open", "--ref", ref}, test.options...)
			code, output, stderr := runReadIntentCLI(t, client, args...)
			if code != 0 || stderr != "" {
				t.Fatalf("code=%d output=%s stderr=%s", code, output, stderr)
			}
			read, retained := metrics.sourceBytes.Load(), metrics.retainedBodyBytes.Load()
			if (!test.body && (read <= 0 || retained != 0 || (test.headers && read >= 64<<10) || (!test.headers && read < int64(len(raw))/2))) || operator.fetchCalls != 0 {
				t.Fatalf("source=%d retained=%d full fetches=%d; headers=%t body=%t", read, retained, operator.fetchCalls, test.headers, test.body)
			}
			if test.human {
				if !strings.Contains(output, "BEGIN LARGE BODY") {
					t.Fatal("human output lost the body")
				}
				return
			}
			var response struct {
				Data struct {
					Message map[string]json.RawMessage `json:"message"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(output), &response); err != nil {
				t.Fatal(err)
			}
			if (response.Data.Message["content"] != nil) != test.body || (test.headers && response.Data.Message["attachments"] != nil) {
				t.Fatalf("projection leaked or lost selected data: %s", output)
			}
			if test.body {
				var content string
				if err := json.Unmarshal(response.Data.Message["content"], &content); err != nil {
					t.Fatal(err)
				}
				if len(content) < 128<<10 || !strings.Contains(content, "BEGIN LARGE BODY") {
					t.Fatal("selected body was not read completely")
				}
			}
			if !test.body {
				t.Logf("source_bytes=%d retained_body_bytes=%d", read, retained)
			}
		})
	}
}

func TestDraftOpenHeadersIgnoreMalformedUnselectedAttachment(t *testing.T) {
	client, _ := newMessagesFixture(t, "draft-body@gmail.com", nil)
	inbox, err := mailref.EncodeMailbox(testAccountID, []string{"INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inbox, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	ref := messageRefWithExpectedID(t, messageRefWithSubject(t, page.Messages, "Status Update"), "102@example.com")
	headers := strings.TrimSuffix(string(localIdentityHeader(t, client.store, 102)), "\r\n")
	raw := []byte(headers + "Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n--b\r\n" +
		"Content-Type: application/pdf\r\nContent-Disposition: attachment; filename=broken.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nnot-base64!\r\n--b--\r\n")
	writeFixtureEMLX(t, client.store, 102, "imap://"+testAccountID+"/INBOX", raw)
	operator := &countingFetchOperator{stubImapOperator: stubImapOperator{fetchErr: &transport.TransportError{Code: transport.CodeIMAPFetchFailed, Err: os.ErrNotExist}}}
	client.send.Imap = operator
	code, output, stderr := runReadIntentCLI(t, client, "drafts", "open", ref, "--json")
	if code != 0 || stderr != "" || operator.fetchCalls != 0 || strings.Contains(output, `"hydration"`) {
		t.Fatalf("unselected attachment caused hydration: code=%d fetches=%d output=%s stderr=%s", code, operator.fetchCalls, output, stderr)
	}
	code, output, stderr = runReadIntentCLI(t, client, "drafts", "open", ref, "--view", "full", "--json")
	if code != 1 || operator.fetchCalls != 1 || stderr != "" {
		t.Fatalf("selected full read bypassed malformed attachment: code=%d fetches=%d output=%s stderr=%s", code, operator.fetchCalls, output, stderr)
	}
}

func TestDraftOpenMissingSourceFetchesHeadersAndRejectsServerRefs(t *testing.T) {
	client, _ := newMessagesFixture(t, "draft-headers@gmail.com", nil)
	inbox, err := mailref.EncodeMailbox(testAccountID, []string{"INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inbox, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	ref := messageRefWithExpectedID(t, messageRefWithSubject(t, page.Messages, "Status Update"), "102@example.com")
	operator := &readIntentHeaderFetcher{
		countingFetchOperator: &countingFetchOperator{stubImapOperator: stubImapOperator{uid: 5001, boxes: []transport.MailboxInfo{{Name: "INBOX"}}}},
		headers:               localIdentityHeader(t, client.store, 102),
	}
	client.send.Imap = operator
	base, err := client.store.messageBasePath(mustMailboxLocation(t, "imap://"+testAccountID+"/INBOX"), 102)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(base + ".emlx"); err != nil {
		t.Fatal(err)
	}
	for index, options := range [][]string{{"--json"}, {"--fields", "summary", "--json"}, {"--fields", "header_fields", "--json"}} {
		code, output, stderr := runReadIntentCLI(t, client, append([]string{"drafts", "open", ref}, options...)...)
		if code != 0 || stderr != "" || operator.headerFetchCalls != index+1 || operator.fetchCalls != 0 || operator.lastHeaderLimit != int64(maximumHeaderBytes) {
			t.Fatalf("header fetch: code=%d headers=%d body=%d limit=%d output=%s stderr=%s", code, operator.headerFetchCalls, operator.fetchCalls, operator.lastHeaderLimit, output, stderr)
		}
	}
	for _, options := range [][]string{{"--json"}, {"--fields", "header_fields", "--json"}, {"--view", "full", "--json"}, nil} {
		code, output, stderr := runReadIntentCLI(t, client, append([]string{"drafts", "open", serverRefFor(t, 5001)}, options...)...)
		if code != 2 || operator.headerFetchCalls != 3 || operator.fetchCalls != 0 || !strings.Contains(output+stderr, "server ref cannot open a draft") {
			t.Fatalf("server ref: code=%d headers=%d body=%d output=%s stderr=%s", code, operator.headerFetchCalls, operator.fetchCalls, output, stderr)
		}
	}
}
