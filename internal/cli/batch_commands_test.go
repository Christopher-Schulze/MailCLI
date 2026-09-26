package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

func TestBatchCommandJSONPreservesOrderedResults(t *testing.T) {
	inputPath := filepath.Join(t.TempDir(), "batch.json")
	payload := []byte(`{"operation":"read","concurrency":2,"items":[{"id":"first","ref":"msg_ref"},{"id":"second","ref":"msg_ref"}]}`)
	if err := os.WriteFile(inputPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), newTestService(), []string{"batch", "--input", inputPath, "--json"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("Run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; stdout = %q", err, stdout.String())
	}
	if !response.OK || response.Data.BatchResult == nil {
		t.Fatalf("response = %+v", response)
	}
	result := response.Data.BatchResult
	if result.Operation != mail.BatchOperationRead || result.Concurrency != 2 || result.Completed != 2 {
		t.Fatalf("batch result = %+v", result)
	}
	if len(result.Items) != 2 || result.Items[0].ID != "first" || result.Items[1].ID != "second" {
		t.Fatalf("batch item order = %+v", result.Items)
	}
}

func TestBatchCommandJSONProjectsReadItemsIndependently(t *testing.T) {
	inputPath := writeBatchInput(t, mail.BatchRequest{
		Operation: mail.BatchOperationRead, Concurrency: 1,
		Items: []mail.BatchItem{
			{ID: "metadata", Ref: "msg_ref", View: stringPointer(outputViewMetadata)},
			{ID: "plain", Ref: "msg_ref", View: stringPointer(outputViewPlain)},
			{ID: "full", Ref: "msg_ref", View: stringPointer(outputViewFull)},
			{ID: "fields", Ref: "msg_ref", Fields: stringSlicePointer([]string{"summary", "content_complete", "content"})},
			{ID: "legacy", Ref: "msg_ref"},
		},
	})
	gateway := &projectionGateway{message: projectionMessage()}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), mail.NewService(gateway),
		[]string{"batch", "--input", inputPath, "--json"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("Run() code = %d, stderr = %q, output = %q", code, stderr.String(), stdout.String())
	}
	var response struct {
		OK   bool `json:"ok"`
		Data struct {
			BatchResult struct {
				Items []struct {
					ID         string                     `json:"id"`
					Message    map[string]json.RawMessage `json:"message"`
					Projection projectionInfo             `json:"projection"`
				} `json:"items"`
			} `json:"batch_result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output = %q", err, stdout.String())
	}
	if !response.OK || len(response.Data.BatchResult.Items) != 5 {
		t.Fatalf("response = %+v", response)
	}
	items := response.Data.BatchResult.Items
	for index, id := range []string{"metadata", "plain", "full", "fields", "legacy"} {
		if items[index].ID != id {
			t.Fatalf("item %d = %+v", index, items[index])
		}
	}
	for _, index := range []int{0, 1, 2, 3, 4} {
		if _, exists := items[index].Message["summary"]; !exists {
			t.Errorf("item %q omitted summary", items[index].ID)
		}
	}
	if _, exists := items[0].Message["content"]; exists || items[0].Projection.View != outputViewMetadata {
		t.Errorf("metadata item = %+v", items[0])
	}
	if _, exists := items[0].Message["headers"]; exists {
		t.Error("metadata item retained headers")
	}
	if _, exists := items[1].Message["content"]; !exists || items[1].Projection.View != outputViewPlain {
		t.Errorf("plain item = %+v", items[1])
	}
	if _, exists := items[1].Message["headers"]; exists {
		t.Error("plain item retained headers")
	}
	if _, exists := items[2].Message["content"]; !exists || items[2].Projection.View != outputViewFull {
		t.Errorf("full item = %+v", items[2])
	}
	if _, exists := items[2].Message["headers"]; !exists {
		t.Error("full item omitted headers")
	}
	if _, exists := items[3].Message["content"]; !exists || items[3].Projection.View != "custom" {
		t.Errorf("fields item = %+v", items[3])
	}
	if _, exists := items[3].Message["headers"]; exists {
		t.Error("fields item retained unrequested headers")
	}
	if _, exists := items[4].Message["content"]; !exists || items[4].Projection.View != outputViewFull {
		t.Errorf("omitted-view item did not preserve full behavior: %+v", items[4])
	}
}

func TestBatchCommandSerializerOverflowReturnsBoundedEvidence(t *testing.T) {
	inputPath := writeBatchInput(t, mail.BatchRequest{
		Operation: mail.BatchOperationRead, Concurrency: 1,
		Items: []mail.BatchItem{{ID: "large-metadata", Ref: "msg_ref", View: stringPointer(outputViewMetadata)}},
	})
	message := projectionMessage()
	message.Summary.Subject = strings.Repeat("s", 4096)
	var completeOutput, completeStderr bytes.Buffer
	completeCode := Run(context.Background(), mail.NewService(&projectionGateway{message: message}),
		[]string{"batch", "--input", inputPath, "--max-bytes", fmt.Sprint(maximumJSONOutputBytes), "--json"},
		&completeOutput, &completeStderr)
	if completeCode != 0 || completeStderr.Len() != 0 {
		t.Fatalf("complete batch baseline failed: code=%d stderr=%q output bytes=%d", completeCode, completeStderr.String(), completeOutput.Len())
	}
	gateway := &projectionGateway{message: message}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), mail.NewService(gateway),
		[]string{"batch", "--input", inputPath, "--max-bytes", "1024", "--json"}, &stdout, &stderr)
	if code != 1 || stderr.Len() != 0 || int64(stdout.Len()) > 1024 {
		t.Fatalf("Run() code = %d, output bytes = %d, stderr = %q, output = %q", code, stdout.Len(), stderr.String(), stdout.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output = %q", err, stdout.String())
	}
	if response.OK || response.Error == nil || response.Error.Code != "output_too_large" ||
		response.Error.Guidance == nil || !response.Error.Guidance.ReplayAllowed ||
		response.Error.Guidance.EffectCertainty != mail.EffectNone ||
		response.Error.Guidance.Retryability != mail.RetryUserInputRequired ||
		response.Error.Guidance.Recovery.Action != mail.RecoveryCorrect ||
		!strings.Contains(response.Error.Guidance.Recovery.Instruction, "fields") ||
		!strings.Contains(response.Error.Guidance.Recovery.Instruction, "--max-bytes") ||
		response.Data.BatchResult == nil || len(response.Data.BatchResult.Items) != 1 ||
		response.Data.BatchResult.Items[0].ID != "large-metadata" ||
		response.Data.BatchResult.Items[0].State != mail.BatchItemCompleted ||
		response.Data.BatchResult.Items[0].Message != nil {
		t.Fatalf("bounded overflow response = %+v", response)
	}
	assertOutputSizeEvidence(t, response, int64(completeOutput.Len()), 1024, string(outputSizeExact))

	var replayOutput, replayStderr bytes.Buffer
	replayGateway := &projectionGateway{message: message}
	replayCode := Run(context.Background(), mail.NewService(replayGateway),
		[]string{"batch", "--input", inputPath, "--max-bytes", "8192", "--json"},
		&replayOutput, &replayStderr)
	var replay envelope
	if replayCode != 0 || replayStderr.Len() != 0 || json.Unmarshal(replayOutput.Bytes(), &replay) != nil ||
		!replay.OK || replay.Data.BatchResult == nil || replayGateway.getCalls != 1 {
		t.Fatalf("corrected read replay: code=%d calls=%d stderr=%q output=%q",
			replayCode, replayGateway.getCalls, replayStderr.String(), replayOutput.String())
	}
}

func TestBatchCommandHundredReadItemsStayWithinBudgetAndOrdered(t *testing.T) {
	const itemCount = 100
	items := make([]mail.BatchItem, itemCount)
	view := outputViewFull
	for index := range items {
		items[index] = mail.BatchItem{ID: fmt.Sprintf("item-%03d", index), Ref: "msg_ref", View: &view}
	}
	inputPath := writeBatchInput(t, mail.BatchRequest{
		Operation: mail.BatchOperationRead, Concurrency: 1, Items: items,
	})
	message := projectionMessage()
	message.Content = strings.Repeat("x", 16*1024)
	var completeOutput, completeStderr bytes.Buffer
	completeCode := Run(context.Background(), mail.NewService(&projectionGateway{message: message}),
		[]string{"batch", "--input", inputPath, "--max-bytes", fmt.Sprint(maximumJSONOutputBytes), "--json"},
		&completeOutput, &completeStderr)
	if completeCode != 0 || completeStderr.Len() != 0 {
		t.Fatalf("complete read baseline failed: code=%d stderr=%q output bytes=%d", completeCode, completeStderr.String(), completeOutput.Len())
	}
	gateway := &projectionGateway{message: message}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), mail.NewService(gateway),
		[]string{"batch", "--input", inputPath, "--max-bytes", "32768", "--json"}, &stdout, &stderr)
	if code != 1 || stderr.Len() != 0 || stdout.Len() > 32768 {
		t.Fatalf("Run() code = %d, output bytes = %d, stderr = %q", code, stdout.Len(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output bytes = %d", err, stdout.Len())
	}
	if response.OK || response.Error == nil || response.Error.Code != "output_too_large" ||
		response.Error.Guidance == nil || !response.Error.Guidance.ReplayAllowed ||
		response.Error.Guidance.EffectCertainty != mail.EffectNone ||
		response.Data.BatchResult == nil || len(response.Data.BatchResult.Items) != maximumBatchReadOverflowItems ||
		response.Data.BatchResult.Total != itemCount ||
		response.Data.BatchResult.Completed != itemCount || gateway.getCalls != itemCount {
		t.Fatalf("100-item overflow response = %+v", response)
	}
	if response.Data.RequiredBytes == nil || *response.Data.RequiredBytes <= 32768 || *response.Data.RequiredBytes > int64(completeOutput.Len()) ||
		response.Data.LimitBytes == nil || *response.Data.LimitBytes != 32768 ||
		response.Data.Measured != string(outputSizeLowerBound) || response.Error.RequiredBytes != nil {
		t.Fatalf("read-content lower-bound evidence = data:%+v error:%+v complete_bytes=%d",
			response.Data, response.Error, completeOutput.Len())
	}
	for index, item := range response.Data.BatchResult.Items {
		if item.ID != fmt.Sprintf("item-%03d", index) || item.State != mail.BatchItemCompleted || item.Message != nil {
			t.Fatalf("item %d = %+v", index, item)
		}
	}
}

func TestBatchCommandMutationOverflowPreservesCompletedEffects(t *testing.T) {
	read := true
	request := mail.BatchRequest{
		Operation: mail.BatchOperationMark,
		Items: []mail.BatchItem{
			{ID: "completed", Ref: "msg_ref", Read: &read},
			{ID: "failed", Ref: "other_ref", Read: &read},
		},
	}
	completedState := mail.MessageSummary{
		Ref: "msg_ref",
		ServerTruth: &mail.ServerMutationEvidence{
			OperationID: "operation-1", Outcome: mail.ServerMutationOutcomeCompleted,
			Command: "STORE", ServerResponse: "OK", Mailbox: "INBOX", UID: 7,
			ExpectedUIDValidity: 8, UIDValidity: 8, FlagsState: mail.MessageServerStateObserved,
			FlagsSource: "FETCH", ActualFlags: []string{"\\Seen"},
		},
	}
	result := mail.BatchResult{
		Operation: mail.BatchOperationMark, Total: 2, Completed: 1, Failed: 1,
		Items: []mail.BatchItemResult{
			{ID: "completed", State: mail.BatchItemCompleted, MessageState: &completedState},
			{ID: "failed", State: mail.BatchItemFailed, Error: &mail.BatchItemError{
				Code: "operation_failed", Message: strings.Repeat("failure detail ", 512),
			}},
		},
	}
	var stdout bytes.Buffer
	if code := writeBatchJSON(&stdout, result, request, 4096); code != 1 || stdout.Len() > 4096 {
		t.Fatalf("writeBatchJSON() code=%d output bytes=%d", code, stdout.Len())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error=%v output=%q", err, stdout.String())
	}
	if response.OK || response.Error == nil || response.Error.Code != "output_too_large" ||
		response.Error.Guidance == nil || response.Error.Guidance.EffectCertainty != mail.EffectPartial ||
		response.Error.Guidance.ReplayAllowed || response.Error.Guidance.Retryability != mail.RetryObserveRequired ||
		response.Data.BatchResult == nil || response.Data.BatchResult.Completed != 1 ||
		response.Data.BatchResult.Failed != 1 || len(response.Data.BatchResult.Items) != 2 {
		t.Fatalf("mutation overflow response = %+v", response)
	}
	completed := response.Data.BatchResult.Items[0]
	if completed.ID != "completed" || completed.State != mail.BatchItemCompleted || completed.MessageState == nil ||
		completed.MessageState.ServerTruth == nil || completed.MessageState.ServerTruth.OperationID != "operation-1" ||
		completed.MessageState.ServerTruth.Outcome != mail.ServerMutationOutcomeCompleted {
		t.Fatalf("completed mutation evidence = %+v", completed)
	}
}

func TestBatchReadProjectionValidationPrecedesRetrieval(t *testing.T) {
	for _, payload := range []string{
		`{"operation":"read","items":[{"id":"one","ref":"msg_ref","view":"unknown"}]}`,
		`{"operation":"read","items":[{"id":"one","ref":"msg_ref","view":"metadata","fields":["summary"]}]}`,
		`{"operation":"read","items":[{"id":"one","ref":"msg_ref","fields":[]}]}`,
		`{"operation":"read","items":[{"id":"one","ref":"msg_ref","fields":["unknown"]}]}`,
		`{"operation":"read","items":[{"id":"one","ref":"msg_ref","fields":["summary,content"]}]}`,
		`{"operation":"read","items":[{"id":"one","ref":"msg_ref","view":null}]}`,
		`{"operation":"read","items":[{"id":"one","ref":"msg_ref","fields":null}]}`,
	} {
		inputPath := filepath.Join(t.TempDir(), "batch.json")
		if err := os.WriteFile(inputPath, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		gateway := &projectionGateway{message: projectionMessage()}
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), mail.NewService(gateway),
			[]string{"batch", "--input", inputPath, "--json"}, &stdout, &stderr)
		if code != 2 || stderr.Len() != 0 || gateway.getCalls != 0 {
			t.Fatalf("payload=%s code=%d calls=%d stderr=%q output=%q", payload, code, gateway.getCalls, stderr.String(), stdout.String())
		}
	}
}

func writeBatchInput(t *testing.T, request mail.BatchRequest) string {
	t.Helper()
	inputPath := filepath.Join(t.TempDir(), "batch.json")
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := os.WriteFile(inputPath, payload, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return inputPath
}

func TestBatchProjectionErrorsNameIndexedJSONPath(t *testing.T) {
	for _, test := range []struct {
		name, field string
		item        mail.BatchItem
	}{
		{name: "empty", field: "fields", item: mail.BatchItem{Fields: stringSlicePointer([]string{})}},
		{name: "empty element", field: "fields", item: mail.BatchItem{Fields: stringSlicePointer([]string{""})}},
		{name: "unknown", field: "fields", item: mail.BatchItem{Fields: stringSlicePointer([]string{"unknown"})}},
		{name: "duplicate", field: "fields", item: mail.BatchItem{Fields: stringSlicePointer([]string{"summary", "summary"})}},
		{name: "mixed all", field: "fields", item: mail.BatchItem{Fields: stringSlicePointer([]string{"all", "summary"})}},
		{name: "comma", field: "fields", item: mail.BatchItem{Fields: stringSlicePointer([]string{"summary,content"})}},
		{name: "conflict", field: "fields", item: mail.BatchItem{View: stringPointer("metadata"), Fields: stringSlicePointer([]string{"summary"})}},
		{name: "view", field: "view", item: mail.BatchItem{View: stringPointer("unknown")}},
	} {
		for _, index := range []int{0, 3} {
			for _, jsonOutput := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/index%d/json%t", test.name, index, jsonOutput), func(t *testing.T) {
					assertBatchProjectionErrorBeforeRetrieval(t, test.item, index, test.field, jsonOutput)
				})
			}
		}
	}
}

func assertBatchProjectionErrorBeforeRetrieval(t *testing.T, item mail.BatchItem, index int, field string, jsonOutput bool) {
	t.Helper()
	items := make([]mail.BatchItem, index+1)
	for preceding := range items {
		items[preceding] = mail.BatchItem{ID: fmt.Sprintf("preceding-%d", preceding), Ref: "msg_ref"}
	}
	item.ID, item.Ref = "target-item", "msg_ref"
	items[index] = item
	inputPath := writeBatchInput(t, mail.BatchRequest{Operation: mail.BatchOperationRead, Items: items})
	gateway := &projectionGateway{message: projectionMessage()}
	args := []string{"batch", "--input", inputPath}
	if jsonOutput {
		args = append(args, "--json")
	}
	code, output, stderr := runProjectionCommand(t, gateway, args...)
	message := stderr
	if jsonOutput {
		var response envelope
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			t.Fatal(err)
		}
		if response.Error == nil || response.Error.Code != "invalid_argument" || response.OK || stderr != "" {
			t.Fatalf("batch error envelope=%+v stderr=%q", response, stderr)
		}
		message = response.Error.Message
	} else if output != "" {
		t.Fatalf("human validation wrote stdout: %q", output)
	}
	if code != 2 || gateway.getCalls != 0 || !strings.Contains(message, fmt.Sprintf("items[%d].%s", index, field)) ||
		!strings.Contains(message, `item "target-item"`) || strings.Contains(message, "--fields") || strings.Contains(message, "--view") {
		t.Fatalf("batch error: code=%d calls=%d message=%q", code, gateway.getCalls, message)
	}
}

func TestBatchProjectionSerializationErrorsNameIndexedJSONPath(t *testing.T) {
	items := make([]mail.BatchItem, 4)
	results := make([]mail.BatchItemResult, 4)
	for index := range items {
		id := fmt.Sprintf("item-%d", index)
		items[index] = mail.BatchItem{ID: id, Ref: "msg_ref"}
		results[index] = mail.BatchItemResult{ID: id, State: mail.BatchItemCompleted}
	}
	items[3].Fields = stringSlicePointer([]string{})
	result := mail.BatchResult{Operation: mail.BatchOperationRead, Items: results}
	projection, err := projectBatchResult(result, items, defaultJSONOutputBytes, true, true, true, true)
	if err == nil || projection != nil || errorCode(err) != "invalid_argument" ||
		!strings.Contains(err.Error(), "items[3].fields") || !strings.Contains(err.Error(), `item "item-3"`) ||
		strings.Contains(err.Error(), "--fields") {
		t.Fatalf("serialization projection=%+v error=%v", projection, err)
	}
}

func stringPointer(value string) *string {
	return &value
}

func stringSlicePointer(value []string) *[]string {
	return &value
}

// A partial JSON batch must report its per-item evidence and exit nonzero so
// shell and agent callers cannot mistake a failed batch for success.
func TestBatchCommandJSONPartialExitsNonzero(t *testing.T) {
	inputPath := filepath.Join(t.TempDir(), "batch.json")
	payload := []byte(`{"operation":"read","items":[{"id":"good","ref":"msg_ref"},{"id":"bad","ref":"bogus"}]}`)
	if err := os.WriteFile(inputPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), mail.NewService(failingRefGateway{}),
		[]string{"batch", "--input", inputPath, "--json"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("Run() code = %d, want 1, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; stdout = %q", err, stdout.String())
	}
	if response.OK || response.Error == nil || response.Error.Code != "batch_partial" {
		t.Fatalf("response = %+v", response)
	}
	result := response.Data.BatchResult
	if result == nil || result.Completed != 1 || result.Failed != 1 || len(result.Items) != 2 {
		t.Fatalf("batch result = %+v", result)
	}
	if result.Items[0].ID != "good" || result.Items[0].State != mail.BatchItemCompleted ||
		result.Items[1].ID != "bad" || result.Items[1].State != mail.BatchItemFailed ||
		result.Items[1].Error == nil || result.Items[1].Error.Code != "not_found" {
		t.Fatalf("batch items = %+v", result.Items)
	}
}

// The human path already exits nonzero for partial batches; keep both modes in
// agreement on a mixed result.
func TestBatchCommandHumanPartialExitsNonzero(t *testing.T) {
	inputPath := filepath.Join(t.TempDir(), "batch.json")
	payload := []byte(`{"operation":"read","items":[{"id":"good","ref":"msg_ref"},{"id":"bad","ref":"bogus"}]}`)
	if err := os.WriteFile(inputPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), mail.NewService(failingRefGateway{}),
		[]string{"batch", "--input", inputPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("Run() code = %d, want 1, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("completed=1")) ||
		!bytes.Contains(stdout.Bytes(), []byte("failed=1")) {
		t.Fatalf("human output = %q", stdout.String())
	}
}

type failingRefGateway struct {
	testGateway
}

func (failingRefGateway) GetMessage(ctx context.Context, ref string) (mail.Message, error) {
	if ref == "bogus" {
		return mail.Message{}, &mail.OperationError{Code: "not_found", Message: "message ref is unavailable"}
	}
	return testGateway{}.GetMessage(ctx, ref)
}

func TestBatchCommandAttachmentConflictFailsBeforeWriting(t *testing.T) {
	inputPath := filepath.Join(t.TempDir(), "batch.json")
	destination := filepath.Join(t.TempDir(), "same.bin")
	payload := []byte(`{"operation":"attachment_save","items":[{"id":"one","ref":"msg_ref","attachment_id":"1","output_path":"` + destination + `"},{"id":"two","ref":"msg_ref","attachment_id":"2","output_path":"` + destination + `"}]}`)
	if err := os.WriteFile(inputPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), newTestService(), []string{"batch", "--input", inputPath, "--json"}, &stdout, &stderr)
	if code != 2 || stderr.Len() != 0 {
		t.Fatalf("Run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; stdout = %q", err, stdout.String())
	}
	if response.OK || response.Error == nil || response.Error.Code != "invalid_argument" {
		t.Fatalf("response = %+v", response)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("destination state = %v, want absent", err)
	}
}

func TestBatchCapabilitiesExposeLimitsAndOperations(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := Run(context.Background(), newTestService(), []string{"capabilities", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("capabilities exit = %d, stderr = %q", code, stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Capabilities == nil {
		t.Fatal("capabilities missing")
	}
	limits := response.Data.Capabilities.Limits
	if limits.MaximumBatchItems != mail.MaximumBatchItems || limits.DefaultBatchConcurrency != mail.DefaultBatchConcurrency || limits.MaximumBatchConcurrency != mail.MaximumBatchConcurrency || limits.MaximumBatchInputBytes != mail.MaximumBatchInputBytes {
		t.Fatalf("batch limits = %+v", limits)
	}
	want := []string{
		string(mail.BatchOperationRead), string(mail.BatchOperationAttachmentSave), string(mail.BatchOperationMark),
		string(mail.BatchOperationMove), string(mail.BatchOperationCopy), string(mail.BatchOperationDelete),
	}
	if len(limits.BatchOperations) != len(want) {
		t.Fatalf("batch operations = %v", limits.BatchOperations)
	}
	for index, operation := range want {
		if limits.BatchOperations[index] != operation {
			t.Fatalf("batch operations = %v", limits.BatchOperations)
		}
	}
}

func TestBatchCommandMoveWithMailboxField(t *testing.T) {
	inputPath := writeBatchInput(t, mail.BatchRequest{
		Operation: mail.BatchOperationMove,
		Items:     []mail.BatchItem{{ID: "one", Ref: batchCommandMessageRef(t), Mailbox: "archive"}},
	})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), newTestService(), []string{"batch", "--input", inputPath, "--json"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("Run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	result := response.Data.BatchResult
	if !response.OK || result == nil || result.Items[0].State != mail.BatchItemCompleted ||
		result.Items[0].MessageState == nil || result.Items[0].MessageState.MailboxRef != "archive" {
		t.Fatalf("move result = %+v", result)
	}
	if result.Items[0].MessageState.ServerTruth == nil || result.Items[0].MessageState.ServerTruth.Command != "MOVE" ||
		result.Items[0].MessageState.ServerTruth.UID != 1 {
		t.Fatalf("move mutation evidence = %+v", result.Items[0].MessageState.ServerTruth)
	}
}

func TestBatchCommandMarkPreservesMutationEvidence(t *testing.T) {
	read := true
	inputPath := writeBatchInput(t, mail.BatchRequest{
		Operation: mail.BatchOperationMark,
		Items:     []mail.BatchItem{{ID: "mark", Ref: batchCommandMessageRef(t), Read: &read}},
	})
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), newTestService(), []string{"batch", "--input", inputPath, "--json"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("Run() code = %d, stderr = %q, output = %q", code, stderr.String(), stdout.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	result := response.Data.BatchResult
	if !response.OK || result == nil || result.Items[0].MessageState == nil ||
		result.Items[0].MessageState.ServerTruth == nil || result.Items[0].MessageState.ServerTruth.Command != "STORE" ||
		result.Items[0].MessageState.ServerTruth.UID != 1 {
		t.Fatalf("mark mutation evidence = %+v", result)
	}
}

func TestBatchCommandAttachmentSavePreservesEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attachment.bin")
	inputPath := writeBatchInput(t, mail.BatchRequest{
		Operation: mail.BatchOperationAttachmentSave,
		Items:     []mail.BatchItem{{ID: "save", Ref: "msg_ref", AttachmentID: "1", OutputPath: path}},
	})
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), newTestService(), []string{"batch", "--input", inputPath, "--json"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("Run() code = %d, stderr = %q, output = %q", code, stderr.String(), stdout.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	result := response.Data.BatchResult
	if !response.OK || result == nil || result.Items[0].SavedAttachment == nil ||
		result.Items[0].SavedAttachment.AttachmentID != "1" || result.Items[0].SavedAttachment.Path != path ||
		result.Items[0].SavedAttachment.Size != int64(len("attachment bytes")) ||
		len(result.Items[0].SavedAttachment.SHA256) != 64 {
		t.Fatalf("attachment-save evidence = %+v", result)
	}
}

func TestBatchJSONAttachmentSaveErrorPreservesVerifiedEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attachment.bin")
	saved := &mail.SavedAttachment{
		AttachmentID: "1", Path: path, Size: 8,
		SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	guidance := mail.OperationGuidance{
		Phase: mail.OperationPhaseCleanup, EffectCertainty: mail.EffectComplete,
		Retryability: mail.RetryObserveRequired, ReplayAllowed: false,
		Recovery: mail.RecoveryGuidance{Action: mail.RecoveryInspect},
	}
	request := mail.BatchRequest{
		Operation: mail.BatchOperationAttachmentSave,
		Items:     []mail.BatchItem{{ID: "save", Ref: "msg_ref", AttachmentID: "1", OutputPath: path}},
	}
	result := mail.BatchResult{
		Operation: mail.BatchOperationAttachmentSave, Total: 1, Failed: 1,
		Items: []mail.BatchItemResult{{
			ID: "save", State: mail.BatchItemFailed, SavedAttachment: saved,
			Error: &mail.BatchItemError{
				Code: "operation_failed", Message: "close failed", Retryable: false, Guidance: &guidance,
			},
		}},
	}
	var stdout bytes.Buffer
	if code := writeBatchJSON(&stdout, result, request, maximumJSONOutputBytes); code != 1 {
		t.Fatalf("writeBatchJSON() code = %d, want partial-batch exit 1", code)
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output = %q", err, stdout.String())
	}
	assertBatchAttachmentSaveFailureResponse(t, response, saved)
}

func assertBatchAttachmentSaveFailureResponse(t *testing.T, response envelope, saved *mail.SavedAttachment) {
	t.Helper()
	if response.OK || response.Data.BatchResult == nil || len(response.Data.BatchResult.Items) != 1 {
		t.Fatalf("response = %+v, want failed batch item", response)
	}
	item := response.Data.BatchResult.Items[0]
	if item.SavedAttachment == nil || item.SavedAttachment.Path != saved.Path || item.SavedAttachment.Size != saved.Size ||
		item.SavedAttachment.SHA256 != saved.SHA256 || item.Error == nil || item.Error.Guidance == nil {
		t.Fatalf("batch item = %+v, want retained save evidence and item guidance", item)
	}
	if item.Error.Retryable || item.Error.Guidance.ReplayAllowed ||
		item.Error.Guidance.EffectCertainty != mail.EffectComplete {
		t.Fatalf("batch item error = %+v, want complete effect with replay disabled", item.Error)
	}
}

func TestBatchCommandDeleteRequiresConfirm(t *testing.T) {
	inputPath := filepath.Join(t.TempDir(), "batch.json")
	payload := []byte(`{"operation":"delete","items":[{"id":"one","ref":"msg_ref"}]}`)
	if err := os.WriteFile(inputPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), newTestService(), []string{"batch", "--input", inputPath, "--json"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("Run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error == nil || response.Error.Code != "confirmation_required" {
		t.Fatalf("response = %+v", response)
	}
}

func TestBatchCommandDeleteWithConfirmCompletes(t *testing.T) {
	inputPath := writeBatchInput(t, mail.BatchRequest{
		Operation: mail.BatchOperationDelete,
		Items:     []mail.BatchItem{{ID: "one", Ref: batchCommandMessageRef(t)}},
	})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), newTestService(), []string{"batch", "--input", inputPath, "--confirm", "--json"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("Run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	result := response.Data.BatchResult
	if !response.OK || result == nil || result.Items[0].State != mail.BatchItemCompleted ||
		result.Items[0].DeleteResult == nil || !result.Items[0].DeleteResult.Deleted ||
		result.Items[0].DeleteResult.ServerTruth == nil || result.Items[0].DeleteResult.ServerTruth.Command != "DELETE" ||
		result.Items[0].DeleteResult.ServerTruth.UID != 1 {
		t.Fatalf("delete result = %+v", result)
	}
}

func TestBatchCommandConfirmRejectedForOtherOperations(t *testing.T) {
	inputPath := filepath.Join(t.TempDir(), "batch.json")
	payload := []byte(`{"operation":"copy","items":[{"id":"one","ref":"msg_ref","mailbox":"archive"}]}`)
	if err := os.WriteFile(inputPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), newTestService(), []string{"batch", "--input", inputPath, "--confirm", "--json"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("Run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error == nil || response.Error.Code != "invalid_argument" {
		t.Fatalf("response = %+v", response)
	}
}

func batchCommandMessageRef(t *testing.T) string {
	t.Helper()
	ref, err := mailref.EncodeMessage(mailref.Message{
		AccountID: "account", MailboxPath: []string{"Inbox"}, LibraryID: "1",
		ExpectedStoreUUID: "store", ExpectedStoreMailboxID: 1, ExpectedStoreMessageID: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}
