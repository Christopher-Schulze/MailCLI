package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

func TestTypedResponseSerializationPreservesProjectedValues(t *testing.T) {
	message := projectionMessage()
	message.Content = "<>&\x00\xff\u2028"
	draft := mail.Draft{Ref: "draft_ref", Body: message.Content, BodySource: "source", BodyHTML: "<p>body</p>"}
	raw := message.Content
	attachments := []mail.Attachment{{ID: "1", Name: "file.txt"}}
	input := responseData{Message: &message, Draft: &draft, RawSource: &raw, Attachments: &attachments}
	for _, target := range []projectionTarget{"", projectionTargetMessage, projectionTargetDraft, projectionTargetRaw, projectionTargetAttachment} {
		for _, view := range []string{outputViewMetadata, outputViewPlain, outputViewFull, "empty-fields"} {
			t.Run(string(target)+"/"+view, func(t *testing.T) {
				options := outputOptions{target: target, view: view, fieldsProvided: view == "empty-fields"}
				data := dataForProjection(input, options, false)
				got, err := data.MarshalJSON()
				if err != nil {
					t.Fatal(err)
				}
				want, err := marshalResponseDataBaseline(data)
				if err != nil {
					t.Fatal(err)
				}
				var actual, expected map[string]json.RawMessage
				if err := json.Unmarshal(got, &actual); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(want, &expected); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(actual, expected) {
					t.Fatalf("serialization changed values: got %s, want %s", got, want)
				}
			})
		}
	}
}

func TestOversizedResponseRetainsExactSizeAndRecovery(t *testing.T) {
	raw := strings.Repeat("<>&\n", 4096)
	data := responseData{RawSource: &raw}
	options := outputOptions{target: projectionTargetRaw, view: outputViewFull, maxBytes: 512}
	full, err := marshalEnvelope(envelope{SchemaVersion: schemaVersion, OK: true, Command: "messages.raw", Data: dataForProjection(data, options, false)})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if code := writeProjectedSuccess(&output, "messages.raw", data, options); code != 1 {
		t.Fatalf("oversized output exit=%d", code)
	}
	var result envelope
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Error == nil || result.Error.Code != "output_too_large" || result.Data.RawSource != nil ||
		!strings.Contains(result.Error.Message, fmt.Sprintf("%d bytes", len(full))) {
		t.Fatalf("oversized response lost size/error contract: %s", &output)
	}
}

func TestFinalizationRetainsTypedValidation(t *testing.T) {
	for _, payload := range []string{
		`{"schema_version":1,"data":{"raw_source":42}}`,
		`{"schema_version":1,"ok":"yes"}`,
		`{"schema_version":1,"data":{"message":{"content_complete":"yes"}}}`,
		`{"schema_version":1} {}`,
	} {
		for _, cleanupErr := range []error{nil, errors.New("cleanup failed")} {
			var output bytes.Buffer
			if code := FinalizeJSON(&output, []string{"messages", "raw", "--json"}, []byte(payload), 0, cleanupErr); code != 1 {
				t.Fatalf("invalid execution output accepted: %s", payload)
			}
			var result envelope
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.OK || result.Error == nil {
				t.Fatalf("invalid failure output: %s", &output)
			}
		}
	}
}

// This frozen pre-optimization serializer is a differential oracle for typed
// projection fields, omission rules and escaping, not a production fallback.
func marshalResponseDataBaseline(data responseData) ([]byte, error) {
	type responseDataAlias responseData
	copy := data
	copy.Message, copy.Draft, copy.RawSource, copy.Attachments = nil, nil, nil, nil
	var messageRaw, draftRaw, rawSourceRaw, attachmentsRaw json.RawMessage
	if data.Message != nil {
		var encoded []byte
		var err error
		if data.serialization != nil && data.serialization.message != nil {
			encoded, err = json.Marshal(data.serialization.message)
		} else {
			encoded, err = json.Marshal(data.Message)
		}
		if err != nil {
			return nil, err
		}
		messageRaw = encoded
	}
	if data.Draft != nil {
		var encoded []byte
		var err error
		if data.serialization != nil && data.serialization.draft != nil {
			encoded, err = json.Marshal(data.serialization.draft)
		} else {
			encoded, err = json.Marshal(data.Draft)
		}
		if err != nil {
			return nil, err
		}
		draftRaw = encoded
	}
	if data.RawSource != nil && (data.serialization == nil || !data.serialization.hideRaw) {
		encoded, err := json.Marshal(data.RawSource)
		if err != nil {
			return nil, err
		}
		rawSourceRaw = encoded
	}
	if data.Attachments != nil {
		var encoded []byte
		var err error
		if data.serialization != nil && data.serialization.attachments != nil {
			encoded, err = json.Marshal(data.serialization.attachments)
		} else {
			encoded, err = json.Marshal(data.Attachments)
		}
		if err != nil {
			return nil, err
		}
		attachmentsRaw = encoded
	}
	return json.Marshal(struct {
		*responseDataAlias
		Message     json.RawMessage `json:"message,omitempty"`
		Draft       json.RawMessage `json:"draft,omitempty"`
		RawSource   json.RawMessage `json:"raw_source,omitempty"`
		Attachments json.RawMessage `json:"attachments,omitempty"`
	}{
		responseDataAlias: (*responseDataAlias)(&copy),
		Message:           messageRaw,
		Draft:             draftRaw,
		RawSource:         rawSourceRaw,
		Attachments:       attachmentsRaw,
	})
}
