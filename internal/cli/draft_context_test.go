package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mailcli/internal/mail"
)

func TestDraftContextEnvelopeKeepsDeadlineSeparate(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller cancellation", true: "operation timeout"}[deadline], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "absent")
			service := mail.NewServiceWithDraftRoot(nil, root)
			ctx, cancel := context.WithCancel(context.Background())
			wantCode, wantNext := "draft_operation_canceled", "stop"
			if deadline {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				wantCode, wantNext = "draft_operation_timeout", "retry"
			} else {
				cancel()
			}
			defer cancel()
			var stdout, stderr bytes.Buffer
			code := Run(ctx, service, []string{"drafts", "list", "--json"}, &stdout, &stderr)
			var response envelope
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != 1 || response.Error == nil || response.Error.Code != wantCode || response.Next == nil || response.Next.Do != wantNext {
				t.Fatalf("code=%d, error=%v, output=%s", code, err, &stdout)
			}
			if deadline && strings.Contains(strings.ToLower(response.Next.Why+response.Error.Message), "cancel") {
				t.Fatal("timeout claims caller cancellation")
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("stopped draft read changed disk state: %v", err)
			}
		})
	}
}

func TestDraftTimeoutCannotReplayUncertainWrite(t *testing.T) {
	for _, command := range []string{"drafts.send", "drafts.update", "drafts.prune"} {
		t.Run(command, func(t *testing.T) {
			failure := &mail.OperationError{Code: "draft_operation_timeout", Message: "draft operation timed out", Err: context.DeadlineExceeded}
			value := envelope{SchemaVersion: schemaVersion, Command: command, Error: newErrorData(command, responseData{}, failure)}
			payload, err := marshalEnvelope(value)
			if err != nil {
				t.Fatal(err)
			}
			var response envelope
			if err := json.Unmarshal(payload, &response); err != nil {
				t.Fatal(err)
			}
			if response.Next == nil || response.Next.Do != "check_state" || response.Error.Guidance.ReplayAllowed || response.Error.Guidance.EffectCertainty == mail.EffectNone {
				t.Fatalf("uncertain timeout permitted replay: %s", payload)
			}
		})
	}
}
