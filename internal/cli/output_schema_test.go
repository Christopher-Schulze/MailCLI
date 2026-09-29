package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"mailcli/internal/compose"
	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

func fixtureOutputContracts(t *testing.T) (capabilityManifest, map[string]outputNode) {
	t.Helper()
	_, _, envelope := captureCapabilitiesJSON(t, "--outputs", "--json")
	manifest := envelope.Data.Capabilities
	if manifest == nil || manifest.OutputDefinitions == nil {
		t.Fatal("missing published definitions")
	}
	outputs := map[string]outputNode{}
	for _, command := range manifest.Commands {
		var schema struct {
			Output outputNode `json:"output"`
		}
		if err := json.Unmarshal(command.Schema, &schema); err != nil {
			t.Fatal(err)
		}
		if schema.Output.Ref == "" {
			t.Fatalf("missing schema.output: %s", command.ID)
		}
		outputs[command.ID] = schema.Output
	}
	return *manifest, outputs
}

func validateFixtureNode(raw json.RawMessage, node outputNode, definitions map[string]outputNode, path string) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if node.Nullable || node.Type == "null" {
			return nil
		}
		return fmt.Errorf("%s: unexpected null", path)
	}
	if node.Ref != "" {
		resolved, exists := definitions[strings.TrimPrefix(node.Ref, "#/$defs/")]
		if !exists {
			return fmt.Errorf("%s: unresolved %s", path, node.Ref)
		}
		return validateFixtureNode(raw, resolved, definitions, path)
	}
	switch node.Type {
	case "object":
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if object == nil {
			return fmt.Errorf("%s: not an object", path)
		}
		for _, field := range node.Fields {
			value, exists := object[field.Name]
			if !exists {
				if field.AlwaysPresent {
					return fmt.Errorf("%s: missing %s", path, field.Name)
				}
				continue
			}
			if err := validateFixtureNode(value, field, definitions, path+"."+field.Name); err != nil {
				return err
			}
			delete(object, field.Name)
		}
		for name, value := range object {
			if node.AdditionalValues == nil {
				return fmt.Errorf("%s: undeclared %s", path, name)
			}
			if err := validateFixtureNode(value, *node.AdditionalValues, definitions, path+"."+name); err != nil {
				return err
			}
		}
	case "array":
		var array []json.RawMessage
		if err := json.Unmarshal(raw, &array); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if node.Items == nil {
			return fmt.Errorf("%s: missing array item contract", path)
		}
		for index, value := range array {
			if err := validateFixtureNode(value, *node.Items, definitions, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	case "string":
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if len(node.Enum) > 0 && !slices.Contains(node.Enum, value) {
			return fmt.Errorf("%s: unknown enum %q", path, value)
		}
	case "boolean":
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	case "integer", "number":
		var value float64
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if node.Type == "integer" && math.Trunc(value) != value {
			return fmt.Errorf("%s: not an integer", path)
		}
	default:
		return fmt.Errorf("%s: invalid contract type %q", path, node.Type)
	}
	return nil
}

func validateCommandFixture(t *testing.T, payload []byte, manifest capabilityManifest, outputs map[string]outputNode) {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"schema_version", "ok", "command", "data", "error"} {
		if _, exists := envelope[name]; !exists {
			t.Fatalf("missing envelope field %s", name)
		}
	}
	for name := range envelope {
		if !slices.Contains([]string{"schema_version", "ok", "command", "data", "error", "next"}, name) {
			t.Fatalf("undeclared envelope field %s", name)
		}
	}
	var command string
	if err := json.Unmarshal(envelope["command"], &command); err != nil {
		t.Fatal(err)
	}
	node, exists := outputs[command]
	if !exists {
		t.Fatalf("unknown output command %s", command)
	}
	shared := manifest.OutputDefinitions["envelope"]
	shared.Fields = slices.Clone(shared.Fields)
	for index := range shared.Fields {
		if shared.Fields[index].Name == "data" {
			shared.Fields[index] = node
		}
		if shared.Fields[index].Name == "error" {
			shared.Fields[index].Ref = "#/$defs/error"
		}
	}
	if err := validateFixtureNode(payload, shared, manifest.OutputDefinitions, "envelope"); err != nil {
		t.Fatalf("%s envelope: %v", command, err)
	}
	if string(envelope["ok"]) == "true" {
		var data map[string]json.RawMessage
		if err := json.Unmarshal(envelope["data"], &data); err != nil {
			t.Fatal(err)
		}
		for _, variant := range node.Variants {
			if variant.When != "ok=true" || len(variant.OneOfRequired) == 0 {
				continue
			}
			matched := false
			for _, required := range variant.OneOfRequired {
				complete := true
				for _, name := range required {
					if _, exists := data[name]; !exists {
						complete = false
					}
				}
				matched = matched || complete
			}
			if !matched {
				t.Fatalf("%s missing success payload; required alternatives=%v data=%s", command, variant.OneOfRequired, envelope["data"])
			}
		}
	}
	if err := validateFixtureNode(envelope["data"], node, manifest.OutputDefinitions, "data"); err != nil {
		t.Fatalf("%s: %v\n%s", command, err, payload)
	}
	if !bytes.Equal(envelope["error"], []byte("null")) {
		if err := validateFixtureNode(envelope["error"], manifest.OutputDefinitions["error"], manifest.OutputDefinitions, "error"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPublishedOutputContractsRealReadFixtures(t *testing.T) {
	manifest, outputs := fixtureOutputContracts(t)
	service := mail.NewService(goldenWorkflowStore(t))
	list := captureOutputFixture(t, service, "messages", "list", "--limit", "1")
	var page struct {
		Data struct{ Page mail.MessagePage }
	}
	if err := json.Unmarshal(list, &page); err != nil {
		t.Fatal(err)
	}
	ref := page.Data.Page.Messages[0].Ref
	accounts := captureOutputFixture(t, service, "accounts", "list")
	var catalog struct {
		Data struct{ Accounts []mail.Account }
	}
	if err := json.Unmarshal(accounts, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Data.Accounts) == 0 {
		t.Fatal("fixture has no account")
	}
	for _, args := range [][]string{{"messages", "search"}, {"messages", "list"}, {"messages", "filter"}, {"messages", "get", "--ref", ref}, {"messages", "raw", "--ref", ref}, {"attachments", "list", "--ref", ref}, {"accounts", "list"}, {"mailboxes", "list"}, {"mailboxes", "resolve", "--account", catalog.Data.Accounts[0].Ref, "--path", "INBOX"}, {"doctor"}, {"version"}, {"capabilities"}} {
		t.Run(strings.Join(args[:min(2, len(args))], "."), func(t *testing.T) {
			validateCommandFixture(t, captureOutputFixture(t, service, args...), manifest, outputs)
		})
	}
	for _, view := range []string{"metadata", "plain", "full"} {
		validateCommandFixture(t, captureOutputFixture(t, service, "messages", "get", "--ref", ref, "--view", view), manifest, outputs)
	}
	for _, field := range projectionFieldNames(projectionTargetMessage) {
		validateCommandFixture(t, captureOutputFixture(t, service, "messages", "get", "--ref", ref, "--fields", field), manifest, outputs)
	}
	for mask := 0; mask < 4; mask++ {
		args := []string{"messages", "search"}
		if mask&1 != 0 {
			args = append(args, "--with-threading")
		}
		if mask&2 != 0 {
			args = append(args, "--with-excerpt")
		}
		validateCommandFixture(t, captureOutputFixture(t, service, args...), manifest, outputs)
	}
}

func TestOutputValidatorRejectsUndeclaredAndMissingFields(t *testing.T) {
	manifest, _ := fixtureOutputContracts(t)
	node := outputNode{Name: "summary", Type: "object", Ref: "#/$defs/" + outputDefinitionName(reflect.TypeFor[mail.MessageSummary]())}
	payload, err := json.Marshal(mail.MessageSummary{})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateFixtureNode(payload, node, manifest.OutputDefinitions, "summary"); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"extra", "remove", "wrong-type"} {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(payload, &object); err != nil {
			t.Fatal(err)
		}
		switch change {
		case "extra":
			object["undeclared"] = json.RawMessage(`true`)
		case "remove":
			delete(object, "ref")
		case "wrong-type":
			object["references"] = json.RawMessage(`"not a list"`)
		}
		broken, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateFixtureNode(broken, node, manifest.OutputDefinitions, "summary"); err == nil {
			t.Fatalf("validator accepted %s", change)
		}
	}
}

func TestOutputErrorContractsPerEffectClass(t *testing.T) {
	manifest, outputs := fixtureOutputContracts(t)
	seen := map[string]bool{}
	for _, command := range manifest.Commands {
		if seen[command.EffectClass] {
			continue
		}
		seen[command.EffectClass] = true
		var stdout bytes.Buffer
		if code := failCommand(command.ID, true, &commandError{code: "invalid_argument", message: "fixture invalid input"}, &stdout, &bytes.Buffer{}); code != 2 {
			t.Fatalf("error exit=%d", code)
		}
		validateCommandFixture(t, stdout.Bytes(), manifest, outputs)
	}
	if len(seen) < 5 {
		t.Fatalf("missing effect classes: %v", seen)
	}
}

// Unavailable source is exercised through real CLI/service validation, never
// a user mailbox. The enrichment call must not mask caller cancellation.
func TestMessageEnrichmentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service := mail.NewService(goldenWorkflowStore(t))
	_, err := service.EnrichMessage(ctx, mail.MessageSummary{Ref: "invalid"}, mail.MessageEnrichmentRequest{Threading: true, Excerpt: true, ExcerptLength: 240})
	if err == nil {
		t.Fatal("cancellation was swallowed")
	}
}

func TestPublishedOutputContractsRemainingFixtures(t *testing.T) {
	manifest, outputs := fixtureOutputContracts(t)
	seen := map[string]bool{}
	validate := func(payload []byte) {
		t.Helper()
		validateCommandFixture(t, payload, manifest, outputs)
		var result envelope
		if err := json.Unmarshal(payload, &result); err != nil {
			t.Fatal(err)
		}
		if !result.OK {
			t.Fatalf("not a success fixture: %s", payload)
		}
		seen[result.Command] = true
	}
	service := mail.NewServiceWithTransport(goldenWorkflowStore(t), t.TempDir(), mail.SendTransport{Submitter: &cliSubmitter{}, Mirror: &cliMirror{}, Credentials: cliCredentials{}})
	create := captureOutputFixture(t, service, "drafts", "create", "--from", "sender@icloud.com", "--to", "recipient@example.com", "--subject", "Fixture", "--body", "Reviewed body")
	validate(create)
	var created envelope
	if err := json.Unmarshal(create, &created); err != nil {
		t.Fatal(err)
	}
	draft := created.Data.Draft
	if draft == nil {
		t.Fatal("created draft missing")
	}
	for _, args := range [][]string{{"drafts", "list"}, {"drafts", "inspect", "--ref", draft.Ref}, {"drafts", "preview", "--ref", draft.Ref}, {"drafts", "update", "--ref", draft.Ref, "--expected-revision", draft.Revision, "--subject", "Updated fixture"}, {"drafts", "prune", "--older-than", "30"}} {
		validate(captureOutputFixture(t, service, args...))
	}
	currentDraft, err := service.GetDraft(draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAILCLI_TEST_EDITOR", "1")
	edited, err := editDraftInput(context.Background(), service, currentDraft.Ref, currentDraft.Revision, draftInputFromStored(currentDraft), os.Args[0], []string{"-test.run=TestDraftEditorHelperProcess", "--"}, draftEditorStreams{})
	if err != nil || edited.Subject != "After" {
		t.Fatalf("real editor update: %+v %v", edited, err)
	}
	var editorOutput bytes.Buffer
	if code := writeDraftResponse(&editorOutput, "drafts.edit", edited, true, outputOptions{target: projectionTargetDraft, view: defaultDraftOutputView, maxBytes: defaultJSONOutputBytes}); code != 0 {
		t.Fatalf("editor output exit=%d", code)
	}
	validate(editorOutput.Bytes())
	validate(captureOutputFixture(t, service, "drafts", "send", "--ref", edited.Ref, "--confirm", "--expected-revision", edited.Revision))
	validate(captureOutputFixture(t, service, "drafts", "reconcile", "--ref", edited.Ref))
	list := captureOutputFixture(t, service, "messages", "list", "--limit", "1")
	var page struct {
		Data struct{ Page mail.MessagePage }
	}
	if err := json.Unmarshal(list, &page); err != nil {
		t.Fatal(err)
	}
	ref := page.Data.Page.Messages[0].Ref
	for _, kind := range []string{"reply", "forward"} {
		args := []string{"messages", kind, "--ref", ref, "--from", "sender@icloud.com", "--body", "Reviewed reply"}
		if kind == "forward" {
			args = append(args, "--to", "recipient@example.com")
		}
		validate(captureOutputFixture(t, service, args...))
	}
	adoption := mail.NewServiceWithDraftRoot(adoptableGateway{}, t.TempDir())
	validate(captureOutputFixture(t, adoption, "drafts", "adopt", "--ref", "msg_store_draft"))
	validate(captureOutputFixture(t, adoption, "drafts", "open", "--ref", "msg_store_draft"))
	boundaryService := mail.NewServiceWithDraftRoot(testGateway{}, t.TempDir())
	for _, args := range [][]string{{"messages", "state", "--ref", "msg_ref"}, {"messages", "thread", "--ref", "msg_ref"}, {"messages", "new"}, {"messages", "mark", "--ref", "msg_ref", "--read=true"}, {"messages", "copy", "--ref", "msg_ref", "--mailbox", "mbx_ref"}, {"messages", "move", "--ref", "msg_ref", "--mailbox", "mbx_ref"}, {"messages", "delete", "--ref", "msg_ref", "--confirm"}, {"sync"}, {"attachments", "save", "--ref", "msg_ref", "--attachment", "a1", "--output", filepath.Join(t.TempDir(), "attachment.txt")}} {
		validate(captureOutputFixture(t, boundaryService, args...))
	}
	batchPath := filepath.Join(t.TempDir(), "batch.json")
	if err := os.WriteFile(batchPath, []byte(`{"operation":"read","items":[{"id":"one","ref":"msg_ref"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	validate(captureOutputFixture(t, boundaryService, "batch", "--input", batchPath))
	localDraft, err := boundaryService.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{To: []mail.Recipient{{Address: "recipient@example.com"}}, Subject: "Native fixture", Body: "Reviewed body"}})
	if err != nil {
		t.Fatal(err)
	}
	var handoffOutput, handoffError bytes.Buffer
	code := runDraftHandoffWith(context.Background(), boundaryService, []string{"--ref", localDraft.Ref, "--json"}, &handoffOutput, &handoffError, func(_ context.Context, request compose.Request) (compose.Result, error) {
		if request.Subject != "Native fixture" || request.PlainBody != "Reviewed body" {
			t.Fatalf("invalid boundary request: %+v", request)
		}
		return compose.Result{Opened: true, MailApplication: "com.apple.mail"}, nil
	})
	if code != 0 || handoffError.Len() != 0 {
		t.Fatalf("handoff code=%d output=%s error=%s", code, &handoffOutput, &handoffError)
	}
	validate(handoffOutput.Bytes())
	// A real retained unknown handoff is resolved through its durable attempt.
	handoffOutput.Reset()
	code = runDraftHandoffWithDispatch(context.Background(), boundaryService, []string{"--ref", localDraft.Ref, "--json"}, &handoffOutput, &handoffError, func(_ context.Context, _ compose.Request, observer compose.DispatchObserver) (compose.Result, error) {
		if err := observer(); err != nil {
			return compose.Result{}, err
		}
		return compose.Result{State: compose.StateOutcomeUnknown}, fmt.Errorf("fixture handoff outcome unavailable")
	})
	if code != 1 {
		t.Fatalf("unknown handoff exit=%d", code)
	}
	retained, err := boundaryService.GetDraft(localDraft.Ref)
	if err != nil || retained.HandoffAttempt == nil {
		t.Fatalf("missing retained attempt: %+v %v", retained, err)
	}
	validate(captureOutputFixture(t, boundaryService, "drafts", "handoff-reconcile", "--ref", localDraft.Ref, "--attempt", retained.HandoffAttempt.ID, "--outcome", "opened", "--confirm"))
	validate(captureOutputFixture(t, boundaryService, "drafts", "discard", "--ref", localDraft.Ref, "--confirm"))
	credentials := newStubSetupCredentials()
	previousCredentials, previousInput := sendSetupCredentials, sendSetupStdin
	sendSetupCredentials = func() transport.CredentialStore { return credentials }
	sendSetupStdin = strings.NewReader("fixture-password\n")
	t.Cleanup(func() { sendSetupCredentials = previousCredentials; sendSetupStdin = previousInput })
	var setupOutput, setupError bytes.Buffer
	code = runSendSetup(context.Background(), []string{"--from", "sender@icloud.com", "--json"}, &setupOutput, &setupError, nil, mail.NewAccountBindingStore(filepath.Join(t.TempDir(), "bindings.json")))
	if code != 0 || setupError.Len() != 0 {
		t.Fatalf("setup code=%d output=%s error=%s", code, &setupOutput, &setupError)
	}
	validate(setupOutput.Bytes())
	server := newUpdateTestServer(t, version, nil, nil)
	defer server.Close()
	var updateOutput, updateError bytes.Buffer
	code = runUpdateWithEnvironment(context.Background(), true, updateTestEnvironment(t, server, version), &updateOutput, &updateError)
	if code != 0 || updateError.Len() != 0 {
		t.Fatalf("update code=%d output=%s error=%s", code, &updateOutput, &updateError)
	}
	validate(updateOutput.Bytes())
	readIDs := []string{"messages.search", "messages.list", "messages.filter", "messages.get", "messages.raw", "attachments.list", "accounts.list", "mailboxes.list", "mailboxes.resolve", "doctor", "version", "capabilities"}
	for id := range outputs {
		if !seen[id] && !slices.Contains(readIDs, id) {
			t.Fatalf("published command lacks a success fixture: %s", id)
		}
	}
}
