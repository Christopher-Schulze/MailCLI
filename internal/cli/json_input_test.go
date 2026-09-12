package cli

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

func TestInputJSONFieldSetsMatchModelsAndRejectEveryAmbiguousKey(t *testing.T) {
	for _, test := range []struct {
		shape inputJSONShape
		model reflect.Type
	}{
		{inputJSONDraft, reflect.TypeFor[mail.DraftInput]()},
		{inputJSONRecipient, reflect.TypeFor[mail.Recipient]()},
		{inputJSONBatch, reflect.TypeFor[mail.BatchRequest]()},
		{inputJSONBatchItem, reflect.TypeFor[mail.BatchItem]()},
	} {
		t.Run(test.model.Name(), func(t *testing.T) {
			fields := inputJSONFields(test.shape)
			count := 0
			for index := 0; index < test.model.NumField(); index++ {
				field := test.model.Field(index)
				name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
				if name == "-" {
					continue
				}
				count++
				declared, err := findInputJSONField(fields, name, "$", 0)
				if err != nil || declared.shape != jsonInputShapeForType(t, field.Type) {
					t.Fatalf("field %s schema=%+v model=%s error=%v", name, declared, field.Type, err)
				}
				value, err := json.Marshal(reflect.Zero(field.Type).Interface())
				if err != nil {
					t.Fatal(err)
				}
				if field.Type.Kind() == reflect.Slice {
					value = []byte("[]")
				}
				if _, err := validateInputJSON([]byte(fmt.Sprintf("{%q:%s}", name, value)), test.shape); err != nil {
					t.Fatalf("valid model field %s rejected: %v", name, err)
				}
				for _, members := range []string{
					fmt.Sprintf("%q:%s,%q:%s", name, value, name, value),
					fmt.Sprintf("%q:%s", strings.ToUpper(name), value),
					fmt.Sprintf("%q:%s,%q:%s", name, value, strings.ToUpper(name), value),
					fmt.Sprintf("%q:%s,%q:%s", strings.ToUpper(name), value, name, value),
				} {
					if _, err := validateInputJSON([]byte("{"+members+"}"), test.shape); err == nil {
						t.Errorf("ambiguous key accepted: %s", members)
					}
				}
			}
			if len(fields) != count {
				t.Fatalf("schema has %d fields, model has %d", len(fields), count)
			}
		})
	}
}

func jsonInputShapeForType(t *testing.T, value reflect.Type) inputJSONShape {
	t.Helper()
	switch value {
	case reflect.TypeFor[[]mail.Recipient]():
		return inputJSONRecipients
	case reflect.TypeFor[[]mail.BatchItem]():
		return inputJSONBatchItems
	case reflect.TypeFor[[]string]():
		return inputJSONAttachments
	}
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.String:
		return inputJSONString
	case reflect.Int:
		return inputJSONNumber
	case reflect.Bool:
		return inputJSONBoolean
	default:
		t.Fatalf("unsupported model field type %s", value)
		return 0
	}
}

func TestInputJSONFieldsMatchPublishedContract(t *testing.T) {
	for _, command := range []string{"drafts.create", "drafts.update", "messages.reply", "messages.forward", "batch"} {
		schema := decodeTestCommandSchema(t, schemaForCommand(command)).JSONInput
		if schema == nil {
			t.Fatalf("missing JSON schema for %s", command)
		}
		shape := inputJSONDraft
		if command == "batch" {
			shape = inputJSONBatch
			assertInputJSONPublishedFields(t, inputJSONFields(inputJSONBatchItem), schema.ItemFields)
		}
		assertInputJSONPublishedFields(t, inputJSONFields(shape), schema.Fields)
	}
}

func assertInputJSONPublishedFields(t *testing.T, fields []inputJSONField, published []testJSONField) {
	t.Helper()
	if len(fields) != len(published) {
		t.Fatalf("parser fields=%d, published=%d", len(fields), len(published))
	}
	for _, field := range fields {
		if jsonFieldByName(published, field.name) == nil {
			t.Errorf("parser field %s missing from published schema", field.name)
		}
	}
}

func TestDraftJSONPreservesDecodedValuesAndPresence(t *testing.T) {
	for _, test := range []struct {
		name, payload string
	}{
		{"omitted fields", `{"body":""}`},
		{"explicit empty", `{"body":"","subject":"","to":[],"cc":[]}`},
		{"escaped keys", `{"bo\u0064y":"こんにちは 🌍\n\\\"{}","sub\u006aect":"ÄÖß","t\u006f":[],"\u0063c":[]}`},
		{"nested unicode", `{"body":"Text","to":[{"name":"Jörg 🌍","address":"one@example.com"},{"name":"李","address":"two@example.com"}],"cc":[{"name":"CC","address":"cc@example.com"}],"bcc":[{"name":"BCC","address":"bcc@example.com"}],"account_ref":"acct","from":"from@example.com","subject":"S","body_format":"plain","attachments":["/tmp/Ä.txt","/tmp/李.txt"]}`},
		{"previously nullable fields", `{"body":"","bcc":null,"from":null,"account_ref":null,"body_format":null,"attachments":null}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := decodeDraftInput(strings.NewReader(test.payload))
			if err != nil {
				t.Fatal(err)
			}
			var want mail.DraftInput
			if err := json.Unmarshal([]byte(test.payload), &want); err != nil {
				t.Fatal(err)
			}
			var present map[string]json.RawMessage
			if err := json.Unmarshal([]byte(test.payload), &present); err != nil {
				t.Fatal(err)
			}
			want.AccountRefSet = present["account_ref"] != nil
			want.FromSet = present["from"] != nil
			want.ToSet = present["to"] != nil
			want.CCSet = present["cc"] != nil
			want.BCCSet = present["bcc"] != nil
			want.SubjectSet = present["subject"] != nil
			want.BodySet = present["body"] != nil
			want.BodyFormatSet = present["body_format"] != nil
			want.AttachmentsSet = present["attachments"] != nil
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("decoded values differ: got=%+v want=%+v", got, want)
			}
		})
	}
}

func TestDraftJSONMalformedShapeAndValueDiagnostics(t *testing.T) {
	for _, test := range []struct{ name, payload, location string }{
		{"null root", `null`, "$"}, {"array root", `[]`, "$"},
		{"scalar root", `"PRIVATE_FIRST"`, "$"}, {"empty", " ", "$"},
		{"wrong close", `{"body":"PRIVATE_FIRST"]`, "$"},
		{"unclosed object", `{"body":"PRIVATE_FIRST"`, "$"},
		{"missing colon", `{"body" "PRIVATE_FIRST"}`, "$.body"},
		{"trailing comma", `{"body":"PRIVATE_FIRST",}`, "$"},
		{"array trailing comma", `{"body":"","to":[{"address":"a@example.com"},]}`, "$.to[1]"},
		{"deep body", `{"body":` + strings.Repeat("[", 10000), "$.body"},
		{"deep recipient", `{"body":"","to":[` + strings.Repeat("[", 10000), "$.to[0]"},
		{"body object", `{"body":{"PRIVATE_FIRST":"PRIVATE_SECOND"}}`, "$.body"},
		{"numeric body", `{"body":12345678901234567890}`, "$.body"},
		{"malformed number", `{"body":1ePRIVATE_FIRST}`, "$.body"},
		{"null body", `{"body":null}`, "$.body"},
		{"null subject", `{"body":"","subject":null}`, "$.subject"},
		{"null to", `{"body":"","to":null}`, "$.to"},
		{"null cc", `{"body":"","cc":null}`, "$.cc"},
		{"unicode fold alias", `{"body":"","ſubject":"PRIVATE_FIRST"}`, "$"},
		{"Go field alias", `{"body":"","AccountRef":"PRIVATE_FIRST"}`, "$"},
		{"derived flag injection", `{"body":"","SubjectSet":true}`, "$"},
		{"trailing scalar", `{"body":""} "PRIVATE_FIRST"`, "one JSON object"},
		{"non-JSON whitespace", "{\"body\":\"\"}\u00a0", "one JSON object"},
		{"form feed", "{\"body\":\"\"}\f", "one JSON object"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeDraftInput(strings.NewReader(test.payload))
			if err == nil || !strings.Contains(err.Error(), test.location) || strings.Contains(err.Error(), "PRIVATE_") {
				t.Fatalf("error=%v, want value-free location %q", err, test.location)
			}
		})
	}
}

func TestDraftJSONInputByteLimit(t *testing.T) {
	const object = `{"body":""}`
	for _, excess := range []int{0, 1, 1024} {
		t.Run(fmt.Sprint(excess), func(t *testing.T) {
			reader := strings.NewReader(object + strings.Repeat(" ", maximumDraftInputBytes-len(object)+excess))
			_, err := decodeDraftInput(reader)
			if excess == 0 && err != nil {
				t.Fatalf("exact byte limit rejected: %v", err)
			}
			if excess > 0 && (err == nil || !strings.Contains(err.Error(), "16 MiB")) {
				t.Fatalf("oversize error = %v", err)
			}
			if reader.Len() != max(0, excess-1) {
				t.Fatalf("reader remainder=%d, input was not bounded to limit+1", reader.Len())
			}
		})
	}
}

func FuzzInputJSONSyntax(f *testing.F) {
	for _, payload := range []string{
		`{"body":""}`, `{"body":"one","body":"two"}`, `{"body":"","Body":"alias"}`,
		`{"body":"","to":[{"address":"a@example.com"}]}`, `{"body":""} {}`,
		`{"operation":"mark","items":[{"id":"one","ref":"msg_ref","read":true}]}`,
		"{\"body\":\"\"}\u00a0", `{"body":null}`, `{"items":[null]}`, "{", "null",
	} {
		f.Add([]byte(payload))
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		for _, shape := range []inputJSONShape{inputJSONDraft, inputJSONBatch} {
			if _, err := validateInputJSON(payload, shape); err == nil && !json.Valid(payload) {
				t.Fatal("strict input parser accepted invalid JSON syntax")
			}
		}
	})
}
