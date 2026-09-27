package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	goast "go/ast"
	goparser "go/parser"
	gotoken "go/token"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"mailcli/internal/mail"
)

func TestPublishedCommandSchemasAreComplete(t *testing.T) {
	for _, contract := range commandContracts {
		if !commandIsPublished(contract) {
			continue
		}
		schema := decodeTestCommandSchema(t, schemaForCommand(contract.ID))
		wantID, wantVersion := contract.ID+"@v1", 1
		if contract.ID == "batch" {
			wantID, wantVersion = "batch@v2", 2
		}
		if schema.ID != wantID || schema.Version != wantVersion {
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

func TestCanonicalCommandSchemasMatchRuntimePayload(t *testing.T) {
	for _, contract := range commandContracts {
		if !commandIsPublished(contract) {
			continue
		}
		path := "schemas/" + contract.ID + ".json"
		canonical, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, canonical); err != nil {
			t.Fatalf("compact %s: %v", path, err)
		}
		canonicalMetadata := projectionSchemaMetadata(t, contract.ID, compact.Bytes())
		runtimeMetadata := projectionSchemaMetadata(t, contract.ID, schemaForCommand(contract.ID))
		if !bytes.Equal(canonicalMetadata, runtimeMetadata) {
			t.Fatalf("%s runtime schema differs from canonical file", contract.ID)
		}
	}
}

func TestLoadCommandSchemasCompactsValidSource(t *testing.T) {
	valid := `{
  "id": "version@v1",
  "version": 1
}`
	got, err := loadCommandSchemas(fstest.MapFS{
		"schemas/version.json": &fstest.MapFile{Data: []byte(valid)},
	}, []string{"version"})
	if err != nil {
		t.Fatalf("load valid schema: %v", err)
	}
	if want := `{"id":"version@v1","version":1}`; string(got["version"]) != want {
		t.Fatalf("compacted schema = %s, want %s", got["version"], want)
	}
}

func TestLoadCommandSchemasRejectsInvalidSources(t *testing.T) {
	for _, test := range []struct {
		name  string
		files fstest.MapFS
		path  string
	}{
		{
			name:  "malformed JSON",
			files: fstest.MapFS{"schemas/version.json": &fstest.MapFile{Data: []byte("{")}},
			path:  "schemas/version.json",
		},
		{
			name:  "identity mismatch",
			files: fstest.MapFS{"schemas/version.json": &fstest.MapFile{Data: []byte(`{"id":"other@v1","version":1}`)}},
			path:  "schemas/version.json",
		},
		{
			name:  "missing inventory",
			files: fstest.MapFS{},
			path:  "schemas",
		},
		{
			name: "unpublished schema",
			files: fstest.MapFS{
				"schemas/version.json": &fstest.MapFile{Data: []byte(`{"id":"version@v1","version":1}`)},
				"schemas/extra.json":   &fstest.MapFile{Data: []byte(`{"id":"extra@v1","version":1}`)},
			},
			path: "schemas/extra.json",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertInvalidCommandSchemaError(t, test.files, test.path)
		})
	}
}

func assertInvalidCommandSchemaError(t *testing.T, files fstest.MapFS, wantPath string) {
	t.Helper()
	_, err := loadCommandSchemas(files, []string{"version"})
	if err == nil {
		t.Fatal("invalid schema inventory was accepted")
	}
	if got := errorCode(err); got != "capability_schema_invalid" || !strings.Contains(err.Error(), wantPath) {
		t.Fatalf("schema error = %v (code %q), want path %q and capability_schema_invalid", err, got, wantPath)
	}
	var stdout, stderr bytes.Buffer
	if code := failCommand("capabilities", true, err, &stdout, &stderr); code != 1 || stderr.Len() != 0 {
		t.Fatalf("capabilities failure exit = %d, stderr = %q", code, stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode capabilities error: %v; output = %s", err, stdout.String())
	}
	if response.OK || response.Error == nil || response.Error.Code != "capability_schema_invalid" {
		t.Fatalf("capabilities error response = %+v", response)
	}
}

// Strip only the registry-owned enum, keeping every other canonical property.
func projectionSchemaMetadata(t *testing.T, id string, source json.RawMessage) []byte {
	t.Helper()
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(source, &schema); err != nil {
		t.Fatal(err)
	}
	if _, projected := projectionTargetForCommand(id); projected {
		schema["flags"] = projectionFieldMetadata(t, schema["flags"], "--fields")
	}
	if id == "batch" {
		var input map[string]json.RawMessage
		if err := json.Unmarshal(schema["json_input"], &input); err != nil {
			t.Fatal(err)
		}
		input["item_fields"] = projectionFieldMetadata(t, input["item_fields"], "fields")
		if defaultsFields, exists := input["defaults_fields"]; exists {
			input["defaults_fields"] = projectionFieldMetadata(t, defaultsFields, "fields")
		}
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		schema["json_input"] = encoded
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func projectionFieldMetadata(t *testing.T, source json.RawMessage, name string) json.RawMessage {
	t.Helper()
	var fields []map[string]json.RawMessage
	if err := json.Unmarshal(source, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range fields {
		var fieldName string
		if err := json.Unmarshal(field["name"], &fieldName); err != nil {
			t.Fatal(err)
		}
		if fieldName == name {
			delete(field, "values")
		}
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestEveryProjectionSchemaEnumMatchesItsTargetRegistry(t *testing.T) {
	seen := make(map[projectionTarget]bool)
	for _, contract := range commandContracts {
		if !commandIsPublished(contract) {
			continue
		}
		schema := decodeTestCommandSchema(t, schemaForCommand(contract.ID))
		for _, field := range schema.Flags {
			if field.Name != "--fields" {
				continue
			}
			target, found := projectionTargetForCommand(contract.ID)
			if !found || field.ValueType != "field_list" || !reflect.DeepEqual(field.Values, projectionFieldNames(target)) {
				t.Fatalf("%s field enum = %+v, target = %q", contract.ID, field, target)
			}
			seen[target] = true
		}
	}
	if len(seen) != 7 {
		t.Fatalf("schemas cover %d projection targets, want seven", len(seen))
	}
	batch := decodeTestCommandSchema(t, schemaForCommand("batch"))
	fields := jsonFieldByName(batch.JSONInput.ItemFields, "fields")
	if fields == nil || !reflect.DeepEqual(fields.Values, projectionFieldNames(projectionTargetMessage)) {
		t.Fatalf("batch field enum = %+v", fields)
	}
	defaultsFields := jsonFieldByName(batch.JSONInput.DefaultsFields, "fields")
	if defaultsFields == nil || !reflect.DeepEqual(defaultsFields.Values, projectionFieldNames(projectionTargetMessage)) {
		t.Fatalf("batch default field enum = %+v", defaultsFields)
	}
	defaultsView := jsonFieldByName(batch.JSONInput.DefaultsFields, "view")
	if defaultsView == nil || !reflect.DeepEqual(defaultsView.Values, []string{outputViewMetadata, outputViewPlain, outputViewFull}) {
		t.Fatalf("batch default view enum = %+v", defaultsView)
	}
}

func TestBatchV2SchemaIsVisibleInCapabilities(t *testing.T) {
	for _, command := range mustCapabilities(t).Commands {
		if command.ID != "batch" {
			continue
		}
		var schema testCommandSchema
		if err := json.Unmarshal(command.Schema, &schema); err != nil {
			t.Fatalf("decode batch capability schema: %v", err)
		}
		if schema.ID != "batch@v2" || schema.Version != 2 {
			t.Fatalf("published batch schema = %q/%d, want batch@v2/2", schema.ID, schema.Version)
		}
		return
	}
	t.Fatal("capabilities omitted the batch command")
}

func TestProjectionSchemaAugmentationPreservesMetadataAndRejectsMissingFields(t *testing.T) {
	for _, test := range []struct {
		name, source string
		invalid      bool
	}{
		{name: "metadata", source: `{"id":"messages.get@v1","extra":{"preserved":true},"flags":[{"name":"--fields","value_type":"field_list","description":"kept","extra":[1,2]}]}`},
		{name: "missing", source: `{"flags":[]}`, invalid: true},
		{name: "duplicate", source: `{"flags":[{"name":"--fields"},{"name":"--fields"}]}`, invalid: true},
		{name: "invalid", source: `{`, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := augmentProjectionSchema("messages.get", json.RawMessage(test.source))
			if test.invalid {
				if err == nil || got != nil {
					t.Fatalf("invalid schema accepted: %s, error=%v", got, err)
				}
				return
			}
			if err != nil || !bytes.Equal(projectionSchemaMetadata(t, "messages.get", got), projectionSchemaMetadata(t, "messages.get", json.RawMessage(test.source))) {
				t.Fatalf("schema metadata changed: %s, error=%v", got, err)
			}
		})
	}
}

func TestPublishedCommandFlagsMatchActualParsers(t *testing.T) {
	strictParsers := strictBooleanParserCalls(t)
	for _, contract := range commandContracts {
		if !commandIsPublished(contract) {
			continue
		}
		schema := decodeTestCommandSchema(t, schemaForCommand(contract.ID))
		want := make(map[string]testCommandFlag, len(schema.Flags))
		for _, option := range schema.Flags {
			want[option.Name] = option
		}
		var got map[string]parserFlagSnapshot
		if allowed, ok := strictParsers[contract.ID]; ok {
			got = make(map[string]parserFlagSnapshot, len(allowed))
			for _, name := range allowed {
				got[name] = parserFlagSnapshot{ValueClass: "bool", Default: "false"}
			}
			delete(strictParsers, contract.ID)
			verifyStrictBooleanParser(t, contract.ID, allowed, want)
		} else {
			got = captureCommandParserFlags(t, contract.ID)
		}
		if err := compareParserFlagParity(contract.ID, want, got); err != nil {
			t.Error(err)
		}
	}
	if len(strictParsers) != 0 {
		t.Fatalf("strict boolean parsers have no published command schema: %+v", strictParsers)
	}
}

func TestParserSchemaDriftComparatorRejectsAddedAndRemovedFlags(t *testing.T) {
	want := map[string]testCommandFlag{
		"--json": {ValueType: "boolean"},
	}
	registered := map[string]parserFlagSnapshot{
		"--json": {ValueClass: "bool", Default: "false"},
	}
	if err := compareParserFlagParity("fixture", want, registered); err != nil {
		t.Fatalf("matching flags failed: %v", err)
	}
	booleanValue := map[string]testCommandFlag{
		"--enabled": {ValueType: "boolean", TakesValue: true, ValueRequired: true},
	}
	if err := compareParserFlagParity("fixture", booleanValue, map[string]parserFlagSnapshot{
		"--enabled": {ValueClass: "func", TakesValue: true},
	}); err != nil {
		t.Fatalf("value-taking boolean flag failed: %v", err)
	}
	added := map[string]parserFlagSnapshot{
		"--json":  {ValueClass: "bool", Default: "false"},
		"--extra": {ValueClass: "value"},
	}
	if err := compareParserFlagParity("fixture", want, added); err == nil {
		t.Fatal("an added parser flag did not fail parity")
	}
	if err := compareParserFlagParity("fixture", want, map[string]parserFlagSnapshot{}); err == nil {
		t.Fatal("a removed parser flag did not fail parity")
	}
}

type parserFlagSnapshot struct {
	ValueClass string
	TakesValue bool
	Default    string
}

func captureCommandParserFlags(t *testing.T, commandID string) map[string]parserFlagSnapshot {
	t.Helper()
	args := strings.Split(commandID, ".")
	args = append(args, "--help")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := runCommand(context.Background(), nil, args, &stdout, &stderr); code != 0 {
		t.Fatalf("%s help exit = %d, stdout=%q, stderr=%q", commandID, code, stdout.String(), stderr.String())
	}
	registered := make(map[string]parserFlagSnapshot)
	for _, line := range strings.Split(stdout.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "--") {
			continue
		}
		name := fields[0]
		if _, exists := registered[name]; exists {
			t.Fatalf("%s help repeats option %s", commandID, name)
		}
		takesValue := len(fields) > 1 && strings.HasPrefix(fields[1], "<") && strings.HasSuffix(fields[1], ">")
		kind := "bool"
		if takesValue {
			kind = "value"
			if fields[1] == "<true|false>" {
				kind = "func"
			}
		}
		registered[name] = parserFlagSnapshot{ValueClass: kind, TakesValue: takesValue, Default: parserDefaultFromHelp(line)}
	}
	if len(registered) == 0 {
		t.Fatalf("%s help output contains no registered options: %q", commandID, stdout.String())
	}
	return registered
}

func parserDefaultFromHelp(line string) string {
	const marker = "(default: "
	index := strings.LastIndex(line, marker)
	if index < 0 || !strings.HasSuffix(line, ")") {
		return ""
	}
	value := line[index+len(marker) : len(line)-1]
	switch value {
	case "standard input":
		return "-"
	case "1 MiB":
		return "1048576"
	case "4 GiB":
		return "4294967296"
	default:
		return value
	}
}

func compareParserFlagParity(
	commandID string,
	want map[string]testCommandFlag,
	got map[string]parserFlagSnapshot,
) error {
	var mismatches []string
	if len(want) != len(got) {
		mismatches = append(mismatches, fmt.Sprintf("parser/schema flag counts differ: parser=%d schema=%d", len(got), len(want)))
	}
	for name, expected := range want {
		actual, exists := got[name]
		if !exists {
			mismatches = append(mismatches, fmt.Sprintf("schema flag %s is not registered by the parser", name))
			continue
		}
		if !parserValueClassMatches(expected.ValueType, actual.ValueClass) {
			mismatches = append(mismatches, fmt.Sprintf("flag %s schema class %s disagrees with help value class %s", name, schemaParserValueClass(expected.ValueType), actual.ValueClass))
		}
		if expected.TakesValue != actual.TakesValue || expected.ValueRequired != actual.TakesValue {
			mismatches = append(mismatches, fmt.Sprintf("flag %s value-taking metadata disagrees: parser=%t schema=%t/%t", name, actual.TakesValue, expected.TakesValue, expected.ValueRequired))
		}
		if !parserDefaultMatches(expected.Default, actual.Default) {
			mismatches = append(mismatches, fmt.Sprintf("flag %s default disagrees: parser=%q schema=%q", name, actual.Default, expected.Default))
		}
	}
	for name := range got {
		if _, exists := want[name]; !exists {
			mismatches = append(mismatches, fmt.Sprintf("parser registers undocumented flag %s", name))
		}
	}
	if len(mismatches) > 0 {
		sort.Strings(mismatches)
		return fmt.Errorf("%s %s", commandID, strings.Join(mismatches, "; "))
	}
	return nil
}

func parserValueClassMatches(schemaType string, parserClass string) bool {
	switch schemaType {
	case "boolean":
		return parserClass == "bool" || parserClass == "func"
	default:
		return parserClass == "value"
	}
}

func schemaParserValueClass(schemaType string) string {
	if schemaType == "boolean" {
		return "boolean"
	}
	return "value-taking"
}

func parserDefaultMatches(schemaDefault string, parserDefault string) bool {
	if schemaDefault != "" && schemaDefault != "0" && schemaDefault != "false" {
		return schemaDefault == parserDefault
	}
	return parserDefault == "" || parserDefault == "0" || parserDefault == "false"
}

func strictBooleanParserCalls(t *testing.T) map[string][]string {
	t.Helper()
	source, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatalf("read strict boolean parser source: %v", err)
	}
	file, err := goparser.ParseFile(gotoken.NewFileSet(), "run.go", source, 0)
	if err != nil {
		t.Fatalf("parse strict boolean parser source: %v", err)
	}
	parsers := make(map[string][]string)
	for _, declaration := range file.Decls {
		function, ok := declaration.(*goast.FuncDecl)
		if !ok || function.Body == nil || !strings.HasPrefix(function.Name.Name, "run") {
			continue
		}
		var calls [][]string
		goast.Inspect(function.Body, func(node goast.Node) bool {
			call, ok := node.(*goast.CallExpr)
			if !ok {
				return true
			}
			name, ok := call.Fun.(*goast.Ident)
			if !ok || name.Name != "parseBooleanFlags" {
				return true
			}
			flags := make([]string, 0, len(call.Args)-1)
			for _, argument := range call.Args[1:] {
				literal, ok := argument.(*goast.BasicLit)
				if !ok || literal.Kind != gotoken.STRING {
					t.Fatalf("%s parseBooleanFlags arguments must be string literals", function.Name.Name)
				}
				flagName, err := strconv.Unquote(literal.Value)
				if err != nil || !strings.HasPrefix(flagName, "--") {
					t.Fatalf("%s has invalid boolean flag literal %q", function.Name.Name, literal.Value)
				}
				flags = append(flags, flagName)
			}
			calls = append(calls, flags)
			return true
		})
		if len(calls) == 0 {
			continue
		}
		if len(calls) != 1 || len(function.Name.Name) <= len("run") {
			t.Fatalf("%s has %d strict boolean parser calls", function.Name.Name, len(calls))
		}
		commandID := strings.ToLower(function.Name.Name[len("run"):len("run")+1]) + function.Name.Name[len("run")+1:]
		parsers[commandID] = calls[0]
	}
	return parsers
}

func verifyStrictBooleanParser(
	t *testing.T,
	commandID string,
	allowed []string,
	want map[string]testCommandFlag,
) {
	t.Helper()
	if len(allowed) != len(want) {
		t.Fatalf("%s strict parser/schema flag counts differ: parser=%d schema=%d", commandID, len(allowed), len(want))
	}
	for name, schemaFlag := range want {
		if schemaFlag.ValueType != "boolean" || schemaFlag.TakesValue || schemaFlag.ValueRequired || schemaFlag.Required || schemaFlag.Repeatable {
			t.Fatalf("%s strict parser schema flag %s is not a plain optional boolean: %+v", commandID, name, schemaFlag)
		}
		found := false
		for _, allowedName := range allowed {
			found = found || allowedName == name
		}
		if !found {
			t.Fatalf("%s schema flag %s is not accepted by strict parser", commandID, name)
		}
	}
	defaults, err := parseBooleanFlags(nil, allowed...)
	if err != nil {
		t.Fatalf("%s strict parser defaults: %v", commandID, err)
	}
	for name, value := range defaults {
		if value {
			t.Fatalf("%s strict parser default for %s = true", commandID, name)
		}
	}
	for _, selected := range allowed {
		values, err := parseBooleanFlags([]string{selected}, allowed...)
		if err != nil {
			t.Fatalf("%s strict parser rejected %s: %v", commandID, selected, err)
		}
		for name, value := range values {
			if value != (name == selected) {
				t.Fatalf("%s strict parser value for %s after %s = %t", commandID, name, selected, value)
			}
		}
	}
}

func TestCapabilitiesScopeKeepsCommandContractMetadata(t *testing.T) {
	full := mustCapabilities(t)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := Run(context.Background(), nil, []string{"capabilities", "--for", "messages.search", "--json"}, &stdout, &stderr); code != 0 {
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
	if code := Run(context.Background(), nil, []string{"capabilities", "--for", "messages.*", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("family capabilities exit = %d, stderr = %q", code, stderr.String())
	}
	response = envelope{}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode family capabilities: %v", err)
	}
	if response.Data.Capabilities == nil || response.Data.Capabilities.Scope != "" || len(response.Data.Capabilities.Commands) != 13 {
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
	if code := Run(context.Background(), nil, []string{"capabilities", "--for", "missing.command", "--json"}, &stdout, &stderr); code != 2 || stderr.Len() != 0 {
		t.Fatalf("unknown selector exit = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"code":"invalid_argument"`)) {
		t.Fatalf("unknown selector response = %s", stdout.String())
	}
}

func TestCapabilitiesSelectedCommandsPreserveFullContract(t *testing.T) {
	full := mustCapabilities(t)
	code, output, response := captureCapabilitiesJSON(t,
		"--for", "messages.get,messages.search", "--json",
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
	if !reflect.DeepEqual(selected.SyncCheckPolicy, full.SyncCheckPolicy) ||
		!reflect.DeepEqual(selected.DraftSavePolicy, full.DraftSavePolicy) {
		t.Fatal("selected manifest dropped or changed shared policies")
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
			args := []string{"--for", test.selector}
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
		name                  string
		selected              string
		singles               []string
		baselineSeparateBytes int
	}{
		{
			name: "search and get", selected: "messages.search,messages.get",
			singles:               []string{"messages.search", "messages.get"},
			baselineSeparateBytes: 17993,
		},
		{
			name: "draft create inspect send", selected: "drafts.create,drafts.inspect,drafts.send",
			singles:               []string{"drafts.create", "drafts.inspect", "drafts.send"},
			baselineSeparateBytes: 26623,
		},
	}
	for _, workflow := range workflows {
		t.Run(workflow.name, func(t *testing.T) {
			code, selected, response := captureCapabilitiesJSON(t, "--for", workflow.selected, "--json")
			if code != 0 || !response.OK || response.Data.Capabilities == nil {
				t.Fatalf("selected manifest exit = %d, response = %+v", code, response)
			}
			selectedBytes := len(selected)
			separateBytes := 0
			for _, id := range workflow.singles {
				code, single, singleResponse := captureCapabilitiesJSON(t, "--for", id, "--json")
				if code != 0 || !singleResponse.OK || singleResponse.Data.Capabilities == nil {
					t.Fatalf("singular manifest for %s exit = %d, response = %+v", id, code, singleResponse)
				}
				separateBytes += len(single)
			}
			// Baseline 3f0ffe376fef0f4592dccf356e17a71df5a7dccf published
			// full limits per singular command. Preserve its 25% reduction
			// guarantee and additionally require sharing to beat scoped singles.
			if selectedBytes*4 > workflow.baselineSeparateBytes*3 || selectedBytes >= separateBytes {
				t.Fatalf("workflow did not preserve baseline savings or beat scoped singles: selected=%d baseline=%d separate=%d", selectedBytes, workflow.baselineSeparateBytes, separateBytes)
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
		"--max-messages", strconv.Itoa(maxMessages), "--max-scan-bytes", strconv.FormatInt(maxBytes, 10), "--json",
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
		flags["--max-scan-bytes"].Default != strconv.FormatInt(mail.DefaultSearchMaxBytes, 10) ||
		*flags["--max-scan-bytes"].Minimum != 1 || *flags["--max-scan-bytes"].Maximum != mail.MaximumSearchMaxBytes {
		t.Fatalf("search schema bounds = %+v/%+v", flags["--max-messages"], flags["--max-scan-bytes"])
	}
	maxBytesFlag := flags["--max-bytes"]
	if maxBytesFlag.Name != "--max-bytes" || maxBytesFlag.Default != strconv.FormatInt(defaultJSONOutputBytes, 10) ||
		maxBytesFlag.Minimum == nil || *maxBytesFlag.Minimum != 1 ||
		maxBytesFlag.Maximum == nil || *maxBytesFlag.Maximum != maximumJSONOutputBytes {
		t.Fatalf("search response-size schema = %+v", maxBytesFlag)
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
	if batch.ID != "batch@v2" || batch.Version != 2 || batch.JSONInput == nil ||
		batch.JSONInput.MaximumBytes != mail.MaximumBatchInputBytes || batch.JSONInput.AdditionalProperties {
		t.Fatalf("batch JSON input = %+v", batch.JSONInput)
	}
	items := jsonFieldByName(batch.JSONInput.Fields, "items")
	concurrency := jsonFieldByName(batch.JSONInput.Fields, "concurrency")
	defaults := jsonFieldByName(batch.JSONInput.Fields, "defaults")
	maxBytes := schemaFlagsByName(batch)["--max-bytes"]
	if items == nil || !items.Required || !schemaHasConstraint(batch, "non_empty") ||
		items.Maximum == nil || *items.Maximum != int64(mail.MaximumBatchItems) ||
		defaults == nil || defaults.ValueType != "batch_read_defaults" ||
		concurrency == nil || concurrency.Minimum == nil || *concurrency.Minimum != 0 ||
		concurrency.Maximum == nil || *concurrency.Maximum != int64(mail.MaximumBatchConcurrency) ||
		maxBytes.Name != "--max-bytes" || maxBytes.Default != "1048576" ||
		maxBytes.Minimum == nil || *maxBytes.Minimum != 1 ||
		maxBytes.Maximum == nil || *maxBytes.Maximum != maximumJSONOutputBytes {
		t.Fatalf("batch items field = %+v, constraints = %+v", items, batch.Constraints)
	}
	view := jsonFieldByName(batch.JSONInput.ItemFields, "view")
	fields := jsonFieldByName(batch.JSONInput.ItemFields, "fields")
	defaultView := jsonFieldByName(batch.JSONInput.DefaultsFields, "view")
	defaultFields := jsonFieldByName(batch.JSONInput.DefaultsFields, "fields")
	id := jsonFieldByName(batch.JSONInput.ItemFields, "id")
	ref := jsonFieldByName(batch.JSONInput.ItemFields, "ref")
	if view == nil || !reflect.DeepEqual(view.Values, []string{outputViewMetadata, outputViewPlain, outputViewFull}) ||
		fields == nil || !reflect.DeepEqual(fields.Values, projectionFieldNames(projectionTargetMessage)) ||
		defaultView == nil || !reflect.DeepEqual(defaultView.Values, []string{outputViewMetadata, outputViewPlain, outputViewFull}) ||
		defaultFields == nil || !reflect.DeepEqual(defaultFields.Values, projectionFieldNames(projectionTargetMessage)) ||
		id == nil || !id.Required || !strings.Contains(id.Description, "non-empty trimmed") ||
		ref == nil || !ref.Required {
		t.Fatalf("batch projection fields = id:%+v ref:%+v item_view:%+v item_fields:%+v defaults_view:%+v defaults_fields:%+v",
			id, ref, view, fields, defaultView, defaultFields)
	}
	if countJSONConstraints(batch.JSONInput, "conditional") != 11 {
		t.Fatalf("batch operation constraints = %+v", batch.JSONInput.Constraints)
	}
	if !jsonInputHasFieldConstraint(batch.JSONInput, "mutually_exclusive", []string{"view", "fields"}) {
		t.Fatalf("batch read view/fields exclusivity is missing: %+v", batch.JSONInput.Constraints)
	}
	if !jsonInputHasFieldConstraint(batch.JSONInput, "mutually_exclusive", []string{"defaults.view", "defaults.fields"}) ||
		!jsonInputHasFieldConstraint(batch.JSONInput, "conditional", []string{"operation", "defaults"}) {
		t.Fatalf("batch defaults constraints are missing: %+v", batch.JSONInput.Constraints)
	}
	if !jsonInputHasFieldConstraint(batch.JSONInput, "unique", []string{"items.id"}) ||
		!jsonInputHasFieldConstraint(batch.JSONInput, "conditional", []string{"operation", "output_path"}) {
		t.Fatalf("batch item uniqueness constraints are missing: %+v", batch.JSONInput.Constraints)
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

func TestReferenceRouteSchemasPublishCanonicalFlagAndOperand(t *testing.T) {
	commands := referenceRouteIDs()
	if len(commands) != 23 {
		t.Fatalf("reference route inventory has %d commands, want 23", len(commands))
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			schema := decodeTestCommandSchema(t, schemaForCommand(command))
			flags := schemaFlagsByName(schema)
			ref, exists := flags["--ref"]
			if !exists || !ref.TakesValue || !ref.ValueRequired || ref.Required || !ref.Repeatable {
				t.Fatalf("--ref schema = %+v, exists=%t", ref, exists)
			}
			if _, exists := flags["--message"]; exists {
				t.Fatal("legacy --message remains in canonical schema")
			}
			if !reflect.DeepEqual(schema.PositionalArguments, []string{"REF"}) {
				t.Fatalf("positional_arguments = %q, want [REF]", schema.PositionalArguments)
			}
			var source *testConstraint
			for index := range schema.Constraints {
				if schema.Constraints[index].Kind == "reference_source" {
					source = &schema.Constraints[index]
				}
			}
			if source == nil || !reflect.DeepEqual(source.Flags, []string{"--ref"}) ||
				!reflect.DeepEqual(source.Fields, []string{"REF"}) ||
				source.Description != "Supply one non-empty reference; all supplied --ref and REF values must be identical." {
				t.Fatalf("reference_source constraint = %+v", source)
			}
		})
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
	DefaultsFields       []testJSONField  `json:"defaults_fields,omitempty"`
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

func schemaHasFlagConstraint(schema testCommandSchema, kind string, flags []string) bool {
	for _, constraint := range schema.Constraints {
		if constraint.Kind == kind && reflect.DeepEqual(constraint.Flags, flags) {
			return true
		}
	}
	return false
}

func jsonInputHasFieldConstraint(input *testJSONInput, kind string, fields []string) bool {
	for _, constraint := range input.Constraints {
		if constraint.Kind == kind && reflect.DeepEqual(constraint.Fields, fields) {
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

func TestMessagePageSchemasPublishFieldProjections(t *testing.T) {
	for _, test := range []struct {
		command string
		target  projectionTarget
	}{
		{command: "messages.list", target: projectionTargetListPage},
		{command: "messages.filter", target: projectionTargetSearchPage},
		{command: "messages.search", target: projectionTargetSearchPage},
	} {
		t.Run(test.command, func(t *testing.T) {
			schema := decodeTestCommandSchema(t, schemaForCommand(test.command))
			fields := schemaFlagsByName(schema)["--fields"]
			if fields.Name != "--fields" || fields.ValueType != "field_list" ||
				!reflect.DeepEqual(fields.Values, projectionFieldNames(test.target)) {
				t.Fatalf("page projection schema fields = %+v", fields)
			}
			if !schemaHasFlagConstraint(schema, "conditional", []string{"--fields"}) {
				t.Fatalf("page projection schema omits all-field exclusivity: %+v", schema.Constraints)
			}
		})
	}
}

func TestSendSetupSchemaDocumentsEndpointConstraints(t *testing.T) {
	schema := decodeTestCommandSchema(t, schemaForCommand("send.setup"))
	if !schemaHasFlagConstraint(schema, "conditional", []string{
		"--smtp-host", "--smtp-port", "--imap-host", "--imap-port", "--account",
	}) {
		t.Fatalf("send.setup schema omits endpoint/account constraints: %+v", schema.Constraints)
	}
}
