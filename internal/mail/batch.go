package mail

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

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
	Operation   BatchOperation `json:"operation"`
	Items       []BatchItem    `json:"items"`
	Concurrency int            `json:"concurrency,omitempty"`
}

// BatchItem identifies one request. Ref is a store-bound message reference for
// every operation; attachment fields are used only by attachment_save and
// state fields only by mark.
type BatchItem struct {
	ID                 string `json:"id"`
	Ref                string `json:"ref"`
	AttachmentID       string `json:"attachment_id"`
	OutputPath         string `json:"output_path"`
	Read               *bool  `json:"read"`
	Flagged            *bool  `json:"flagged"`
	Junk               *bool  `json:"junk"`
	AllowDraftMutation bool   `json:"allow_draft_mutation,omitempty"`
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
	Error           *BatchItemError  `json:"error,omitempty"`
}

type BatchResult struct {
	Operation   BatchOperation    `json:"operation"`
	Concurrency int               `json:"concurrency"`
	Total       int               `json:"total"`
	Completed   int               `json:"completed"`
	Failed      int               `json:"failed"`
	Skipped     int               `json:"skipped"`
	Uncertain   int               `json:"uncertain"`
	Items       []BatchItemResult `json:"items"`
}

type batchExecution struct {
	ctx     context.Context
	service *Service
	request BatchRequest
	result  *BatchResult
	jobs    chan int
	wait    sync.WaitGroup
	next    int
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
	execution.run()
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
	case BatchOperationRead, BatchOperationAttachmentSave, BatchOperationMark:
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
			if item.AttachmentID != "" || item.OutputPath != "" || item.Read != nil || item.Flagged != nil || item.Junk != nil || item.AllowDraftMutation {
				return 0, validationError(fmt.Sprintf("batch read item %q contains unsupported fields", item.ID))
			}
		case BatchOperationAttachmentSave:
			if item.AttachmentID == "" || item.OutputPath == "" {
				return 0, validationError(fmt.Sprintf("batch attachment item %q requires attachment_id and output_path", item.ID))
			}
			if item.Read != nil || item.Flagged != nil || item.Junk != nil || item.AllowDraftMutation {
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
			if item.AttachmentID != "" || item.OutputPath != "" || (item.Read == nil && item.Flagged == nil && item.Junk == nil) {
				return 0, validationError(fmt.Sprintf("batch mark item %q requires state fields only", item.ID))
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
				result.Message = &message
			}
			result.Error = run.itemError(err)
			return result
		}
		result.State = BatchItemCompleted
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
				(run.ctx.Err() != nil && result.Error.Code == transport.CodeIMAPTimeout) ||
				(state.ServerTruth != nil && state.ServerTruth.Outcome == transport.MutationOutcomeUnknown) ||
				result.Error.Code == transport.CodeIMAPFlagsOutcomeUnknown {
				result.State = BatchItemUncertain
			}
			return result
		}
		result.State = BatchItemCompleted
		result.MessageState = &state
	}
	return result
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
	retryable := run.request.Operation != BatchOperationMark
	if retryable {
		switch code {
		case transport.CodeIMAPConnectFailed, transport.CodeIMAPTimeout, transport.CodeIMAPFetchFailed, "operation_timeout", "operation_canceled":
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
