package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

type projectionGateway struct {
	testGateway
	message   mail.Message
	raw       string
	getCalls  int
	rawCalls  int
	openCalls int
	getErr    error
}

func (g *projectionGateway) GetMessage(context.Context, string) (mail.Message, error) {
	g.getCalls++
	return g.message, g.getErr
}

func (g *projectionGateway) OpenDraft(context.Context, string) (mail.Message, error) {
	g.openCalls++
	return g.message, g.getErr
}

func (g *projectionGateway) GetRawSource(context.Context, string) (string, error) {
	g.rawCalls++
	return g.raw, nil
}

func projectionMessage() mail.Message {
	return mail.Message{
		Summary: mail.MessageSummary{Ref: "msg_ref", MessageID: "<message@example.com>", Subject: "Subject"},
		ReplyTo: "reply@example.com", To: []mail.Recipient{{Address: "to@example.com"}},
		CC: []mail.Recipient{{Address: "cc@example.com"}}, BCC: []mail.Recipient{{Address: "bcc@example.com"}},
		Headers: "X-Trace: private\r\n", Content: "body bytes", ContentSource: "emlx_full",
		ContentComplete: true, MissingParts: []string{}, Attachments: []mail.Attachment{{ID: "1", Name: "a.txt"}},
	}
}

func runProjectionCommand(t *testing.T, gateway *projectionGateway, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), mail.NewService(gateway), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestMessageProjectionViewsSelectOnlyRequestedContent(t *testing.T) {
	for _, test := range []struct {
		name      string
		args      []string
		want      []string
		forbidden []string
		view      string
	}{
		{name: "metadata", args: []string{"messages", "get", "--ref", "msg_ref", "--json"}, want: []string{`"content_complete":true`, `"content_source":"emlx_full"`, `"view":"metadata"`}, forbidden: []string{`"headers"`, `"content":"body bytes"`}, view: outputViewMetadata},
		{name: "plain", args: []string{"messages", "get", "--ref", "msg_ref", "--view", "plain", "--json"}, want: []string{`"content":"body bytes"`, `"view":"plain"`}, forbidden: []string{`"headers"`}, view: outputViewPlain},
		{name: "full", args: []string{"messages", "get", "--ref", "msg_ref", "--view", "full", "--json"}, want: []string{`"content":"body bytes"`, `"headers":"X-Trace: private\r\n"`, `"view":"full"`}, view: outputViewFull},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, output, stderr := runProjectionCommand(t, &projectionGateway{message: projectionMessage()}, test.args...)
			if code != 0 || stderr != "" {
				t.Fatalf("code = %d, stderr = %q, output = %q", code, stderr, output)
			}
			for _, wanted := range test.want {
				if !strings.Contains(output, wanted) {
					t.Fatalf("output = %q, missing %q", output, wanted)
				}
			}
			for _, forbidden := range test.forbidden {
				if strings.Contains(output, forbidden) {
					t.Fatalf("output = %q, unexpectedly contains %q", output, forbidden)
				}
			}
			var response envelope
			if err := json.Unmarshal([]byte(output), &response); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if response.Data.Projection == nil || response.Data.Projection.View != test.view {
				t.Fatalf("projection = %+v", response.Data.Projection)
			}
		})
	}
}

func TestProjectionValidationPrecedesMessageRetrieval(t *testing.T) {
	for _, args := range [][]string{
		{"messages", "get", "--ref", "msg_ref", "--view", "unknown", "--json"},
		{"messages", "get", "--ref", "msg_ref", "--fields", "summary,unknown", "--json"},
		{"messages", "get", "--ref", "msg_ref", "--view", "full", "--fields", "summary", "--json"},
	} {
		gateway := &projectionGateway{message: projectionMessage()}
		code, output, stderr := runProjectionCommand(t, gateway, args...)
		if code != 2 || stderr != "" || gateway.getCalls != 0 {
			t.Fatalf("args = %v, code = %d, stderr = %q, calls = %d, output = %q", args, code, stderr, gateway.getCalls, output)
		}
		if !strings.Contains(output, `"code":"invalid_argument"`) {
			t.Fatalf("args = %v, output = %q", args, output)
		}
	}
}

func TestProjectionValidationPrecedesDraftMutation(t *testing.T) {
	root := t.TempDir()
	service := mail.NewServiceWithDraftRoot(testGateway{}, root)
	code, output, stderr := runProjectionDraftCreate(service,
		"--to", "recipient@example.com", "--body", "body", "--view", "unknown", "--json")
	if code != 2 || stderr != "" || !strings.Contains(output, `"code":"invalid_argument"`) {
		t.Fatalf("code = %d, stderr = %q, output = %q", code, stderr, output)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("draft files created before validation: %+v", entries)
	}
}

func TestProjectionFieldsAreExplicitAndCanonical(t *testing.T) {
	gateway := &projectionGateway{message: projectionMessage()}
	code, output, stderr := runProjectionCommand(t, gateway,
		"messages", "get", "--ref", "msg_ref", "--fields", "summary,content_complete", "--json")
	if code != 0 || stderr != "" || gateway.getCalls != 1 {
		t.Fatalf("code = %d, stderr = %q, calls = %d, output = %q", code, stderr, gateway.getCalls, output)
	}
	if strings.Contains(output, `"content":"body bytes"`) || strings.Contains(output, `"headers"`) ||
		!strings.Contains(output, `"fields":["content_complete","content_source","hydration","missing_parts","summary"]`) {
		t.Fatalf("unexpected field projection: %s", output)
	}
}

func TestRawProjectionAcceptsItsPublishedField(t *testing.T) {
	gateway := &projectionGateway{raw: "raw bytes"}
	code, output, stderr := runProjectionCommand(t, gateway,
		"messages", "raw", "--ref", "msg_ref", "--fields", "raw_source", "--json")
	if code != 0 || stderr != "" || gateway.rawCalls != 1 ||
		!strings.Contains(output, `"raw_source":"raw bytes"`) ||
		!strings.Contains(output, `"fields":["raw_source"]`) {
		t.Fatalf("code = %d, stderr = %q, calls = %d, output = %q", code, stderr, gateway.rawCalls, output)
	}
}

func TestProjectionCapabilityPublishesSchemasAndLimits(t *testing.T) {
	projection := capabilities().Limits.OutputProjection
	if projection.ViewFlag != "--view" || projection.FieldsFlag != "--fields" ||
		projection.MaxBytesFlag != "--max-bytes" || projection.ExportFlag != "--export" ||
		projection.MessageDefaultView != outputViewMetadata || projection.DraftDefaultView != outputViewPlain ||
		projection.RawDefaultView != outputViewFull || projection.DefaultJSONBytes != defaultJSONOutputBytes ||
		projection.MaximumJSONBytes != maximumJSONOutputBytes ||
		projection.MaximumContentExportBytes != maximumContentExportBytes {
		t.Fatalf("output projection capability = %+v", projection)
	}
	if !slices.Equal(projection.MessageFields, projectionFieldNames(projectionTargetMessage)) ||
		!slices.Equal(projection.DraftFields, projectionFieldNames(projectionTargetDraft)) ||
		!slices.Equal(projection.AttachmentFields, projectionFieldNames(projectionTargetAttachment)) ||
		!slices.Equal(projection.RawFields, projectionFieldNames(projectionTargetRaw)) ||
		!slices.Equal(projection.ExportCommands, []string{"messages.get", "messages.raw", "drafts.inspect"}) {
		t.Fatalf("output projection schema = %+v", projection)
	}
}

func TestExportPathValidationPrecedesMessageRetrieval(t *testing.T) {
	directory := t.TempDir()
	existing := filepath.Join(directory, "existing.txt")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	for _, path := range []string{"relative.txt", existing} {
		gateway := &projectionGateway{message: projectionMessage()}
		code, output, stderr := runProjectionCommand(t, gateway,
			"messages", "get", "--ref", "msg_ref", "--view", "full", "--export", path, "--json")
		if code != 2 || stderr != "" || gateway.getCalls != 0 || !strings.Contains(output, `"code":"invalid_argument"`) {
			t.Fatalf("path = %q, code = %d, stderr = %q, calls = %d, output = %q", path, code, stderr, gateway.getCalls, output)
		}
	}
}

func TestFailedHydrationProjectionRetainsRecoveryContent(t *testing.T) {
	gateway := &projectionGateway{
		message: projectionMessage(),
		getErr:  &testCodedError{code: "imap_timeout", message: "raw secret-token"},
	}
	gateway.message.ContentComplete = false
	gateway.message.Hydration = &mail.HydrationDiagnostic{State: mail.HydrationStateFailed, AttemptedSource: "imap", Remediation: "retry"}
	code, output, stderr := runProjectionCommand(t, gateway, "messages", "get", "--ref", "msg_ref", "--json")
	if code != 1 || stderr != "" || !strings.Contains(output, `"ok":false`) ||
		!strings.Contains(output, `"content":"body bytes"`) || !strings.Contains(output, `"content_complete":false`) ||
		strings.Contains(output, "secret-token") {
		t.Fatalf("code = %d, stderr = %q, output = %q", code, stderr, output)
	}
}

func TestOversizedProjectionReturnsExplicitFailureWithoutContent(t *testing.T) {
	gateway := &projectionGateway{message: projectionMessage()}
	gateway.message.Content = strings.Repeat("x", 4096)
	code, output, stderr := runProjectionCommand(t, gateway, "messages", "get", "--ref", "msg_ref", "--view", "full", "--max-bytes", "512", "--json")
	if code != 1 || stderr != "" || !strings.Contains(output, `"code":"output_too_large"`) ||
		strings.Contains(output, `"content":"`) || !strings.Contains(output, `"summary"`) ||
		!strings.Contains(output, `"content_complete":true`) {
		t.Fatalf("code = %d, stderr = %q, output = %q", code, stderr, output)
	}
}

func TestMessageAndRawExportAreExclusiveAndContentFreeInJSON(t *testing.T) {
	directory := t.TempDir()
	bodyPath := filepath.Join(directory, "body.txt")
	rawPath := filepath.Join(directory, "message.eml")
	gateway := &projectionGateway{message: projectionMessage(), raw: "raw bytes\r\n"}
	code, output, stderr := runProjectionCommand(t, gateway, "messages", "get", "--ref", "msg_ref", "--view", "full", "--export", bodyPath, "--json")
	if code != 0 || stderr != "" || strings.Contains(output, `"content":"`) || !strings.Contains(output, `"content_export"`) {
		t.Fatalf("body export: code = %d, stderr = %q, output = %q", code, stderr, output)
	}
	assertExportFile(t, bodyPath, []byte("body bytes"), output)
	code, output, stderr = runProjectionCommand(t, gateway, "messages", "raw", "--ref", "msg_ref", "--export", rawPath, "--json")
	if code != 0 || stderr != "" || strings.Contains(output, `"raw_source"`) || !strings.Contains(output, `"content_export"`) {
		t.Fatalf("raw export: code = %d, stderr = %q, output = %q", code, stderr, output)
	}
	assertExportFile(t, rawPath, []byte("raw bytes\r\n"), output)
}

func TestDraftProjectionViewsAndExport(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := mail.NewServiceWithDraftRoot(nil, root)
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		To: []mail.Recipient{{Address: "recipient@example.com"}}, Subject: "Subject",
		Body: "canonical body", BodyFormat: mail.DraftBodyMarkdown,
	}})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	for _, test := range []struct {
		name      string
		args      []string
		want      string
		forbidden []string
	}{
		{name: "metadata", args: []string{"--ref", draft.Ref, "--json"}, want: `"view":"metadata"`, forbidden: []string{`"body":`, `"body_source"`, `"body_html"`}},
		{name: "plain", args: []string{"--ref", draft.Ref, "--view", "plain", "--json"}, want: `"body":"canonical body"`, forbidden: []string{`"body_source"`, `"body_html"`}},
		{name: "full", args: []string{"--ref", draft.Ref, "--view", "full", "--json"}, want: `"body":"canonical body"`, forbidden: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runDraftInspect(service, test.args, &stdout, &stderr)
			if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), test.want) {
				t.Fatalf("code = %d, stderr = %q, output = %q", code, stderr.String(), stdout.String())
			}
			for _, forbidden := range test.forbidden {
				if strings.Contains(stdout.String(), forbidden) {
					t.Fatalf("output = %q, unexpectedly contains %q", stdout.String(), forbidden)
				}
			}
		})
	}
	exportPath := filepath.Join(t.TempDir(), "draft-body.txt")
	var stdout, stderr bytes.Buffer
	code := runDraftInspect(service, []string{"--ref", draft.Ref, "--view", "full", "--export", exportPath, "--json"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || strings.Contains(stdout.String(), `"body":`) || !strings.Contains(stdout.String(), `"content_export"`) {
		t.Fatalf("export code = %d, stderr = %q, output = %q", code, stderr.String(), stdout.String())
	}
	assertExportFile(t, exportPath, []byte("canonical body"), stdout.String())
}

func runProjectionDraftCreate(service *mail.Service, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), service, append([]string{"drafts", "create"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func assertExportFile(t *testing.T, path string, want []byte, output string) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if !bytes.Equal(actual, want) {
		t.Fatalf("exported content = %q, want %q", actual, want)
	}
	digest := sha256.Sum256(want)
	if !strings.Contains(output, `"size":`+itoa(len(want))) || !strings.Contains(output, `"sha256":"`+hex.EncodeToString(digest[:])+`"`) {
		t.Fatalf("output = %q, missing verified export proof", output)
	}
}

func itoa(value int) string {
	return fmt.Sprintf("%d", value)
}
