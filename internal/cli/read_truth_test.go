package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

type partialMessageGateway struct {
	testGateway
}

func (partialMessageGateway) GetMessage(context.Context, string) (mail.Message, error) {
	return mail.Message{
		Summary: mail.MessageSummary{Ref: "msg_ref", Subject: "Partial"},
		Content: "available text", ContentSource: "emlx_partial", ContentComplete: false,
		MissingParts: []string{"external attachment bytes"},
		Attachments: []mail.Attachment{{
			ID: "part-1", Name: "invoice.pdf", Size: 0, SizeKnown: false, Downloaded: false,
		}},
	}, nil
}

func TestHumanReadOutputPreservesCompletenessTable(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "message detail", args: []string{"messages", "get", "--ref", "msg_ref"},
			want: []string{"Content source: emlx_partial", "Content complete: false", "Missing parts: external attachment bytes"},
		},
		{
			name: "attachment list", args: []string{"attachments", "list", "--message", "msg_ref"},
			want: []string{"size=unknown", "size_known=false", "content\tsource=emlx_partial\tcomplete=false", "missing=external attachment bytes"},
		},
	}
	service := mail.NewService(partialMessageGateway{})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := Run(context.Background(), service, test.args, &stdout, &stderr)
			if code != 0 || stderr.Len() != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
			}
			for _, wanted := range test.want {
				if !strings.Contains(stdout.String(), wanted) {
					t.Fatalf("stdout = %q, want %q", stdout.String(), wanted)
				}
			}
		})
	}
}

func TestAttachmentJSONPreservesCompleteness(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(
		context.Background(), mail.NewService(partialMessageGateway{}),
		[]string{"attachments", "list", "--message", "msg_ref", "--json"},
		&stdout, &stderr,
	)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if response.Data.ContentComplete == nil || *response.Data.ContentComplete ||
		response.Data.ContentSource != "emlx_partial" || response.Data.MissingParts == nil ||
		len(*response.Data.MissingParts) != 1 || response.Data.Attachments == nil ||
		len(*response.Data.Attachments) != 1 || (*response.Data.Attachments)[0].SizeKnown {
		t.Fatalf("response data = %+v", response.Data)
	}
}
