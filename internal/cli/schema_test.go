package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"

	"mailcli/internal/mail"
)

func TestPublishedCommandSchemasAreComplete(t *testing.T) {
	if schemaSpecCount != len(commandContracts) {
		t.Fatalf("schema spec count = %d, command contract count = %d", schemaSpecCount, len(commandContracts))
	}
	for _, contract := range commandContracts {
		if !commandIsPublished(contract) {
			continue
		}
		schema := decodeTestCommandSchema(t, schemaForCommand(contract.ID))
		if schema.ID != contract.ID+"@v1" || schema.Version != capabilitySchemaVersion {
			t.Fatalf("%s schema identity = %q/%d", contract.ID, schema.ID, schema.Version)
		}
		if len(schema.Flags) == 0 {
			t.Fatalf("%s has no flags", contract.ID)
		}
		seen := make(map[string]struct{}, len(schema.Flags))
		for _, flag := range schema.Flags {
			if flag.Name == "" || flag.ValueType == "" || flag.Description == "" {
				t.Fatalf("%s has incomplete flag schema %+v", contract.ID, flag)
			}
			if _, exists := seen[flag.Name]; exists {
				t.Fatalf("%s repeats flag %q", contract.ID, flag.Name)
			}
			seen[flag.Name] = struct{}{}
			if flag.TakesValue != flag.ValueRequired {
				t.Fatalf("%s flag %q value metadata = takes=%t required=%t", contract.ID, flag.Name, flag.TakesValue, flag.ValueRequired)
			}
			if flag.Minimum != nil && flag.Maximum != nil && *flag.Minimum > *flag.Maximum {
				t.Fatalf("%s flag %q has inverted bounds", contract.ID, flag.Name)
			}
		}
		for _, constraint := range schema.Constraints {
			if constraint.Kind == "" || constraint.Description == "" {
				t.Fatalf("%s has incomplete constraint schema %+v", contract.ID, constraint)
			}
		}
	}
}

func TestCapabilitiesScopeKeepsCommandContractMetadata(t *testing.T) {
	full := capabilities()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := Run(context.Background(), nil, []string{"capabilities", "--scope", "messages.search", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("scoped capabilities exit = %d, stderr = %q", code, stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode scoped capabilities: %v", err)
	}
	if response.Data.Capabilities == nil {
		t.Fatalf("scoped response = %+v", response)
	}
	manifest := response.Data.Capabilities
	if manifest.Scope != "messages.search" || len(manifest.Commands) != 1 {
		t.Fatalf("scope = %+v, commands = %d", manifest.Scope, len(manifest.Commands))
	}
	if !reflect.DeepEqual(manifest.Commands[0], full.Commands[10]) {
		t.Fatalf("scoped command = %+v, full command = %+v", manifest.Commands[0], full.Commands[10])
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), nil, []string{"capabilities", "--family", "messages", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("family capabilities exit = %d, stderr = %q", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode family capabilities: %v", err)
	}
	if response.Data.Capabilities == nil || response.Data.Capabilities.Scope != "messages" || len(response.Data.Capabilities.Commands) != 13 {
		t.Fatalf("family response = %+v", response.Data.Capabilities)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), nil, []string{"capabilities", "--command", "messages.search", "--family", "messages", "--json"}, &stdout, &stderr); code != 2 || stderr.Len() != 0 {
		t.Fatalf("invalid selector exit = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"code":"invalid_argument"`)) {
		t.Fatalf("invalid selector response = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), nil, []string{"capabilities", "--scope", "missing.command", "--json"}, &stdout, &stderr); code != 2 || stderr.Len() != 0 {
		t.Fatalf("unknown selector exit = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"code":"invalid_argument"`)) {
		t.Fatalf("unknown selector response = %s", stdout.String())
	}
}

func TestCapabilitiesSelectedCommandsPreserveFullContract(t *testing.T) {
	full := capabilities()
	code, output, response := captureCapabilitiesJSON(t,
		"--commands", "messages.get,messages.search", "--json",
	)
	if code != 0 || !response.OK || response.Data.Capabilities == nil {
		t.Fatalf("selected capabilities exit = %d, response = %+v, output = %s", code, response, output)
	}
	selected := response.Data.Capabilities
	if selected.Scope != "" || len(selected.Commands) != 2 ||
		selected.Commands[0].ID != "messages.search" || selected.Commands[1].ID != "messages.get" {
		t.Fatalf("selected command order/scope = %q/%+v", selected.Scope, selected.Commands)
	}
	fullCommands := make(map[string]commandCapability, len(full.Commands))
	for _, command := range full.Commands {
		fullCommands[command.ID] = command
	}
	for _, command := range selected.Commands {
		if !reflect.DeepEqual(command, fullCommands[command.ID]) {
			t.Fatalf("selected command %s differs from full contract: %+v vs %+v", command.ID, command, fullCommands[command.ID])
		}
	}
	if !reflect.DeepEqual(selected.Limits, full.Limits) ||
		!reflect.DeepEqual(selected.SyncCheckPolicy, full.SyncCheckPolicy) ||
		!reflect.DeepEqual(selected.DraftSavePolicy, full.DraftSavePolicy) {
		t.Fatal("selected manifest dropped or changed shared limits or policies")
	}
	if bytes.Count(output, []byte(`"limits":`)) != 1 {
		t.Fatalf("selected response should serialize limits exactly once; bytes=%d", len(output))
	}
}

func TestCapabilitiesSelectedCommandSelectorsFailClosed(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		legacy   []string
	}{
		{name: "empty"},
		{name: "empty entry", selector: "messages.search,,messages.get"},
		{name: "leading empty entry", selector: ",messages.search"},
		{name: "trailing empty entry", selector: "messages.search,"},
		{name: "duplicate", selector: "messages.search,messages.search"},
		{name: "unknown", selector: "messages.search,missing.command"},
		{name: "mixed singular command", selector: "messages.search", legacy: []string{"--command", "messages.get"}},
		{name: "mixed family", selector: "messages.search", legacy: []string{"--family", "messages"}},
		{name: "mixed scope", selector: "messages.search", legacy: []string{"--scope", "messages.search"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"--commands", test.selector}
			args = append(args, test.legacy...)
			args = append(args, "--json")
			code, output, response := captureCapabilitiesJSON(t, args...)
			if code != 2 || response.OK || response.Error == nil || response.Error.Code != "invalid_argument" {
				t.Fatalf("invalid selector exit = %d, response = %+v, output = %s", code, response, output)
			}
		})
	}
}

func TestCapabilitiesSelectedWorkflowByteSavings(t *testing.T) {
	workflows := []struct {
		name     string
		selected string
		singles  []string
	}{
		{
			name: "search and get", selected: "messages.search,messages.get",
			singles: []string{"messages.search", "messages.get"},
		},
		{
			name: "draft create inspect send", selected: "drafts.create,drafts.inspect,drafts.send",
			singles: []string{"drafts.create", "drafts.inspect", "drafts.send"},
		},
	}
	for _, workflow := range workflows {
		t.Run(workflow.name, func(t *testing.T) {
			code, selected, response := captureCapabilitiesJSON(t, "--commands", workflow.selected, "--json")
			if code != 0 || !response.OK || response.Data.Capabilities == nil {
				t.Fatalf("selected manifest exit = %d, response = %+v", code, response)
			}
			selectedBytes := len(selected)
			separateBytes := 0
			for _, id := range workflow.singles {
				code, single, singleResponse := captureCapabilitiesJSON(t, "--command", id, "--json")
				if code != 0 || !singleResponse.OK || singleResponse.Data.Capabilities == nil {
					t.Fatalf("singular manifest for %s exit = %d, response = %+v", id, code, singleResponse)
				}
				separateBytes += len(single)
			}
			if selectedBytes*4 > separateBytes*3 {
				t.Fatalf("selected workflow is not 25%% smaller: selected=%d separate=%d", selectedBytes, separateBytes)
			}
			t.Logf("serialized bytes: selected=%d separate=%d reduction=%d%%",
				selectedBytes, separateBytes, (separateBytes-selectedBytes)*100/separateBytes)
		})
	}
}

func captureCapabilitiesJSON(t *testing.T, flags ...string) (int, []byte, envelope) {
	t.Helper()
	args := append([]string{"capabilities"}, flags...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(context.Background(), nil, args, &stdout, &stderr)
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode capabilities response: %v; stderr=%q; output=%q", err, stderr.String(), stdout.String())
	}
	return code, stdout.Bytes(), response
}

func TestSearchSchemaBoundsAreReachableThroughCLI(t *testing.T) {
	gateway := &searchQueryCaptureGateway{}
	maxMessages := 1234
	maxBytes := int64(987654)
	args := []string{
		"messages", "search", "--query", "needle",
		"--max-messages", strconv.Itoa(maxMessages), "--max-bytes", strconv.FormatInt(maxBytes, 10), "--json",
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := Run(context.Background(), mail.NewService(gateway), args, &stdout, &stderr); code != 0 {
		t.Fatalf("search exit = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if gateway.query.Query.MaxMessages != maxMessages || gateway.query.Query.MaxBytes != maxBytes {
		t.Fatalf("captured search bounds = %d/%d", gateway.query.Query.MaxMessages, gateway.query.Query.MaxBytes)
	}
	searchSchema := decodeTestCommandSchema(t, schemaForCommand("messages.search"))
	flags := schemaFlagsByName(searchSchema)
	if flags["--max-messages"].Default != strconv.Itoa(mail.DefaultSearchMaxMessages) ||
		*flags["--max-messages"].Minimum != 1 || *flags["--max-messages"].Maximum != int64(mail.MaximumSearchMaxMessages) ||
		flags["--max-bytes"].Default != strconv.FormatInt(mail.DefaultSearchMaxBytes, 10) ||
		*flags["--max-bytes"].Minimum != 1 || *flags["--max-bytes"].Maximum != mail.MaximumSearchMaxBytes {
		t.Fatalf("search schema bounds = %+v/%+v", flags["--max-messages"], flags["--max-bytes"])
	}
	if !schemaHasConstraint(searchSchema, "ordered") {
		t.Fatal("search schema omits after/before ordering constraint")
	}
}

func TestDraftAndBatchJSONSchemasExposeBoundedInput(t *testing.T) {
	draft := decodeTestCommandSchema(t, schemaForCommand("drafts.create"))
	if draft.JSONInput == nil || draft.JSONInput.MaximumBytes != maximumDraftInputBytes || draft.JSONInput.AdditionalProperties {
		t.Fatalf("draft JSON input = %+v", draft.JSONInput)
	}
	if field := jsonFieldByName(draft.JSONInput.Fields, "body"); field == nil || !field.Required {
		t.Fatalf("draft body field = %+v", field)
	}
	if !schemaHasConstraint(draft, "mutually_exclusive") || !schemaHasConstraint(draft, "one_of") {
		t.Fatalf("draft constraints = %+v", draft.Constraints)
	}
	batch := decodeTestCommandSchema(t, schemaForCommand("batch"))
	if batch.JSONInput == nil || batch.JSONInput.MaximumBytes != mail.MaximumBatchInputBytes || batch.JSONInput.AdditionalProperties {
		t.Fatalf("batch JSON input = %+v", batch.JSONInput)
	}
	items := jsonFieldByName(batch.JSONInput.Fields, "items")
	concurrency := jsonFieldByName(batch.JSONInput.Fields, "concurrency")
	maxBytes := schemaFlagsByName(batch)["--max-bytes"]
	if items == nil || !items.Required || !schemaHasConstraint(batch, "non_empty") ||
		concurrency == nil || concurrency.Minimum == nil || *concurrency.Minimum != 0 ||
		concurrency.Maximum == nil || *concurrency.Maximum != int64(mail.MaximumBatchConcurrency) ||
		maxBytes.Name != "--max-bytes" || maxBytes.Default != "1048576" ||
		maxBytes.Minimum == nil || *maxBytes.Minimum != 1 ||
		maxBytes.Maximum == nil || *maxBytes.Maximum != maximumJSONOutputBytes {
		t.Fatalf("batch items field = %+v, constraints = %+v", items, batch.Constraints)
	}
	view := jsonFieldByName(batch.JSONInput.ItemFields, "view")
	fields := jsonFieldByName(batch.JSONInput.ItemFields, "fields")
	if view == nil || !reflect.DeepEqual(view.Values, []string{outputViewMetadata, outputViewPlain, outputViewFull}) ||
		fields == nil || !reflect.DeepEqual(fields.Values, projectionFieldNames(projectionTargetMessage)) {
		t.Fatalf("batch read projection fields = view:%+v fields:%+v", view, fields)
	}
	if countJSONConstraints(batch.JSONInput, "conditional") != 9 {
		t.Fatalf("batch operation constraints = %+v", batch.JSONInput.Constraints)
	}
	uniqueSources, uniqueCopyPairs := false, false
	for _, constraint := range batch.JSONInput.Constraints {
		if constraint.Kind != "conditional" {
			continue
		}
		uniqueSources = uniqueSources || reflect.DeepEqual(constraint.Fields, []string{"operation", "ref"}) &&
			constraint.Description == "mark, move, and delete items require unique source refs"
		uniqueCopyPairs = uniqueCopyPairs || reflect.DeepEqual(constraint.Fields, []string{"operation", "ref", "mailbox"}) &&
			constraint.Description == "copy items may repeat a source ref only with distinct destination mailbox refs; each source/destination pair is unique"
	}
	if !uniqueSources || !uniqueCopyPairs {
		t.Fatalf("batch reference uniqueness constraints = %+v", batch.JSONInput.Constraints)
	}
}

func TestMessageThreadSchemaExposesContinuationCursor(t *testing.T) {
	schema := decodeTestCommandSchema(t, schemaForCommand("messages.thread"))
	cursor, exists := schemaFlagsByName(schema)["--cursor"]
	if !exists || cursor.ValueType != "cursor" || !cursor.TakesValue || !cursor.ValueRequired {
		t.Fatalf("messages.thread --cursor schema = %+v, exists=%t", cursor, exists)
	}
}

func TestSchemaDescribesRepeatableDraftFlags(t *testing.T) {
	for _, command := range []string{"drafts.create", "drafts.update", "messages.reply", "messages.forward"} {
		flags := schemaFlagsByName(decodeTestCommandSchema(t, schemaForCommand(command)))
		for _, name := range []string{"--to", "--cc", "--bcc", "--attach"} {
			flag, exists := flags[name]
			if !exists || !flag.TakesValue || !flag.ValueRequired || !flag.Repeatable {
				t.Fatalf("%s flag %s metadata = %+v", command, name, flag)
			}
		}
	}
}

type testCommandSchema struct {
	ID                  string            `json:"id"`
	Version             int               `json:"version"`
	Flags               []testCommandFlag `json:"flags"`
	PositionalArguments []string          `json:"positional_arguments"`
	JSONInput           *testJSONInput    `json:"json_input,omitempty"`
	Constraints         []testConstraint  `json:"constraints"`
}

type testCommandFlag struct {
	Name          string   `json:"name"`
	ValueType     string   `json:"value_type"`
	TakesValue    bool     `json:"takes_value"`
	ValueRequired bool     `json:"value_required,omitempty"`
	Repeatable    bool     `json:"repeatable,omitempty"`
	Required      bool     `json:"required,omitempty"`
	Default       string   `json:"default,omitempty"`
	Minimum       *int64   `json:"minimum,omitempty"`
	Maximum       *int64   `json:"maximum,omitempty"`
	Values        []string `json:"values,omitempty"`
	Description   string   `json:"description"`
}

type testJSONInput struct {
	Flag                 string           `json:"flag"`
	ValueType            string           `json:"value_type"`
	MaximumBytes         int64            `json:"maximum_bytes"`
	AdditionalProperties bool             `json:"additional_properties"`
	Fields               []testJSONField  `json:"fields"`
	ItemFields           []testJSONField  `json:"item_fields,omitempty"`
	Constraints          []testConstraint `json:"constraints"`
}

type testJSONField struct {
	Name        string   `json:"name"`
	ValueType   string   `json:"value_type"`
	Required    bool     `json:"required,omitempty"`
	Repeatable  bool     `json:"repeatable,omitempty"`
	Minimum     *int64   `json:"minimum,omitempty"`
	Maximum     *int64   `json:"maximum,omitempty"`
	Values      []string `json:"values,omitempty"`
	Description string   `json:"description"`
}

type testConstraint struct {
	Kind        string   `json:"kind"`
	Flags       []string `json:"flags,omitempty"`
	Fields      []string `json:"fields,omitempty"`
	Description string   `json:"description"`
}

func decodeTestCommandSchema(t *testing.T, raw json.RawMessage) testCommandSchema {
	t.Helper()
	var schema testCommandSchema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode command schema: %v; raw=%s", err, raw)
	}
	return schema
}

func schemaFlagsByName(schema testCommandSchema) map[string]testCommandFlag {
	flags := make(map[string]testCommandFlag, len(schema.Flags))
	for _, flag := range schema.Flags {
		flags[flag.Name] = flag
	}
	return flags
}

func schemaHasConstraint(schema testCommandSchema, kind string) bool {
	for _, constraint := range schema.Constraints {
		if constraint.Kind == kind {
			return true
		}
	}
	return false
}

func countJSONConstraints(input *testJSONInput, kind string) int {
	count := 0
	for _, constraint := range input.Constraints {
		if constraint.Kind == kind {
			count++
		}
	}
	return count
}

func jsonFieldByName(fields []testJSONField, name string) *testJSONField {
	for index := range fields {
		if fields[index].Name == name {
			return &fields[index]
		}
	}
	return nil
}
