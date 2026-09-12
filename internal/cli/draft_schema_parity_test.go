package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

func TestPartialUpdateSchemaFieldParity(t *testing.T) {
	schema := decodeTestCommandSchema(t, schemaForCommand("drafts.update"))
	base := mail.DraftInput{
		AccountRef: "account", From: "old@example.com", Subject: "kept", Body: "kept body",
		BodyFormat: mail.DraftBodyPlain, To: []mail.Recipient{{Address: "to@example.com"}},
		CC: []mail.Recipient{{Address: "cc@example.com"}}, BCC: []mail.Recipient{{Address: "bcc@example.com"}},
		Attachments: []string{"/kept.txt"},
	}
	current := mail.Draft{
		AccountRef: base.AccountRef, From: base.From, Subject: base.Subject, Body: base.Body,
		BodyFormat: base.BodyFormat, To: base.To, CC: base.CC, BCC: base.BCC,
		Attachments: []mail.DraftAttachment{{Path: base.Attachments[0]}},
	}
	fields := []struct {
		name, flag, replacement, native, clear string
	}{
		{"account_ref", "--account", `"changed"`, "changed", `""`},
		{"from", "--from", `"new@example.com"`, "new@example.com", `""`},
		{"subject", "--subject", `"changed"`, "changed", `""`},
		{"body", "--body", `"changed"`, "changed", `""`},
		{"body_format", "--format", `"plain"`, "plain", `""`},
		{"to", "--to", `[{"name":"","address":"new@example.com"}]`, "new@example.com", "[]"},
		{"cc", "--cc", `[{"name":"","address":"new@example.com"}]`, "new@example.com", "[]"},
		{"bcc", "--bcc", `[{"name":"","address":"new@example.com"}]`, "new@example.com", "[]"},
		{"attachments", "--attach", `["/changed.txt"]`, "/changed.txt", "[]"},
	}
	if len(schema.JSONInput.Fields) != len(fields) {
		t.Fatal("editable schema field set changed without parity coverage")
	}
	for _, field := range fields {
		published := jsonFieldByName(schema.JSONInput.Fields, field.name)
		flag := schemaFlagsByName(schema)[field.flag]
		if published == nil || published.Required || flag.Name != field.flag || flag.Required || flag.Default != "" {
			t.Fatalf("%s publishes required or defaulted patch input", field.name)
		}
		for _, mode := range []string{"json", "flags"} {
			for _, change := range []string{"omitted", "provided", "clear"} {
				t.Run(field.name+"/"+mode+"/"+change, func(t *testing.T) {
					payload := "{}"
					args := []string{"--subject", base.Subject}
					expected := draftInputJSONFields(t, base)
					if change != "omitted" {
						value, native := field.replacement, field.native
						if change == "clear" {
							value, native = field.clear, ""
						}
						payload = `{"` + field.name + `":` + value + "}"
						args = []string{field.flag, native}
						expected[field.name] = json.RawMessage(value)
						if change == "clear" && field.name != "body" {
							delete(expected, field.name)
						}
					}
					if mode == "json" {
						path := filepath.Join(t.TempDir(), "patch.json")
						if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
							t.Fatal(err)
						}
						args = []string{"--input", path}
					}
					flags := newFlagSet("update parity", &bytes.Buffer{})
					options := registerDraftInputFlags(flags)
					input, err := mail.DraftInput{}, flags.Parse(args)
					if err == nil {
						input, err = options.readUpdate()
					}
					if mode == "flags" && change == "clear" && (field.name == "attachments" || field.name == "body_format") {
						if err == nil {
							t.Fatal("unsupported native empty value accepted")
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					merged, err := mergeDraftUpdateInput(current, input)
					if err != nil {
						t.Fatal(err)
					}
					if actual := draftInputJSONFields(t, merged); !reflect.DeepEqual(actual, expected) {
						t.Fatalf("patch differs: got %s, want %s", actual, expected)
					}
				})
			}
		}
	}
}

func draftInputJSONFields(t *testing.T, input mail.DraftInput) map[string]json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestPartialUpdateSchemaRejectionsMatchParser(t *testing.T) {
	update := decodeTestCommandSchema(t, schemaForCommand("drafts.update"))
	create := decodeTestCommandSchema(t, schemaForCommand("drafts.create"))
	if !schemaHasConstraint(update, "mutually_exclusive") || !schemaHasConstraint(create, "one_of") ||
		!schemaFlagsByName(update)["--expected-revision"].Required {
		t.Fatal("published input or revision constraints are missing")
	}
	for _, args := range [][]string{
		{"--body", "", "--body-file", "/must-not-be-opened"},
		{"--input", "/must-not-be-opened", "--subject", "changed"},
	} {
		flags := newFlagSet("invalid update", &bytes.Buffer{})
		options := registerDraftInputFlags(flags)
		if err := flags.Parse(args); err != nil {
			t.Fatal(err)
		}
		if _, err := options.readUpdate(); err == nil {
			t.Fatalf("incompatible input accepted: %v", args)
		}
	}
	for _, payload := range []string{
		`{"subject":"first","subject":"second"}`, `{"unknown":"value"}`,
		`{"expected_revision":"not-editable"}`, `{"to":null}`,
	} {
		if _, err := decodeDraftInputMode(strings.NewReader(payload), false); err == nil {
			t.Fatalf("invalid patch accepted: %s", payload)
		}
	}
	if _, err := decodeDraftInputMode(strings.NewReader("{}"), true); err == nil {
		t.Fatal("create accepted omitted body")
	}
	flags := newFlagSet("create parity", &bytes.Buffer{})
	options := registerDraftInputFlags(flags)
	if err := flags.Parse([]string{"--subject", "no body"}); err != nil {
		t.Fatal(err)
	}
	if _, err := options.read(); err == nil {
		t.Fatal("native create accepted omitted body")
	}
}
