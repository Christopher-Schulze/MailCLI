package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

func TestFinalizationPreservesGeneratedProjectionData(t *testing.T) {
	message := projectionMessage()
	message.Summary.ThreadingRequested, message.Summary.ExcerptRequested = true, true
	message.Summary.InReplyTo = []string{}
	message.Summary.References = []string{"parent@example.com"}
	draft := mail.Draft{Ref: "draft_ref", Body: "plain body", BodySource: "private source", BodyHTML: "<p>private HTML</p>"}
	fields, err := parseProjectionFields(projectionTargetMessage, "header_fields,excerpt")
	if err != nil {
		t.Fatal(err)
	}
	messageOptions := outputOptions{target: projectionTargetMessage, view: "custom", fields: fields, fieldsProvided: true, omitUnselectedMessageState: true}
	draftFields, err := parseProjectionFields(projectionTargetDraft, "ref,send_blockers")
	if err != nil {
		t.Fatal(err)
	}
	batchFields := []string{"header_fields", "excerpt"}
	batch := mail.BatchResult{Operation: mail.BatchOperationRead, Total: 1, Completed: 1, Items: []mail.BatchItemResult{{ID: "read", State: mail.BatchItemCompleted, Message: &message}}}
	batchProjection, err := projectBatchResult(batch, []mail.BatchItem{{ID: "read", Ref: "msg_ref", Fields: &batchFields}}, defaultJSONOutputBytes, true, true, true, true)
	if err != nil {
		t.Fatal(err)
	}
	pageFields := map[string]struct{}{"subject": {}}
	entries := []draftListEntry{{DraftSummary: mail.DraftSummary{Ref: "draft_ref", Subject: "subject"}}}
	for _, test := range []struct {
		name, command string
		data          responseData
	}{
		{"message custom", "messages.get", dataForProjection(responseData{Message: &message}, messageOptions, false)},
		{"message full", "messages.get", dataForProjection(responseData{Message: &message}, outputOptions{target: projectionTargetMessage, view: outputViewFull}, false)},
		{"draft custom", "drafts.inspect", dataForProjection(responseData{Draft: &draft}, outputOptions{target: projectionTargetDraft, view: "custom", fields: draftFields, fieldsProvided: true}, false)},
		{"draft plain", "drafts.inspect", dataForProjection(responseData{Draft: &draft}, outputOptions{target: projectionTargetDraft, view: outputViewPlain}, false)},
		{"batch", "batch", responseData{BatchResult: &batch, serialization: &serializedProjection{batch: batchProjection}}},
		{"message list", "messages.list", responseData{Page: projectMessageListPage(mail.MessagePage{Messages: []mail.MessageSummary{message.Summary}}, pageFields)}},
		{"draft list", "drafts.list", dataForProjection(responseData{Drafts: &entries}, outputOptions{target: projectionTargetDraftList, fieldsProvided: true, fields: map[string]struct{}{"age_days": {}}}, false)},
	} {
		for _, failed := range []bool{false, true} {
			t.Run(test.name+map[bool]string{false: "/success", true: "/existing failure"}[failed], func(t *testing.T) {
				original := envelope{SchemaVersion: schemaVersion, Command: test.command, OK: !failed, Data: test.data}
				code := 0
				if failed {
					original.Error = newErrorData(test.command, test.data, &transport.SubmissionError{Stage: "final reply"})
					code = 1
				}
				payload, err := marshalEnvelope(original)
				if err != nil {
					t.Fatal(err)
				}
				var output bytes.Buffer
				if FinalizeJSON(&output, []string{"--json"}, payload, code, errors.New("cleanup failed")) != 1 {
					t.Fatal("cleanup did not fail")
				}
				var before, after struct {
					Data map[string]json.RawMessage `json:"data"`
				}
				if err := json.Unmarshal(payload, &before); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(output.Bytes(), &after); err != nil {
					t.Fatal(err)
				}
				if len(after.Data) != len(before.Data)+1 || after.Data["finalization"] == nil {
					t.Fatalf("finalization changed projection keys: before=%s after=%s", payload, &output)
				}
				for key, expected := range before.Data {
					var want, got any
					if err := json.Unmarshal(expected, &want); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(after.Data[key], &got); err != nil {
						t.Fatalf("lost data.%s: %v", key, err)
					}
					if !reflect.DeepEqual(want, got) {
						t.Errorf("data.%s changed: before=%s after=%s", key, expected, after.Data[key])
					}
				}
				var response envelope
				if err := json.Unmarshal(output.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if response.OK || response.Error == nil || response.Next == nil || response.Next.Do != "check_state" || response.Data.Finalization == nil || response.Data.Finalization.State != "failed" {
					t.Fatalf("cleanup envelope: %s", &output)
				}
				if failed && !reflect.DeepEqual(response.Error, original.Error) {
					t.Fatalf("original error changed: got=%+v want=%+v", response.Error, original.Error)
				}
			})
		}
	}
}
