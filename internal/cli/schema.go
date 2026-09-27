package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

var (
	schemaPayloadOnce  sync.Once
	schemaPayloadBytes []byte
)

func schemaPayload() []byte {
	schemaPayloadOnce.Do(func() {
		reader, err := gzip.NewReader(strings.NewReader(schemaPayloadGZIP))
		if err != nil {
			return
		}
		decoded, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil {
			return
		}
		schemaPayloadBytes = decoded
	})
	return schemaPayloadBytes
}

func schemaForCommand(id string) json.RawMessage {
	prefix := []byte(`{"id":"` + id + `@v`)
	for _, line := range bytes.Split(schemaPayload(), []byte{'\n'}) {
		if bytes.HasPrefix(line, prefix) {
			schema, err := augmentProjectionSchema(id, line)
			if err != nil {
				return nil
			}
			return schema
		}
	}
	return emptyCommandSchema(id)
}

func projectionTargetForCommand(id string) (projectionTarget, bool) {
	switch id {
	case "messages.get", "drafts.open":
		return projectionTargetMessage, true
	case "drafts.create", "drafts.edit", "drafts.adopt", "drafts.inspect", "drafts.update", "messages.reply", "messages.forward":
		return projectionTargetDraft, true
	case "attachments.list":
		return projectionTargetAttachment, true
	case "messages.raw":
		return projectionTargetRaw, true
	case "drafts.list":
		return projectionTargetDraftList, true
	case "messages.list":
		return projectionTargetListPage, true
	case "messages.search", "messages.filter":
		return projectionTargetSearchPage, true
	default:
		return "", false
	}
}

// Only projection enums are derived in memory; canonical schema metadata stays
// intact and the stored schema source remains unchanged.
func augmentProjectionSchema(id string, source json.RawMessage) (json.RawMessage, error) {
	normalized, err := normalizeSchemaJSON(source)
	if err != nil {
		return nil, err
	}
	source = normalized
	target, projected := projectionTargetForCommand(id)
	if !projected && id != "batch" {
		return append(json.RawMessage(nil), source...), nil
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(source, &schema); err != nil {
		return nil, fmt.Errorf("decode projection schema: %w", err)
	}
	if projected {
		fields, err := projectionSchemaFields(schema["flags"], "--fields", target)
		if err != nil {
			return nil, err
		}
		schema["flags"] = fields
	} else {
		input, err := augmentBatchProjectionSchema(schema["json_input"])
		if err != nil {
			return nil, err
		}
		schema["json_input"] = input
	}
	encoded, err := marshalCLIJSON(schema)
	if err != nil {
		return nil, fmt.Errorf("encode projection schema: %w", err)
	}
	return encoded, nil
}

func augmentBatchProjectionSchema(source json.RawMessage) (json.RawMessage, error) {
	var input map[string]json.RawMessage
	if err := json.Unmarshal(source, &input); err != nil {
		return nil, fmt.Errorf("decode batch projection schema: %w", err)
	}
	fields, err := projectionSchemaFields(input["item_fields"], "fields", projectionTargetMessage)
	if err != nil {
		return nil, err
	}
	input["item_fields"] = fields
	if defaultsFields, exists := input["defaults_fields"]; exists {
		fields, err := projectionSchemaFields(defaultsFields, "fields", projectionTargetMessage)
		if err != nil {
			return nil, err
		}
		input["defaults_fields"] = fields
	}
	encoded, err := marshalCLIJSON(input)
	if err != nil {
		return nil, fmt.Errorf("encode batch projection schema: %w", err)
	}
	return encoded, nil
}

func projectionSchemaFields(source json.RawMessage, name string, target projectionTarget) (json.RawMessage, error) {
	var fields []map[string]json.RawMessage
	if err := json.Unmarshal(source, &fields); err != nil {
		return nil, fmt.Errorf("decode projection fields: %w", err)
	}
	found := false
	for _, field := range fields {
		var fieldName string
		if err := json.Unmarshal(field["name"], &fieldName); err != nil {
			return nil, fmt.Errorf("decode projection field name: %w", err)
		}
		if fieldName != name {
			continue
		}
		if found {
			return nil, fmt.Errorf("duplicate projection field %s", name)
		}
		values, err := marshalCLIJSON(projectionFieldNames(target))
		if err != nil {
			return nil, fmt.Errorf("encode projection values: %w", err)
		}
		field["values"] = values
		found = true
	}
	if !found {
		return nil, fmt.Errorf("missing projection field %s", name)
	}
	encoded, err := marshalCLIJSON(fields)
	if err != nil {
		return nil, fmt.Errorf("encode projection fields: %w", err)
	}
	return encoded, nil
}

func emptyCommandSchema(id string) json.RawMessage {
	encoded, err := marshalCLIJSON(id + "@v1")
	if err != nil {
		return json.RawMessage(`{"id":"unknown@v1","version":1,"flags":[],"positional_arguments":[],"constraints":[]}`)
	}
	output := append([]byte(`{"id":`), encoded...)
	output = append(output, `,"version":1,"flags":[],"positional_arguments":[],"constraints":[]}`...)
	return json.RawMessage(output)
}

// normalizeSchemaJSON removes only HTML Unicode escapes and preserves schema
// property order and every other source escape.
func normalizeSchemaJSON(source json.RawMessage) (json.RawMessage, error) {
	if !json.Valid(source) {
		return nil, fmt.Errorf("normalize command schema: invalid JSON")
	}
	if !containsHTMLEscapedString(source) {
		return append(json.RawMessage(nil), source...), nil
	}
	var normalized bytes.Buffer
	normalized.Grow(len(source))
	inString := false
	for index := 0; index < len(source); index++ {
		current := source[index]
		if current == '"' {
			inString = !inString
			normalized.WriteByte(current)
			continue
		}
		if inString && current == '\\' {
			if source[index+1] == 'u' {
				escaped := source[index+2 : index+6]
				switch {
				case bytes.EqualFold(escaped, []byte("0026")):
					normalized.WriteByte('&')
					index += 5
					continue
				case bytes.EqualFold(escaped, []byte("003c")):
					normalized.WriteByte('<')
					index += 5
					continue
				case bytes.EqualFold(escaped, []byte("003e")):
					normalized.WriteByte('>')
					index += 5
					continue
				}
			}
			normalized.Write(source[index : index+2])
			index++
			continue
		}
		normalized.WriteByte(current)
	}
	return normalized.Bytes(), nil
}

func containsHTMLEscapedString(source []byte) bool {
	return bytes.Contains(source, []byte(`\u0026`)) || bytes.Contains(source, []byte(`\u003c`)) ||
		bytes.Contains(source, []byte(`\u003C`)) || bytes.Contains(source, []byte(`\u003e`)) ||
		bytes.Contains(source, []byte(`\u003E`))
}

func resolveCapabilityScope(command, family, scope string) (string, string, error) {
	command = strings.TrimSpace(command)
	family = strings.TrimSpace(family)
	scope = strings.TrimSpace(scope)
	selectors := 0
	if command != "" {
		selectors++
	}
	if family != "" {
		selectors++
	}
	if scope != "" {
		selectors++
	}
	if selectors > 1 {
		return "", "", &commandError{code: "invalid_argument", message: "--command, --family, and --scope are mutually exclusive"}
	}
	if command != "" {
		if !publishedCommandID(command) {
			return "", "", unknownCapabilityScope(command)
		}
		return command, "", nil
	}
	if family != "" {
		if !publishedCommandFamily(family) {
			return "", "", unknownCapabilityScope(family)
		}
		return "", family, nil
	}
	if scope == "" {
		return "", "", nil
	}
	if publishedCommandID(scope) {
		return scope, "", nil
	}
	if publishedCommandFamily(scope) {
		return "", scope, nil
	}
	return "", "", unknownCapabilityScope(scope)
}

func resolveCapabilityCommands(selector string) ([]string, error) {
	values := strings.Split(selector, ",")
	if strings.TrimSpace(selector) == "" {
		return nil, &commandError{code: "invalid_argument", message: "--commands requires at least one command ID"}
	}
	selected := make(map[string]struct{}, len(values))
	for _, value := range values {
		id := strings.TrimSpace(value)
		if id == "" {
			return nil, &commandError{code: "invalid_argument", message: "--commands cannot contain an empty command ID"}
		}
		if _, exists := selected[id]; exists {
			return nil, &commandError{code: "invalid_argument", message: fmt.Sprintf("--commands repeats command ID %q", id)}
		}
		if !publishedCommandID(id) {
			return nil, unknownCapabilityScope(id)
		}
		selected[id] = struct{}{}
	}
	canonical := make([]string, 0, len(selected))
	for _, contract := range commandContracts {
		if !commandIsPublished(contract) {
			continue
		}
		if _, ok := selected[contract.ID]; ok {
			canonical = append(canonical, contract.ID)
		}
	}
	return canonical, nil
}

func publishedCommandID(id string) bool {
	for _, contract := range commandContracts {
		if commandIsPublished(contract) && contract.ID == id {
			return true
		}
	}
	return false
}

func publishedCommandFamily(family string) bool {
	for _, contract := range commandContracts {
		if commandIsPublished(contract) && (contract.ID == family || strings.HasPrefix(contract.ID, family+".")) {
			return true
		}
	}
	return false
}

func unknownCapabilityScope(value string) error {
	return &commandError{code: "invalid_argument", message: fmt.Sprintf("unknown capability scope %q", value)}
}
