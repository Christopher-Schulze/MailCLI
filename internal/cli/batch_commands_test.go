package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"mailcli/internal/mail"
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
	inputPath := filepath.Join(t.TempDir(), "batch.json")
	payload := []byte(`{"operation":"move","items":[{"id":"one","ref":"msg_ref","mailbox":"archive"}]}`)
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
		t.Fatal(err)
	}
	result := response.Data.BatchResult
	if !response.OK || result == nil || result.Items[0].State != mail.BatchItemCompleted ||
		result.Items[0].MessageState == nil || result.Items[0].MessageState.MailboxRef != "archive" {
		t.Fatalf("move result = %+v", result)
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
	inputPath := filepath.Join(t.TempDir(), "batch.json")
	payload := []byte(`{"operation":"delete","items":[{"id":"one","ref":"msg_ref"}]}`)
	if err := os.WriteFile(inputPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
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
		result.Items[0].DeleteResult == nil || !result.Items[0].DeleteResult.Deleted {
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
