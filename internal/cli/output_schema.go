package cli

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode"

	"mailcli/internal/mail"
)

// outputNode is a closed typed tree. References resolve against the containing
// capability manifest's $defs; map values use additional_values, never a
// permissive undeclared-field escape hatch.
type outputNode struct {
	Name             string          `json:"name"`
	Type             string          `json:"type"`
	Nullable         bool            `json:"nullable"`
	AlwaysPresent    bool            `json:"always_present"`
	Description      string          `json:"description"`
	Ref              string          `json:"$ref,omitempty"`
	Enum             []string        `json:"enum,omitempty"`
	Fields           []outputNode    `json:"fields,omitempty"`
	Items            *outputNode     `json:"items,omitempty"`
	AdditionalValues *outputNode     `json:"additional_values,omitempty"`
	Variants         []outputVariant `json:"variants,omitempty"`
}

type outputVariant struct {
	When          string              `json:"when"`
	Required      []string            `json:"required,omitempty"`
	Fields        map[string][]string `json:"field_selection,omitempty"`
	Views         map[string][]string `json:"views,omitempty"`
	OneOfRequired [][]string          `json:"one_of_required,omitempty"`
}

// These types describe canonical parameter JSON, whose values are serialized
// from the embedded files. They are checked against every actual schema by
// the recursive capabilities fixture, including dynamically added selectors.
type outputCommandParameters struct {
	ID                  string             `json:"id"`
	Version             int                `json:"version"`
	Flags               []outputParameter  `json:"flags"`
	PositionalArguments []string           `json:"positional_arguments"`
	Constraints         []outputConstraint `json:"constraints"`
	JSONInput           *outputJSONInput   `json:"json_input,omitempty"`
	Output              *outputNode        `json:"output,omitempty"`
}

type outputParameter struct {
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

type outputConstraint struct {
	Kind        string   `json:"kind"`
	Flags       []string `json:"flags,omitempty"`
	Fields      []string `json:"fields,omitempty"`
	Description string   `json:"description"`
}

type outputJSONInput struct {
	Flag                 string             `json:"flag"`
	ValueType            string             `json:"value_type"`
	MaximumBytes         int64              `json:"maximum_bytes"`
	AdditionalProperties bool               `json:"additional_properties"`
	Fields               []outputInputField `json:"fields"`
	ItemFields           []outputInputField `json:"item_fields,omitempty"`
	DefaultsFields       []outputInputField `json:"defaults_fields,omitempty"`
	Constraints          []outputConstraint `json:"constraints"`
}

type outputSchemaBuilder struct {
	definitions map[string]outputNode
	// definitionTypes proves each public $defs key names exactly one Go type.
	definitionTypes map[string]reflect.Type
}

type outputInputField struct {
	Name        string   `json:"name"`
	ValueType   string   `json:"value_type"`
	Required    bool     `json:"required,omitempty"`
	Minimum     *int64   `json:"minimum,omitempty"`
	Maximum     *int64   `json:"maximum,omitempty"`
	Values      []string `json:"values,omitempty"`
	Description string   `json:"description"`
}

// outputDefinitionName is the stable public $defs key: the snake_case type name
// without the Go package path, so internal moves do not change the contract.
func outputDefinitionName(value reflect.Type) string {
	runes := []rune(value.Name())
	var name strings.Builder
	for index, character := range runes {
		upper := unicode.IsUpper(character)
		if upper && index > 0 {
			previousLower := !unicode.IsUpper(runes[index-1])
			nextLower := index+1 < len(runes) && unicode.IsLower(runes[index+1])
			if previousLower || nextLower {
				name.WriteByte('_')
			}
		}
		name.WriteRune(unicode.ToLower(character))
	}
	return name.String()
}

func outputDescription(name string) string {
	switch name {
	case "in_reply_to":
		return "Every valid In-Reply-To msg-id in header order, including angle brackets; [] when absent or unavailable."
	case "references":
		return "Every valid References msg-id in header order, including angle brackets; [] when absent or unavailable."
	case "from":
		return "Structured From identity; decoded name and address with only its domain lowercased; existing sender is unchanged."
	case "threading_complete":
		return "True only after a complete readable RFC header block; false for unrequested, unavailable or bounded-out headers."
	case "excerpt":
		return "Plain-first, HTML-fallback excerpt with quoted lines/signature removed, whitespace collapsed and at most --excerpt-length runes."
	case "excerpt_complete":
		return "True only when the available RFC source and MIME text parse fit the 256 KiB source/decoded-text and 64-part bounds; rune truncation is intentional."
	case "excerpt_source":
		return "local, imap-partial or unavailable; bounded-out, unrequested and missing text must be interpreted with excerpt_complete."
	case "header_fields":
		return "Ordered unfolded RFC header name/value pairs; duplicates and original field spelling are retained."
	case "headers":
		return "Original bounded RFC header block; selected explicitly or by --view full."
	case "summary":
		return "Message identity, local metadata and opt-in reply/excerpt evidence; empty enrichment is not proof of absence."
	case "message_id":
		return "Existing Message-ID representation; consumers normalize optional outer angle brackets before exact comparison."
	case "coverage":
		return "Search coverage and consistency evidence; partial or missing sources do not prove absence."
	case "data":
		return "Command payload; fields may retain partial evidence on errors; success requirements and projection variants are declared separately."
	case "schema":
		return "Resolved canonical parameter contract with output; scoped discovery may instead emit schema_ref."
	case "$defs":
		return "Shared recursive output definitions; $ref resolves here within the containing capability manifest."
	default:
		return strings.ReplaceAll(name, "_", " ") + " as serialized by its owning command/type; omission follows JSON tags and declared projections."
	}
}

func (builder *outputSchemaBuilder) node(value reflect.Type, name string, present bool) (outputNode, error) {
	node := outputNode{Name: name, AlwaysPresent: present, Description: outputDescription(name)}
	for value.Kind() == reflect.Pointer {
		value = value.Elem()
		node.Nullable = true
	}
	if value == reflect.TypeFor[time.Time]() {
		node.Type = "string"
		return node, nil
	}
	if value == reflect.TypeFor[json.RawMessage]() {
		return node, fmt.Errorf("output type override required for %s", name)
	}
	switch value.Kind() {
	case reflect.String:
		node.Type = "string"
	case reflect.Bool:
		node.Type = "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		node.Type = "integer"
	case reflect.Float32, reflect.Float64:
		node.Type = "number"
	case reflect.Slice, reflect.Array:
		node.Type = "array"
		node.Nullable = value.Kind() == reflect.Slice
		item, err := builder.node(value.Elem(), "item", true)
		if err != nil {
			return node, err
		}
		node.Items = &item
	case reflect.Map:
		if value.Key().Kind() != reflect.String {
			return node, fmt.Errorf("output map key for %s is not a string", name)
		}
		node.Type = "object"
		node.Nullable = true
		item, err := builder.node(value.Elem(), "value", true)
		if err != nil {
			return node, err
		}
		node.AdditionalValues = &item
	case reflect.Struct:
		node.Type = "object"
		key := outputDefinitionName(value)
		node.Ref = "#/$defs/" + key
		if existing, claimed := builder.definitionTypes[key]; claimed && existing != value {
			return node, fmt.Errorf("output definition %s names both %s and %s", key, existing, value)
		}
		if _, exists := builder.definitions[key]; !exists {
			builder.definitionTypes[key] = value
			builder.definitions[key] = outputNode{}
			fields, err := builder.structFields(value)
			if err != nil {
				return node, err
			}
			builder.definitions[key] = outputNode{Name: key, Type: "object", AlwaysPresent: true, Description: "Closed JSON object " + key + ".", Fields: fields}
		}
	default:
		return node, fmt.Errorf("unsupported output type %s for %s", value, name)
	}
	if value == reflect.TypeFor[mail.ExcerptSource]() {
		node.Enum = []string{"local", "imap-partial", "unavailable"}
	}
	return node, nil
}

func (builder *outputSchemaBuilder) structFields(value reflect.Type) ([]outputNode, error) {
	fields := []outputNode{}
	for index := 0; index < value.NumField(); index++ {
		field := value.Field(index)
		tag := field.Tag.Get("json")
		if field.PkgPath != "" || tag == "-" {
			continue
		}
		if field.Anonymous && tag == "" {
			nested, err := builder.structFields(field.Type)
			if err != nil {
				return nil, err
			}
			fields = append(fields, nested...)
			continue
		}
		name, options, _ := strings.Cut(tag, ",")
		if name == "" {
			name = field.Name
		}
		fieldType := outputFieldType(value, name, field.Type)
		node, err := builder.node(fieldType, name, !strings.Contains(options, "omitempty"))
		if err != nil {
			return nil, err
		}
		if strings.Contains(options, "omitempty") {
			node.Nullable = outputNullableContainer(fieldType)
		}
		if value == reflect.TypeFor[mail.MessageSummary]() && (name == "in_reply_to" || name == "references") {
			node.Nullable = false
		}
		if value == reflect.TypeFor[capabilityLimits]() || value == reflect.TypeFor[outputProjectionCapability]() {
			node.AlwaysPresent = false
		}
		fields = append(fields, node)
	}
	return fields, nil
}

func outputFieldType(owner reflect.Type, name string, original reflect.Type) reflect.Type {
	if owner == reflect.TypeFor[commandCapability]() && name == "schema" {
		return reflect.TypeFor[*outputCommandParameters]()
	}
	if owner == reflect.TypeFor[responseData]() {
		switch name {
		case "message":
			return reflect.TypeFor[*messageProjection]()
		case "draft":
			return reflect.TypeFor[*draftProjection]()
		case "attachments":
			return reflect.TypeFor[*[]attachmentProjection]()
		case "drafts":
			return reflect.TypeFor[*[]draftListEntryProjection]()
		case "batch_result":
			return reflect.TypeFor[*batchResultProjection]()
		}
	}
	return original
}

func outputSchemaForCommand(command string) outputNode {
	node := outputNode{Name: "data", Type: "object", AlwaysPresent: true, Description: outputDescription("data"), Ref: "#/$defs/data." + command}
	if target, ok := projectionTargetForCommand(command); ok {
		fields := map[string][]string{}
		for _, field := range projectionFieldNames(target) {
			selection := map[string]struct{}{field: {}}
			fields[field] = projectionFields(target, outputOptions{target: target, fields: selection, fieldsProvided: true}, false)
		}
		views := map[string][]string{}
		for _, view := range []string{outputViewMetadata, outputViewPlain, outputViewFull} {
			if validProjectionView(target, view) {
				views[view] = projectionFields(target, outputOptions{target: target, view: view}, false)
			}
		}
		node.Variants = []outputVariant{{When: "ok=true; --fields overrides the default view; mandatory state evidence and excerpt peers remain", Fields: fields, Views: views}}
	}
	node.Variants = append(node.Variants, outputVariant{When: "ok=true", OneOfRequired: outputSuccessFields(command)})
	return node
}

func outputSuccessFields(command string) [][]string {
	switch command {
	case "messages.get", "drafts.open":
		return [][]string{{"message"}}
	case "messages.raw":
		return [][]string{{"raw_source"}, {"content_export"}}
	case "messages.search", "messages.list", "messages.filter":
		return [][]string{{"page"}}
	case "accounts.list":
		return [][]string{{"accounts", "page", "complete", "identity_coverage_complete"}}
	case "mailboxes.list":
		return [][]string{{"mailboxes", "page"}}
	case "mailboxes.resolve":
		return [][]string{{"mailbox"}}
	case "drafts.list":
		return [][]string{{"drafts", "page"}}
	case "attachments.list":
		return [][]string{{"attachments", "page", "content_source", "content_complete", "missing_parts"}}
	case "capabilities":
		return [][]string{{"capabilities"}}
	case "version":
		return [][]string{{"name", "version"}}
	case "doctor":
		return [][]string{{"checks"}}
	case "drafts.reconcile":
		return [][]string{{"send_result"}, {"saved_draft"}}
	case "drafts.send":
		return [][]string{{"send_result"}}
	case "sync":
		return [][]string{{"sync_result"}, {"sync_check"}}
	case "messages.mark", "messages.move", "messages.copy":
		return [][]string{{"message_state"}}
	case "drafts.create", "drafts.edit", "drafts.update", "drafts.adopt", "drafts.inspect", "messages.reply", "messages.forward":
		return [][]string{{"draft"}}
	case "drafts.discard":
		return nil
	default:
		owned := commandDataFields[command]
		if len(owned) > 0 {
			return [][]string{{owned[0]}}
		}
		return nil
	}
}

func (builder *outputSchemaBuilder) commandData(command string) error {
	allowed := slices.Clone(commandDataFields[command])
	allowed = append(allowed, commandDataFields[envelopeLayerOwner]...)
	if _, ok := projectionTargetForCommand(command); ok || command == "batch" {
		allowed = append(allowed, "projection")
	}
	value := reflect.TypeFor[responseData]()
	fields := []outputNode{}
	for index := 0; index < value.NumField(); index++ {
		field := value.Field(index)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if !slices.Contains(allowed, name) {
			continue
		}
		fieldType := outputFieldType(value, name, field.Type)
		if name == "page" {
			fieldType = outputPageType(command)
		}
		node, err := builder.node(fieldType, name, false)
		if err != nil {
			return err
		}
		node.Nullable = outputNullableContainer(fieldType)
		fields = append(fields, node)
	}
	builder.definitions["data."+command] = outputNode{Name: "data", Type: "object", AlwaysPresent: true, Description: outputDescription("data"), Fields: fields}
	return nil
}

func outputPageType(command string) reflect.Type {
	switch command {
	case "messages.list":
		return reflect.TypeFor[*messageListPageProjection]()
	case "messages.search", "messages.filter":
		return reflect.TypeFor[*searchPageProjection]()
	case "drafts.list":
		return reflect.TypeFor[*mail.DraftPagination]()
	default:
		return reflect.TypeFor[*catalogPageMetadata]()
	}
}

func outputNullableContainer(value reflect.Type) bool {
	if value.Kind() != reflect.Pointer {
		return false
	}
	for value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	return value.Kind() == reflect.Slice || value.Kind() == reflect.Map
}

func publishOutputDefinitions(manifest *capabilityManifest) error {
	builder := outputSchemaBuilder{definitions: map[string]outputNode{}, definitionTypes: map[string]reflect.Type{}}
	if !schemasIncludeOutput(manifest.Commands) {
		return nil
	}
	for _, command := range manifest.Commands {
		if err := builder.commandData(command.ID); err != nil {
			return err
		}
	}
	errorNode, err := builder.node(reflect.TypeFor[errorData](), "error", true)
	if err != nil {
		return err
	}
	builder.definitions["error"] = errorNode
	envelopeNode := outputNode{Name: "envelope", Type: "object", AlwaysPresent: true, Description: "Shared envelope schema 1; resolve data against the selected command output and error against #/$defs/error."}
	for _, field := range []struct {
		name, kind string
		nullable   bool
	}{{"schema_version", "integer", false}, {"ok", "boolean", false}, {"command", "string", false}, {"data", "object", false}, {"error", "object", true}} {
		envelopeNode.Fields = append(envelopeNode.Fields, outputNode{Name: field.name, Type: field.kind, AlwaysPresent: true, Nullable: field.nullable, Description: outputDescription(field.name)})
	}
	next, err := builder.node(reflect.TypeFor[nextAction](), "next", false)
	if err != nil {
		return err
	}
	envelopeNode.Fields = append(envelopeNode.Fields, next)
	builder.definitions["envelope"] = envelopeNode
	manifest.OutputDefinitions = builder.definitions
	return nil
}
