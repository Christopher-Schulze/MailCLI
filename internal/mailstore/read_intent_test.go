package mailstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"mailcli/internal/cli"
	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

type countingReadIntentIMAP struct {
	stubImapOperator
	fetchCalls int
}

type legacyReadIntentGateway struct {
	mail.Gateway
}

func (s *countingReadIntentIMAP) FetchMessage(
	ctx context.Context,
	cfg transport.ImapConfig,
	mailbox string,
	uid uint32,
	expectedUIDValidity uint32,
	maxBytes int64,
) ([]byte, error) {
	s.fetchCalls++
	return s.stubImapOperator.FetchMessage(ctx, cfg, mailbox, uid, expectedUIDValidity, maxBytes)
}

func TestMessageReadIntentsMeasureBoundedSourceAndRetainNoBody(t *testing.T) {
	store, ref, raw := newLargeReadIntentFixture(t, 512<<10)
	if len(raw) < 1<<20 {
		t.Fatalf("fixture source bytes = %d, want at least 1 MiB", len(raw))
	}
	metrics := &readMetrics{}
	store.readMetrics = metrics
	imap := &countingReadIntentIMAP{}
	client := &Client{store: store, send: mail.SendTransport{Imap: imap}}
	ctx := context.Background()
	baseline, err := store.GetMessage(ctx, ref)
	if err != nil {
		t.Fatalf("GetMessage() baseline error = %v", err)
	}

	for _, test := range []struct {
		name          string
		intent        mail.MessageReadIntent
		maxSourceRead int64
		minSourceRead int64
		wantZeroRead  bool
	}{
		{name: "index summary", intent: mail.MessageReadIntentIndex, wantZeroRead: true},
		{name: "requested headers", intent: mail.MessageReadIntentHeaders, maxSourceRead: 64 << 10, minSourceRead: 1},
		{name: "attachment metadata", intent: mail.MessageReadIntentAttachments, minSourceRead: int64(len(raw)) / 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			var allocationErr error
			allocations := testing.AllocsPerRun(3, func() {
				if _, err := client.GetMessageWithIntent(ctx, ref, test.intent); err != nil {
					allocationErr = err
				}
			})
			if allocationErr != nil {
				t.Fatalf("GetMessageWithIntent() error = %v", allocationErr)
			}
			metrics.sourceBytes.Store(0)
			metrics.retainedBodyBytes.Store(0)
			started := time.Now()
			message, err := client.GetMessageWithIntent(ctx, ref, test.intent)
			elapsed := time.Since(started)
			if err != nil {
				t.Fatalf("GetMessageWithIntent() error = %v", err)
			}
			sourceBytes := metrics.sourceBytes.Load()
			retainedBodyBytes := metrics.retainedBodyBytes.Load()
			if test.wantZeroRead && sourceBytes != 0 {
				t.Fatalf("index-only source bytes = %d, want zero", sourceBytes)
			}
			if !test.wantZeroRead && sourceBytes < test.minSourceRead ||
				(test.maxSourceRead > 0 && sourceBytes >= test.maxSourceRead) {
				t.Fatalf("source bytes = %d, want [%d,%d)", sourceBytes, test.minSourceRead, test.maxSourceRead)
			}
			if retainedBodyBytes != 0 || message.Content != "" || imap.fetchCalls != 0 {
				t.Fatalf("retained body bytes = %d, content length = %d, IMAP FETCH calls = %d", retainedBodyBytes, len(message.Content), imap.fetchCalls)
			}
			if test.intent == mail.MessageReadIntentIndex {
				wantSummary := baseline.Summary
				wantSummary.MessageID = ""
				wantSummary.AttachmentCount = message.Summary.AttachmentCount
				if message.Summary.MessageID != "" || !reflect.DeepEqual(message.Summary, wantSummary) {
					t.Fatalf("index projection summary = %+v, want index evidence %+v", message.Summary, wantSummary)
				}
			}
			if test.intent == mail.MessageReadIntentHeaders && message.Summary.MessageID != "101@example.com" {
				t.Fatalf("header projection Message-ID = %q", message.Summary.MessageID)
			}
			if test.intent == mail.MessageReadIntentHeaders &&
				(!strings.Contains(message.Headers, "Reply-To: Reply <reply@example.com>") ||
					len(message.BCC) != 1 || message.BCC[0].Address != "blind@example.com") {
				t.Fatalf("header projection = %+v", message)
			}
			if test.intent == mail.MessageReadIntentAttachments &&
				(!message.ContentComplete || len(message.Attachments) < 1) {
				t.Fatalf("attachment projection completeness=%t attachments=%+v", message.ContentComplete, message.Attachments)
			}
			t.Logf("intent=%s elapsed=%s source_bytes=%d retained_body_bytes=%d allocations_per_read=%.1f imap_fetch_calls=%d",
				test.intent, elapsed, sourceBytes, retainedBodyBytes, allocations, imap.fetchCalls)
		})
	}

	metrics.sourceBytes.Store(0)
	metrics.retainedBodyBytes.Store(0)
	started := time.Now()
	code, output, stderr := runReadIntentCLI(t, client, "messages", "get", "--ref", ref, "--fields", "summary", "--json")
	elapsed := time.Since(started)
	if code != 0 || stderr != "" {
		t.Fatalf("header-only summary JSON code = %d, stderr = %q, output = %s", code, stderr, output)
	}
	var response struct {
		Data struct {
			Message    mail.Message `json:"message"`
			Projection struct {
				Fields []string `json:"fields"`
			} `json:"projection"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatalf("unmarshal header-only summary JSON: %v", err)
	}
	sourceBytes := metrics.sourceBytes.Load()
	retainedBodyBytes := metrics.retainedBodyBytes.Load()
	if sourceBytes == 0 || sourceBytes >= 64<<10 || retainedBodyBytes != 0 ||
		response.Data.Message.Summary.MessageID != "101@example.com" ||
		!reflect.DeepEqual(response.Data.Projection.Fields, []string{"summary"}) || imap.fetchCalls != 0 {
		t.Fatalf("summary read elapsed=%s source_bytes=%d retained_body_bytes=%d fetches=%d projection=%+v",
			elapsed, sourceBytes, retainedBodyBytes, imap.fetchCalls, response.Data.Projection)
	}
	t.Logf("single_summary elapsed=%s source_bytes=%d retained_body_bytes=%d imap_fetch_calls=%d",
		elapsed, sourceBytes, retainedBodyBytes, imap.fetchCalls)
}

func TestDefaultMetadataAndPartialAttachmentJSONPreserveEvidence(t *testing.T) {
	store, ref, raw := newLargeReadIntentFixture(t, 512<<10)
	baseline, err := store.GetMessage(context.Background(), ref)
	if err != nil {
		t.Fatalf("legacy GetMessage() error = %v", err)
	}
	client := &Client{store: store}
	metrics := &readMetrics{}
	store.readMetrics = metrics
	started := time.Now()
	code, output, stderr := runReadIntentCLI(t, client, "messages", "get", "--ref", ref, "--json")
	elapsed := time.Since(started)
	if code != 0 || stderr != "" {
		t.Fatalf("default metadata code = %d, stderr = %q, output = %s", code, stderr, output)
	}
	if sourceBytes := metrics.sourceBytes.Load(); sourceBytes < int64(len(raw))/2 || metrics.retainedBodyBytes.Load() != 0 {
		t.Fatalf("default metadata source_bytes=%d retained_body_bytes=%d; want diagnostic MIME scan without retained body",
			sourceBytes, metrics.retainedBodyBytes.Load())
	}
	t.Logf("default_metadata elapsed=%s source_bytes=%d retained_body_bytes=%d",
		elapsed, metrics.sourceBytes.Load(), metrics.retainedBodyBytes.Load())
	legacyCode, legacyOutput, legacyStderr := runLegacyReadIntentCLI(
		t, client, "messages", "get", "--ref", ref, "--json",
	)
	if legacyCode != code || legacyStderr != stderr || !bytes.Equal([]byte(legacyOutput), []byte(output)) {
		t.Fatalf("default metadata changed: optimized=%s legacy=%s", output, legacyOutput)
	}
	var metadata struct {
		Data struct {
			Message    json.RawMessage `json:"message"`
			Projection struct {
				View   string   `json:"view"`
				Fields []string `json:"fields"`
			} `json:"projection"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &metadata); err != nil {
		t.Fatalf("unmarshal metadata response: %v", err)
	}
	var visible struct {
		Summary         mail.MessageSummary `json:"summary"`
		ReplyTo         string              `json:"reply_to"`
		To              []mail.Recipient    `json:"to"`
		CC              []mail.Recipient    `json:"cc"`
		BCC             []mail.Recipient    `json:"bcc"`
		ContentSource   string              `json:"content_source"`
		ContentComplete bool                `json:"content_complete"`
		MissingParts    []string            `json:"missing_parts"`
		Attachments     []mail.Attachment   `json:"attachments"`
	}
	if err := json.Unmarshal(metadata.Data.Message, &visible); err != nil {
		t.Fatalf("unmarshal metadata message: %v", err)
	}
	if !reflect.DeepEqual(visible.Summary, baseline.Summary) ||
		visible.ReplyTo != baseline.ReplyTo || !reflect.DeepEqual(visible.To, baseline.To) ||
		!reflect.DeepEqual(visible.CC, baseline.CC) || !reflect.DeepEqual(visible.BCC, baseline.BCC) ||
		visible.ContentSource != baseline.ContentSource ||
		visible.ContentComplete != baseline.ContentComplete ||
		!reflect.DeepEqual(visible.MissingParts, baseline.MissingParts) ||
		!reflect.DeepEqual(visible.Attachments, baseline.Attachments) ||
		metadata.Data.Projection.View != "metadata" ||
		!reflect.DeepEqual(metadata.Data.Projection.Fields, []string{"attachments", "bcc", "cc", "content_complete", "content_source", "hydration", "missing_parts", "reply_to", "summary", "to"}) {
		t.Fatalf("default metadata changed: visible=%+v baseline=%+v projection=%+v", visible, baseline, metadata.Data.Projection)
	}
	var visibleFields map[string]json.RawMessage
	if err := json.Unmarshal(metadata.Data.Message, &visibleFields); err != nil {
		t.Fatalf("unmarshal metadata fields: %v", err)
	}
	if len(visibleFields) != 9 || visibleFields["content"] != nil || visibleFields["headers"] != nil {
		t.Fatalf("default metadata fields = %v", visibleFields)
	}
	attachmentArgs := []string{"attachments", "list", "--ref", ref, "--json"}
	optimizedCode, optimizedOutput, optimizedStderr := runReadIntentCLI(t, client, attachmentArgs...)
	legacyCode, legacyOutput, legacyStderr = runLegacyReadIntentCLI(t, client, attachmentArgs...)
	if optimizedCode != legacyCode || optimizedStderr != legacyStderr ||
		!bytes.Equal([]byte(optimizedOutput), []byte(legacyOutput)) {
		t.Fatalf("complete attachment JSON changed: optimized=%s legacy=%s", optimizedOutput, legacyOutput)
	}
	projectionArgs := []string{"messages", "get", "--ref", ref, "--fields", "attachments", "--json"}
	optimizedCode, optimizedOutput, optimizedStderr = runReadIntentCLI(t, client, projectionArgs...)
	legacyCode, legacyOutput, legacyStderr = runLegacyReadIntentCLI(t, client, projectionArgs...)
	if optimizedCode != legacyCode || optimizedStderr != legacyStderr ||
		!bytes.Equal([]byte(optimizedOutput), []byte(legacyOutput)) {
		t.Fatalf("complete attachment projection changed: optimized=%s legacy=%s", optimizedOutput, legacyOutput)
	}

	allRef, err := mailref.EncodeMailbox(testAccountID, []string{"All"})
	if err != nil {
		t.Fatalf("EncodeMailbox() error = %v", err)
	}
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: allRef, Limit: mail.DefaultPageLimit})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("ListMessages() error = %v, messages = %d", err, len(page.Messages))
	}
	ref = page.Messages[0].Ref
	base, err := store.messageBasePath(mustMailboxLocation(t, "imap://"+testAccountID+"/%5BGmail%5D/All"), 101)
	if err != nil {
		t.Fatalf("messageBasePath() error = %v", err)
	}
	if err := os.Remove(base + ".emlx"); err != nil {
		t.Fatalf("remove full test source: %v", err)
	}
	partialSource := raw[:bytes.Index(raw, []byte("BEGIN LARGE BODY"))+len("BEGIN LARGE BODY")+1024]
	framed := append([]byte(fmt.Sprintf("%-10d\n", len(partialSource))), partialSource...)
	framed = append(framed, validPlistTrailer()...)
	if err := os.WriteFile(base+".partial.emlx", framed, 0o600); err != nil {
		t.Fatalf("write partial test source: %v", err)
	}
	legacyPartial, err := store.GetMessage(context.Background(), ref)
	if err != nil {
		t.Fatalf("legacy partial GetMessage() error = %v", err)
	}
	optimizedPartial, err := client.GetMessageWithIntent(context.Background(), ref, mail.MessageReadIntentAttachments)
	if err != nil {
		t.Fatalf("partial attachment intent error = %v", err)
	}
	if optimizedPartial.ContentComplete || optimizedPartial.ContentSource != "emlx_partial" ||
		optimizedPartial.ContentComplete != legacyPartial.ContentComplete ||
		!reflect.DeepEqual(optimizedPartial.MissingParts, legacyPartial.MissingParts) ||
		!reflect.DeepEqual(optimizedPartial.Attachments, legacyPartial.Attachments) {
		t.Fatalf("partial evidence changed: optimized=%+v legacy=%+v", optimizedPartial, legacyPartial)
	}
	code, output, stderr = runReadIntentCLI(t, client, "attachments", "list", "--ref", ref, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("partial attachment JSON code = %d, stderr = %q, output = %s", code, stderr, output)
	}
	var partialJSON struct {
		Data struct {
			Attachments     []mail.Attachment `json:"attachments"`
			ContentSource   string            `json:"content_source"`
			ContentComplete bool              `json:"content_complete"`
			MissingParts    []string          `json:"missing_parts"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &partialJSON); err != nil {
		t.Fatalf("unmarshal partial attachment JSON: %v", err)
	}
	legacyCode, legacyOutput, legacyStderr = runLegacyReadIntentCLI(t, client,
		"attachments", "list", "--ref", ref, "--json",
	)
	if legacyCode != code || legacyStderr != stderr || !bytes.Equal([]byte(legacyOutput), []byte(output)) {
		t.Fatalf("partial attachment JSON changed: optimized=%s legacy=%s", output, legacyOutput)
	}
	if partialJSON.Data.ContentSource != legacyPartial.ContentSource || partialJSON.Data.ContentComplete ||
		!reflect.DeepEqual(partialJSON.Data.MissingParts, legacyPartial.MissingParts) ||
		!reflect.DeepEqual(partialJSON.Data.Attachments, legacyPartial.Attachments) {
		t.Fatalf("partial attachment JSON evidence = %+v, legacy = %+v", partialJSON.Data, legacyPartial)
	}
}

type readIntentHeaderFetcher struct {
	*metadataResolverStub
	headers          []byte
	headerFetchCalls int
	lastHeaderLimit  int64
}

func (operator *readIntentHeaderFetcher) FetchMessageHeaders(
	ctx context.Context,
	_ transport.ImapConfig,
	_ string,
	_ uint32,
	_ uint32,
	maxBytes int64,
) ([]byte, error) {
	operator.headerFetchCalls++
	operator.lastHeaderLimit = maxBytes
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(operator.headers)) > maxBytes {
		return nil, &transport.TransportError{Code: transport.CodeIMAPRawSourceTooLarge, Message: "header test literal exceeds bound"}
	}
	return append([]byte(nil), operator.headers...), nil
}

func TestMissingSourceHeaderIntentUsesBoundedHeaderFetcher(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "header-intent fixture store")
	installImapIdentityFixture(t, store, "metadata@gmail.com")
	updateFixtureMessage(t, store, `UPDATE messages SET message_id = NULL, remote_id = NULL, remote_mailbox = NULL WHERE ROWID = 102`)
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inboxRef, Limit: 3})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	ref := messageRefWithSubject(t, page.Messages, "Status Update")
	location, err := parseMailboxURL("imap://" + testAccountID + "/INBOX")
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.messageBasePath(location, 102)
	if err != nil {
		t.Fatalf("messageBasePath() error = %v", err)
	}
	if err := os.Remove(base + ".emlx"); err != nil {
		t.Fatalf("remove local source: %v", err)
	}
	resolver := &readIntentHeaderFetcher{
		metadataResolverStub: &metadataResolverStub{
			stubImapOperator: stubImapOperator{boxes: []transport.MailboxInfo{{Name: "INBOX"}}},
			identity:         transport.MessageIdentity{UID: 202, UIDValidity: 12345, MessageID: "<remote-102@example.com>"},
		},
		headers: []byte("From: Alice <alice@example.com>\r\nSubject: Status Update\r\nMessage-ID: <remote-102@example.com>\r\n\r\n"),
	}
	client := &Client{store: store, send: mail.SendTransport{
		Imap: resolver, Credentials: stubCredentials{"metadata@gmail.com": "secret"},
	}}
	message, err := client.GetMessageWithIntent(context.Background(), ref, mail.MessageReadIntentHeaders)
	if err != nil {
		t.Fatalf("GetMessageWithIntent(headers) error = %v", err)
	}
	if message.Summary.MessageID != "remote-102@example.com" || message.ContentSource != "imap_raw" ||
		message.Content != "" || resolver.headerFetchCalls != 1 || resolver.lastHeaderLimit != int64(maximumHeaderBytes) ||
		resolver.fetchCalls != 0 {
		t.Fatalf("header result=%+v header_fetches=%d bound=%d full_fetches=%d",
			message, resolver.headerFetchCalls, resolver.lastHeaderLimit, resolver.fetchCalls)
	}
}

func newLargeReadIntentFixture(t *testing.T, bodyBytes int) (*Store, string, []byte) {
	t.Helper()
	store, _ := newSearchFixture(t)
	t.Cleanup(func() { closeTestResource(t, store, "read-intent fixture store") })
	mailboxRef, err := mailref.EncodeMailbox(testAccountID, []string{"All"})
	if err != nil {
		t.Fatalf("EncodeMailbox() error = %v", err)
	}
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: mailboxRef, Limit: mail.DefaultPageLimit,
	})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("ListMessages() error = %v, messages = %d", err, len(page.Messages))
	}
	body := "BEGIN LARGE BODY" + strings.Repeat("x", bodyBytes) + "END LARGE BODY"
	raw := []byte(fmt.Sprintf(
		"From: Alice <alice@example.com>\r\nTo: Christopher <christopher@example.com>\r\nCc: Copy <copy@example.com>\r\nBcc: Blind <blind@example.com>\r\nReply-To: Reply <reply@example.com>\r\nSubject: Read intent fixture\r\nMessage-ID: <101@example.com>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=outer\r\n\r\n"+
			"--outer\r\nContent-Type: multipart/alternative; boundary=alternative\r\n\r\n"+
			"--alternative\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n"+
			"--alternative\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<html><body>%s</body></html>\r\n"+
			"--alternative--\r\n"+
			"--outer\r\nContent-Type: application/pdf; name=invoice.pdf\r\nContent-Disposition: attachment; filename=invoice.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n--outer--\r\n",
		body, body,
	))
	writeFixtureEMLX(t, store, 101, "imap://"+testAccountID+"/%5BGmail%5D/All", raw)
	return store, page.Messages[0].Ref, raw
}

func runReadIntentCLI(
	t *testing.T,
	client *Client,
	args ...string,
) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), mail.NewService(client), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func runLegacyReadIntentCLI(
	t *testing.T,
	gateway mail.Gateway,
	args ...string,
) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	service := mail.NewService(legacyReadIntentGateway{Gateway: gateway})
	code := cli.Run(context.Background(), service, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func mustMailboxLocation(t *testing.T, value string) mailboxLocation {
	t.Helper()
	location, err := parseMailboxURL(value)
	if err != nil {
		t.Fatalf("parseMailboxURL() error = %v", err)
	}
	return location
}
