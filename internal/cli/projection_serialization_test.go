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
	message.Content = "<>&\x00\n\xff\u2028"
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
				var actual, expected map[string]any
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

func TestJSONStringOutputBytesMatchesCLIEncoder(t *testing.T) {
	controls := make([]byte, 0x20)
	for value := byte(0); value < 0x20; value++ {
		controls[value] = value
	}
	for _, value := range []string{
		"plain text", `quote"slash\\`, "<>&", string(controls), "\u2028\u2029", "\xff", "emoji 🚀",
	} {
		encoded, err := marshalCLIJSON(value)
		if err != nil {
			t.Fatal(err)
		}
		if got := jsonStringOutputBytes(value); got != int64(len(encoded)) {
			t.Errorf("jsonStringOutputBytes(%q) = %d, encoder size = %d (%s)", value, got, len(encoded), encoded)
		}
	}
}

func TestBatchReadContentBudgetUsesExactCLIEncodingThresholds(t *testing.T) {
	content := strings.Repeat("<&>", 512) + "\x00\n\u2028"
	encoded, err := marshalCLIJSON(content)
	if err != nil {
		t.Fatal(err)
	}
	requiredBytes := int64(len(encoded))
	for _, test := range []struct {
		name         string
		limitBytes   int64
		wantExceeded bool
		wantRetained bool
	}{
		{name: "below", limitBytes: requiredBytes - 1, wantExceeded: true},
		{name: "at", limitBytes: requiredBytes, wantRetained: true},
		{name: "above", limitBytes: requiredBytes + 1, wantRetained: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			budget := newBatchReadContentBudget(test.limitBytes)
			admission := budget.admit(content, "", true, false)
			result := budget.result()
			if result.requiredBytes != requiredBytes || result.limitBytes != test.limitBytes ||
				result.exceeded != test.wantExceeded || admission.RetainContent != test.wantRetained ||
				admission.RetainHeaders {
				t.Fatalf("budget result/admission = %+v/%+v, want required=%d limit=%d exceeded=%t retained=%t",
					result, admission, requiredBytes, test.limitBytes, test.wantExceeded, test.wantRetained)
			}
			if test.wantExceeded {
				request := mail.BatchRequest{
					Operation: mail.BatchOperationRead, Concurrency: 1,
					Items: []mail.BatchItem{{ID: "body", Ref: "message-ref", RetainReadContent: true}},
				}
				batchResult := mail.BatchResult{
					Operation: mail.BatchOperationRead, Concurrency: 1, Total: 1, Completed: 1,
					Items: []mail.BatchItemResult{{ID: "body", State: mail.BatchItemCompleted}},
				}
				var output bytes.Buffer
				if code := writeBatchJSON(&output, batchResult, request, test.limitBytes, result); code != 1 ||
					int64(output.Len()) > test.limitBytes {
					t.Fatalf("bounded below-threshold overflow = code:%d bytes:%d limit:%d",
						code, output.Len(), test.limitBytes)
				}
				var response envelope
				if err := json.Unmarshal(output.Bytes(), &response); err != nil {
					t.Fatalf("decode below-threshold overflow: %v", err)
				}
				assertOutputSizeEvidence(t, response, requiredBytes, test.limitBytes, string(outputSizeLowerBound))
			}
		})
	}
}

func TestCLIJSONEncoderRemovesHTMLEscapingWithoutChangingControlSafety(t *testing.T) {
	body := strings.Repeat("<>&", 3333) + "<\x00\n"
	data := responseData{RawSource: &body}
	actual, err := marshalEnvelope(envelope{SchemaVersion: schemaVersion, OK: true, Command: "messages.raw", Data: data})
	if err != nil {
		t.Fatal(err)
	}
	legacyData, err := marshalResponseDataBaseline(data)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := json.Marshal(struct {
		SchemaVersion int             `json:"schema_version"`
		OK            bool            `json:"ok"`
		Command       string          `json:"command"`
		Data          json.RawMessage `json:"data"`
		Error         json.RawMessage `json:"error"`
	}{SchemaVersion: schemaVersion, OK: true, Command: "messages.raw", Data: legacyData, Error: json.RawMessage("null")})
	if err != nil {
		t.Fatal(err)
	}
	legacy = append(legacy, '\n')
	if difference := len(legacy) - len(actual); difference < 50_000 {
		t.Fatalf("HTML output shrank by %d bytes, want at least 50000", difference)
	}
	if !bytes.Contains(actual, []byte("<>&")) || bytes.Contains(actual, []byte{0}) ||
		!bytes.Contains(actual, []byte(`\u0000`)) || !bytes.Contains(actual, []byte(`\n`)) {
		t.Fatalf("encoded output has wrong HTML/control escaping: %s", actual[:min(160, len(actual))])
	}
	var decoded envelope
	if err := json.Unmarshal(actual, &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if decoded.Data.RawSource == nil {
		t.Fatal("JSON round-trip omitted the raw body")
	}
	if *decoded.Data.RawSource != body {
		t.Fatalf("JSON round-trip changed body: got %d bytes, want %d", len(*decoded.Data.RawSource), len(body))
	}
}

func TestPublishedSchemasUseUnescapedJSONStrings(t *testing.T) {
	for _, command := range []string{"drafts.create", "batch", "messages.list"} {
		schema := schemaForCommand(command)
		if bytes.Contains(schema, []byte(`\u003c`)) || bytes.Contains(schema, []byte(`\u003e`)) ||
			bytes.Contains(schema, []byte(`\u0026`)) {
			t.Errorf("%s schema still HTML-escapes strings: %s", command, schema)
		}
	}
	if !bytes.Contains(schemaForCommand("drafts.create"), []byte("array<recipient>")) ||
		!bytes.Contains(schemaForCommand("batch"), []byte("array<batch_item>")) {
		t.Fatal("schema normalization lost the original value-type strings")
	}
}

func TestNormalizeSchemaJSONUnescapesHTMLWithoutChangingOtherEscapes(t *testing.T) {
	source := json.RawMessage(`{"literal":"\\u003c","markup":"\u003c\u003e\u0026"}`)
	normalized, err := normalizeSchemaJSON(source)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		t.Fatalf("decode normalized schema: %v", err)
	}
	if decoded["literal"] != `\u003c` || decoded["markup"] != "<>&" {
		t.Fatalf("normalized schema strings = %+v, want literal escape preserved and HTML decoded", decoded)
	}
}

func TestJSONOutputPagesUseSharedEscapingPolicy(t *testing.T) {
	page := &mail.MessagePage{Messages: []mail.MessageSummary{{Subject: "<>&"}}}
	payload, err := marshalEnvelope(envelope{
		SchemaVersion: schemaVersion, OK: true, Command: "messages.list",
		Data: responseData{Page: messageResponsePage(page)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte("<>&")) || bytes.Contains(payload, []byte(`\u003c`)) ||
		bytes.Contains(payload, []byte(`\u003e`)) || bytes.Contains(payload, []byte(`\u0026`)) {
		t.Fatalf("page output did not use the shared HTML escaping policy: %s", payload)
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
	assertOutputSizeEvidence(t, result, int64(len(full)), options.maxBytes, string(outputSizeExact))
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
				if target == projectionTargetMessage && selected != "all" && (selected == "excerpt" || selected == "excerpt_complete" || selected == "excerpt_source") {
					for _, peer := range []string{"excerpt", "excerpt_complete", "excerpt_source"} {
						if !slices.Contains(want, peer) {
							want = append(want, peer)
						}
					}
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
				return target == projectionTargetMessage && (field == "content" || field == "headers" || field == "header_fields" || field == "excerpt" || field == "excerpt_complete" || field == "excerpt_source" ||
					field == "attachments" || field == "content_source" || field == "content_complete" || field == "missing_parts" || field == "hydration") ||
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
	for _, field := range []string{"all", "age_days", "created_at"} {
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
	if target == projectionTargetListPage {
		// Unified inbox rows include their resolved account identity.
		message.Summary.Account = "account"
	}
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

func TestMessageListProjectionPreservesAvailableAccountIdentity(t *testing.T) {
	for _, account := range []string{"", "account"} {
		for _, selector := range []string{"subject,mailbox_ref,account", "all"} {
			t.Run(account+"/"+selector, func(t *testing.T) {
				fields, err := parseProjectionFields(projectionTargetListPage, selector)
				if err != nil {
					t.Fatal(err)
				}
				actual := serializedPageRegistryFixture(t, projectionTargetListPage, mail.MessageSummary{
					Ref: "message", MailboxRef: "mailbox", Account: account, Subject: "subject",
				}, fields)
				value, present := actual["account"]
				if present != (account != "") || present && string(value) != `"account"` ||
					string(actual["ref"]) != `"message"` || string(actual["mailbox_ref"]) != `"mailbox"` {
					t.Fatalf("account=%q selector=%s projection=%v", account, selector, actual)
				}
			})
		}
	}
}

func registryFixtureOptions(target projectionTarget, fields map[string]struct{}) outputOptions {
	options := outputOptions{target: target, view: "custom", fields: fields, fieldsProvided: fields != nil}
	if fields == nil {
		options.view = outputViewFull
		if target == projectionTargetMessage || target == projectionTargetAttachment {
			options.view = outputViewMetadata
		}
		// messages get omits the unselected state fields in the metadata view.
		options.omitUnselectedMessageState = target == projectionTargetMessage
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
	if _, all := fields["all"]; all {
		// The complete page of a request that asked for both enrichments.
		summary.ThreadingRequested, summary.ExcerptRequested = true, true
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
// projection fields and omission rules, not the production escaping policy.
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
