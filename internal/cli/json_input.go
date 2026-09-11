package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

type inputJSONShape uint8

const (
	inputJSONString inputJSONShape = iota
	inputJSONNumber
	inputJSONBoolean
	inputJSONDraft
	inputJSONRecipient
	inputJSONBatch
	inputJSONBatchItem
	inputJSONRecipients
	inputJSONAttachments
	inputJSONBatchItems
)

type inputJSONField struct {
	name    string
	shape   inputJSONShape
	nonnull bool
}

// These are the only user-supplied CLI JSON schemas. Their acyclic shapes
// bound descent to root -> array -> object -> scalar, even for hostile input.
// Tests bind the field sets to the actual model tags and published schemas.
func inputJSONFields(shape inputJSONShape) []inputJSONField {
	switch shape {
	case inputJSONDraft:
		return []inputJSONField{
			{"account_ref", inputJSONString, false}, {"from", inputJSONString, false},
			{"to", inputJSONRecipients, true}, {"cc", inputJSONRecipients, true},
			{"bcc", inputJSONRecipients, false}, {"subject", inputJSONString, true},
			{"body", inputJSONString, true}, {"body_format", inputJSONString, false},
			{"attachments", inputJSONAttachments, false},
		}
	case inputJSONRecipient:
		return []inputJSONField{{"name", inputJSONString, false}, {"address", inputJSONString, false}}
	case inputJSONBatch:
		return []inputJSONField{
			{"operation", inputJSONString, false}, {"items", inputJSONBatchItems, false},
			{"concurrency", inputJSONNumber, false},
		}
	case inputJSONBatchItem:
		return []inputJSONField{
			{"id", inputJSONString, false}, {"ref", inputJSONString, false},
			{"attachment_id", inputJSONString, false}, {"output_path", inputJSONString, false},
			{"read", inputJSONBoolean, false}, {"flagged", inputJSONBoolean, false},
			{"junk", inputJSONBoolean, false}, {"allow_draft_mutation", inputJSONBoolean, false},
		}
	default:
		return nil
	}
}

// validateInputJSON walks the already byte-bounded payload without retaining
// values or a second object graph. Only active objects' finite key sets live
// across tokens; the returned root set preserves omission versus explicit empty.
func validateInputJSON(payload []byte, shape inputJSONShape) (map[string]bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	token, err := inputJSONToken(decoder, "$")
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, invalidDraftInput("input must be one JSON object at $")
	}
	fields, err := readInputJSONObject(decoder, shape, "$")
	if err != nil {
		return nil, err
	}
	if len(bytes.Trim(payload[decoder.InputOffset():], " \t\r\n")) != 0 {
		return nil, invalidDraftInput("input must contain exactly one JSON object")
	}
	return fields, nil
}

func readInputJSONObject(decoder *json.Decoder, shape inputJSONShape, path string) (map[string]bool, error) {
	fields := inputJSONFields(shape)
	seen := make(map[string]bool, len(fields))
	for decoder.More() {
		token, err := inputJSONToken(decoder, path)
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok {
			return nil, invalidDraftInput("expected JSON field at " + path)
		}
		field, err := findInputJSONField(fields, name, path, decoder.InputOffset())
		if err != nil {
			return nil, err
		}
		fieldPath := path + "." + field.name
		if seen[name] {
			return nil, invalidDraftInput("duplicate JSON field " + fieldPath)
		}
		seen[name] = true
		if err := readInputJSONValue(decoder, field, fieldPath); err != nil {
			return nil, err
		}
	}
	if _, err := inputJSONToken(decoder, path); err != nil {
		return nil, err
	}
	return seen, nil
}

func findInputJSONField(fields []inputJSONField, name, path string, offset int64) (inputJSONField, error) {
	for _, field := range fields {
		if field.name == name {
			return field, nil
		}
	}
	return inputJSONField{}, invalidDraftInput(fmt.Sprintf(
		"unknown JSON field %.64q at %s near byte %d; use exact schema spelling", name, path, offset))
}

func readInputJSONValue(decoder *json.Decoder, field inputJSONField, path string) error {
	token, err := inputJSONToken(decoder, path)
	if err != nil {
		return err
	}
	if token == nil {
		if field.nonnull {
			return invalidDraftInput("JSON field " + path + " must not be null")
		}
		return nil
	}
	switch field.shape {
	case inputJSONString:
		if _, ok := token.(string); ok {
			return nil
		}
	case inputJSONNumber:
		if _, ok := token.(json.Number); ok {
			return nil
		}
	case inputJSONBoolean:
		if _, ok := token.(bool); ok {
			return nil
		}
	case inputJSONRecipient, inputJSONBatchItem:
		if token == json.Delim('{') {
			_, err := readInputJSONObject(decoder, field.shape, path)
			return err
		}
	case inputJSONRecipients, inputJSONAttachments, inputJSONBatchItems:
		if token == json.Delim('[') {
			return readInputJSONArray(decoder, field.shape, path)
		}
	}
	return invalidDraftInput("invalid JSON value type at " + path)
}

func readInputJSONArray(decoder *json.Decoder, shape inputJSONShape, path string) error {
	element := inputJSONField{shape: inputJSONString}
	switch shape {
	case inputJSONRecipients:
		element.shape = inputJSONRecipient
	case inputJSONBatchItems:
		element.shape = inputJSONBatchItem
	}
	for index := 0; decoder.More(); index++ {
		if err := readInputJSONValue(decoder, element, fmt.Sprintf("%s[%d]", path, index)); err != nil {
			return err
		}
	}
	_, err := inputJSONToken(decoder, path)
	return err
}

func inputJSONToken(decoder *json.Decoder, path string) (json.Token, error) {
	token, err := decoder.Token()
	if err != nil {
		// Decoder errors can contain input values. Keep only our schema path
		// and the last consumed byte position, never the original error text.
		return nil, invalidDraftInput(fmt.Sprintf("invalid JSON at %s near byte %d", path, decoder.InputOffset()))
	}
	return token, nil
}

func inputJSONDecodeError(err error) error {
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &typeError) {
		return invalidDraftInput(fmt.Sprintf("invalid JSON value at $.%s near byte %d", typeError.Field, typeError.Offset))
	}
	return invalidDraftInput("invalid JSON value for the input schema")
}
