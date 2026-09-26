package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
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

func TestProjectionSerializedKeysMatchEveryRegistry(t *testing.T) {
	for _, target := range []projectionTarget{
		projectionTargetMessage, projectionTargetDraft, projectionTargetAttachment,
		projectionTargetRaw, projectionTargetDraftList, projectionTargetListPage, projectionTargetSearchPage,
	} {
		t.Run(string(target), func(t *testing.T) {
			registry := projectionRegistry(target)
			for _, selected := range projectionFieldNames(target) {
				fields, err := parseProjectionFields(target, selected)
				if err != nil {
					t.Fatal(err)
				}
				actual := serializedRegistryFixture(t, target, fields)
				want := slices.Clone(registry.core)
				if selected == "all" {
					want = append(want, registry.optional...)
				} else if !slices.Contains(want, selected) {
					want = append(want, selected)
				}
				// A healthy draft summary never invents a corrupt-state diagnostic.
				if target == projectionTargetDraftList {
					want = slices.DeleteFunc(want, func(field string) bool { return field == "state_error" })
				}
				sort.Strings(want)
				keys := make([]string, 0, len(actual))
				for key := range actual {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				if !slices.Equal(keys, want) {
					t.Fatalf("%s fields=%q keys=%v want=%v", target, selected, keys, want)
				}
			}
		})
	}
}

func TestDefaultProjectionKeysPreserveTargetPolicies(t *testing.T) {
	for _, target := range []projectionTarget{
		projectionTargetMessage, projectionTargetDraft, projectionTargetAttachment,
		projectionTargetRaw, projectionTargetDraftList, projectionTargetListPage, projectionTargetSearchPage,
	} {
		t.Run(string(target), func(t *testing.T) {
			registry := projectionRegistry(target)
			want := append(slices.Clone(registry.core), registry.optional...)
			want = slices.DeleteFunc(want, func(field string) bool {
				return target == projectionTargetMessage && (field == "content" || field == "headers") ||
					target == projectionTargetDraft && (field == "body_source" || field == "body_html") ||
					target == projectionTargetDraftList && field == "state_error"
			})
			actual := serializedRegistryFixture(t, target, nil)
			keys := make([]string, 0, len(actual))
			for key := range actual {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			sort.Strings(want)
			if !slices.Equal(keys, want) {
				t.Fatalf("default %s keys=%v want=%v", target, keys, want)
			}
		})
	}
}

func TestDraftListRegistryKeepsCorruptStateEvidence(t *testing.T) {
	for _, field := range []string{"all", "age_days", "ref"} {
		fields, err := parseProjectionFields(projectionTargetDraftList, field)
		if err != nil {
			t.Fatal(err)
		}
		entry := draftListEntry{DraftSummary: mail.DraftSummary{Ref: "retained", StateError: "invalid retained state"}}
		encoded, err := json.Marshal(draftListEntryProjectionFor(entry, outputOptions{fields: fields, fieldsProvided: true}))
		if err != nil {
			t.Fatal(err)
		}
		var actual map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &actual); err != nil {
			t.Fatal(err)
		}
		if len(actual) != 2 || string(actual["ref"]) != `"retained"` || string(actual["state_error"]) != `"invalid retained state"` {
			t.Fatalf("corrupt summary lost bounded evidence: %s", encoded)
		}
	}
}

func serializedRegistryFixture(t *testing.T, target projectionTarget, fields map[string]struct{}) map[string]json.RawMessage {
	t.Helper()
	message := projectionMessage()
	message.Hydration = &mail.HydrationDiagnostic{State: mail.HydrationStateFailed}
	message.Summary.MailboxRef, message.Summary.DateReceived, message.Summary.DateSent = "mailbox", "received", "sent"
	message.Summary.ConversationID, message.Summary.StalenessNote = 7, "retained metadata"
	message.Summary.ServerTruth = &mail.ServerMutationEvidence{Command: "STORE", UID: 3, UIDValidity: 5}
	if target == projectionTargetListPage || target == projectionTargetSearchPage {
		return serializedPageRegistryFixture(t, target, message.Summary, fields)
	}
	encoded, err := encodedRegistryFixture(target, message, registryFixtureOptions(target, fields))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	delete(result, "projection")
	return result
}

func registryFixtureOptions(target projectionTarget, fields map[string]struct{}) outputOptions {
	options := outputOptions{target: target, view: "custom", fields: fields, fieldsProvided: fields != nil}
	if fields == nil {
		options.view = outputViewFull
		if target == projectionTargetMessage || target == projectionTargetAttachment {
			options.view = outputViewMetadata
		}
		if target == projectionTargetDraft {
			options.view = outputViewPlain
		}
	}
	return options
}

func encodedRegistryFixture(target projectionTarget, message mail.Message, options outputOptions) ([]byte, error) {
	switch target {
	case projectionTargetMessage:
		return json.Marshal(messageProjectionFor(message, options, false))
	case projectionTargetDraft:
		draft := mail.Draft{Ref: "draft", SendAttempt: &mail.SendAttempt{}, SaveAttempt: &mail.DraftSaveAttempt{}, HandoffAttempt: &mail.HandoffAttempt{}}
		return json.Marshal(draftProjectionFor(draft, options))
	case projectionTargetAttachment:
		mime := "text/plain"
		return json.Marshal(attachmentProjections([]mail.Attachment{{MIMEType: &mime}}, options)[0])
	case projectionTargetRaw:
		raw := "raw bytes"
		data := projectedRawData(responseData{RawSource: &raw}, options, false)
		return data.MarshalJSON()
	case projectionTargetDraftList:
		entry := draftListEntry{DraftSummary: mail.DraftSummary{
			Ref: "draft", AccountRef: "account", Subject: "subject", From: "sender", SendAttempt: &mail.DraftSendAttemptSummary{},
			SaveAttempt: &mail.DraftSaveAttemptSummary{}, HandoffAttempt: &mail.DraftHandoffAttemptSummary{},
		}}
		if !options.fieldsProvided {
			return json.Marshal(entry)
		}
		return json.Marshal(draftListEntryProjectionFor(entry, options))
	default:
		return nil, fmt.Errorf("uncovered projection target %q", target)
	}
}

func serializedPageRegistryFixture(t *testing.T, target projectionTarget, summary mail.MessageSummary, fields map[string]struct{}) map[string]json.RawMessage {
	t.Helper()
	if fields == nil {
		fields = map[string]struct{}{"all": {}}
	}
	if target == projectionTargetListPage {
		page := projectMessageListPage(mail.MessagePage{Messages: []mail.MessageSummary{summary}}, fields)
		var value struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		}
		if err := json.Unmarshal(*page, &value); err != nil {
			t.Fatal(err)
		}
		return value.Messages[0]
	}
	page := projectSearchPage(mail.SearchPage{Messages: []mail.SearchMessage{{Summary: summary, Snippet: "snippet"}}}, fields)
	var value struct {
		Messages []struct {
			Summary map[string]json.RawMessage `json:"summary"`
			Snippet json.RawMessage            `json:"snippet"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(*page, &value); err != nil {
		t.Fatal(err)
	}
	result := value.Messages[0].Summary
	if value.Messages[0].Snippet != nil {
		result["snippet"] = value.Messages[0].Snippet
	}
	return result
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
