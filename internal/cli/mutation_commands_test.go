package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestMutationCommandsJSONTable(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		command string
	}{
		{name: "mark", args: []string{"messages", "mark", "--ref", "msg_ref", "--read", "true", "--json"}, command: "messages.mark"},
		{name: "move", args: []string{"messages", "move", "--ref", "msg_ref", "--mailbox", "mbx_ref", "--json"}, command: "messages.move"},
		{name: "copy", args: []string{"messages", "copy", "--ref", "msg_ref", "--mailbox", "mbx_ref", "--json"}, command: "messages.copy"},
		{name: "delete", args: []string{"messages", "delete", "--ref", "msg_ref", "--confirm", "--json"}, command: "messages.delete"},
		{name: "sync", args: []string{"sync", "--json"}, command: "sync"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := Run(context.Background(), newTestService(), test.args, &stdout, &stderr)
			if code != 0 || !strings.Contains(stdout.String(), `"command":"`+test.command+`"`) {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestMessageDeleteReturnsServerTruth(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), newTestService(),
		[]string{"messages", "delete", "--ref", "msg_ref", "--confirm", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"server_truth":{"command":"DELETE"`) {
		t.Fatalf("delete output lacks server truth: %q", stdout.String())
	}
}

func TestMutationMarkMoveCarryServerTruth(t *testing.T) {
	for _, args := range [][]string{
		{"messages", "mark", "--ref", "msg_ref", "--read", "true", "--json"},
		{"messages", "move", "--ref", "msg_ref", "--mailbox", "mbx_ref", "--json"},
	} {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		code := Run(context.Background(), newTestService(), args, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("Run(%v) code = %d, stderr = %q", args, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), `"server_truth":{"command":"STORE"`) &&
			!strings.Contains(stdout.String(), `"server_truth":{"command":"MOVE"`) {
			t.Fatalf("output for %v lacks server truth: %q", args, stdout.String())
		}
	}
}

func TestMessageDeleteRequiresConfirmation(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(
		context.Background(), newTestService(),
		[]string{"messages", "delete", "--ref", "msg_ref", "--json"}, &stdout, &stderr,
	)
	if code != 1 || !strings.Contains(stdout.String(), `"code":"confirmation_required"`) {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}
