package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestDecodeDraftInputStrictTable(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "valid", input: `{"to":[{"address":"ada@example.com"}],"subject":"Hello","body":"Line one\\nLine two"}`},
		{name: "unknown field", input: `{"to":[],"body":"","html":"no"}`, wantErr: true},
		{name: "trailing object", input: `{"to":[],"body":""} {}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input, err := decodeDraftInput(strings.NewReader(test.input))
			if (err != nil) != test.wantErr {
				t.Fatalf("decodeDraftInput() input = %+v, error = %v", input, err)
			}
		})
	}
}

func TestDraftSendRequiresConfirmation(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runDraftSend(
		context.Background(), newTestService(), []string{"--ref", "draft_ref", "--json"}, &stdout, &stderr,
	)
	if code != 1 || !strings.Contains(stdout.String(), `"code":"confirmation_required"`) {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}
