package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestCapabilityLimitReferencesMatchSelectedWireKeys(t *testing.T) {
	full := mustCapabilities(t)
	fullPayload, err := json.Marshal(full.Limits)
	if err != nil {
		t.Fatal(err)
	}
	fullKeys := flattenedLimitValues(t, fullPayload)
	for _, command := range full.Commands {
		t.Run(command.ID, func(t *testing.T) {
			code, output, response := captureCapabilitiesJSON(t, "--for", command.ID, "--json")
			if code != 0 || !response.OK || response.Data.Capabilities == nil || len(response.Data.Capabilities.Commands) != 1 {
				t.Fatalf("code=%d output=%s", code, output)
			}
			selected := response.Data.Capabilities.Commands[0]
			selected = resolveCapabilitySchema(t, selected)
			if !reflect.DeepEqual(selected, command) {
				t.Fatalf("selected contract changed: %+v vs %+v", selected, command)
			}
			var envelope struct {
				Data struct {
					Capabilities struct {
						Limits json.RawMessage `json:"limits"`
					} `json:"capabilities"`
				} `json:"data"`
			}
			if err := json.Unmarshal(output, &envelope); err != nil {
				t.Fatal(err)
			}
			keys := flattenedLimitValues(t, envelope.Data.Capabilities.Limits)
			if len(keys) != len(command.LimitRefs) {
				t.Fatalf("keys=%v refs=%v", keys, command.LimitRefs)
			}
			seen := map[string]bool{}
			for _, ref := range command.LimitRefs {
				if seen[ref] {
					t.Fatalf("duplicate limit ref %s", ref)
				}
				seen[ref] = true
				want, exists := fullKeys[ref]
				if !exists || !bytes.Equal(keys[ref], want) {
					t.Fatalf("limit %s changed or missing: got=%s want=%s", ref, keys[ref], want)
				}
			}
		})
	}
}

func flattenedLimitValues(t *testing.T, payload []byte) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	result := map[string]json.RawMessage{}
	for name, value := range fields {
		if name != "output_projection" {
			result[name] = value
			continue
		}
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(value, &nested); err != nil {
			t.Fatal(err)
		}
		for child, value := range nested {
			result[name+"."+child] = value
		}
	}
	return result
}

func TestCapabilityLimitsPreserveFalseAndRejectUnknownReferences(t *testing.T) {
	for _, test := range []struct {
		name      string
		refs      []string
		wantError bool
	}{
		{name: "false value", refs: []string{"compose_write"}},
		{name: "unknown parent", refs: []string{"missing"}, wantError: true},
		{name: "unknown child", refs: []string{"output_projection.missing"}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(capabilityLimits{selectedRefs: test.refs})
			if (err != nil) != test.wantError {
				t.Fatalf("payload=%s err=%v", payload, err)
			}
			if !test.wantError && string(payload) != `{"compose_write":false}` {
				t.Fatalf("false capability lost: %s", payload)
			}
		})
	}
}

func TestCapabilityForWildcardsOrderingAndSelectorFailures(t *testing.T) {
	full := mustCapabilities(t)
	code, first, response := captureCapabilitiesJSON(t, "--for", "drafts.*,messages.search", "--json")
	if code != 0 || !response.OK || response.Data.Capabilities == nil {
		t.Fatalf("output=%s", first)
	}
	want := []string{}
	for _, command := range full.Commands {
		if command.ID == "messages.search" || strings.HasPrefix(command.ID, "drafts.") {
			want = append(want, command.ID)
		}
	}
	got := []string{}
	for _, command := range response.Data.Capabilities.Commands {
		got = append(got, command.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
	code, second, _ := captureCapabilitiesJSON(t, "--for", "messages.search,drafts.*", "--json")
	if code != 0 || !bytes.Equal(first, second) {
		t.Fatal("selection order changed serialized result")
	}
	for _, args := range [][]string{
		{"--for", ""}, {"--for", "messages.*,messages.get"}, {"--for", "drafts.*,drafts.*"},
		{"--for", "missing.*"}, {"--for", "*"}, {"--for", ".*"}, {"--for", "batch.*"},
		{"--for", "messages.get", "--for", "messages.search"}, {"--for", "messages.get", "--limits"},
		{"--command", "messages.get"}, {"--commands", "messages.get"}, {"--family", "messages"}, {"--scope", "messages"},
	} {
		args = append(args, "--json")
		code, output, response := captureCapabilitiesJSON(t, args...)
		if code != 2 || response.OK || response.Error == nil || response.Error.Code != "invalid_argument" {
			t.Fatalf("args=%v code=%d output=%s", args, code, output)
		}
	}
}

func TestCapabilityLimitsSelectorReturnsCompleteLimitsOnly(t *testing.T) {
	code, output, response := captureCapabilitiesJSON(t, "--limits", "--json")
	if code != 0 || !response.OK || response.Data.Capabilities == nil || response.Data.Capabilities.Commands == nil || len(response.Data.Capabilities.Commands) != 0 {
		t.Fatalf("code=%d output=%s", code, output)
	}
	if !reflect.DeepEqual(response.Data.Capabilities.Limits, mustCapabilities(t).Limits) {
		t.Fatal("limits selector dropped full limits")
	}
}

func TestCapabilityScopedListIncludesOnlyNeededLimits(t *testing.T) {
	manifest, err := capabilitiesForCommands([]string{"messages.list"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(manifest.Limits)
	if err != nil {
		t.Fatal(err)
	}
	keys := flattenedLimitValues(t, payload)
	want := []string{"maximum_page_size", "output_projection.default_json_bytes", "output_projection.maximum_json_bytes", "output_projection.fields_flag", "output_projection.max_bytes_flag", "output_projection.list_page_fields"}
	if len(keys) != len(want) {
		t.Fatalf("irrelevant keys: %v", keys)
	}
	for _, key := range want {
		if _, exists := keys[key]; !exists {
			t.Fatalf("missing %s", key)
		}
	}
}
