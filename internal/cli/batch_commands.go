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
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if !input.set || input.value == "" {
		return failCommand("batch", *jsonOutput, invalidDraftInput("missing required --input"), stdout, stderr)
	}
	request, err := readBatchInput(input.value)
	if err != nil {
		return failCommand("batch", *jsonOutput, err, stdout, stderr)
	}
	if *concurrency != 0 {
		request.Concurrency = *concurrency
	}
	operationCtx, cancel := context.WithTimeout(ctx, batchTimeout)
	defer cancel()
	result, err := service.ExecuteBatch(operationCtx, request)
	if err != nil {
		return failCommand("batch", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		response := envelope{
			SchemaVersion: schemaVersion,
			OK:            result.Complete(),
			Command:       "batch",
			Data:          responseData{BatchResult: &result},
		}
		if !result.Complete() {
			response.Error = &errorData{Code: "batch_partial", Message: "one or more batch items did not complete"}
		}
		return writeJSON(stdout, response)
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
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var request mail.BatchRequest
	if err := decoder.Decode(&request); err != nil {
		return mail.BatchRequest{}, invalidDraftInput("decode batch JSON: " + err.Error())
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return mail.BatchRequest{}, invalidDraftInput("batch input must contain exactly one JSON object")
	}
	return request, nil
}

func writeBatchHumanItem(stdout io.Writer, item mail.BatchItemResult) {
	writeFormat(stdout, "%s\t%s\n", item.ID, item.State)
}
