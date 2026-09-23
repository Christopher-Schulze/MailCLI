package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"mailcli/internal/mail"
)

const batchTimeout = 15 * time.Minute

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
		request.ReadContentBudgetBytes = *maxBytes
		if err := preflightBatchOutputBudget(request, *maxBytes); err != nil {
			return failCommand("batch", true, err, stdout, stderr)
		}
	}
	operationCtx, cancel := context.WithTimeout(ctx, batchTimeout)
	defer cancel()
	result, err := service.ExecuteBatch(operationCtx, request)
	if err != nil {
		return failCommand("batch", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeBatchJSON(stdout, result, request, *maxBytes)
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
	if request.Operation != mail.BatchOperationRead {
		return nil
	}
	for index := range request.Items {
		options, err := batchReadOutputOptions(request.Items[index], maxBytes)
		if err != nil {
			return fmt.Errorf("batch read item %q: %w", request.Items[index].ID, err)
		}
		request.Items[index].RetainReadContent = options.includes("content")
		request.Items[index].RetainReadHeaders = options.includes("headers")
	}
	return nil
}

func writeBatchJSON(stdout io.Writer, result mail.BatchResult, request mail.BatchRequest, maxBytes int64) int {
	if result.ReadContentExceeded {
		return writeBatchOutputTooLarge(stdout, result, request, maxBytes, 0)
	}
	data, err := batchResponseData(result, request, maxBytes, true, true, true, true)
	if err != nil {
		return failCommand("batch", true, err, stdout, io.Discard)
	}
	response := envelope{SchemaVersion: schemaVersion, OK: result.Complete(), Command: "batch", Data: data}
	if !result.Complete() {
		response.Error = newErrorData("batch", response.Data, &commandError{
			code: "batch_partial", message: "one or more batch items did not complete",
		})
	}
	payload, err := marshalEnvelope(response)
	if err != nil {
		return failCommand("batch", true, &commandError{code: "serialization_failed", message: "could not serialize batch JSON response"}, stdout, io.Discard)
	}
	if int64(len(payload)) > maxBytes {
		return writeBatchOutputTooLarge(stdout, result, request, maxBytes, int64(len(payload)))
	}
	if code := writeEnvelopeBytes(stdout, payload); code != 0 {
		return code
	}
	if result.Complete() {
		return 0
	}
	return 1
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

func writeBatchOutputTooLarge(stdout io.Writer, result mail.BatchResult, request mail.BatchRequest, maxBytes, actualBytes int64) int {
	for _, includeItemErrors := range []bool{true, false} {
		data, err := batchResponseData(result, request, maxBytes, false, false, true, includeItemErrors)
		if err != nil {
			return failCommand("batch", true, err, stdout, io.Discard)
		}
		response := batchOutputFailureEnvelope(result, data, batchOutputTooLargeError(result, maxBytes, actualBytes))
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
	response := batchOutputFailureEnvelope(result, data, batchOutputTooLargeError(result, maxBytes, actualBytes))
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
	if result.Operation != mail.BatchOperationRead {
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

func batchOutputTooLargeError(result mail.BatchResult, maxBytes, actualBytes int64) error {
	if result.ReadContentExceeded {
		return &commandError{code: "output_too_large", message: fmt.Sprintf(
			"batch read content requires at least %d encoded JSON bytes, above the --max-bytes limit of %d",
			result.ReadContentRequiredBytes, maxBytes,
		)}
	}
	return &commandError{code: "output_too_large", message: fmt.Sprintf(
		"batch JSON output is %d bytes, above the --max-bytes limit of %d", actualBytes, maxBytes,
	)}
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
	for index, item := range request.Items {
		result.Items[index] = mail.BatchItemResult{ID: item.ID, State: mail.BatchItemCompleted}
	}
	serializerSampleData, err := batchResponseData(result, request, maxBytes, false, false, false, false)
	if err != nil {
		return err
	}
	serializerSample := batchOutputFailureEnvelope(
		result, serializerSampleData, &commandError{code: "output_too_large", message: fmt.Sprintf(
			"batch JSON output is %d bytes, above the --max-bytes limit of %d", int64(^uint64(0)>>1), maxBytes,
		)},
	)
	serializerPayload, err := marshalEnvelope(serializerSample)
	if err != nil {
		return &commandError{code: "serialization_failed", message: "could not validate batch output budget"}
	}
	result.ReadContentExceeded = true
	result.ReadContentRequiredBytes = mail.MaximumRawSourceBytes * mail.MaximumBatchItems
	contentSampleData, err := batchResponseData(result, request, maxBytes, false, false, false, false)
	if err != nil {
		return err
	}
	contentSample := batchOutputFailureEnvelope(result, contentSampleData, batchOutputTooLargeError(result, maxBytes, 0))
	contentPayload, err := marshalEnvelope(contentSample)
	if err != nil {
		return &commandError{code: "serialization_failed", message: "could not validate batch output budget"}
	}
	if int64(max(len(serializerPayload), len(contentPayload))) > maxBytes {
		return &commandError{code: "invalid_argument", message: "--max-bytes is too small to retain every batch item ID and status; no batch operation was started"}
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
