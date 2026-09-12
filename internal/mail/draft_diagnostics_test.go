package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestDraftDiagnosticsSurviveCreationAndUpdate(t *testing.T) {
	for _, test := range []struct {
		name   string
		format DraftBodyFormat
		body   string
		want   []ContentDiagnostic
	}{
		{name: "plain", format: DraftBodyPlain, body: "Plain body"},
		{name: "lossless HTML", format: DraftBodyHTML, body: "<p>Hello</p>", want: []ContentDiagnostic{}},
		{name: "lossless Markdown", format: DraftBodyMarkdown, body: "**Hello**", want: []ContentDiagnostic{}},
		{name: "HTML", format: DraftBodyHTML, body: `<p onclick="private()">Visible</p><p onclick="again()">Again</p><img src="https://private.example/pixel">`, want: []ContentDiagnostic{
			{Code: ContentDiagnosticUnsafeAttribute, Element: "p", Attribute: "onclick"},
			{Code: ContentDiagnosticRemovedElement, Element: "img"},
			{Code: ContentDiagnosticRemoteResource, Element: "img", Attribute: "src"},
		}},
		{name: "Markdown", format: DraftBodyMarkdown, body: "![Logo](https://private.example/pixel)", want: []ContentDiagnostic{
			{Code: ContentDiagnosticRemovedElement, Element: "img"},
			{Code: ContentDiagnosticRemoteResource, Element: "img", Attribute: "src"},
		}},
	} {
		for _, kind := range []DraftKind{DraftKindNew, DraftKindReply, DraftKindForward} {
			t.Run(test.name+"/"+string(kind), func(t *testing.T) {
				root := t.TempDir()
				service := NewServiceWithDraftRoot(nil, root)
				observer := &contentRenderCounter{}
				service.contentObserver = observer
				request := CreateDraftRequest{Kind: kind, Input: DraftInput{
					To: []Recipient{{Address: "recipient@example.com"}}, Body: test.body, BodyFormat: test.format,
				}}
				if kind != DraftKindNew {
					request.SourceRef, request.SourceMessageID = storeBoundSourceRef(t), "<source@example.com>"
				}
				created, err := service.CreateDraftContext(context.Background(), request)
				if err != nil {
					t.Fatal(err)
				}
				if observer.calls.Load() != 1 {
					t.Errorf("creation rendered %d times", observer.calls.Load())
				}
				for _, operation := range []string{"create", "update"} {
					draft := created
					if operation == "update" {
						observer.calls.Store(0)
						draft, err = service.UpdateDraftContext(context.Background(), UpdateDraftRequest{Ref: created.Ref, ExpectedRevision: created.Revision, Input: request.Input})
						if err != nil {
							t.Fatal(err)
						}
						// The operation observer counts replacement preparation;
						// stored-content validation uses no observer.
						if observer.calls.Load() != 1 {
							t.Errorf("update prepared content %d times", observer.calls.Load())
						}
					}
					// Inspect the serialized document before legacy reconstruction can
					// hide an omission in the creation or replacement result.
					stored, err := loadDraftDocument(root, draft.Ref)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(draft.ContentDiagnostics, test.want) || !reflect.DeepEqual(stored.ContentDiagnostics, test.want) {
						t.Errorf("%s lost diagnostics: returned=%+v stored=%+v want=%+v", operation, draft.ContentDiagnostics, stored.ContentDiagnostics, test.want)
					}
					read, err := service.GetDraft(draft.Ref)
					if err != nil || !reflect.DeepEqual(read.ContentDiagnostics, test.want) || read.Revision != draft.Revision || draft.Revision != created.Revision || read.Body != draft.Body || read.BodyHTML != draft.BodyHTML || read.BodySource != draft.BodySource {
						t.Errorf("%s changed canonical content or revision across persistence: %+v, %v", operation, read, err)
					}
				}
			})
		}
	}
}

func TestDraftUpdateReplacesContentDiagnostics(t *testing.T) {
	root := t.TempDir()
	service := NewServiceWithDraftRoot(nil, root)
	input := DraftInput{To: []Recipient{{Address: "recipient@example.com"}}, Body: "Initial"}
	draft, err := service.CreateDraft(CreateDraftRequest{Input: input})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		format DraftBodyFormat
		body   string
		want   []ContentDiagnostic
	}{
		{DraftBodyHTML, `<p onclick="private()">Visible</p>`, []ContentDiagnostic{{Code: ContentDiagnosticUnsafeAttribute, Element: "p", Attribute: "onclick"}}},
		{DraftBodyHTML, `<p>Changed</p><script>private()</script>`, []ContentDiagnostic{{Code: ContentDiagnosticRemovedElement, Element: "script"}}},
		{DraftBodyMarkdown, "**Safe**", []ContentDiagnostic{}},
		{DraftBodyPlain, "Plain", nil},
	} {
		t.Run(string(test.format)+"/"+test.body, func(t *testing.T) {
			input.BodyFormat, input.Body = test.format, test.body
			updated, err := service.UpdateDraftContext(context.Background(), UpdateDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision, Input: input})
			if err != nil {
				t.Fatal(err)
			}
			stored, err := loadDraftDocument(root, updated.Ref)
			if err != nil || !reflect.DeepEqual(updated.ContentDiagnostics, test.want) || !reflect.DeepEqual(stored.ContentDiagnostics, test.want) || updated.Ref != draft.Ref || updated.Revision == draft.Revision || updated.CreatedAt != draft.CreatedAt {
				t.Fatalf("replacement retained obsolete content metadata: %+v, %v", updated, err)
			}
			draft = updated
		})
	}
}

func TestStoredDraftDiagnosticIntegrity(t *testing.T) {
	want := []ContentDiagnostic{
		{Code: ContentDiagnosticUnsafeAttribute, Element: "p", Attribute: "onclick"},
		{Code: ContentDiagnosticRemovedElement, Element: "script"},
	}
	for _, test := range []struct {
		name, diagnostics string
		invalid           bool
	}{
		{name: "legacy omitted"},
		{name: "legacy null", diagnostics: "null"},
		{name: "canonical", diagnostics: `[{"code":"unsafe_attribute_removed","element":"p","attribute":"onclick"},{"code":"removed_element","element":"script"}]`},
		{name: "empty", diagnostics: "[]", invalid: true},
		{name: "changed", diagnostics: `[{"code":"removed_element","element":"img"}]`, invalid: true},
		{name: "reordered", diagnostics: `[{"code":"removed_element","element":"script"},{"code":"unsafe_attribute_removed","element":"p","attribute":"onclick"}]`, invalid: true},
		{name: "duplicate", diagnostics: `[{"code":"unsafe_attribute_removed","element":"p","attribute":"onclick"},{"code":"removed_element","element":"script"},{"code":"removed_element","element":"script"}]`, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			service := NewServiceWithDraftRoot(nil, root)
			draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
				To: []Recipient{{Address: "recipient@example.com"}}, BodyFormat: DraftBodyHTML,
				Body: `<p onclick="private()">Visible</p><script>private()</script>`,
			}})
			if err != nil {
				t.Fatal(err)
			}
			path, err := draftPath(root, draft.Ref)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]json.RawMessage
			if err := json.Unmarshal(payload, &document); err != nil {
				t.Fatal(err)
			}
			delete(document, "content_diagnostics")
			if test.diagnostics != "" {
				document["content_diagnostics"] = json.RawMessage(test.diagnostics)
			}
			payload, err = json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			read, err := service.GetDraft(draft.Ref)
			if test.invalid {
				// The inspection path trusts stored diagnostics; the canonical
				// mutation gate still rejects them.
				if err != nil {
					t.Fatalf("GetDraft() structural read failed: %v", err)
				}
				_, err = service.UpdateDraftContext(context.Background(), UpdateDraftRequest{
					Ref: draft.Ref, ExpectedRevision: read.Revision,
					Input: DraftInput{To: read.To, Body: read.Body, BodyFormat: read.BodyFormat},
				})
				var operation *OperationError
				if !errors.As(err, &operation) || operation.Code != "draft_state_error" {
					t.Errorf("tampered diagnostics accepted by mutation gate: %+v, %v", read.ContentDiagnostics, err)
				}
			} else if err != nil || !reflect.DeepEqual(read.ContentDiagnostics, want) || read.Revision != draft.Revision || read.Body != draft.Body || read.BodyHTML != draft.BodyHTML || read.BodySource != draft.BodySource {
				t.Errorf("valid stored content changed: %+v, %v", read, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(payload, after) {
				t.Fatalf("reading changed persisted state: %v", err)
			}
		})
	}
}
