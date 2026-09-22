package cli

import (
	"reflect"
	"strings"
	"testing"
)

func responseDataJSONTags() []string {
	value := reflect.TypeOf(responseData{})
	tags := make([]string, 0, value.NumField())
	for index := 0; index < value.NumField(); index++ {
		field := value.Field(index)
		tag := field.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" || name == "" {
			continue
		}
		tags = append(tags, name)
	}
	return tags
}

func TestResponseDataFieldOrderPinned(t *testing.T) {
	want := []string{
		"name", "version", "capabilities", "checks", "timings",
		"accounts", "complete", "identity_coverage_complete", "mailboxes",
		"mailbox", "page", "message", "message_state", "state", "thread",
		"raw_source", "attachments", "projection", "content_export",
		"content_source", "content_complete", "missing_parts",
		"saved_attachment", "draft", "draft_preview", "draft_handoff",
		"handoff_reconcile", "drafts", "prune", "saved_draft", "send_result",
		"send_receipt", "send_setup", "delete_result", "sync_result",
		"sync_check", "batch_result", "store_profile", "finalization",
		"update_result",
	}
	got := responseDataJSONTags()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("responseData field order = %v, want %v", got, want)
	}
}

func TestCommandDataFieldsCoversEveryJSONField(t *testing.T) {
	owned := map[string][]string{}
	for owner, fields := range commandDataFields {
		for _, field := range fields {
			owned[field] = append(owned[field], owner)
		}
	}
	for _, tag := range responseDataJSONTags() {
		if len(owned[tag]) == 0 {
			t.Fatalf("responseData field %q has no owner in commandDataFields", tag)
		}
	}
	for field := range owned {
		found := false
		for _, tag := range responseDataJSONTags() {
			if tag == field {
				found = true
			}
		}
		if !found {
			t.Fatalf("commandDataFields lists %q, not a responseData JSON field", field)
		}
	}
}

func TestCommandDataFieldsOwnersAreCommands(t *testing.T) {
	contractIDs := map[string]bool{}
	for _, contract := range commandContracts {
		contractIDs[contract.ID] = true
	}
	for owner := range commandDataFields {
		if owner == envelopeLayerOwner || owner == projectionLayerOwner {
			continue
		}
		if !contractIDs[owner] {
			t.Fatalf("commandDataFields owner %q is not a command contract", owner)
		}
	}
	for _, contract := range commandContracts {
		if _, documented := commandDataFields[contract.ID]; !documented {
			t.Fatalf("command %q missing from commandDataFields", contract.ID)
		}
	}
}
