package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

func outputReferenceClosure(t *testing.T, roots []outputNode, definitions map[string]outputNode) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	var visit func(outputNode)
	visit = func(node outputNode) {
		if node.Ref != "" {
			key, valid := strings.CutPrefix(node.Ref, "#/$defs/")
			definition, exists := definitions[key]
			if !valid || !exists {
				t.Fatalf("unresolved output reference %s", node.Ref)
			}
			if !seen[key] {
				seen[key] = true
				visit(definition)
			}
		}
		for _, field := range node.Fields {
			visit(field)
		}
		if node.Items != nil {
			visit(*node.Items)
		}
		if node.AdditionalValues != nil {
			visit(*node.AdditionalValues)
		}
	}
	for _, root := range roots {
		visit(root)
	}
	return seen
}

func TestCapabilityOutputSchemaViewPreservesReachableContracts(t *testing.T) {
	selectors := []string{"messages.*", "drafts.*", "messages.list,messages.search", "sync", "batch", "capabilities"}
	for _, command := range mustCapabilities(t).Commands {
		selectors = append(selectors, command.ID)
	}
	for _, selector := range selectors {
		t.Run(selector, func(t *testing.T) {
			fullCode, fullBytes, fullResponse := captureCapabilitiesJSON(t, "--for", selector, "--outputs", "--json")
			code, narrowBytes, response := captureCapabilitiesJSON(t, "--for", selector, "--output-schema", "--json")
			full, narrow := fullResponse.Data.Capabilities, response.Data.Capabilities
			if fullCode != 0 || code != 0 || !response.OK || full == nil || narrow == nil ||
				full.ContractSHA256 == "" || full.ContractSHA256 != narrow.ContractSHA256 ||
				!reflect.DeepEqual(full.Commands, narrow.Commands) || full.Scope != narrow.Scope ||
				len(full.ErrorCodes) == 0 || len(narrow.ErrorCodes) != 0 {
				t.Fatalf("full/narrow contract mismatch: exits=%d/%d", fullCode, code)
			}
			for _, omitted := range []string{"envelope", "error"} {
				if _, exists := full.OutputDefinitions[omitted]; !exists {
					t.Fatalf("full view lost %s", omitted)
				}
				if _, exists := narrow.OutputDefinitions[omitted]; exists {
					t.Fatalf("narrow view includes unconditional %s", omitted)
				}
			}
			var roots []outputNode
			for _, command := range narrow.Commands {
				var schema outputCommandParameters
				if err := json.Unmarshal(command.Schema, &schema); err != nil || schema.Output == nil || schema.ID == "" || len(schema.Flags) == 0 || command.SchemaRef != nil {
					t.Fatalf("missing inline parameter/output schema: %s %v", command.ID, err)
				}
				roots = append(roots, *schema.Output)
			}
			closure := outputReferenceClosure(t, roots, narrow.OutputDefinitions)
			if len(closure) != len(narrow.OutputDefinitions) {
				t.Fatalf("unreachable definitions: reachable=%d published=%d", len(closure), len(narrow.OutputDefinitions))
			}
			for key, definition := range narrow.OutputDefinitions {
				if !reflect.DeepEqual(definition, full.OutputDefinitions[key]) {
					t.Fatalf("data definition changed: %s", key)
				}
			}
			if _, exists := narrow.OutputDefinitions["finalization_data"]; !exists {
				t.Fatal("data finalization evidence was removed")
			}
			if _, exists := narrow.OutputDefinitions["error_data"]; !exists {
				t.Fatal("referenced error evidence was removed")
			}
			if selector == "messages.list" && len(narrowBytes)*4 > len(fullBytes)*3 {
				t.Fatalf("narrow list view saves less than 25%%: full=%d narrow=%d", len(fullBytes), len(narrowBytes))
			}
			t.Logf("bytes: full=%d narrow=%d", len(fullBytes), len(narrowBytes))
		})
	}
}

func TestCapabilityOutputSchemaViewValidationAndRedundantSchemas(t *testing.T) {
	for _, args := range [][]string{
		{"--output-schema"}, {"--output-schema", "--schemas"},
		{"--for", "messages.list", "--output-schema", "--outputs"},
		{"--for", "messages.list", "--output-schema", "--limits"},
		{"--for", "messages.list", "--output-schema", "--errors", "operation_failed"},
		{"--for", "messages.list", "--output-schema", "--errors="},
		{"--for", "unknown", "--output-schema"},
		{"--for", "messages.list,messages.list", "--output-schema"},
		{"--for", "messages.*", "--for", "drafts.*", "--output-schema"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, _, response := captureCapabilitiesJSON(t, append(args, "--json")...)
			if code != 2 || response.OK || response.Error == nil || response.Error.Code != "invalid_argument" {
				t.Fatalf("accepted invalid view: code=%d response=%+v", code, response)
			}
		})
	}
	code, plain, response := captureCapabilitiesJSON(t, "--for", "messages.list", "--output-schema", "--json")
	redundantCode, redundant, _ := captureCapabilitiesJSON(t, "--for", "messages.list", "--output-schema", "--schemas", "--json")
	_, _, errors := captureCapabilitiesJSON(t, "--for", "messages.list", "--errors", "operation_timeout", "--json")
	if code != 0 || redundantCode != 0 || !bytes.Equal(plain, redundant) || errors.Data.Capabilities == nil ||
		errors.Data.Capabilities.ContractSHA256 != response.Data.Capabilities.ContractSHA256 ||
		len(errors.Data.Capabilities.ErrorCodes) != 1 || errors.Data.Capabilities.OutputDefinitions != nil {
		t.Fatal("redundant schema flag or error view changed the contract")
	}
	var stdout, stderr bytes.Buffer
	if Run(context.Background(), nil, []string{"capabilities", "--help"}, &stdout, &stderr) != 0 ||
		!strings.Contains(stdout.String(), "--output-schema") || stderr.Len() != 0 ||
		RequiresMailService([]string{"capabilities", "--for", "messages.list", "--output-schema"}) {
		t.Fatalf("missing standalone help/discovery support: %s %s", &stdout, &stderr)
	}
}

func TestCapabilityOutputSchemaValidatesActualDataIncludingFinalization(t *testing.T) {
	service := mail.NewService(goldenWorkflowStore(t))
	listed, _ := invokeGoldenWorkflow(t, service, 0, "messages", "list")
	ref := goldenMessagePage(t, listed).Messages[0].Ref
	for _, args := range [][]string{
		{"messages", "list", "--with-excerpt", "--with-threading", "--fields", "sender,read"},
		{"messages", "get", ref, "--view", "metadata"},
		{"messages", "get", ref, "--view", "plain"},
		{"messages", "get", ref, "--view", "full"},
		{"capabilities", "--for", "capabilities", "--output-schema"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			payload := captureOutputFixture(t, service, args...)
			var response struct {
				Command string          `json:"command"`
				Data    json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(payload, &response); err != nil {
				t.Fatal(err)
			}
			_, _, contract := captureCapabilitiesJSON(t, "--for", response.Command, "--output-schema", "--json")
			manifest := contract.Data.Capabilities
			var schema outputCommandParameters
			if manifest == nil || json.Unmarshal(manifest.Commands[0].Schema, &schema) != nil || schema.Output == nil {
				t.Fatal("missing output schema")
			}
			if err := validateFixtureNode(response.Data, *schema.Output, manifest.OutputDefinitions, "data"); err != nil {
				t.Fatal(err)
			}
			var finalized bytes.Buffer
			if FinalizeJSON(&finalized, append(args, "--json"), payload, 0, fmt.Errorf("fixture close failed")) != 1 ||
				json.Unmarshal(finalized.Bytes(), &response) != nil {
				t.Fatalf("missing finalized data: %s", &finalized)
			}
			if err := validateFixtureNode(response.Data, *schema.Output, manifest.OutputDefinitions, "data"); err != nil {
				t.Fatalf("finalization data did not resolve: %v", err)
			}
		})
	}
}
