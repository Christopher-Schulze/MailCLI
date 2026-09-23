package mail

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"mailcli/internal/transport"
)

const (
	DefaultBatchConcurrency = 2
	MaximumBatchConcurrency = 8
	MaximumBatchItems       = 100
	MaximumBatchInputBytes  = 16 * 1024 * 1024
)

type BatchOperation = string

const (
	BatchOperationRead           = "read"
	BatchOperationAttachmentSave = "attachment_save"
	BatchOperationMark           = "mark"
	BatchOperationMove           = "move"
	BatchOperationCopy           = "copy"
	BatchOperationDelete         = "delete"
)

type BatchItemState = string

const (
	BatchItemCompleted = "completed"
	BatchItemFailed    = "failed"
	BatchItemSkipped   = "skipped"
	BatchItemUncertain = "uncertain"
)

// BatchRequest is an explicit, bounded set of independent operations. Items
// are never discovered implicitly and retain their input order in the result.
type BatchRequest struct {
	Operation              BatchOperation `json:"operation"`
	Items                  []BatchItem    `json:"items"`
	Concurrency            int            `json:"concurrency,omitempty"`
	ReadContentBudgetBytes int64          `json:"-"`
}

// BatchItem identifies one request. Ref is a store-bound message reference for
// every operation; attachment fields are used only by attachment_save, state
// fields only by mark, Mailbox by move/copy, and view/fields by read.
type BatchItem struct {
	ID                 string    `json:"id"`
	Ref                string    `json:"ref"`
	AttachmentID       string    `json:"attachment_id"`
	OutputPath         string    `json:"output_path"`
	Read               *bool     `json:"read"`
	Flagged            *bool     `json:"flagged"`
	Junk               *bool     `json:"junk"`
	Mailbox            string    `json:"mailbox"`
	AllowDraftMutation bool      `json:"allow_draft_mutation,omitempty"`
	View               *string   `json:"view,omitempty"`
	Fields             *[]string `json:"fields,omitempty"`
	RetainReadContent  bool      `json:"-"`
	RetainReadHeaders  bool      `json:"-"`
}

type BatchItemError struct {
	Code      string             `json:"code"`
	Message   string             `json:"message"`
	Retryable bool               `json:"retryable,omitempty"`
	Guidance  *OperationGuidance `json:"guidance,omitempty"`
}

type BatchItemResult struct {
	ID              string           `json:"id"`
	State           BatchItemState   `json:"state"`
	Message         *Message         `json:"message,omitempty"`
	MessageState    *MessageSummary  `json:"message_state,omitempty"`
	SavedAttachment *SavedAttachment `json:"saved_attachment,omitempty"`
	DeleteResult    *DeleteResult    `json:"delete_result,omitempty"`
	Error           *BatchItemError  `json:"error,omitempty"`
}

type BatchResult struct {
	Operation                BatchOperation    `json:"operation"`
	Concurrency              int               `json:"concurrency"`
	Total                    int               `json:"total"`
	Completed                int               `json:"completed"`
	Failed                   int               `json:"failed"`
	Skipped                  int               `json:"skipped"`
	Uncertain                int               `json:"uncertain"`
	Items                    []BatchItemResult `json:"items"`
	ReadContentExceeded      bool              `json:"-"`
	ReadContentRequiredBytes int64             `json:"-"`
}

type batchExecution struct {
	ctx        context.Context
	service    *Service
	request    BatchRequest
	result     *BatchResult
	jobs       chan int
	wait       sync.WaitGroup
	next       int
	readBudget *batchReadBudget
}

type batchReadBudget struct {
	mu            sync.Mutex
	remaining     int64
	requiredBytes int64
	exceeded      bool
}

func (r BatchResult) Complete() bool {
	return r.Total > 0 && r.Completed == r.Total
}

func (s *Service) ExecuteBatch(ctx context.Context, request BatchRequest) (BatchResult, error) {
	if s == nil || s.gateway == nil {
		return BatchResult{}, &OperationError{Code: "mail_service_unavailable", Message: "mail service is unavailable"}
	}
	concurrency, err := validateBatchRequest(request)
	if err != nil {
		return BatchResult{}, err
	}
	request.Concurrency = concurrency
	result := BatchResult{
		Operation: request.Operation, Concurrency: concurrency, Total: len(request.Items),
		Items: make([]BatchItemResult, len(request.Items)),
	}
	canceledError := &OperationError{Code: "batch_canceled", Message: "batch was canceled before item started"}
	canceledGuidance := GuidanceForError(string(request.Operation), canceledError)
	for index, item := range request.Items {
		result.Items[index] = BatchItemResult{
			ID: item.ID, State: BatchItemSkipped,
			Error: &BatchItemError{
				Code: canceledError.Code, Message: canceledError.Message, Guidance: &canceledGuidance,
			},
		}
	}
	execution := batchExecution{ctx: ctx, service: s, request: request, result: &result}
	if request.Operation == BatchOperationRead && request.ReadContentBudgetBytes > 0 {
		execution.readBudget = &batchReadBudget{remaining: request.ReadContentBudgetBytes}
	}
	execution.run()
	if execution.readBudget != nil {
		result.ReadContentExceeded = execution.readBudget.exceeded
		result.ReadContentRequiredBytes = execution.readBudget.requiredBytes
	}
	return result, nil
}

func (run *batchExecution) run() {
	run.jobs = make(chan int)
	run.wait.Add(min(run.request.Concurrency, len(run.request.Items)))
	for worker := 0; worker < run.request.Concurrency && worker < len(run.request.Items); worker++ {
		go run.worker()
	}
	for run.next < len(run.request.Items) {
		if run.ctx.Err() != nil {
			run.next = len(run.request.Items)
			break
		}
		select {
		case run.jobs <- run.next:
			run.next++
		case <-run.ctx.Done():
			run.next = len(run.request.Items)
		}
	}
	close(run.jobs)
	run.wait.Wait()
	for index := run.next; index < len(run.result.Items); index++ {
		run.result.Items[index].State = BatchItemSkipped
	}
	for _, item := range run.result.Items {
		switch item.State {
		case BatchItemCompleted:
			run.result.Completed++
		case BatchItemFailed:
			run.result.Failed++
		case BatchItemUncertain:
			run.result.Uncertain++
		case BatchItemSkipped:
			run.result.Skipped++
		}
	}
}

func (run *batchExecution) worker() {
	defer run.wait.Done()
	for index := range run.jobs {
		if run.ctx.Err() == nil {
			run.result.Items[index] = run.execute(run.request.Items[index])
		}
	}
}

func validateBatchRequest(request BatchRequest) (int, error) {
	switch request.Operation {
	case BatchOperationRead, BatchOperationAttachmentSave, BatchOperationMark,
		BatchOperationMove, BatchOperationCopy, BatchOperationDelete:
	default:
		return 0, validationError(fmt.Sprintf("unsupported batch operation %q", request.Operation))
	}
	if len(request.Items) == 0 {
		return 0, validationError("batch requires at least one item")
	}
	if len(request.Items) > MaximumBatchItems {
		return 0, validationError(fmt.Sprintf("batch contains %d items; maximum is %d", len(request.Items), MaximumBatchItems))
	}
	concurrency := request.Concurrency
	if concurrency == 0 {
		concurrency = DefaultBatchConcurrency
	}
	if concurrency < 1 || concurrency > MaximumBatchConcurrency {
		return 0, validationError(fmt.Sprintf("batch concurrency must be between 1 and %d", MaximumBatchConcurrency))
	}
	if request.ReadContentBudgetBytes < 0 || request.ReadContentBudgetBytes > MaximumRawSourceBytes {
		return 0, validationError(fmt.Sprintf("batch read content budget must be between 0 and %d bytes", MaximumRawSourceBytes))
	}
	ids := make(map[string]struct{}, len(request.Items))
	destinations := make(map[string]string)
	for _, item := range request.Items {
		if item.ID == "" || item.ID != strings.TrimSpace(item.ID) {
			return 0, validationError("batch item id must be non-empty and trimmed")
		}
		if _, exists := ids[item.ID]; exists {
			return 0, validationError(fmt.Sprintf("duplicate batch item id %q", item.ID))
		}
		ids[item.ID] = struct{}{}
		if item.Ref == "" {
			return 0, validationError(fmt.Sprintf("batch item %q requires ref", item.ID))
		}
		switch request.Operation {
		case BatchOperationRead:
			if item.AttachmentID != "" || item.OutputPath != "" || item.Read != nil || item.Flagged != nil || item.Junk != nil ||
				item.Mailbox != "" || item.AllowDraftMutation {
				return 0, validationError(fmt.Sprintf("batch read item %q contains unsupported fields", item.ID))
			}
			if item.View != nil && item.Fields != nil {
				return 0, validationError(fmt.Sprintf("batch read item %q cannot combine view and fields", item.ID))
			}
		case BatchOperationAttachmentSave:
			if item.AttachmentID == "" || item.OutputPath == "" {
				return 0, validationError(fmt.Sprintf("batch attachment item %q requires attachment_id and output_path", item.ID))
			}
			if item.Read != nil || item.Flagged != nil || item.Junk != nil || item.Mailbox != "" || item.AllowDraftMutation || item.View != nil || item.Fields != nil {
				return 0, validationError(fmt.Sprintf("batch attachment item %q contains unsupported fields", item.ID))
			}
			if err := validateAttachmentRequest(SaveAttachmentRequest{
				MessageRef: item.Ref, AttachmentID: item.AttachmentID, OutputPath: item.OutputPath,
			}); err != nil {
				return 0, fmt.Errorf("validate batch attachment %q: %w", item.ID, err)
			}
			path := filepath.Clean(item.OutputPath)
			if prior, exists := destinations[strings.ToLower(path)]; exists {
				return 0, validationError(fmt.Sprintf("batch attachment items %q and %q use the same output path", prior, item.ID))
			}
			destinations[strings.ToLower(path)] = item.ID
		case BatchOperationMark:
			if item.AttachmentID != "" || item.OutputPath != "" || item.Mailbox != "" || item.View != nil || item.Fields != nil ||
				(item.Read == nil && item.Flagged == nil && item.Junk == nil) {
				return 0, validationError(fmt.Sprintf("batch mark item %q requires state fields only", item.ID))
			}
		case BatchOperationMove:
			if item.Mailbox == "" {
				return 0, validationError(fmt.Sprintf("batch move item %q requires mailbox", item.ID))
			}
			if item.AttachmentID != "" || item.OutputPath != "" || item.Read != nil || item.Flagged != nil || item.Junk != nil || item.View != nil || item.Fields != nil {
				return 0, validationError(fmt.Sprintf("batch move item %q contains unsupported fields", item.ID))
			}
		case BatchOperationCopy:
			if item.Mailbox == "" {
				return 0, validationError(fmt.Sprintf("batch copy item %q requires mailbox", item.ID))
			}
			if item.AttachmentID != "" || item.OutputPath != "" || item.Read != nil || item.Flagged != nil || item.Junk != nil ||
				item.AllowDraftMutation || item.View != nil || item.Fields != nil {
				return 0, validationError(fmt.Sprintf("batch copy item %q contains unsupported fields", item.ID))
			}
		case BatchOperationDelete:
			if item.AttachmentID != "" || item.OutputPath != "" || item.Read != nil || item.Flagged != nil || item.Junk != nil ||
				item.Mailbox != "" || item.View != nil || item.Fields != nil {
				return 0, validationError(fmt.Sprintf("batch delete item %q contains unsupported fields", item.ID))
			}
		}
	}
	return concurrency, nil
}

func (run *batchExecution) execute(item BatchItem) BatchItemResult {
	result := BatchItemResult{ID: item.ID}
	switch run.request.Operation {
	case BatchOperationRead:
		message, err := run.service.GetMessage(run.ctx, item.Ref)
		if err != nil {
			result.State = BatchItemFailed
			if hasBatchMessageEvidence(message) {
				run.retainReadMessage(&message, item)
				result.Message = &message
			}
			result.Error = run.itemError(err)
			return result
		}
		result.State = BatchItemCompleted
		run.retainReadMessage(&message, item)
		result.Message = &message
	case BatchOperationAttachmentSave:
		saved, err := run.service.SaveAttachment(run.ctx, SaveAttachmentRequest{
			MessageRef: item.Ref, AttachmentID: item.AttachmentID, OutputPath: item.OutputPath,
		})
		if err != nil {
			result.State = BatchItemFailed
			result.Error = run.itemError(err)
			return result
		}
		result.State = BatchItemCompleted
		result.SavedAttachment = &saved
	case BatchOperationMark:
		state, err := run.service.MarkMessage(run.ctx, MarkMessageRequest{
			Ref: item.Ref, Read: item.Read, Flagged: item.Flagged, Junk: item.Junk,
			AllowDraftMutation: item.AllowDraftMutation,
		})
		if err != nil {
			result.State = BatchItemFailed
			if state.Ref != "" || state.ServerTruth != nil {
				result.MessageState = &state
			}
			result.Error = run.itemError(err)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
				(run.ctx.Err() != nil && transport.IsTimeout(err)) ||
				state.ServerTruth.OutcomeUnknown() ||
				transport.IsMutationOutcomeUnknown(err) {
				result.State = BatchItemUncertain
			}
			return result
		}
		result.State = BatchItemCompleted
		result.MessageState = &state
	case BatchOperationMove, BatchOperationCopy:
		state, err := run.service.TransferMessage(run.ctx, TransferMessageRequest{
			Ref: item.Ref, DestinationMailbox: item.Mailbox,
			Copy:               run.request.Operation == BatchOperationCopy,
			AllowDraftMutation: item.AllowDraftMutation,
		})
		if err != nil {
			result.State = BatchItemFailed
			if state.Ref != "" || state.ServerTruth != nil {
				result.MessageState = &state
			}
			result.Error = run.itemError(err)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
				(run.ctx.Err() != nil && transport.IsTimeout(err)) ||
				state.ServerTruth.OutcomeUnknown() ||
				transport.IsMutationOutcomeUnknown(err) {
				result.State = BatchItemUncertain
			}
			return result
		}
		result.State = BatchItemCompleted
		result.MessageState = &state
	case BatchOperationDelete:
		deleted, err := run.service.DeleteMessage(run.ctx, DeleteMessageRequest{
			Ref: item.Ref, AllowDraftMutation: item.AllowDraftMutation,
		})
		if err != nil {
			result.State = BatchItemFailed
			if deleted.MessageRef != "" || deleted.ServerTruth != nil {
				result.DeleteResult = &deleted
			}
			result.Error = run.itemError(err)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
				(run.ctx.Err() != nil && transport.IsTimeout(err)) ||
				deleted.ServerTruth.OutcomeUnknown() ||
				transport.IsMutationOutcomeUnknown(err) {
				result.State = BatchItemUncertain
			}
			return result
		}
		result.State = BatchItemCompleted
		result.DeleteResult = &deleted
	}
	return result
}

func (run *batchExecution) retainReadMessage(message *Message, item BatchItem) {
	if run.readBudget == nil {
		return
	}
	if !item.RetainReadContent {
		message.Content = ""
	}
	if !item.RetainReadHeaders {
		message.Headers = ""
	}
	requiredBytes := int64(0)
	if item.RetainReadContent {
		requiredBytes += jsonStringOutputBytes(message.Content)
	}
	if item.RetainReadHeaders {
		requiredBytes += jsonStringOutputBytes(message.Headers)
	}
	run.readBudget.mu.Lock()
	defer run.readBudget.mu.Unlock()
	run.readBudget.requiredBytes += requiredBytes
	if requiredBytes > run.readBudget.remaining {
		if item.RetainReadContent {
			message.Content = ""
		}
		if item.RetainReadHeaders {
			message.Headers = ""
		}
		run.readBudget.exceeded = true
		return
	}
	run.readBudget.remaining -= requiredBytes
}

func jsonStringOutputBytes(value string) int64 {
	encodedBytes := int64(2)
	for index := 0; index < len(value); {
		current := value[index]
		if current < utf8.RuneSelf {
			switch current {
			case '"', '\\':
				encodedBytes += 2
			case '<', '>', '&':
				encodedBytes += 6
			case '\b', '\f', '\n', '\r', '\t':
				encodedBytes += 2
			case 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x0e, 0x0f,
				0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19,
				0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f:
				encodedBytes += 6
			default:
				encodedBytes++
			}
			index++
			continue
		}
		rune, width := utf8.DecodeRuneInString(value[index:])
		if rune == utf8.RuneError && width == 1 {
			encodedBytes += 3
		} else if rune == 0x2028 || rune == 0x2029 {
			encodedBytes += 6
		} else {
			encodedBytes += int64(width)
		}
		index += width
	}
	return encodedBytes
}

func (run *batchExecution) itemError(err error) *BatchItemError {
	var coded interface{ ErrorCode() string }
	code := ""
	if errors.As(err, &coded) {
		code = coded.ErrorCode()
	}
	if code == "" {
		switch {
		case errors.Is(err, context.Canceled):
			code = "operation_canceled"
		case errors.Is(err, context.DeadlineExceeded):
			code = "operation_timeout"
		default:
			code = "operation_failed"
		}
	}
	retryable := run.request.Operation == BatchOperationRead || run.request.Operation == BatchOperationAttachmentSave
	if retryable {
		switch {
		case transport.IsTransientReadFailure(err):
		case code == "operation_timeout" || code == "operation_canceled":
		default:
			retryable = false
		}
	}
	guidance := GuidanceForError(string(run.request.Operation), err)
	return &BatchItemError{Code: code, Message: err.Error(), Retryable: retryable, Guidance: &guidance}
}

func hasBatchMessageEvidence(message Message) bool {
	return message.Summary.Ref != "" || message.Summary.MessageID != "" || message.Content != "" ||
		message.Headers != "" || message.ContentSource != "" || message.Hydration != nil ||
		len(message.To) > 0 || len(message.CC) > 0 || len(message.BCC) > 0 || len(message.Attachments) > 0
}
