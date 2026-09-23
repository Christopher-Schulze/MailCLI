package mail

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"mailcli/internal/transport"
)

type batchGateway struct {
	*gatewayStub
	mu              sync.Mutex
	reads           []string
	messages        map[string]Message
	readErrs        map[string]error
	markErr         error
	markSummary     MessageSummary
	transfers       []TransferMessageRequest
	transferErrs    map[string]error
	transferResults map[string]MessageSummary
	deletes         []DeleteMessageRequest
	deleteErrs      map[string]error
	deleteResults   map[string]DeleteResult
	started         chan struct{}
}

func (g *batchGateway) GetMessage(_ context.Context, ref string) (Message, error) {
	g.mu.Lock()
	g.reads = append(g.reads, ref)
	message := g.messages[ref]
	err := g.readErrs[ref]
	g.mu.Unlock()
	return message, err
}

func (g *batchGateway) MarkMessage(ctx context.Context, request MarkMessageRequest) (MessageSummary, error) {
	if g.started != nil {
		select {
		case <-g.started:
		default:
			close(g.started)
		}
	}
	if g.markErr != nil {
		return MessageSummary{}, g.markErr
	}
	if g.markSummary.Ref != "" {
		return g.markSummary, nil
	}
	<-ctx.Done()
	return MessageSummary{}, ctx.Err()
}

func (g *batchGateway) SaveAttachmentTo(_ context.Context, _ string, _ string, path string) error {
	return os.WriteFile(path, []byte("batch attachment"), 0o600)
}

func (g *batchGateway) TransferMessage(_ context.Context, request TransferMessageRequest) (MessageSummary, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.transfers = append(g.transfers, request)
	return g.transferResults[request.Ref], g.transferErrs[request.Ref]
}

func (g *batchGateway) DeleteMessage(_ context.Context, request DeleteMessageRequest) (DeleteResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deletes = append(g.deletes, request)
	return g.deleteResults[request.Ref], g.deleteErrs[request.Ref]
}

func TestExecuteBatchReadPreservesOrderAndPerItemErrors(t *testing.T) {
	gateway := &batchGateway{
		gatewayStub: &gatewayStub{},
		messages: map[string]Message{
			"ref-one":   {Summary: MessageSummary{Ref: "ref-one"}},
			"ref-three": {Summary: MessageSummary{Ref: "ref-three"}},
		},
		readErrs: map[string]error{"missing": &OperationError{Code: "not_found", Message: "message not found"}},
	}
	result, err := NewService(gateway).ExecuteBatch(context.Background(), BatchRequest{
		Operation:   BatchOperationRead,
		Concurrency: 2,
		Items: []BatchItem{
			{ID: "first", Ref: "ref-one"},
			{ID: "missing", Ref: "missing"},
			{ID: "third", Ref: "ref-three"},
		},
	})
	if err != nil {
		t.Fatalf("ExecuteBatch() error = %v", err)
	}
	if result.Complete() || result.Completed != 2 || result.Failed != 1 || result.Skipped != 0 {
		t.Fatalf("result counts = %+v", result)
	}
	if got := []string{result.Items[0].ID, result.Items[1].ID, result.Items[2].ID}; got[0] != "first" || got[1] != "missing" || got[2] != "third" {
		t.Fatalf("result order = %v", got)
	}
	if result.Items[0].State != BatchItemCompleted || result.Items[2].State != BatchItemCompleted {
		t.Fatalf("successful states = %+v", result.Items)
	}
	if result.Items[1].State != BatchItemFailed || result.Items[1].Error == nil || result.Items[1].Error.Code != "not_found" {
		t.Fatalf("failed item = %+v", result.Items[1])
	}
	if result.Items[0].Message == nil || result.Items[0].Message.Summary.Ref != "ref-one" {
		t.Fatalf("first message = %+v", result.Items[0].Message)
	}
	if len(gateway.reads) != 3 {
		t.Fatalf("gateway reads = %d, want 3", len(gateway.reads))
	}
}

func TestExecuteBatchRejectsDuplicateMutationIDsAndDestinationsBeforeEffects(t *testing.T) {
	gateway := &batchGateway{gatewayStub: &gatewayStub{}}
	service := NewService(gateway)
	if _, err := service.ExecuteBatch(context.Background(), BatchRequest{
		Operation: BatchOperationMark,
		Items:     []BatchItem{{ID: "same", Ref: "one", Read: boolPointer(true)}, {ID: "same", Ref: "two", Read: boolPointer(false)}},
	}); err == nil {
		t.Fatal("duplicate mark IDs were accepted")
	}
	destination := filepath.Join(t.TempDir(), "attachment.bin")
	if _, err := service.ExecuteBatch(context.Background(), BatchRequest{
		Operation: BatchOperationAttachmentSave,
		Items: []BatchItem{
			{ID: "one", Ref: "one", AttachmentID: "1", OutputPath: destination},
			{ID: "two", Ref: "two", AttachmentID: "2", OutputPath: destination},
		},
	}); err == nil {
		t.Fatal("duplicate attachment destinations were accepted")
	}
	if len(gateway.reads) != 0 {
		t.Fatalf("gateway was called during preflight: %v", gateway.reads)
	}
}

func TestExecuteBatchAttachmentAndMarkSuccess(t *testing.T) {
	gateway := &batchGateway{
		gatewayStub: &gatewayStub{},
		markSummary: MessageSummary{Ref: "mark-ref", Read: true},
	}
	path := filepath.Join(t.TempDir(), "saved.bin")
	attachment, err := NewService(gateway).ExecuteBatch(context.Background(), BatchRequest{
		Operation: BatchOperationAttachmentSave,
		Items:     []BatchItem{{ID: "attachment", Ref: "message", AttachmentID: "1", OutputPath: path}},
	})
	if err != nil || !attachment.Complete() || attachment.Items[0].SavedAttachment == nil {
		t.Fatalf("attachment result = %+v, error = %v", attachment, err)
	}
	mark, err := NewService(gateway).ExecuteBatch(context.Background(), BatchRequest{
		Operation: BatchOperationMark,
		Items:     []BatchItem{{ID: "mark", Ref: "message", Read: boolPointer(true)}},
	})
	if err != nil || !mark.Complete() || mark.Items[0].MessageState == nil || mark.Items[0].MessageState.Ref != "mark-ref" {
		t.Fatalf("mark result = %+v, error = %v", mark, err)
	}
}

func TestExecuteBatchMarkCancellationDistinguishesUncertainAndUnstarted(t *testing.T) {
	started := make(chan struct{})
	gateway := &batchGateway{gatewayStub: &gatewayStub{}, started: started}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan BatchResult, 1)
	go func() {
		result, _ := NewService(gateway).ExecuteBatch(ctx, BatchRequest{
			Operation:   BatchOperationMark,
			Concurrency: 1,
			Items: []BatchItem{
				{ID: "active", Ref: "one", Read: boolPointer(true)},
				{ID: "queued", Ref: "two", Read: boolPointer(true)},
			},
		})
		resultCh <- result
	}()
	<-started
	cancel()
	result := <-resultCh
	if result.Uncertain != 1 || result.Skipped != 1 || result.Completed != 0 {
		t.Fatalf("cancellation counts = %+v", result)
	}
	if result.Items[0].State != BatchItemUncertain || result.Items[1].State != BatchItemSkipped {
		t.Fatalf("cancellation states = %+v", result.Items)
	}
	if result.Items[1].Error == nil || result.Items[1].Error.Code != "batch_canceled" {
		t.Fatalf("unstarted evidence = %+v", result.Items[1].Error)
	}
}

func TestExecuteBatchMoveMixedOutcomes(t *testing.T) {
	gateway := &batchGateway{
		gatewayStub: &gatewayStub{},
		transferResults: map[string]MessageSummary{
			"ok":      {Ref: "ok"},
			"unknown": {Ref: "unknown", ServerTruth: &ServerMutationEvidence{Outcome: ServerMutationOutcomeUnknown}},
		},
		transferErrs: map[string]error{
			"rejected": &transport.TransportError{Code: transport.CodeIMAPMutationFailed, Message: "mailbox rejected"},
			"unknown": &transport.MutationOutcomeError{
				Code:    transport.CodeIMAPMoveOutcomeUnknown,
				Message: "expunge answer lost",
			},
		},
	}
	result, err := NewService(gateway).ExecuteBatch(context.Background(), BatchRequest{
		Operation:   BatchOperationMove,
		Concurrency: 2,
		Items: []BatchItem{
			{ID: "first", Ref: "ok", Mailbox: "archive"},
			{ID: "second", Ref: "rejected", Mailbox: "archive"},
			{ID: "third", Ref: "unknown", Mailbox: "archive", AllowDraftMutation: true},
		},
	})
	if err != nil {
		t.Fatalf("ExecuteBatch() error = %v", err)
	}
	if result.Completed != 1 || result.Failed != 1 || result.Uncertain != 1 || result.Skipped != 0 {
		t.Fatalf("result counts = %+v", result)
	}
	if result.Items[0].State != BatchItemCompleted || result.Items[0].MessageState == nil ||
		result.Items[0].MessageState.Ref != "ok" {
		t.Fatalf("completed item = %+v", result.Items[0])
	}
	if result.Items[1].State != BatchItemFailed || result.Items[1].Error == nil ||
		result.Items[1].Error.Code != transport.CodeIMAPMutationFailed {
		t.Fatalf("failed item = %+v", result.Items[1])
	}
	if result.Items[2].State != BatchItemUncertain || result.Items[2].MessageState == nil ||
		result.Items[2].MessageState.ServerTruth == nil ||
		!result.Items[2].MessageState.ServerTruth.OutcomeUnknown() {
		t.Fatalf("uncertain item evidence = %+v", result.Items[2])
	}
	for index, item := range result.Items {
		if item.Error != nil && item.Error.Retryable {
			t.Fatalf("item %d mutation error marked retryable: %+v", index, item.Error)
		}
	}
	if len(gateway.transfers) != 3 {
		t.Fatalf("transfers = %d, want 3", len(gateway.transfers))
	}
	draftForwarded := false
	for _, request := range gateway.transfers {
		if request.Copy || request.DestinationMailbox != "archive" {
			t.Fatalf("move request = %+v", request)
		}
		if request.Ref == "unknown" && request.AllowDraftMutation {
			draftForwarded = true
		}
	}
	if !draftForwarded {
		t.Fatalf("draft flag was not forwarded: %+v", gateway.transfers)
	}
}

func TestExecuteBatchCopyForwardsCopyFlag(t *testing.T) {
	gateway := &batchGateway{
		gatewayStub: &gatewayStub{},
		transferResults: map[string]MessageSummary{
			"one": {Ref: "one"},
		},
	}
	result, err := NewService(gateway).ExecuteBatch(context.Background(), BatchRequest{
		Operation: BatchOperationCopy,
		Items:     []BatchItem{{ID: "copy", Ref: "one", Mailbox: "archive"}},
	})
	if err != nil || !result.Complete() {
		t.Fatalf("copy result = %+v, error = %v", result, err)
	}
	if result.Items[0].MessageState == nil || result.Items[0].MessageState.Ref != "one" {
		t.Fatalf("copy evidence = %+v", result.Items[0])
	}
	if len(gateway.transfers) != 1 || !gateway.transfers[0].Copy || gateway.transfers[0].AllowDraftMutation {
		t.Fatalf("copy request = %+v", gateway.transfers)
	}
}

func TestExecuteBatchDeleteMixedOutcomes(t *testing.T) {
	gateway := &batchGateway{
		gatewayStub: &gatewayStub{},
		deleteResults: map[string]DeleteResult{
			"ok":      {MessageRef: "ok", Deleted: true},
			"unknown": {MessageRef: "unknown", ServerTruth: &ServerMutationEvidence{Outcome: ServerMutationOutcomeUnknown}},
		},
		deleteErrs: map[string]error{
			"unknown": &transport.MutationOutcomeError{
				Code:    transport.CodeIMAPMoveOutcomeUnknown,
				Message: "expunge answer lost",
			},
		},
	}
	result, err := NewService(gateway).ExecuteBatch(context.Background(), BatchRequest{
		Operation: BatchOperationDelete,
		Items: []BatchItem{
			{ID: "first", Ref: "ok"},
			{ID: "second", Ref: "unknown", AllowDraftMutation: true},
		},
	})
	if err != nil {
		t.Fatalf("ExecuteBatch() error = %v", err)
	}
	if result.Completed != 1 || result.Uncertain != 1 {
		t.Fatalf("result counts = %+v", result)
	}
	if result.Items[0].State != BatchItemCompleted || result.Items[0].DeleteResult == nil ||
		!result.Items[0].DeleteResult.Deleted {
		t.Fatalf("completed item = %+v", result.Items[0])
	}
	if result.Items[1].State != BatchItemUncertain || result.Items[1].DeleteResult == nil ||
		result.Items[1].DeleteResult.ServerTruth == nil {
		t.Fatalf("uncertain item evidence = %+v", result.Items[1])
	}
	if result.Items[1].Error == nil || result.Items[1].Error.Retryable {
		t.Fatalf("uncertain error must not be retryable: %+v", result.Items[1].Error)
	}
	if len(gateway.deletes) != 2 {
		t.Fatalf("delete request count = %d, want 2: %+v", len(gateway.deletes), gateway.deletes)
	}
	requestsByRef := make(map[string]DeleteMessageRequest, len(gateway.deletes))
	for _, request := range gateway.deletes {
		requestsByRef[request.Ref] = request
	}
	okRequest, hasOK := requestsByRef["ok"]
	unknownRequest, hasUnknown := requestsByRef["unknown"]
	if len(requestsByRef) != 2 || !hasOK || okRequest.AllowDraftMutation ||
		!hasUnknown || !unknownRequest.AllowDraftMutation {
		t.Fatalf("delete requests = %+v", gateway.deletes)
	}
}

func TestExecuteBatchMutationValidationRejectsStrayFieldsBeforeEffects(t *testing.T) {
	gateway := &batchGateway{gatewayStub: &gatewayStub{}}
	service := NewService(gateway)
	cases := []struct {
		name    string
		request BatchRequest
	}{
		{"move without mailbox", BatchRequest{Operation: BatchOperationMove, Items: []BatchItem{{ID: "a", Ref: "r"}}}},
		{"move with state field", BatchRequest{Operation: BatchOperationMove, Items: []BatchItem{{ID: "a", Ref: "r", Mailbox: "m", Read: boolPointer(true)}}}},
		{"copy without mailbox", BatchRequest{Operation: BatchOperationCopy, Items: []BatchItem{{ID: "a", Ref: "r"}}}},
		{"copy with draft flag", BatchRequest{Operation: BatchOperationCopy, Items: []BatchItem{{ID: "a", Ref: "r", Mailbox: "m", AllowDraftMutation: true}}}},
		{"delete with mailbox", BatchRequest{Operation: BatchOperationDelete, Items: []BatchItem{{ID: "a", Ref: "r", Mailbox: "m"}}}},
		{"delete with attachment", BatchRequest{Operation: BatchOperationDelete, Items: []BatchItem{{ID: "a", Ref: "r", AttachmentID: "1"}}}},
		{"mark with mailbox", BatchRequest{Operation: BatchOperationMark, Items: []BatchItem{{ID: "a", Ref: "r", Read: boolPointer(true), Mailbox: "m"}}}},
		{"read with mailbox", BatchRequest{Operation: BatchOperationRead, Items: []BatchItem{{ID: "a", Ref: "r", Mailbox: "m"}}}},
	}
	for _, test := range cases {
		if _, err := service.ExecuteBatch(context.Background(), test.request); err == nil {
			t.Fatalf("%s was accepted", test.name)
		}
	}
	if len(gateway.transfers) != 0 || len(gateway.deletes) != 0 || len(gateway.reads) != 0 {
		t.Fatal("gateway was called during preflight")
	}
}

func boolPointer(value bool) *bool {
	return &value
}
