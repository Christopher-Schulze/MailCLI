package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"mailcli/internal/mail"
)

const batchTimeout = 15 * time.Minute
const maximumBatchReadOverflowItems = 10

type batchReadBudgetResult struct {
	exceeded      bool
	limitBytes    int64
	requiredBytes int64
	measured      outputSizeMeasurement
	failure       error
}

type batchReadContentBudget struct {
	mu             sync.Mutex
	limit          int64
	remaining      int64
	requiredBytes  int64
	exceeded       bool
	itemExtraBytes int64
	encodedBytes   int64
	measured       outputSizeMeasurement
	failure        error
}

func newBatchReadContentBudget(limit int64) *batchReadContentBudget {
	return &batchReadContentBudget{limit: limit, remaining: limit}
}

func (budget *batchReadContentBudget) admit(
	content, headers string,
	retainContent, retainHeaders bool,
) mail.BatchReadAdmission {
	requiredBytes := int64(0)
	if retainContent {
		requiredBytes += jsonStringOutputBytes(content)
	}
	if retainHeaders {
		requiredBytes += jsonStringOutputBytes(headers)
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	budget.requiredBytes += requiredBytes
	if budget.exceeded || requiredBytes > budget.remaining {
		budget.exceeded = true
		return mail.BatchReadAdmission{BudgetExceeded: true}
	}
	budget.remaining -= requiredBytes
	return mail.BatchReadAdmission{RetainContent: retainContent, RetainHeaders: retainHeaders}
}

func (budget *batchReadContentBudget) result() batchReadBudgetResult {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	requiredBytes := budget.requiredBytes
	if budget.measured != "" {
		requiredBytes = budget.encodedBytes
	}
	return batchReadBudgetResult{
		exceeded: budget.exceeded, limitBytes: budget.limit, requiredBytes: requiredBytes,
		measured: budget.measured, failure: budget.failure,
	}
}

func (budget *batchReadContentBudget) admitResult(result mail.BatchResult, index int, request mail.BatchRequest) bool {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.exceeded {
		return false
	}
	extra, err := batchReadItemExtraBytes(result.Items[index], index, request, budget.limit)
	if err == nil {
		budget.itemExtraBytes += extra
		var size int64
		size, err = batchReadSkeletonBytes(result, request, budget.limit)
		if err == nil && size+budget.itemExtraBytes > budget.limit {
			budget.exceeded = true
			budget.encodedBytes = size + budget.itemExtraBytes
			budget.measured = outputSizeLowerBound
			if result.Skipped == 0 {
				budget.measured = outputSizeExact
			}
		}
	}
	budget.failure = err
	return err == nil && !budget.exceeded
}

func batchReadItemExtraBytes(item mail.BatchItemResult, index int, request mail.BatchRequest, limit int64) (int64, error) {
	projected, err := projectBatchItem(item, index, mail.BatchOperationRead, request.Items, limit, true, true, true, true)
	if err != nil {
		return 0, err
	}
	payload, err := marshalCLIJSON(projected)
	if err != nil {
		return 0, err
	}
	projected.Message, projected.Error = nil, nil
	skeleton, err := marshalCLIJSON(projected)
	return int64(len(payload) - len(skeleton)), err
}

func batchReadSkeletonBytes(result mail.BatchResult, request mail.BatchRequest, limit int64) (int64, error) {
	result.Items = append([]mail.BatchItemResult(nil), result.Items...)
	for index := range result.Items {
		result.Items[index].Message, result.Items[index].Error = nil, nil
	}
	data, err := batchResponseData(result, request, limit, true, true, true, true)
	if err != nil {
		return 0, err
	}
	payload, err := marshalEnvelope(batchJSONEnvelope(result, data))
	return int64(len(payload)), err
}

func runBatch(
	ctx context.Context,
	service *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	flags := newFlagSet("batch", stderr)
	input := &trackedStringFlag{}
	flags.Var(input, "input", "JSON batch request file or - for standard input")
	concurrency := flags.Int("concurrency", 0, "maximum concurrent items (1-8)")
	confirm := flags.Bool("confirm", false, "confirm the delete operation")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	maxBytes := flags.Int64("max-bytes", defaultJSONOutputBytes, "maximum JSON response bytes")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if err := validateOutputByteLimit(*maxBytes); err != nil {
		return failCommand("batch", *jsonOutput, err, stdout, stderr)
	}
	if !input.set || input.value == "" {
		return failCommand("batch", *jsonOutput, invalidDraftInput("missing required --input"), stdout, stderr)
	}
	request, err := readBatchInput(input.value)
	if err != nil {
		return failCommand("batch", *jsonOutput, err, stdout, stderr)
	}
	if err := prepareBatchReadProjection(&request, *maxBytes); err != nil {
		return failCommand("batch", *jsonOutput, err, stdout, stderr)
	}
	if request.Operation == mail.BatchOperationDelete && !*confirm {
		return failCommand("batch", *jsonOutput, confirmationRequired("batch delete"), stdout, stderr)
	}
	if request.Operation != mail.BatchOperationDelete && *confirm {
		return failCommand(
			"batch", *jsonOutput,
			&commandError{code: "invalid_argument", message: "--confirm applies only to the delete operation"},
			stdout, stderr,
		)
	}
	if *concurrency != 0 {
		request.Concurrency = *concurrency
	}
	if *jsonOutput {
		if err := preflightBatchOutputBudget(request, *maxBytes); err != nil {
			return failCommand("batch", true, err, stdout, stderr)
		}
	}
	var readBudget *batchReadContentBudget
	if *jsonOutput && request.Operation == mail.BatchOperationRead {
		readBudget = newBatchReadContentBudget(*maxBytes)
		request.ReadContentAdmission = readBudget.admit
		request.ReadResultAdmission = func(result mail.BatchResult, index int) bool {
			return readBudget.admitResult(result, index, request)
		}
	}
	operationCtx, cancel := context.WithTimeout(ctx, batchTimeout)
	defer cancel()
	result, err := service.ExecuteBatch(operationCtx, request)
	if err != nil {
		return failCommand("batch", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		budgetResult := batchReadBudgetResult{}
		if readBudget != nil {
			budgetResult = readBudget.result()
		}
		return writeBatchJSON(stdout, result, request, *maxBytes, budgetResult)
	}
	for _, item := range result.Items {
		writeBatchHumanItem(stdout, item)
	}
	writeFormat(
		stdout, "total=%d\tcompleted=%d\tfailed=%d\tuncertain=%d\tskipped=%d\n",
		result.Total, result.Completed, result.Failed, result.Uncertain, result.Skipped,
	)
	if result.Complete() {
		return 0
	}
	return 1
}

func prepareBatchReadProjection(request *mail.BatchRequest, maxBytes int64) error {
	if request.Defaults != nil {
		if request.Operation != mail.BatchOperationRead {
			return &commandError{code: "invalid_argument", message: "batch defaults are only valid for read operations"}
		}
		if err := validateBatchReadDefaults(request.Defaults); err != nil {
			return err
		}
		for index := range request.Items {
			if request.Items[index].View == nil && request.Items[index].Fields == nil {
				request.Items[index].View = request.Defaults.View
				request.Items[index].Fields = request.Defaults.Fields
			}
		}
		request.Defaults = nil
	}
	if request.Operation != mail.BatchOperationRead {
		return nil
	}
	for index := range request.Items {
		options, err := batchReadOutputOptions(request.Items[index], index, maxBytes)
		if err != nil {
			return err
		}
		request.Items[index].RetainReadContent = options.includes("content")
		request.Items[index].RetainReadHeaders = options.includes("headers")
		request.Items[index].ReadIntent = messageReadIntentForProjection(options)
	}
	return nil
}

func writeBatchJSON(
	stdout io.Writer,
	result mail.BatchResult,
	request mail.BatchRequest,
	maxBytes int64,
	readBudget batchReadBudgetResult,
) int {
	if readBudget.failure != nil {
		return failCommand("batch", true, readBudget.failure, stdout, io.Discard)
	}
	if readBudget.exceeded {
		return writeBatchOutputTooLarge(stdout, result, request, maxBytes, 0, readBudget)
	}
	data, err := batchResponseData(result, request, maxBytes, true, true, true, true)
	if err != nil {
		return failCommand("batch", true, err, stdout, io.Discard)
	}
	response := batchJSONEnvelope(result, data)
	payload, err := marshalEnvelope(response)
	if err != nil {
		return failCommand("batch", true, &commandError{code: "serialization_failed", message: "could not serialize batch JSON response"}, stdout, io.Discard)
	}
	if int64(len(payload)) > maxBytes {
		return writeBatchOutputTooLarge(stdout, result, request, maxBytes, int64(len(payload)), readBudget)
	}
	if code := writeEnvelopeBytes(stdout, payload); code != 0 {
		return code
	}
	if result.Complete() {
		return 0
	}
	return 1
}

func batchJSONEnvelope(result mail.BatchResult, data responseData) envelope {
	response := envelope{SchemaVersion: schemaVersion, OK: result.Complete(), Command: "batch", Data: data}
	if !result.Complete() {
		response.Error = newErrorData("batch", response.Data, &commandError{
			code: "batch_partial", message: "one or more batch items did not complete",
		})
	}
	return response
}

func batchResponseData(
	result mail.BatchResult,
	request mail.BatchRequest,
	maxBytes int64,
	includeReadMessages bool,
	includeProjection bool,
	includeMutationEvidence bool,
	includeItemErrors bool,
) (responseData, error) {
	projected, err := projectBatchResult(
		result, request.Items, maxBytes, includeReadMessages, includeProjection,
		includeMutationEvidence, includeItemErrors,
	)
	if err != nil {
		return responseData{}, err
	}
	data := responseData{
		BatchResult:   &result,
		serialization: &serializedProjection{batch: projected},
	}
	if invocationStoreProfile != nil {
		data.StoreProfile = invocationStoreProfile
	}
	return data, nil
}

func writeBatchOutputTooLarge(
	stdout io.Writer,
	result mail.BatchResult,
	request mail.BatchRequest,
	maxBytes, actualBytes int64,
	readBudget batchReadBudgetResult,
) int {
	for _, includeItemErrors := range []bool{true, false} {
		data, err := batchResponseData(result, request, maxBytes, false, false, true, includeItemErrors)
		if err != nil {
			return failCommand("batch", true, err, stdout, io.Discard)
		}
		response := batchOutputFailureEnvelope(result, data, batchOutputTooLargeError(maxBytes, actualBytes, readBudget))
		payload, marshalErr := marshalEnvelope(response)
		if marshalErr == nil && int64(len(payload)) <= maxBytes {
			if code := writeEnvelopeBytes(stdout, payload); code != 0 {
				return code
			}
			return 1
		}
	}
	data, err := batchResponseData(result, request, maxBytes, false, false, false, false)
	if err != nil {
		return failCommand("batch", true, err, stdout, io.Discard)
	}
	response := batchOutputFailureEnvelope(result, data, batchOutputTooLargeError(maxBytes, actualBytes, readBudget))
	payload, marshalErr := marshalEnvelope(response)
	if marshalErr != nil || int64(len(payload)) > maxBytes {
		return 1
	}
	if code := writeEnvelopeBytes(stdout, payload); code != 0 {
		return code
	}
	return 1
}

func batchOutputFailureEnvelope(result mail.BatchResult, data responseData, err error) envelope {
	response := envelope{SchemaVersion: schemaVersion, OK: false, Command: "batch", Data: data}
	response.Error = newErrorData("batch", data, err)
	guidance := mail.OperationGuidance{
		Phase: mail.OperationPhaseExecution, EffectCertainty: mail.EffectNone,
		Retryability: mail.RetryObserveRequired, ReplayAllowed: false,
		Recovery: mail.RecoveryGuidance{Action: mail.RecoveryInspect},
	}
	if result.Operation == mail.BatchOperationRead {
		guidance.Retryability = mail.RetryUserInputRequired
		guidance.ReplayAllowed = true
		guidance.Recovery = mail.RecoveryGuidance{
			Action:      mail.RecoveryCorrect,
			Instruction: "Narrow the requested read view or fields, or raise --max-bytes, then replay this same read batch.",
		}
		if data.serialization != nil && data.serialization.batch != nil &&
			len(data.serialization.batch.Items) > maximumBatchReadOverflowItems {
			data.serialization.batch.Items = data.serialization.batch.Items[:maximumBatchReadOverflowItems]
		}
	} else {
		switch {
		case result.Uncertain > 0:
			guidance.EffectCertainty = mail.EffectUnknown
		case result.Completed > 0 && result.Complete():
			guidance.EffectCertainty = mail.EffectComplete
		case result.Completed > 0:
			guidance.EffectCertainty = mail.EffectPartial
		}
	}
	response.Error.Guidance = &guidance
	return response
}

func batchOutputTooLargeError(
	maxBytes, actualBytes int64,
	readBudget batchReadBudgetResult,
) error {
	if readBudget.exceeded {
		if readBudget.measured != "" {
			return newOutputTooLargeError(readBudget.requiredBytes, readBudget.limitBytes, readBudget.measured, "batch read envelope")
		}
		return newOutputTooLargeError(
			readBudget.requiredBytes, readBudget.limitBytes, outputSizeLowerBound, "batch read content",
		)
	}
	return newOutputTooLargeError(actualBytes, maxBytes, outputSizeExact, "batch")
}

func preflightBatchOutputBudget(request mail.BatchRequest, maxBytes int64) error {
	concurrency := request.Concurrency
	if concurrency == 0 {
		concurrency = mail.DefaultBatchConcurrency
	}
	result := mail.BatchResult{
		Operation: request.Operation, Concurrency: concurrency, Total: len(request.Items),
		Completed: len(request.Items), Items: make([]mail.BatchItemResult, len(request.Items)),
	}
	state := mail.BatchItemCompleted
	if request.Operation == mail.BatchOperationRead {
		state = mail.BatchItemSkippedBudget
		result.Completed, result.Skipped = 0, len(request.Items)
	}
	for index, item := range request.Items {
		result.Items[index] = mail.BatchItemResult{ID: item.ID, State: state}
	}
	serializerSampleData, err := batchResponseData(result, request, maxBytes, false, false, false, false)
	if err != nil {
		return err
	}
	serializerSample := batchOutputFailureEnvelope(
		result, serializerSampleData,
		newOutputTooLargeError(int64(^uint64(0)>>1), maxBytes, outputSizeExact, "batch"),
	)
	serializerPayload, err := marshalEnvelope(serializerSample)
	if err != nil {
		return &commandError{code: "serialization_failed", message: "could not validate batch output budget"}
	}
	readBudget := batchReadBudgetResult{
		exceeded: true, limitBytes: maxBytes,
		requiredBytes: maximumJSONOutputBytes * mail.MaximumBatchItems,
	}
	contentSampleData, err := batchResponseData(result, request, maxBytes, false, false, false, false)
	if err != nil {
		return err
	}
	contentSample := batchOutputFailureEnvelope(
		result, contentSampleData, batchOutputTooLargeError(maxBytes, 0, readBudget),
	)
	contentPayload, err := marshalEnvelope(contentSample)
	if err != nil {
		return &commandError{code: "serialization_failed", message: "could not validate batch output budget"}
	}
	if int64(max(len(serializerPayload), len(contentPayload))) > maxBytes {
		message := "--max-bytes is too small to retain every batch item ID and status; no batch operation was started"
		if request.Operation == mail.BatchOperationRead {
			message = "--max-bytes is too small to retain bounded read-batch overflow evidence; no batch operation was started"
		}
		return &commandError{code: "invalid_argument", message: message}
	}
	return nil
}

func readBatchInput(path string) (mail.BatchRequest, error) {
	if path == "" {
		return mail.BatchRequest{}, invalidDraftInput("batch input path is required")
	}
	var reader io.Reader = os.Stdin
	var file *os.File
	if path != "-" {
		opened, err := os.Open(path)
		if err != nil {
			return mail.BatchRequest{}, fmt.Errorf("open batch input: %w", err)
		}
		file = opened
		reader = opened
	}
	payload, readErr := io.ReadAll(io.LimitReader(reader, mail.MaximumBatchInputBytes+1))
	if file != nil {
		readErr = errors.Join(readErr, file.Close())
	}
	if readErr != nil {
		return mail.BatchRequest{}, fmt.Errorf("read batch input: %w", readErr)
	}
	if len(payload) == 0 {
		return mail.BatchRequest{}, invalidDraftInput("batch input is empty")
	}
	if len(payload) > mail.MaximumBatchInputBytes {
		return mail.BatchRequest{}, invalidDraftInput("batch input exceeds 16 MiB")
	}
	if _, err := validateInputJSON(payload, inputJSONBatch); err != nil {
		return mail.BatchRequest{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var request mail.BatchRequest
	if err := decoder.Decode(&request); err != nil {
		return mail.BatchRequest{}, inputJSONDecodeError(err)
	}
	return request, nil
}

func writeBatchHumanItem(stdout io.Writer, item mail.BatchItemResult) {
	writeFormat(stdout, "%s\t%s\n", item.ID, item.State)
}
