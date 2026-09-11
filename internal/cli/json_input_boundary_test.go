package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

type jsonInputGateway struct {
	testGateway
	calls atomic.Int32
}

func (g *jsonInputGateway) MessageThreadSource(context.Context, string) (mail.ThreadSource, error) {
	g.calls.Add(1)
	return mail.ThreadSource{Subject: "Original", From: "source@example.com", MessageID: "<source@example.com>"}, nil
}

func (g *jsonInputGateway) MarkMessage(ctx context.Context, request mail.MarkMessageRequest) (mail.MessageSummary, error) {
	g.calls.Add(1)
	return g.testGateway.MarkMessage(ctx, request)
}

type jsonInputFileState struct {
	digest   [sha256.Size]byte
	mode     os.FileMode
	modified time.Time
}

func jsonInputDirectoryState(t *testing.T, root string) map[string]jsonInputFileState {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	state := make(map[string]jsonInputFileState, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("unexpected draft entry %s: %v", entry.Name(), err)
		}
		payload, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		state[entry.Name()] = jsonInputFileState{sha256.Sum256(payload), info.Mode(), info.ModTime()}
	}
	return state
}

func TestDraftJSONRejectsAmbiguityBeforeAnyMutation(t *testing.T) {
	const recipients = `"to":[{"address":"recipient@example.com"}],`
	sourceRef := jsonInputSourceRef(t)
	for _, test := range []struct {
		name, fields, location string
	}{
		{"duplicate body", recipients + `"body":"PRIVATE_FIRST","body":"PRIVATE_SECOND"`, "$.body"},
		{"reversed body", recipients + `"body":"PRIVATE_SECOND","body":"PRIVATE_FIRST"`, "$.body"},
		{"identical body", recipients + `"body":"PRIVATE_FIRST","body":"PRIVATE_FIRST"`, "$.body"},
		{"body alias", recipients + `"body":"PRIVATE_FIRST","Body":"PRIVATE_SECOND"`, "$"},
		{"alias first", recipients + `"Body":"PRIVATE_SECOND","body":"PRIVATE_FIRST"`, "$"},
		{"escaped duplicate", recipients + `"body":"PRIVATE_FIRST","bo\u0064y":"PRIVATE_SECOND"`, "$.body"},
		{"duplicate subject", recipients + `"body":"PRIVATE_FIRST","subject":"first","subject":"second"`, "$.subject"},
		{"duplicate recipient", `"body":"PRIVATE_FIRST","to":[{"address":"first@example.com","address":"second@example.com"}]`, "$.to[0].address"},
		{"escaped recipient", `"body":"PRIVATE_FIRST","to":[{"name":"first","na\u006de":"second","address":"recipient@example.com"}]`, "$.to[0].name"},
		{"recipient alias", `"body":"PRIVATE_FIRST","to":[{"Address":"recipient@example.com"}]`, "$.to[0]"},
		{"cc duplicate", recipients + `"body":"PRIVATE_FIRST","cc":[{"name":"one","name":"two","address":"cc@example.com"}]`, "$.cc[0].name"},
		{"bcc unknown", recipients + `"body":"PRIVATE_FIRST","bcc":[{"address":"bcc@example.com","extra":"PRIVATE_SECOND"}]`, "$.bcc[0]"},
		{"options unknown", recipients + `"body":"PRIVATE_FIRST","options":{}`, "$"},
		{"object attachment", recipients + `"body":"PRIVATE_FIRST","attachments":[{"path":"PRIVATE_SECOND"}]`, "$.attachments[0]"},
		{"trailing object", recipients + `"body":"PRIVATE_FIRST"} {"body":"PRIVATE_SECOND"`, "one JSON object"},
		{"truncated value", recipients + `"body":"PRIVATE_FIRST`, "$.body"},
	} {
		for _, command := range []string{"create", "update", "reply", "forward"} {
			t.Run(command+"/"+test.name, func(t *testing.T) {
				root := t.TempDir()
				gateway := &jsonInputGateway{}
				service := mail.NewServiceWithDraftRoot(gateway, root)
				draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
					To: []mail.Recipient{{Address: "recipient@example.com"}}, Body: "Original body",
				}})
				if err != nil {
					t.Fatal(err)
				}
				before := jsonInputDirectoryState(t, root)
				inputPath := filepath.Join(t.TempDir(), "input.json")
				if err := os.WriteFile(inputPath, []byte("{"+test.fields+"}"), 0o600); err != nil {
					t.Fatal(err)
				}
				args := []string{"drafts", command, "--input", inputPath, "--json"}
				switch command {
				case "update":
					args = append(args, "--ref", draft.Ref, "--expected-revision", draft.Revision)
				case "reply", "forward":
					args[0] = "messages"
					args = append(args, "--message", sourceRef)
				}
				assertJSONInputRejected(t, service, args, test.location)
				if after := jsonInputDirectoryState(t, root); !reflect.DeepEqual(before, after) {
					t.Errorf("draft storage changed: before=%v after=%v", before, after)
				}
				if gateway.calls.Load() != 0 {
					t.Errorf("gateway dispatched %d calls for invalid input", gateway.calls.Load())
				}
			})
		}
	}
}

func assertJSONInputRejected(t *testing.T, service *mail.Service, args []string, location string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), service, args, &stdout, &stderr)
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("invalid envelope: %v", err)
	}
	if code != 2 || stderr.Len() != 0 || response.OK || response.Error == nil || response.Error.Code != "invalid_input" {
		t.Errorf("input not rejected: code=%d ok=%t error=%+v stderr bytes=%d", code, response.OK, response.Error, stderr.Len())
	}
	if response.Error == nil || !strings.Contains(response.Error.Message, location) {
		t.Errorf("missing field location %q: %+v", location, response.Error)
	}
	if strings.Contains(stdout.String(), "PRIVATE_") || strings.Contains(stderr.String(), "PRIVATE_") {
		t.Error("input values leaked into diagnostic output")
	}
}

func TestBatchJSONRejectsAmbiguityBeforeDispatch(t *testing.T) {
	for _, test := range []struct{ name, payload, location string }{
		{"operation duplicate", `{"operation":"mark","operation":"mark","items":[{"id":"one","ref":"msg_ref","read":true}]}`, "$.operation"},
		{"operation alias", `{"operation":"mark","Operation":"mark","items":[{"id":"one","ref":"msg_ref","read":true}]}`, "$"},
		{"late item duplicate", `{"operation":"mark","items":[{"id":"one","ref":"msg_ref","read":true},{"id":"two","ref":"msg_ref","read":true,"read":false}]}`, "$.items[1].read"},
		{"item alias", `{"operation":"mark","items":[{"id":"one","ref":"msg_ref","Read":true}]}`, "$.items[0]"},
		{"item unknown", `{"operation":"mark","items":[{"id":"one","ref":"msg_ref","read":true,"extra":"PRIVATE_FIRST"}]}`, "$.items[0]"},
		{"trailing", `{"operation":"mark","items":[{"id":"one","ref":"msg_ref","read":true}]} {}`, "one JSON object"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "batch.json")
			if err := os.WriteFile(path, []byte(test.payload), 0o600); err != nil {
				t.Fatal(err)
			}
			gateway := &jsonInputGateway{}
			assertJSONInputRejected(t, mail.NewServiceWithDraftRoot(gateway, t.TempDir()),
				[]string{"batch", "--input", path, "--json"}, test.location)
			if gateway.calls.Load() != 0 {
				t.Errorf("batch dispatched %d calls for invalid input", gateway.calls.Load())
			}
		})
	}
}

func jsonInputSourceRef(t *testing.T) string {
	t.Helper()
	ref, err := mailref.EncodeMessage(mailref.Message{
		AccountID: "account", MailboxPath: []string{"Inbox"}, LibraryID: "1",
		ExpectedMessageID: "<source@example.com>", ExpectedStoreUUID: "store", ExpectedStoreMailboxID: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestDraftJSONValidInputKeepsWorkflowMeaning(t *testing.T) {
	for _, command := range []string{"create", "update", "reply", "forward"} {
		t.Run(command, func(t *testing.T) {
			service := mail.NewServiceWithDraftRoot(&jsonInputGateway{}, t.TempDir())
			draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
				To: []mail.Recipient{{Address: "before@example.com"}}, Body: "Before",
			}})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "input.json")
			payload := `{"to":[{"name":"Jörg 🌍","address":"after@example.com"}],"subject":"","cc":[],"body":"Grüße 李\nEscaped \"quote\" and \\ path."}`
			if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"drafts", command, "--input", path, "--json"}
			switch command {
			case "update":
				args = append(args, "--ref", draft.Ref, "--expected-revision", draft.Revision)
			case "reply", "forward":
				args[0] = "messages"
				args = append(args, "--message", jsonInputSourceRef(t))
			}
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), service, args, &stdout, &stderr)
			var response envelope
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != 0 || stderr.Len() != 0 || !response.OK || response.Data.Draft == nil {
				t.Fatalf("valid input failed: code=%d error=%v stdout=%s stderr=%s", code, err, &stdout, &stderr)
			}
			stored, err := service.GetDraft(response.Data.Draft.Ref)
			if err != nil || stored.Subject != "" || len(stored.CC) != 0 || len(stored.To) != 1 ||
				stored.To[0].Name != "Jörg 🌍" || stored.To[0].Address != "after@example.com" ||
				stored.Body != "Grüße 李\nEscaped \"quote\" and \\ path." {
				t.Fatalf("valid input changed meaning: draft=%+v error=%v", stored, err)
			}
			if command == "update" && (stored.Ref != draft.Ref || stored.Revision == draft.Revision) {
				t.Fatal("update did not replace the reviewed revision")
			}
			if (command == "reply" || command == "forward") && stored.SourceMessageID != "<source@example.com>" {
				t.Fatal("derived draft lost its thread identity")
			}
		})
	}
}

func TestDraftJSONEditorRejectsAmbiguityWithoutChangingStorage(t *testing.T) {
	for _, payload := range []string{
		`{"to":[{"address":"recipient@example.com"}],"body":"PRIVATE_FIRST","body":"PRIVATE_SECOND"}`,
		`{"to":[{"address":"recipient@example.com"}],"body":"PRIVATE_FIRST","Body":"PRIVATE_SECOND"}`,
		`{"to":[{"name":"first","name":"second","address":"recipient@example.com"}],"body":"PRIVATE_FIRST"}`,
	} {
		root := t.TempDir()
		service := mail.NewServiceWithDraftRoot(nil, root)
		draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
			To: []mail.Recipient{{Address: "original@example.com"}}, Body: "Original body",
		}})
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("MAILCLI_TEST_JSON_EDITOR", payload)
		before := jsonInputDirectoryState(t, root)
		assertJSONInputRejected(t, service, []string{
			"drafts", "edit", "--ref", draft.Ref, "--editor", os.Args[0],
			"--editor-arg=-test.run=TestDraftJSONEditorProcess", "--editor-arg=--", "--json",
		}, "$")
		if after := jsonInputDirectoryState(t, root); !reflect.DeepEqual(before, after) {
			t.Errorf("editor changed draft storage: before=%v after=%v", before, after)
		}
	}
}

func TestDraftJSONEditorProcess(t *testing.T) {
	payload := os.Getenv("MAILCLI_TEST_JSON_EDITOR")
	if payload == "" {
		return
	}
	if err := os.WriteFile(os.Args[len(os.Args)-1], []byte(payload), 0o600); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestJSONInputRejectsOversizeAndOverflowBeforeDispatch(t *testing.T) {
	for _, test := range []struct{ name, payload, location string }{
		{"draft", `{"body":"PRIVATE_FIRST"}` + strings.Repeat(" ", maximumDraftInputBytes), "16 MiB"},
		{"batch", `{"operation":"mark","items":[{"id":"one","ref":"msg_ref","read":true}]}` + strings.Repeat(" ", mail.MaximumBatchInputBytes), "16 MiB"},
		{"batch", `{"operation":"mark","concurrency":123456789012345678901234567890,"items":[{"id":"one","ref":"msg_ref","read":true}]}`, "$.concurrency"},
		{"batch", `{"operation":"mark","concurrency":1e9999,"items":[{"id":"one","ref":"msg_ref","read":true}]}`, "$.concurrency"},
	} {
		t.Run(test.name+"/"+test.location, func(t *testing.T) {
			root := t.TempDir()
			gateway := &jsonInputGateway{}
			service := mail.NewServiceWithDraftRoot(gateway, root)
			path := filepath.Join(t.TempDir(), "input.json")
			if err := os.WriteFile(path, []byte(test.payload), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"batch", "--input", path, "--json"}
			if test.name == "draft" {
				args = append([]string{"drafts", "create"}, args[1:]...)
			}
			assertJSONInputRejected(t, service, args, test.location)
			if len(jsonInputDirectoryState(t, root)) != 0 || gateway.calls.Load() != 0 {
				t.Fatal("oversized or overflowing input changed storage or dispatched an operation")
			}
		})
	}
}

func TestJSONInputStdinUsesTheSameStrictBoundary(t *testing.T) {
	for _, test := range []struct {
		name, payload string
		args          []string
	}{
		{"draft", `{"to":[{"address":"recipient@example.com"}],"body":"PRIVATE_FIRST","body":"PRIVATE_SECOND"}`, []string{"drafts", "create", "--input", "-", "--json"}},
		{"batch", `{"operation":"mark","items":[{"id":"one","ref":"msg_ref","read":true,"Read":false}]}`, []string{"batch", "--input", "-", "--json"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stdin.json")
			if err := os.WriteFile(path, []byte(test.payload), 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			original := os.Stdin
			os.Stdin = file
			t.Cleanup(func() {
				os.Stdin = original
				if err := file.Close(); err != nil {
					t.Error(err)
				}
			})
			root := t.TempDir()
			gateway := &jsonInputGateway{}
			assertJSONInputRejected(t, mail.NewServiceWithDraftRoot(gateway, root), test.args, "$")
			if len(jsonInputDirectoryState(t, root)) != 0 || gateway.calls.Load() != 0 {
				t.Fatal("ambiguous stdin reached storage or dispatch")
			}
		})
	}
}
