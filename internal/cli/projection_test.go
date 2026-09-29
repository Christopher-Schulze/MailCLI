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
	"sync"
	"testing"

	"mailcli/internal/mail"
)

type projectionGateway struct {
	testGateway
	mu        sync.Mutex
	message   mail.Message
	raw       string
	getCalls  int
	rawCalls  int
	openCalls int
	getErr    error
}

func (g *projectionGateway) GetMessage(context.Context, string) (mail.Message, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.getCalls++
	return g.message, g.getErr
}

func (g *projectionGateway) OpenDraft(context.Context, string) (mail.Message, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.openCalls++
	return g.message, g.getErr
}

func (g *projectionGateway) GetRawSource(context.Context, string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
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
		{name: "metadata", args: []string{"messages", "get", "--ref", "msg_ref", "--json"}, want: []string{`"view":"metadata"`, `"summary"`}, forbidden: []string{`"headers"`, `"content":"body bytes"`, `"content_complete"`, `"content_source"`, `"attachments"`, `"missing_parts"`}, view: outputViewMetadata},
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

func trackedLinkMessage() mail.Message {
	message := projectionMessage()
	message.Content = "Upload\nhttps://click.example.com/f/a/upYx533wyWJ-C15_1C36yg~~/AAAmIhA~/6m7yqwKk6656jrzYqqLXL1faPlsHRF6\n\n\n\nBye"
	return message
}

func TestMessageGetLinksModesReduceOnlyTheReturnedContent(t *testing.T) {
	contentOf := func(t *testing.T, output string) string {
		t.Helper()
		var response struct {
			Data struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			t.Fatal(err)
		}
		return response.Data.Message.Content
	}
	for _, test := range []struct{ links, want string }{
		{"", trackedLinkMessage().Content},
		{"full", trackedLinkMessage().Content},
		{"host", "Upload\n<click.example.com>\n\nBye"},
		{"none", "Upload\n\nBye"},
	} {
		args := []string{"messages", "get", "--ref", "msg_ref", "--view", "plain", "--json"}
		if test.links != "" {
			args = append(args, "--links", test.links)
		}
		code, output, stderr := runProjectionCommand(t, &projectionGateway{message: trackedLinkMessage()}, args...)
		if got := contentOf(t, output); code != 0 || stderr != "" || got != test.want {
			t.Fatalf("--links %q: code=%d stderr=%q content=%q, want %q", test.links, code, stderr, got, test.want)
		}
	}
	// An export always carries the complete content.
	exportPath := filepath.Join(t.TempDir(), "body.txt")
	code, output, stderr := runProjectionCommand(t, &projectionGateway{message: trackedLinkMessage()},
		"messages", "get", "--ref", "msg_ref", "--view", "plain", "--links", "none", "--export", exportPath, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("export: code=%d stderr=%q output=%s", code, stderr, output)
	}
	assertExportFile(t, exportPath, []byte(trackedLinkMessage().Content), output)
	// An invalid mode fails before any retrieval.
	gateway := &projectionGateway{message: trackedLinkMessage()}
	code, output, stderr = runProjectionCommand(t, gateway, "messages", "get", "--ref", "msg_ref", "--links", "short", "--json")
	if code != 2 || stderr != "" || gateway.getCalls != 0 || !strings.Contains(output, `"code":"invalid_argument"`) {
		t.Fatalf("invalid mode: code=%d calls=%d stderr=%q output=%s", code, gateway.getCalls, stderr, output)
	}
}

func TestFullViewOmitsHeaderFieldsUnlessSelected(t *testing.T) {
	code, full, stderr := runProjectionCommand(t, &projectionGateway{message: projectionMessage()},
		"messages", "get", "--ref", "msg_ref", "--view", "full", "--json")
	if code != 0 || stderr != "" || !strings.Contains(full, `"headers"`) || strings.Contains(full, `"header_fields"`) {
		t.Fatalf("full view: code=%d stderr=%q output=%s", code, stderr, full)
	}
	code, selected, stderr := runProjectionCommand(t, &projectionGateway{message: projectionMessage()},
		"messages", "get", "--ref", "msg_ref", "--fields", "summary,header_fields", "--json")
	if code != 0 || stderr != "" || !strings.Contains(selected, `"header_fields"`) {
		t.Fatalf("selected header_fields: code=%d stderr=%q output=%s", code, stderr, selected)
	}
}

func TestDraftReviewShowsSenderAndSendBlockers(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(testGateway{}, t.TempDir())
	code, output, stderr := runProjectionDraftCreate(service, "--to", "recipient@example.com", "--body", "body", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("create: code=%d stderr=%q output=%s", code, stderr, output)
	}
	var created struct {
		Data struct {
			Draft struct {
				Ref          string   `json:"ref"`
				From         *string  `json:"from"`
				SendBlockers []string `json:"send_blockers"`
			} `json:"draft"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &created); err != nil {
		t.Fatal(err)
	}
	if created.Data.Draft.From == nil || *created.Data.Draft.From != "" ||
		!slices.Equal(created.Data.Draft.SendBlockers, []string{"from_missing"}) {
		t.Fatalf("created draft = %+v, want an empty from and the from_missing blocker: %s", created.Data.Draft, output)
	}
	var stdout, errOut bytes.Buffer
	code = Run(context.Background(), service, []string{"drafts", "preview", "--ref", created.Data.Draft.Ref, "--json"}, &stdout, &errOut)
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("preview: code=%d stderr=%q output=%s", code, errOut.String(), stdout.String())
	}
	var preview struct {
		Data struct {
			Preview map[string]json.RawMessage `json:"draft_preview"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if string(preview.Data.Preview["from"]) != `""` || string(preview.Data.Preview["send_blockers"]) != `["from_missing"]` {
		t.Fatalf("preview from=%s send_blockers=%s, want an empty from and the blocker", preview.Data.Preview["from"], preview.Data.Preview["send_blockers"])
	}
}

func TestDraftProjectionKeepsEmptyListsArrays(t *testing.T) {
	encoded, err := json.Marshal(draftProjectionFor(mail.Draft{Ref: "draft"},
		outputOptions{target: projectionTargetDraft, view: outputViewPlain}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"content_diagnostics":[]`) || strings.Contains(string(encoded), "null") {
		t.Fatalf("draft projection = %s", encoded)
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

func TestProjectionFieldErrorsNameCLIFlag(t *testing.T) {
	for _, value := range []string{"", "summary,", "unknown", "summary,summary", "all,summary"} {
		t.Run(value, func(t *testing.T) {
			gateway := &projectionGateway{message: projectionMessage()}
			code, output, stderr := runProjectionCommand(t, gateway,
				"messages", "get", "--ref", "msg_ref", "--fields", value, "--json")
			var response envelope
			if err := json.Unmarshal([]byte(output), &response); err != nil {
				t.Fatal(err)
			}
			if code != 2 || stderr != "" || gateway.getCalls != 0 || response.Error == nil ||
				response.Error.Code != "invalid_argument" || !strings.Contains(response.Error.Message, "--fields") ||
				strings.Contains(response.Error.Message, "items[") {
				t.Fatalf("CLI field error: code=%d calls=%d stderr=%q response=%+v", code, gateway.getCalls, stderr, response.Error)
			}
			if value == "" && response.Error.Message != "--fields must contain at least one field" {
				t.Fatalf("CLI empty-fields wording changed: %q", response.Error.Message)
			}
		})
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

func TestProjectionRegistriesValidateAllSevenTargets(t *testing.T) {
	for _, target := range []projectionTarget{
		projectionTargetMessage, projectionTargetDraft, projectionTargetAttachment,
		projectionTargetRaw, projectionTargetDraftList, projectionTargetListPage, projectionTargetSearchPage,
	} {
		t.Run(string(target), func(t *testing.T) {
			registry := projectionRegistry(target)
			names := projectionOutputFieldNames(target)
			inputNames := projectionFieldNames(target)
			seen := make(map[string]bool)
			for _, name := range names {
				if seen[name] {
					t.Fatalf("duplicate registry field %q", name)
				}
				seen[name] = true
				if name != "all" && requiredProjectionField(target, name, false) != slices.Contains(registry.core, name) {
					t.Fatalf("core policy differs for %q", name)
				}
			}
			if !seen["all"] || len(names) != len(registry.core)+len(registry.optional)+1 {
				t.Fatalf("incomplete registry: %+v", names)
			}
			for _, name := range inputNames {
				fields, err := parseProjectionFields(target, name)
				if err != nil || len(fields) != 1 {
					t.Fatalf("published selector %q rejected: %v", name, err)
				}
			}
			if target == projectionTargetDraftList {
				want := append(slices.Clone(registry.optional), "all")
				if !slices.Equal(inputNames, want) {
					t.Fatalf("draft list selectors=%v, want optional fields plus all: %v", inputNames, want)
				}
				for _, field := range registry.core {
					if _, err := parseProjectionFields(target, field); err == nil {
						t.Fatalf("fixed core field %q was accepted as a selector", field)
					}
				}
			} else if !slices.Equal(inputNames, names) {
				t.Fatalf("%s selectors differ from registry: got %v, want %v", target, inputNames, names)
			}
			for _, value := range []string{"", "unknown", inputNames[0] + "," + inputNames[0], "all," + inputNames[0]} {
				if _, err := parseProjectionFields(target, value); err == nil {
					t.Fatalf("invalid selector %q accepted", value)
				}
			}
		})
	}
}

func TestMessageAndRawAllFieldsMatchFullViews(t *testing.T) {
	for _, command := range []string{"get", "raw"} {
		t.Run(command, func(t *testing.T) {
			args := []string{"messages", command, "--ref", "msg_ref", "--json"}
			fullCode, full, fullErr := runProjectionCommand(t, &projectionGateway{message: projectionMessage(), raw: "raw bytes"}, append(args, "--view", "full")...)
			allCode, all, allErr := runProjectionCommand(t, &projectionGateway{message: projectionMessage(), raw: "raw bytes"}, append(args, "--fields", "all")...)
			var fullEnvelope, allEnvelope envelope
			if err := json.Unmarshal([]byte(full), &fullEnvelope); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(all), &allEnvelope); err != nil {
				t.Fatal(err)
			}
			fullEnvelope.Data.Projection, allEnvelope.Data.Projection = nil, nil
			fullData, fullMarshal := json.Marshal(fullEnvelope.Data)
			allData, allMarshal := json.Marshal(allEnvelope.Data)
			if fullCode != 0 || allCode != 0 || fullErr != "" || allErr != "" || fullMarshal != nil || allMarshal != nil || !bytes.Equal(fullData, allData) {
				t.Fatalf("all changed full data: full=%s all=%s", full, all)
			}
		})
	}
}

func TestBatchReadFieldsUseMessageRegistry(t *testing.T) {
	for _, field := range projectionFieldNames(projectionTargetMessage) {
		fields := []string{field}
		options, err := batchReadOutputOptions(mail.BatchItem{Fields: &fields}, 0, defaultJSONOutputBytes)
		if err != nil || options.target != projectionTargetMessage || !options.fieldsProvided || !options.includes(field) {
			t.Fatalf("batch field %q differs from message parser: options=%+v error=%v", field, options, err)
		}
	}
	for _, fields := range [][]string{{"unknown"}, {"all", "content"}, {"content", "content"}, {"summary,content"}} {
		if _, err := batchReadOutputOptions(mail.BatchItem{Fields: &fields}, 0, defaultJSONOutputBytes); err == nil {
			t.Fatalf("invalid batch field selector accepted: %v", fields)
		}
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
	projection := mustCapabilities(t).Limits.OutputProjection
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
		!slices.Equal(projection.DraftListFields, projectionFieldNames(projectionTargetDraftList)) ||
		!slices.Equal(projection.DraftListCoreFields, projectionCoreFieldNames(projectionTargetDraftList)) ||
		!slices.Equal(projection.DraftListOptionalFields, projectionOptionalFieldNames(projectionTargetDraftList)) ||
		!slices.Equal(projection.ListPageFields, projectionFieldNames(projectionTargetListPage)) ||
		!slices.Equal(projection.SearchPageFields, projectionFieldNames(projectionTargetSearchPage)) ||
		!slices.Equal(projection.ExportCommands, []string{"messages.get", "messages.raw", "drafts.inspect"}) {
		t.Fatalf("output projection schema = %+v", projection)
	}
	encoded, err := json.Marshal(mustCapabilities(t))
	if err != nil {
		t.Fatal(err)
	}
	var published struct {
		Limits struct {
			OutputProjection struct {
				DraftListFields         []string `json:"draft_list_fields"`
				DraftListCoreFields     []string `json:"draft_list_core_fields"`
				DraftListOptionalFields []string `json:"draft_list_optional_fields"`
			} `json:"output_projection"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(encoded, &published); err != nil {
		t.Fatal(err)
	}
	wireProjection := published.Limits.OutputProjection
	if !slices.Equal(wireProjection.DraftListFields, projectionFieldNames(projectionTargetDraftList)) ||
		!slices.Equal(wireProjection.DraftListCoreFields, projectionCoreFieldNames(projectionTargetDraftList)) ||
		!slices.Equal(wireProjection.DraftListOptionalFields, projectionOptionalFieldNames(projectionTargetDraftList)) {
		t.Fatalf("serialized draft-list field capability = %+v", wireProjection)
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
		{name: "all", args: []string{"--ref", draft.Ref, "--fields", "all", "--json"}, want: `"body":"canonical body"`, forbidden: nil},
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
