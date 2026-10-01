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

func TestJSONInputRejectsInvalidUnicodeBeforeDecoding(t *testing.T) {
	for _, test := range []struct {
		name, payload string
		shape         inputJSONShape
	}{
		{"invalid UTF-8 body", "{\"body\":\"PRIVATE_\xff\"}", inputJSONDraft},
		{"overlong UTF-8", "{\"body\":\"\xc0\xaf\"}", inputJSONDraft},
		{"UTF-8 surrogate", "{\"body\":\"\xed\xa0\x80\"}", inputJSONDraft},
		{"high body", `{"body":"PRIVATE_\ud800"}`, inputJSONDraft},
		{"low body", `{"body":"PRIVATE_\udfff"}`, inputJSONDraft},
		{"high then BMP", `{"body":"PRIVATE_\ud800\u0041"}`, inputJSONDraft},
		{"high then high", `{"body":"PRIVATE_\ud800\udbff"}`, inputJSONDraft},
		{"reversed pair", `{"body":"PRIVATE_\udc00\ud800"}`, inputJSONDraft},
		{"split pair", `{"body":"PRIVATE_\ud800 \udc00"}`, inputJSONDraft},
		{"escaped low continuation", `{"body":"PRIVATE_\ud800\\udc00"}`, inputJSONDraft},
		{"subject", `{"body":"","subject":"PRIVATE_\ud800"}`, inputJSONDraft},
		{"recipient", `{"body":"","to":[{"name":"PRIVATE_\ud800"}]}`, inputJSONDraft},
		{"attachment path", `{"body":"","attachments":["/PRIVATE_\ud800"]}`, inputJSONDraft},
		{"key", `{"body":"","PRIVATE_\ud800":""}`, inputJSONDraft},
		{"batch ID", `{"operation":"read","items":[{"id":"PRIVATE_\ud800","ref":"ref"}]}`, inputJSONBatch},
		{"batch output path", `{"operation":"attachment_save","items":[{"id":"one","output_path":"/PRIVATE_\ud800"}]}`, inputJSONBatch},
		{"batch projection", `{"defaults":{"fields":["PRIVATE_\ud800"]}}`, inputJSONBatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := validateInputJSON([]byte(test.payload), test.shape)
			if err == nil || errorCode(err) != "invalid_input" || strings.Contains(err.Error(), "PRIVATE_") {
				t.Fatalf("invalid Unicode accepted or disclosed: %v", err)
			}
		})
	}
}

func TestJSONInputValidUnicodeKeepsDecodedMeaning(t *testing.T) {
	for _, encoded := range []string{
		`"Jörg 李 🌍 �"`, `"\uFFFD"`, `"\ud800\udc00"`, `"\uDBFF\uDFFF"`,
		`"\uD83D\uDE00"`, `"\\ud800"`, `"\\\\udfff"`, `"\"\\ud800\""`, `"\u005cud800"`,
	} {
		t.Run(encoded, func(t *testing.T) {
			input, err := decodeDraftInput(strings.NewReader(`{"body":` + encoded + `}`))
			var expected string
			if decodeErr := json.Unmarshal([]byte(encoded), &expected); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if err != nil || input.Body != expected || !input.BodySet {
				t.Fatalf("valid Unicode changed: input=%+v, error=%v, expected=%q", input, err, expected)
			}
		})
	}
}

func TestInvalidJSONUnicodePreventsDraftBatchAndEditorEffects(t *testing.T) {
	for _, command := range []string{"create", "update", "reply", "forward", "batch", "edit"} {
		for _, invalid := range []string{`\ud800`, "\xff"} {
			t.Run(command+"/"+invalid, func(t *testing.T) {
				root := t.TempDir()
				gateway := &jsonInputGateway{}
				service := mail.NewServiceWithDraftRoot(gateway, root)
				draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{To: []mail.Recipient{{Address: "recipient@example.com"}}, Body: "Original"}})
				if err != nil {
					t.Fatal(err)
				}
				before := jsonInputDirectoryState(t, root)
				payload := `{"to":[{"address":"recipient@example.com"}],"body":"PRIVATE_` + invalid + `"}`
				path := filepath.Join(t.TempDir(), "input.json")
				args := []string{"drafts", command, "--input", path, "--json"}
				switch command {
				case "update":
					args = append(args, "--ref", draft.Ref, "--expected-revision", draft.Revision)
				case "reply", "forward":
					args[0] = "messages"
					args = append(args, "--ref", jsonInputSourceRef(t))
				case "batch":
					payload = `{"operation":"mark","items":[{"id":"PRIVATE_` + invalid + `","ref":"` + jsonInputSourceRef(t) + `","read":true}]}`
					args = []string{"batch", "--input", path, "--json"}
				case "edit":
					t.Setenv("MAILCLI_TEST_JSON_EDITOR", payload)
					var stdout, stderr bytes.Buffer
					code := runDraftEditWithTestTerminal(t, root, []string{"drafts", "edit", "--ref", draft.Ref, "--editor", os.Args[0], "--editor-arg=-test.run=TestDraftJSONEditorProcess", "--editor-arg=--", "--json"}, &stdout, &stderr)
					var response envelope
					if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != 2 || response.Error == nil || response.Error.Code != "invalid_input" || strings.Contains(stdout.String()+stderr.String(), "PRIVATE_") {
						t.Fatalf("editor input accepted or disclosed: code=%d, error=%v, output=%s, stderr=%s", code, err, &stdout, &stderr)
					}
				}
				if command != "edit" {
					if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
						t.Fatal(err)
					}
					assertJSONInputRejected(t, service, args, "$")
				}
				if after := jsonInputDirectoryState(t, root); !reflect.DeepEqual(before, after) || gateway.calls.Load() != 0 {
					t.Fatalf("invalid Unicode reached publication or dispatch: before=%v, after=%v, calls=%d", before, after, gateway.calls.Load())
				}
			})
		}
	}
}
