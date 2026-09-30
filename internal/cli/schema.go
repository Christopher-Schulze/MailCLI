package cli

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed schemas/*.json
var commandSchemaFiles embed.FS

var (
	commandSchemaCatalogOnce sync.Once
	commandSchemaCatalog     map[string]json.RawMessage
	commandSchemaCatalogErr  error
)

func embeddedCommandSchemaCatalog() (map[string]json.RawMessage, error) {
	commandSchemaCatalogOnce.Do(func() {
		commandSchemaCatalog, commandSchemaCatalogErr = loadCommandSchemas(commandSchemaFiles, publishedCommandSchemaIDs())
	})
	return commandSchemaCatalog, commandSchemaCatalogErr
}

func publishedCommandSchemaIDs() []string {
	ids := make([]string, 0, len(commandContracts))
	for _, contract := range commandContracts {
		if commandIsPublished(contract) {
			ids = append(ids, contract.ID)
		}
	}
	return ids
}

func loadCommandSchemas(files fs.FS, expectedIDs []string) (map[string]json.RawMessage, error) {
	expected, err := expectedCommandSchemaIDs(expectedIDs)
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(files, "schemas")
	if err != nil {
		return nil, invalidEmbeddedCommandSchema("schemas", err)
	}
	schemas := make(map[string]json.RawMessage, len(entries))
	for _, entry := range entries {
		id, schema, err := readEmbeddedCommandSchema(files, entry, expected)
		if err != nil {
			return nil, err
		}
		if _, exists := schemas[id]; exists {
			return nil, invalidEmbeddedCommandSchema("schemas/"+id+".json", fmt.Errorf("command %q has multiple schema files", id))
		}
		schemas[id] = schema
	}
	if err := validateCommandSchemaInventory(expected, schemas); err != nil {
		return nil, err
	}
	return schemas, nil
}

func expectedCommandSchemaIDs(expectedIDs []string) (map[string]struct{}, error) {
	expected := make(map[string]struct{}, len(expectedIDs))
	for _, id := range expectedIDs {
		if id == "" {
			return nil, invalidEmbeddedCommandSchema("schemas", fmt.Errorf("published command ID is empty"))
		}
		if _, exists := expected[id]; exists {
			return nil, invalidEmbeddedCommandSchema("schemas", fmt.Errorf("published command ID %q is repeated", id))
		}
		expected[id] = struct{}{}
	}
	if len(expected) == 0 {
		return nil, invalidEmbeddedCommandSchema("schemas", fmt.Errorf("published command inventory is empty"))
	}
	return expected, nil
}

func readEmbeddedCommandSchema(
	files fs.FS,
	entry fs.DirEntry,
	expected map[string]struct{},
) (string, json.RawMessage, error) {
	filename, id, err := schemaFileIdentity(entry)
	if err != nil {
		return "", nil, err
	}
	path := "schemas/" + filename
	compact, err := readNormalizedEmbeddedSchema(files, path)
	if err != nil {
		return "", nil, err
	}
	commandID, err := embeddedSchemaCommandID(path, filename, id, compact)
	if err != nil {
		return "", nil, err
	}
	if _, published := expected[commandID]; !published {
		return "", nil, invalidEmbeddedCommandSchema(path, fmt.Errorf("command %q is not in the published registry", commandID))
	}
	schema, err := augmentNormalizedProjectionSchema(commandID, compact)
	if err != nil {
		return "", nil, invalidEmbeddedCommandSchema(path, err)
	}
	return commandID, schema, nil
}

func schemaFileIdentity(entry fs.DirEntry) (string, string, error) {
	filename := entry.Name()
	id := strings.TrimSuffix(filename, ".json")
	if entry.IsDir() || id == "" || id == filename {
		return "", "", invalidEmbeddedCommandSchema("schemas/"+filename, fmt.Errorf("unexpected schema entry"))
	}
	return filename, id, nil
}

func readNormalizedEmbeddedSchema(files fs.FS, path string) (json.RawMessage, error) {
	raw, err := fs.ReadFile(files, path)
	if err != nil {
		return nil, invalidEmbeddedCommandSchema(path, err)
	}
	compact, err := normalizeSchemaJSON(raw)
	if err != nil {
		return nil, invalidEmbeddedCommandSchema(path, err)
	}
	return compact, nil
}

func embeddedSchemaCommandID(path, filename, fileID string, source json.RawMessage) (string, error) {
	var identity struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(source, &identity); err != nil {
		return "", invalidEmbeddedCommandSchema(path, err)
	}
	commandID, versionText, versioned := strings.Cut(identity.ID, "@v")
	version, versionErr := strconv.Atoi(versionText)
	if !versioned || commandID == "" || versionErr != nil || version <= 0 ||
		strconv.Itoa(version) != versionText || identity.Version != version || commandID != fileID {
		return "", invalidEmbeddedCommandSchema(path,
			fmt.Errorf("schema identity %q/%d does not match filename %s", identity.ID, identity.Version, filename))
	}
	return commandID, nil
}

func validateCommandSchemaInventory(
	expected map[string]struct{},
	schemas map[string]json.RawMessage,
) error {
	missing := make([]string, 0)
	for id := range expected {
		if _, exists := schemas[id]; !exists {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return invalidEmbeddedCommandSchema("schemas", fmt.Errorf("missing published command schemas: %s", strings.Join(missing, ", ")))
	}
	return nil
}

func invalidEmbeddedCommandSchema(path string, cause error) error {
	return &commandError{
		code:    "capability_schema_invalid",
		message: fmt.Sprintf("embedded command schema %s: %v", path, cause),
		cause:   cause,
	}
}

func checkedSchemaForCommand(id string) (json.RawMessage, error) {
	schemas, err := embeddedCommandSchemaCatalog()
	if err != nil {
		return nil, err
	}
	schema, exists := schemas[id]
	if !exists {
		return nil, invalidEmbeddedCommandSchema("schemas/"+id+".json", fmt.Errorf("schema is not in the published inventory"))
	}
	return append(json.RawMessage(nil), schema...), nil
}

// schemaForCommand is a best-effort parser hint; capability publication uses
// checkedSchemaForCommand so invalid embedded data cannot become a fallback schema.
func schemaForCommand(id string) json.RawMessage {
	schema, err := checkedSchemaForCommand(id)
	if err != nil {
		return nil
	}
	return schema
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
	case "messages.list", "messages.thread":
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
	return augmentNormalizedProjectionSchema(id, normalized)
}

func augmentNormalizedProjectionSchema(id string, source json.RawMessage) (json.RawMessage, error) {
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

// normalizeSchemaJSON validates and compacts schema JSON, removes only HTML
// Unicode escapes, and preserves property order and every other source escape.
func normalizeSchemaJSON(source json.RawMessage) (json.RawMessage, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, source); err != nil {
		return nil, fmt.Errorf("normalize command schema: %w", err)
	}
	source = compact.Bytes()
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

func resolveCapabilityCommands(selector string) ([]string, error) {
	values := strings.Split(selector, ",")
	if strings.TrimSpace(selector) == "" {
		return nil, &commandError{code: "invalid_argument", message: "--for requires at least one command ID or family.* wildcard"}
	}
	selected := make(map[string]struct{}, len(values))
	for _, value := range values {
		ids, err := capabilitySelectorIDs(strings.TrimSpace(value))
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if _, exists := selected[id]; exists {
				return nil, &commandError{code: "invalid_argument", message: fmt.Sprintf("--for repeats or overlaps command ID %q", id)}
			}
			selected[id] = struct{}{}
		}
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

func capabilitySelectorIDs(value string) ([]string, error) {
	if value == "" {
		return nil, &commandError{code: "invalid_argument", message: "--for cannot contain an empty selector"}
	}
	if !strings.HasSuffix(value, ".*") {
		if !publishedCommandID(value) {
			return nil, unknownCapabilityScope(value)
		}
		return []string{value}, nil
	}
	family := strings.TrimSuffix(value, ".*")
	if family == "" || strings.Contains(family, "*") {
		return nil, unknownCapabilityScope(value)
	}
	ids := []string{}
	for _, contract := range commandContracts {
		if commandIsPublished(contract) && strings.HasPrefix(contract.ID, family+".") {
			ids = append(ids, contract.ID)
		}
	}
	if len(ids) == 0 {
		return nil, unknownCapabilityScope(value)
	}
	return ids, nil
}

func unknownCapabilityScope(value string) error {
	return &commandError{code: "invalid_argument", message: fmt.Sprintf("unknown capability selector %q", value)}
}
