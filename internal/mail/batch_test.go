package mail

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"mailcli/internal/transport"
)

type batchGateway struct {
	*gatewayStub
	mu              sync.Mutex
	reads           []string
	markRequests    []MarkMessageRequest
	messages        map[string]Message
	readErrs        map[string]error
	attachmentErr   error
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
	g.mu.Lock()
	g.markRequests = append(g.markRequests, request)
	g.mu.Unlock()
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
	if g.attachmentErr != nil {
		return g.attachmentErr
	}
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

func TestExecuteBatchReadBudgetsConcurrentContentBeforeRetention(t *testing.T) {
	const itemCount = 100
	const budget = int64(32 * 1024)
	body := strings.Repeat("<&\n", 2048)
	header := strings.Repeat("X", 256)
	messages := make(map[string]Message, itemCount)
	items := make([]BatchItem, itemCount)
	for index := range items {
		ref := "ref-" + strconv.Itoa(index)
		messages[ref] = Message{
			Summary: MessageSummary{Ref: ref}, Content: body, Headers: header,
		}
		items[index] = BatchItem{ID: "item-" + strconv.Itoa(index), Ref: ref, RetainReadContent: true, RetainReadHeaders: true}
	}
	result, err := NewService(&batchGateway{gatewayStub: &gatewayStub{}, messages: messages}).ExecuteBatch(
		context.Background(), BatchRequest{
			Operation: BatchOperationRead, Concurrency: MaximumBatchConcurrency,
			Items: items, ReadContentBudgetBytes: budget,
		},
	)
	if err != nil {
		t.Fatalf("ExecuteBatch() error = %v", err)
	}
	if !result.ReadContentExceeded || result.ReadContentRequiredBytes <= budget {
		t.Fatalf("content budget result = exceeded:%t required:%d", result.ReadContentExceeded, result.ReadContentRequiredBytes)
	}
	if len(result.Items) != itemCount || result.Completed != itemCount {
		t.Fatalf("batch result counts = %+v", result)
	}
	retainedBytes := int64(0)
	retainedMessages := 0
	for index, item := range result.Items {
		if item.ID != "item-"+strconv.Itoa(index) || item.State != BatchItemCompleted || item.Message == nil {
			t.Fatalf("item %d order/state = %+v", index, item)
		}
		if item.Message.Content != "" || item.Message.Headers != "" {
			retainedMessages++
			retainedBytes += jsonStringOutputBytes(item.Message.Content) + jsonStringOutputBytes(item.Message.Headers)
		}
	}
	if retainedMessages == 0 || retainedBytes > budget {
		t.Fatalf("retained messages/encoded bytes = %d/%d, budget %d", retainedMessages, retainedBytes, budget)
	}
}

func TestJSONStringOutputBytesMatchesJSONEncoding(t *testing.T) {
	for _, value := range []string{
		"plain text", `quote"slash\\`, "<>&", "\x00\x08\n\r\t", "\u2028\u2029", "\xff",
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if got := jsonStringOutputBytes(value); got != int64(len(encoded)) {
			t.Errorf("jsonStringOutputBytes(%q) = %d, encoded size = %d (%s)", value, got, len(encoded), encoded)
		}
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

func TestExecuteBatchRejectsConflictingMessageRefsBeforeDispatch(t *testing.T) {
	cases := []struct {
		name    string
		request BatchRequest
		ref     string
	}{
		{
			name: "contradictory marks",
			request: BatchRequest{Operation: BatchOperationMark, Items: []BatchItem{
				{ID: "mark-read", Ref: "private-mark-ref", Read: boolPointer(true)},
				{ID: "mark-unread", Ref: "private-mark-ref", Read: boolPointer(false)},
			}},
			ref: "private-mark-ref",
		},
		{
			name: "matching marks",
			request: BatchRequest{Operation: BatchOperationMark, Items: []BatchItem{
				{ID: "mark-first", Ref: "private-same-ref", Read: boolPointer(true)},
				{ID: "mark-second", Ref: "private-same-ref", Read: boolPointer(true)},
			}},
			ref: "private-same-ref",
		},
		{
			name: "moves",
			request: BatchRequest{Operation: BatchOperationMove, Items: []BatchItem{
				{ID: "move-first", Ref: "private-move-ref", Mailbox: "archive"},
				{ID: "move-second", Ref: "private-move-ref", Mailbox: "trash"},
			}},
			ref: "private-move-ref",
		},
		{
			name: "deletes",
			request: BatchRequest{Operation: BatchOperationDelete, Items: []BatchItem{
				{ID: "delete-first", Ref: "private-delete-ref"},
				{ID: "delete-second", Ref: "private-delete-ref"},
			}},
			ref: "private-delete-ref",
		},
		{
			name: "repeated copy pair",
			request: BatchRequest{Operation: BatchOperationCopy, Items: []BatchItem{
				{ID: "copy-first", Ref: "private-copy-ref", Mailbox: "archive"},
				{ID: "copy-second", Ref: "private-copy-ref", Mailbox: "archive"},
			}},
			ref: "private-copy-ref",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			gateway := &batchGateway{
				gatewayStub: &gatewayStub{}, markSummary: MessageSummary{Ref: "completed-mark"},
			}
			_, err := NewService(gateway).ExecuteBatch(context.Background(), test.request)
			if err == nil {
				t.Fatal("conflicting message refs were accepted")
			}
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error = %T %v, want validation error", err, err)
			}
			if validationErr.ErrorCode() != "invalid_argument" {
				t.Fatalf("validation error code = %q, want invalid_argument", validationErr.ErrorCode())
			}
			if strings.Contains(err.Error(), test.ref) {
				t.Fatalf("validation error exposes message ref: %v", err)
			}
			if !strings.Contains(err.Error(), test.request.Items[0].ID) ||
				!strings.Contains(err.Error(), test.request.Items[1].ID) {
				t.Fatalf("validation error omits conflicting item IDs: %v", err)
			}
			if len(gateway.markRequests) != 0 || len(gateway.transfers) != 0 || len(gateway.deletes) != 0 {
				t.Fatalf("mutation gateway calls occurred before validation: marks=%d transfers=%d deletes=%d",
					len(gateway.markRequests), len(gateway.transfers), len(gateway.deletes))
			}
		})
	}
}

func TestExecuteBatchAllowsIndependentMutationRefsInInputOrder(t *testing.T) {
	const concurrency = 1
	refs := []string{"ref-z", "ref-a", "ref-middle"}
	ids := []string{"item-z", "item-a", "item-middle"}
	for _, operation := range []BatchOperation{BatchOperationMark, BatchOperationMove, BatchOperationDelete} {
		t.Run(operation, func(t *testing.T) {
			gateway := &batchGateway{
				gatewayStub: &gatewayStub{}, markSummary: MessageSummary{Ref: "marked"},
			}
			items := make([]BatchItem, len(refs))
			for index, ref := range refs {
				items[index] = BatchItem{ID: ids[index], Ref: ref}
				switch operation {
				case BatchOperationMark:
					items[index].Read = boolPointer(index%2 == 0)
				case BatchOperationMove:
					items[index].Mailbox = "archive"
				}
			}
			result, err := NewService(gateway).ExecuteBatch(context.Background(), BatchRequest{
				Operation: operation, Concurrency: concurrency, Items: items,
			})
			if err != nil || !result.Complete() {
				t.Fatalf("ExecuteBatch() = (%+v, %v), want complete result", result, err)
			}
			for index, item := range result.Items {
				if item.ID != ids[index] {
					t.Fatalf("result item %d ID = %q, want %q", index, item.ID, ids[index])
				}
			}
			var gotRefs []string
			switch operation {
			case BatchOperationMark:
				for _, request := range gateway.markRequests {
					gotRefs = append(gotRefs, request.Ref)
				}
			case BatchOperationMove:
				for _, request := range gateway.transfers {
					gotRefs = append(gotRefs, request.Ref)
				}
			case BatchOperationDelete:
				for _, request := range gateway.deletes {
					gotRefs = append(gotRefs, request.Ref)
				}
			}
			if !reflect.DeepEqual(gotRefs, refs) {
				t.Fatalf("mutation refs = %v, want input order %v", gotRefs, refs)
			}
		})
	}
}

func TestExecuteBatchCopyAllowsDistinctDestinationsInInputOrder(t *testing.T) {
	items := []BatchItem{
		{ID: "copy-z", Ref: "shared-ref", Mailbox: "archive"},
		{ID: "copy-a", Ref: "shared-ref", Mailbox: "sent"},
		{ID: "copy-middle", Ref: "other-ref", Mailbox: "archive"},
	}
	gateway := &batchGateway{gatewayStub: &gatewayStub{}}
	result, err := NewService(gateway).ExecuteBatch(context.Background(), BatchRequest{
		Operation: BatchOperationCopy, Concurrency: 1, Items: items,
	})
	if err != nil || !result.Complete() {
		t.Fatalf("ExecuteBatch() = (%+v, %v), want complete result", result, err)
	}
	if len(gateway.transfers) != len(items) {
		t.Fatalf("copy effects = %+v, want %d", gateway.transfers, len(items))
	}
	for index, item := range result.Items {
		if item.ID != items[index].ID {
			t.Fatalf("result item %d ID = %q, want %q", index, item.ID, items[index].ID)
		}
		request := gateway.transfers[index]
		if !request.Copy || request.Ref != items[index].Ref || request.DestinationMailbox != items[index].Mailbox {
			t.Fatalf("copy effect %d = %+v, want request for %+v", index, request, items[index])
		}
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

type batchAttachmentSaveCase struct {
	name          string
	certainty     EffectCertainty
	replayAllowed bool
	saved         bool
}

func TestExecuteBatchAttachmentSaveOutcomeEvidence(t *testing.T) {
	for _, test := range []batchAttachmentSaveCase{
		{name: "before publish", certainty: EffectNone, replayAllowed: true},
		{name: "after publish", certainty: EffectComplete, saved: true},
		{name: "ambiguous cleanup", certainty: EffectUnknown},
	} {
		t.Run(test.name, func(t *testing.T) { runBatchAttachmentSaveCase(t, test) })
	}
}

func TestBatchAttachmentSaveUnknownErrorDisallowsRetry(t *testing.T) {
	run := batchExecution{request: BatchRequest{Operation: BatchOperationAttachmentSave}}
	itemError := run.itemError(errors.New("attachment publication setup failed"))
	if itemError.Retryable || itemError.Guidance == nil || itemError.Guidance.ReplayAllowed ||
		itemError.Guidance.EffectCertainty != EffectUnknown {
		t.Fatalf("attachment item error = %+v, want conservative non-retryable guidance", itemError)
	}
}

func runBatchAttachmentSaveCase(t *testing.T, test batchAttachmentSaveCase) {
	t.Helper()
	output := filepath.Join(t.TempDir(), "attachment.bin")
	gateway := &batchGateway{gatewayStub: &gatewayStub{}}
	configureBatchAttachmentSaveCase(t, test.name, gateway, output)
	result, err := NewService(gateway).ExecuteBatch(context.Background(), BatchRequest{
		Operation: BatchOperationAttachmentSave,
		Items:     []BatchItem{{ID: "save", Ref: "message", AttachmentID: "1", OutputPath: output}},
	})
	if err != nil || result.Complete() || len(result.Items) != 1 {
		t.Fatalf("ExecuteBatch() = (%+v, %v), want one failed item", result, err)
	}
	assertBatchAttachmentSaveOutcome(t, test, result.Items[0], output)
}

func configureBatchAttachmentSaveCase(t *testing.T, name string, gateway *batchGateway, output string) {
	t.Helper()
	switch name {
	case "before publish":
		gateway.attachmentErr = context.DeadlineExceeded
	case "after publish":
		closeErr := errors.New("close failed")
		previous := attachmentClose
		attachmentClose = func(file *os.File) error { return errors.Join(file.Close(), closeErr) }
		t.Cleanup(func() { attachmentClose = previous })
	case "ambiguous cleanup":
		setAttachmentPublicationHook(t, func(stage string, path string) error {
			if stage != "before-cleanup" || path != output {
				return nil
			}
			if err := os.Rename(path, path+".original"); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("replacement"), 0o600)
		})
	}
}

func assertBatchAttachmentSaveOutcome(t *testing.T, test batchAttachmentSaveCase, item BatchItemResult, output string) {
	t.Helper()
	if item.State != BatchItemFailed || item.Error == nil || item.Error.Guidance == nil {
		t.Fatalf("item = %+v, want failed item with guidance", item)
	}
	guidance := item.Error.Guidance
	if guidance.EffectCertainty != test.certainty || guidance.ReplayAllowed != test.replayAllowed ||
		item.Error.Retryable != guidance.ReplayAllowed {
		t.Fatalf("item error = %+v, want certainty %q and retryable %t", item.Error, test.certainty, test.replayAllowed)
	}
	if test.saved {
		if item.SavedAttachment == nil {
			t.Fatal("saved attachment = nil, want verified output evidence")
		}
		assertSavedAttachmentEvidence(t, *item.SavedAttachment, output, []byte("batch attachment"))
		assertAttachmentFile(t, output, []byte("batch attachment"), 0o600)
	} else if item.SavedAttachment != nil {
		t.Fatalf("saved attachment = %+v, want no verified output evidence", item.SavedAttachment)
	}
	assertBatchAttachmentSaveTarget(t, test.certainty, output)
}

func assertBatchAttachmentSaveTarget(t *testing.T, certainty EffectCertainty, output string) {
	t.Helper()
	if certainty == EffectNone {
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			t.Fatalf("pre-publish output exists: %v", err)
		}
	}
	if certainty == EffectUnknown {
		assertAttachmentFile(t, output, []byte("replacement"), 0o600)
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
