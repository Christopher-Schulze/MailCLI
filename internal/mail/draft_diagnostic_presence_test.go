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

func TestDraftDiagnosticPresenceAvoidsRepeatedRendering(t *testing.T) {
	for _, fixture := range []struct {
		name, body string
		format     DraftBodyFormat
		losses     bool
	}{
		{"plain", "Hello", DraftBodyPlain, false},
		{"HTML", "<p>Hello</p>", DraftBodyHTML, false},
		{"Markdown", "**Hello**", DraftBodyMarkdown, false},
		{"empty HTML", "", DraftBodyHTML, false},
		{"empty Markdown", "", DraftBodyMarkdown, false},
		{"lossy HTML", "<p onclick='private()'>Hello</p>", DraftBodyHTML, true},
	} {
		for _, presence := range []string{"modern", "omitted", "null", "malformed"} {
			t.Run(fixture.name+"/"+presence, func(t *testing.T) {
				root := t.TempDir()
				service := NewServiceWithDraftRoot(nil, root)
				observer := &contentRenderCounter{}
				service.contentObserver = observer
				draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
					To: []Recipient{{Address: "recipient@example.com"}}, Body: fixture.body, BodyFormat: fixture.format,
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
				if presence == "modern" && fixture.format != DraftBodyPlain {
					if draft.ContentDiagnostics == nil || (!fixture.losses && string(document["content_diagnostics"]) != "[]") {
						t.Errorf("computed empty diagnostics were not retained: %s", document["content_diagnostics"])
					}
				}
				if presence != "modern" {
					delete(document, "content_diagnostics")
					if presence == "null" {
						document["content_diagnostics"] = json.RawMessage("null")
					}
					if presence == "malformed" {
						document["content_diagnostics"] = json.RawMessage("42")
					}
					payload, err = json.Marshal(document)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, payload, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				before, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				observer.calls.Store(0)
				for range 3 {
					read, err := service.GetDraft(draft.Ref)
					if presence == "malformed" {
						var operation *OperationError
						if !errors.As(err, &operation) || operation.Code != "draft_state_error" {
							t.Fatalf("malformed diagnostics accepted: %v", err)
						}
						continue
					}
					if err != nil || read.Revision != draft.Revision || read.Body != draft.Body ||
						read.BodyHTML != draft.BodyHTML || read.BodySource != draft.BodySource ||
						!reflect.DeepEqual(read.ContentDiagnostics, draft.ContentDiagnostics) {
						t.Fatalf("read changed content or review identity: %+v %v", read, err)
					}
				}
				wantRenders := int64(0)
				if fixture.format != DraftBodyPlain && (presence == "omitted" || presence == "null") {
					wantRenders = 3
				}
				if observer.calls.Load() != wantRenders {
					t.Errorf("renders=%d, want=%d", observer.calls.Load(), wantRenders)
				}
				after, err := os.Stat(path)
				if err != nil || !before.ModTime().Equal(after.ModTime()) {
					t.Fatalf("inspection changed stored mtime: %v", err)
				}
				actual, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(payload, actual) {
					t.Fatalf("inspection rewrote stored state: %v", err)
				}
			})
		}
	}
}

func TestEmptyDiagnosticsDoNotBypassCanonicalMutationGates(t *testing.T) {
	for _, format := range []DraftBodyFormat{DraftBodyHTML, DraftBodyMarkdown} {
		for _, operation := range []string{"update", "send", "save", "handoff"} {
			t.Run(string(format)+"/"+operation, func(t *testing.T) {
				root := t.TempDir()
				service := NewServiceWithDraftRoot(nil, root)
				input := DraftInput{To: []Recipient{{Address: "recipient@example.com"}}, Body: "Hello", BodyFormat: format}
				draft, err := service.CreateDraft(CreateDraftRequest{Input: input})
				if err != nil {
					t.Fatal(err)
				}
				draft.Body = "tampered"
				draft.ContentDiagnostics = []ContentDiagnostic{}
				if err := writeDraftFile(root, draft); err != nil {
					t.Fatal(err)
				}
				path, err := draftPath(root, draft.Ref)
				if err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				switch operation {
				case "update":
					_, err = service.UpdateDraftContext(context.Background(), UpdateDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision, Input: input})
				case "send":
					_, err = service.SendDraft(context.Background(), SendDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision})
				case "save":
					_, err = service.SaveDraft(context.Background(), draft.Ref)
				case "handoff":
					var session *DraftHandoffSession
					session, err = service.BeginDraftHandoffContext(context.Background(), draft.Ref)
					if session != nil {
						if closeErr := session.Close(); closeErr != nil {
							t.Error(closeErr)
						}
					}
				}
				var failure *OperationError
				if !errors.As(err, &failure) || failure.Code != "draft_state_error" {
					t.Fatalf("canonical mutation gate accepted tampered rich content: %v", err)
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("rejected mutation changed state: %v", err)
				}
			})
		}
	}
}
