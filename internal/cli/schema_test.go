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
	if response.Data.Capabilities == nil || response.Data.Capabilities.Scope != "messages" || len(response.Data.Capabilities.Commands) != 11 {
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
	if items == nil || !items.Required || !schemaHasConstraint(batch, "non_empty") ||
		concurrency == nil || concurrency.Minimum == nil || *concurrency.Minimum != 0 ||
		concurrency.Maximum == nil || *concurrency.Maximum != int64(mail.MaximumBatchConcurrency) {
		t.Fatalf("batch items field = %+v, constraints = %+v", items, batch.Constraints)
	}
	if countJSONConstraints(batch.JSONInput, "conditional") != 3 {
		t.Fatalf("batch operation constraints = %+v", batch.JSONInput.Constraints)
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
