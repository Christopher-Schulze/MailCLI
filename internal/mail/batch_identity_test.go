package mail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"mailcli/internal/mailref"
)

func TestBatchConflictsUseDecodedReferenceIdentity(t *testing.T) {
	message := mailref.Message{
		Version: 1, AccountID: "batch-account", MailboxPath: []string{"Inbox"}, LibraryID: "42",
		ExpectedStoreUUID: "batch-store", ExpectedStoreMailboxID: 1, ExpectedStoreMessageID: 7,
	}
	payload, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	legacy := "msg_" + base64.RawURLEncoding.EncodeToString(payload)
	compact, err := mailref.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	firstMailbox := batchTestMailboxRef(t, "Caf\u00e9")
	secondMailbox := batchTestMailboxRef(t, "Cafe\u0301")
	for _, operation := range []string{BatchOperationMark, BatchOperationMove, BatchOperationDelete, BatchOperationCopy} {
		t.Run(operation, func(t *testing.T) {
			items := []BatchItem{{ID: "first", Ref: legacy}, {ID: "second", Ref: compact}}
			if operation == BatchOperationMark {
				items[0].Read, items[1].Read = boolPointer(true), boolPointer(false)
			}
			if operation == BatchOperationMove || operation == BatchOperationCopy {
				items[0].Mailbox, items[1].Mailbox = firstMailbox, secondMailbox
			}
			gateway := &batchGateway{gatewayStub: &gatewayStub{}}
			_, err := NewService(gateway).ExecuteBatch(context.Background(), BatchRequest{Operation: operation, Items: items})
			if errorCode(err) != "invalid_argument" || !strings.Contains(err.Error(), "first") || !strings.Contains(err.Error(), "second") {
				t.Fatalf("decoded conflict was not rejected: %v", err)
			}
			if strings.Contains(err.Error(), legacy) || strings.Contains(err.Error(), compact) ||
				len(gateway.markRequests)+len(gateway.transfers)+len(gateway.deletes) != 0 {
				t.Fatalf("conflict leaked refs or dispatched effects: %v", err)
			}
		})
	}
}

func TestBatchRejectsInvalidReferencesBeforeDispatch(t *testing.T) {
	for _, test := range []struct {
		name    string
		ref     string
		mailbox string
	}{
		{name: "invalid message", ref: "msg_QR", mailbox: "Archive"},
		{name: "invalid mailbox", ref: batchTestMessageRef(t, "42"), mailbox: "mbx_QR"},
	} {
		t.Run(test.name, func(t *testing.T) {
			gateway := &batchGateway{gatewayStub: &gatewayStub{}}
			_, err := NewService(gateway).ExecuteBatch(context.Background(), BatchRequest{
				Operation: BatchOperationCopy, Items: []BatchItem{{ID: "item", Ref: test.ref, Mailbox: test.mailbox}},
			})
			if errorCode(err) != "invalid_reference" || len(gateway.transfers) != 0 {
				t.Fatalf("invalid ref accepted: %v, effects=%+v", err, gateway.transfers)
			}
		})
	}
}
