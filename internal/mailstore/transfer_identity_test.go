package mailstore

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

type transferIdentityOperator struct {
	*stubImapOperator
	messageID  string
	headers    int
	rawHeaders []byte
}

func (o *transferIdentityOperator) MoveMessage(ctx context.Context, cfg transport.ImapConfig, mailbox string, uid, validity uint32, destination, messageID string) (transport.MutationEvidence, error) {
	o.messageID = messageID
	if messageID == "" {
		return transport.MutationEvidence{}, fmt.Errorf("MOVE fallback has no independent Message-ID")
	}
	return o.stubImapOperator.MoveMessage(ctx, cfg, mailbox, uid, validity, destination, messageID)
}

func (o *transferIdentityOperator) DeleteMessage(ctx context.Context, cfg transport.ImapConfig, mailbox string, uid, validity uint32, messageID string) (transport.MutationEvidence, error) {
	o.messageID = messageID
	if messageID == "" {
		return transport.MutationEvidence{}, fmt.Errorf("Trash transfer has no independent Message-ID")
	}
	return o.stubImapOperator.DeleteMessage(ctx, cfg, mailbox, uid, validity, messageID)
}

func (o *transferIdentityOperator) FetchMessageHeaders(_ context.Context, _ transport.ImapConfig, mailbox string, uid, validity uint32, limit int64) ([]byte, error) {
	o.headers++
	if mailbox != "INBOX" || uid != 5001 || validity != 900 || limit != int64(maximumHeaderBytes) {
		return nil, fmt.Errorf("unexpected header identity %s %d %d, limit %d", mailbox, uid, validity, limit)
	}
	if o.rawHeaders != nil {
		return o.rawHeaders, nil
	}
	return []byte("From: Sender <sender@example.com>\r\nSubject: Status Update\r\nMessage-ID: <102@example.com>\r\n\r\n"), nil
}

func TestTransferResolvesMessageIDForMappedUID(t *testing.T) {
	for _, test := range []struct {
		operation string
		missing   bool
	}{
		{operation: "copy"}, {operation: "move"}, {operation: "delete"},
		{operation: "copy", missing: true}, {operation: "move", missing: true}, {operation: "delete", missing: true},
	} {
		t.Run(fmt.Sprintf("%s/missing=%t", test.operation, test.missing), func(t *testing.T) {
			client, operator, ref := mappedTransferFixture(t, test.missing)
			err := runMappedTransfer(t, client, ref, test.operation)
			if err != nil || operator.mutationCalls != 1 || (test.operation != "copy" && operator.messageID != "102@example.com") {
				t.Fatalf("transfer error=%v calls=%d messageID=%q", err, operator.mutationCalls, operator.messageID)
			}
			wantHeaders := 0
			if test.missing {
				wantHeaders = 1
			}
			if operator.headers != wantHeaders || operator.lastFetchMax != 0 {
				t.Fatalf("header reads=%d want=%d full fetch bound=%d", operator.headers, wantHeaders, operator.lastFetchMax)
			}
		})
	}
}

func mappedTransferFixture(t *testing.T, missing bool) (*Client, *transferIdentityOperator, string) {
	t.Helper()
	client, recent := newMessagesFixture(t, "transfer-identity@gmail.com", nil)
	operator := &transferIdentityOperator{stubImapOperator: recent.stubImapOperator}
	operator.boxes = append(operator.boxes, transport.MailboxInfo{Name: "Trash", Flags: []string{`\Trash`}})
	operator.uidvalidity = 900
	operator.searchMatchesByMailbox = map[string][]int{"Sent": {0, 1}}
	client.send.Imap = operator
	mailbox, err := mailref.EncodeMailbox(testAccountID, []string{"INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: mailbox, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	ref := messageRefWithSubject(t, page.Messages, "Status Update")
	identity, err := mailref.DecodeMessage(ref)
	if err != nil || identity.ExpectedMessageID != "" || identity.ExpectedIMAPUID != 5001 {
		t.Fatalf("fixture ref=%+v error=%v", identity, err)
	}
	if missing {
		if err := os.Remove(mappedTransferSourcePath(t, client.store)); err != nil {
			t.Fatal(err)
		}
	}
	return client, operator, ref
}

func mappedTransferSourcePath(t *testing.T, store *Store) string {
	t.Helper()
	path, err := store.messageBasePath(mustMailboxLocation(t, "imap://"+testAccountID+"/INBOX"), 102)
	if err != nil {
		t.Fatal(err)
	}
	return path + ".emlx"
}

func runMappedTransfer(t *testing.T, client *Client, ref, operation string) error {
	t.Helper()
	if operation == "delete" {
		_, err := client.DeleteMessage(context.Background(), mail.DeleteMessageRequest{Ref: ref})
		return err
	}
	destination, err := mailref.EncodeMailbox(testAccountID, []string{"Sent"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.TransferMessage(context.Background(), mail.TransferMessageRequest{Ref: ref, DestinationMailbox: destination, Copy: operation == "copy"})
	if err == nil && result.MessageID != "102@example.com" {
		t.Fatalf("transfer lost resolved Message-ID: %+v", result)
	}
	return err
}

func TestMappedTransferKeepsGuardedCopy(t *testing.T) {
	client, operator, ref := mappedTransferFixture(t, false)
	guarded := &guardedCopyOperator{stubImapOperator: operator.stubImapOperator}
	operator.searchMatchesByMailbox["Sent"] = []int{1}
	client.send.Imap = guarded
	if err := runMappedTransfer(t, client, ref, "copy"); err != nil {
		t.Fatal(err)
	}
	if guarded.guardedID != "102@example.com" || guarded.guardCalls != 1 || guarded.copiedUnderIt != 1 || guarded.legacyCopies != 0 {
		t.Fatalf("guarded COPY lost identity or ordering: %+v", guarded)
	}
}

func TestMappedCopyRejectsGenuinelyMissingMessageID(t *testing.T) {
	client, operator, ref := mappedTransferFixture(t, true)
	operator.rawHeaders = []byte("Subject: Status Update\r\n\r\n")
	err := runMappedTransfer(t, client, ref, "copy")
	if transport.ErrorCode(err) != transport.CodeIMAPCopyOutcomeUnknown || operator.mutationCalls != 0 || operator.headers != 1 {
		t.Fatalf("missing identity error=%v mutations=%d headers=%d", err, operator.mutationCalls, operator.headers)
	}
}

func TestMappedFlagIdentitySkipsHeaderReads(t *testing.T) {
	client, operator, ref := mappedTransferFixture(t, false)
	raw := "Subject: " + strings.Repeat("x", maximumHeaderBytes) + "\r\n\r\n"
	framed := append([]byte(fmt.Sprintf("%010d\n%s", len(raw), raw)), validPlistTrailer()...)
	if err := os.WriteFile(mappedTransferSourcePath(t, client.store), framed, 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := client.store.openRawSource(context.Background(), ref)
	if err != nil {
		t.Fatalf("oversized RFC header fixture has invalid EMLX framing: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	target, err := client.resolveImapTargetForMutation(context.Background(), ref)
	if err != nil || target.uid != 5001 || target.messageID != "" || operator.headers != 0 {
		t.Fatalf("flag identity=%+v error=%v headers=%d", target, err, operator.headers)
	}
	if err := runMappedTransfer(t, client, ref, "copy"); transport.ErrorCode(err) != "invalid_message_source" || operator.mutationCalls != 0 {
		t.Fatalf("invalid local headers transfer error=%v mutations=%d", err, operator.mutationCalls)
	}
}

func TestMappedNativeTransferWithoutAvailableHeaderReader(t *testing.T) {
	for _, operation := range []string{"move", "delete", "copy"} {
		t.Run(operation, func(t *testing.T) {
			client, operator, ref := mappedTransferFixture(t, true)
			client.send.Imap = operator.stubImapOperator
			var err error
			if operation == "delete" {
				_, err = client.DeleteMessage(context.Background(), mail.DeleteMessageRequest{Ref: ref})
			} else {
				destination, encodeErr := mailref.EncodeMailbox(testAccountID, []string{"Sent"})
				if encodeErr != nil {
					t.Fatal(encodeErr)
				}
				_, err = client.TransferMessage(context.Background(), mail.TransferMessageRequest{Ref: ref, DestinationMailbox: destination, Copy: operation == "copy"})
			}
			if operation == "copy" {
				if transport.ErrorCode(err) != transport.CodeIMAPCopyOutcomeUnknown || operator.mutationCalls != 0 {
					t.Fatalf("COPY without identity error=%v mutations=%d", err, operator.mutationCalls)
				}
			} else if err != nil || operator.mutationCalls != 1 {
				t.Fatalf("verified UID native transfer error=%v mutations=%d", err, operator.mutationCalls)
			}
			if operator.headers != 0 || operator.lastFetchMax != 0 {
				t.Fatalf("unsupported header reader fetched content: headers=%d full=%d", operator.headers, operator.lastFetchMax)
			}
		})
	}
}
