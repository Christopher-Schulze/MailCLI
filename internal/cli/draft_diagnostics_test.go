package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"mailcli/internal/mail"
)

func TestDraftCommandsReturnPersistedDiagnostics(t *testing.T) {
	want := []mail.ContentDiagnostic{
		{Code: mail.ContentDiagnosticUnsafeAttribute, Element: "p", Attribute: "onclick"},
	}
	for _, command := range []string{"create", "update", "reply", "forward"} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			gateway := &jsonInputGateway{}
			service := mail.NewServiceWithDraftRoot(gateway, root)
			args := []string{"drafts", command, "--to", "recipient@example.com", "--format", "html",
				"--body", `<p onclick="private()">Visible</p>`, "--json", "--view", "full"}
			var previous mail.Draft
			switch command {
			case "update":
				var err error
				previous, err = service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
					To: []mail.Recipient{{Address: "recipient@example.com"}}, Body: "Before",
				}})
				if err != nil {
					t.Fatal(err)
				}
				args = append(args, "--ref", previous.Ref, "--expected-revision", previous.Revision)
			case "reply", "forward":
				args[0] = "messages"
				args = append(args, "--message", jsonInputSourceRef(t))
			}
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), service, args, &stdout, &stderr)
			var response envelope
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != 0 || stderr.Len() != 0 || !response.OK || response.Data.Draft == nil {
				t.Fatalf("draft command failed: code=%d error=%v stdout=%s stderr=%s", code, err, &stdout, &stderr)
			}
			returned := response.Data.Draft
			payload, err := os.ReadFile(filepath.Join(root, returned.Ref+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var stored mail.Draft
			if err := json.Unmarshal(payload, &stored); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(returned.ContentDiagnostics, want) || !reflect.DeepEqual(stored.ContentDiagnostics, want) || returned.Revision == "" || returned.Revision != stored.Revision || returned.Body != stored.Body || returned.BodyHTML != stored.BodyHTML || returned.BodySource != stored.BodySource {
				t.Fatalf("CLI/persistence diagnostic or content mismatch: returned=%+v stored=%+v", returned, stored)
			}
			if command == "update" && (stored.Ref != previous.Ref || stored.Revision == previous.Revision) {
				t.Fatal("update did not replace the reviewed content")
			}
			wantSourceCalls := int32(0)
			if command == "reply" || command == "forward" {
				wantSourceCalls = 1
				if stored.SourceRef != jsonInputSourceRef(t) || stored.SourceMessageID != "<source@example.com>" || string(stored.Kind) != command {
					t.Fatalf("derived draft lost source identity: %+v", stored)
				}
			}
			if gateway.calls.Load() != wantSourceCalls {
				t.Fatalf("gateway calls=%d, want %d", gateway.calls.Load(), wantSourceCalls)
			}
		})
	}
}
