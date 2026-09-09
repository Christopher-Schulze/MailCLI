package mail

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type batchGateway struct {
	*gatewayStub
	mu          sync.Mutex
	reads       []string
	messages    map[string]Message
	readErrs    map[string]error
	markErr     error
	markSummary MessageSummary
	started     chan struct{}
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

func boolPointer(value bool) *bool {
	return &value
}
