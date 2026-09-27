package cli

import (
	"encoding/json"
	"reflect"
	"testing"
)

func resolveCapabilitySchema(t *testing.T, command commandCapability) commandCapability {
	t.Helper()
	ref := command.SchemaRef
	if ref == nil || len(command.Schema) != 0 || len(ref.Resolve) != 6 || ref.Resolve[0] != "mailcli" || ref.Resolve[1] != "capabilities" {
		t.Fatalf("invalid schema reference: %+v", command)
	}
	code, output, response := captureCapabilitiesJSON(t, ref.Resolve[2:]...)
	if code != 0 || !response.OK || response.Data.Capabilities == nil || len(response.Data.Capabilities.Commands) != 1 {
		t.Fatalf("resolve %v: code=%d output=%s", ref.Resolve, code, output)
	}
	resolved := response.Data.Capabilities.Commands[0]
	var identity struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resolved.Schema, &identity); err != nil {
		t.Fatal(err)
	}
	if identity.ID != ref.ID || resolved.SchemaRef != nil {
		t.Fatalf("schema identity mismatch: %+v vs %s", ref, resolved.Schema)
	}
	command.SchemaRef = nil
	command.Schema = resolved.Schema
	if !reflect.DeepEqual(command, resolved) {
		t.Fatalf("schema resolution changed command metadata: %+v vs %+v", command, resolved)
	}
	return resolved
}

func TestCapabilityScopedWireBudgets(t *testing.T) {
	for _, test := range []struct {
		selector string
		maximum  int
	}{
		{"messages.list", 2000},
		{"messages.get", 2500},
		{"messages.search,messages.get,drafts.create,drafts.send", 7000},
	} {
		t.Run(test.selector, func(t *testing.T) {
			code, output, response := captureCapabilitiesJSON(t, "--for", test.selector, "--json")
			if code != 0 || !response.OK || len(output) > test.maximum {
				t.Fatalf("code=%d bytes=%d maximum=%d output=%s", code, len(output), test.maximum, output)
			}
			t.Logf("bytes=%d maximum=%d", len(output), test.maximum)
		})
	}
}

func TestCapabilitySchemasRequireSelection(t *testing.T) {
	for _, flags := range [][]string{{"--schemas", "--json"}, {"--schemas", "--limits", "--json"}} {
		code, output, response := captureCapabilitiesJSON(t, flags...)
		if code != 2 || response.OK || response.Error == nil || response.Error.Code != "invalid_argument" {
			t.Fatalf("flags=%v code=%d output=%s", flags, code, output)
		}
	}
}

func TestCapabilitySchemaReferencesRejectInvalidIdentity(t *testing.T) {
	for _, schema := range []string{`null`, `{`, `{"id":"messages.get@v1","version":2}`, `{"id":"batch@v2","version":2}`} {
		commands := []commandCapability{{ID: "messages.get", Schema: json.RawMessage(schema)}}
		if err := referenceCapabilitySchemas(commands); err == nil {
			t.Fatalf("accepted invalid schema %s", schema)
		}
		if commands[0].SchemaRef != nil || string(commands[0].Schema) != schema {
			t.Fatal("invalid schema was replaced")
		}
	}
}
