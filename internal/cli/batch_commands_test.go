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
	if len(limits.BatchOperations) != 3 || limits.BatchOperations[0] != string(mail.BatchOperationRead) || limits.BatchOperations[1] != string(mail.BatchOperationAttachmentSave) || limits.BatchOperations[2] != string(mail.BatchOperationMark) {
		t.Fatalf("batch operations = %v", limits.BatchOperations)
	}
}
