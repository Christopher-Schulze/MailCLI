package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"mailcli/internal/mail"
)

type intentProjectionGateway struct {
	*projectionGateway
	intentMu sync.Mutex
	intents  []mail.MessageReadIntent
	refs     []string
}

func (g *intentProjectionGateway) GetMessageWithIntent(
	_ context.Context,
	ref string,
	intent mail.MessageReadIntent,
) (mail.Message, error) {
	g.intentMu.Lock()
	defer g.intentMu.Unlock()
	g.intents = append(g.intents, intent)
	g.refs = append(g.refs, ref)
	return g.message, g.getErr
}

func runIntentProjectionCommand(
	t *testing.T,
	gateway *intentProjectionGateway,
	args ...string,
) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), mail.NewService(gateway), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestMessageGetSelectsNarrowReadIntentsOnlyForJSONProjections(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantIntent   mail.MessageReadIntent
		wantFullRead int
		wantFields   []string
		wantState    bool
	}{
		{
			name: "summary includes header-derived Message-ID", args: []string{"messages", "get", "--ref", "msg_ref", "--fields", "summary", "--json"},
			wantIntent: mail.MessageReadIntentHeaders, wantFields: []string{"summary"},
		},
		{
			name: "requested headers", args: []string{"messages", "get", "--ref", "msg_ref", "--fields", "summary,headers", "--json"},
			wantIntent: mail.MessageReadIntentHeaders, wantFields: []string{"headers", "summary"},
		},
		{
			name: "requested diagnostic state uses no-body MIME metadata", args: []string{"messages", "get", "--ref", "msg_ref", "--fields", "summary,content_complete", "--json"},
			wantIntent: mail.MessageReadIntentAttachments, wantFields: []string{"content_complete", "content_source", "hydration", "missing_parts", "summary"}, wantState: true,
		},
		{
			name: "source diagnostic uses no-body MIME metadata", args: []string{"messages", "get", "--ref", "msg_ref", "--fields", "content_source", "--json"},
			wantIntent: mail.MessageReadIntentAttachments,
			wantFields: []string{"content_complete", "content_source", "hydration", "missing_parts", "summary"}, wantState: true,
		},
		{
			name: "default metadata uses no-body MIME metadata", args: []string{"messages", "get", "--ref", "msg_ref", "--json"},
			wantIntent: mail.MessageReadIntentAttachments,
			wantFields: []string{"attachments", "bcc", "cc", "content_complete", "content_source", "hydration", "missing_parts", "reply_to", "summary", "to"},
			wantState:  true,
		},
		{
			name: "human output stays full", args: []string{"messages", "get", "--ref", "msg_ref", "--fields", "summary"},
			wantFullRead: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gateway := &intentProjectionGateway{projectionGateway: &projectionGateway{message: projectionMessage()}}
			code, output, stderr := runIntentProjectionCommand(t, gateway, test.args...)
			if code != 0 || stderr != "" {
				t.Fatalf("code = %d, stderr = %q, output = %q", code, stderr, output)
			}
			if gateway.getCalls != test.wantFullRead {
				t.Fatalf("full GetMessage calls = %d, want %d", gateway.getCalls, test.wantFullRead)
			}
			if test.wantIntent == "" {
				if len(gateway.intents) != 0 {
					t.Fatalf("narrow read intents = %v, want none", gateway.intents)
				}
			} else if len(gateway.intents) != 1 || gateway.intents[0] != test.wantIntent {
				t.Fatalf("narrow read intents = %v, want [%s]", gateway.intents, test.wantIntent)
			}
			if test.wantFields == nil {
				return
			}
			var response struct {
				Data struct {
					Message    json.RawMessage `json:"message"`
					Projection projectionInfo  `json:"projection"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(output), &response); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if !slicesEqual(response.Data.Projection.Fields, test.wantFields) {
				t.Fatalf("projection fields = %v, want %v", response.Data.Projection.Fields, test.wantFields)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(response.Data.Message, &fields); err != nil {
				t.Fatalf("unmarshal projected message: %v", err)
			}
			_, hasState := fields["content_complete"]
			if hasState != test.wantState {
				t.Fatalf("projected content state present = %t, want %t: %s", hasState, test.wantState, response.Data.Message)
			}
			if test.wantIntent == mail.MessageReadIntentIndex &&
				(len(fields) != 1 || strings.Contains(output, `"content_source"`) || strings.Contains(output, `"missing_parts"`)) {
				t.Fatalf("index projection leaked unselected message state: %s", output)
			}
		})
	}
}

func TestBatchReadUsesMessageReadIntentPolicy(t *testing.T) {
	inputPath := filepath.Join(t.TempDir(), "batch.json")
	input := `{"operation":"read","items":[{"id":"metadata","ref":"metadata_ref","view":"metadata"},{"id":"summary","ref":"summary_ref","fields":["summary"]},{"id":"diagnostics","ref":"diagnostics_ref","fields":["summary","content_source"]},{"id":"full","ref":"full_ref","view":"full"}]}`
	if err := os.WriteFile(inputPath, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	gateway := &intentProjectionGateway{projectionGateway: &projectionGateway{message: projectionMessage()}}
	code, output, stderr := runIntentProjectionCommand(t, gateway, "batch", "--input", inputPath, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("batch code = %d, stderr = %q, output = %s", code, stderr, output)
	}
	gateway.intentMu.Lock()
	gotIntents := make(map[string]mail.MessageReadIntent, len(gateway.intents))
	for index, ref := range gateway.refs {
		gotIntents[ref] = gateway.intents[index]
	}
	gateway.intentMu.Unlock()
	wantIntents := map[string]mail.MessageReadIntent{
		"metadata_ref":    mail.MessageReadIntentAttachments,
		"summary_ref":     mail.MessageReadIntentHeaders,
		"diagnostics_ref": mail.MessageReadIntentAttachments,
	}
	if len(gotIntents) != len(wantIntents) || gateway.getCalls != 1 {
		t.Fatalf("narrow intents = %v, full calls = %d; want %v and one full call", gotIntents, gateway.getCalls, wantIntents)
	}
	for ref, want := range wantIntents {
		if gotIntents[ref] != want {
			t.Errorf("read intent for %s = %q, want %q", ref, gotIntents[ref], want)
		}
	}
}

func TestMessagesGetAttachmentsProjectionUsesMetadataRead(t *testing.T) {
	gateway := &intentProjectionGateway{projectionGateway: &projectionGateway{message: projectionMessage()}}
	code, output, stderr := runIntentProjectionCommand(t, gateway,
		"messages", "get", "--ref", "msg_ref", "--fields", "attachments", "--json")
	if code != 0 || stderr != "" || len(gateway.intents) != 1 ||
		gateway.intents[0] != mail.MessageReadIntentAttachments || gateway.getCalls != 0 ||
		!strings.Contains(output, `"fields":["attachments","summary"]`) {
		t.Fatalf("code = %d, stderr = %q, intents = %v, full calls = %d, output = %s", code, stderr, gateway.intents, gateway.getCalls, output)
	}
}

func TestAttachmentsListPreservesPartialEvidenceAfterHydrationFailure(t *testing.T) {
	message := projectionMessage()
	message.ContentSource = "emlx_partial"
	message.ContentComplete = false
	message.MissingParts = []string{"2"}
	gateway := &intentProjectionGateway{
		projectionGateway: &projectionGateway{
			message: message,
			getErr:  &testCodedError{code: "imap_timeout", message: "hydration failed"},
		},
	}
	code, output, stderr := runIntentProjectionCommand(t, gateway,
		"attachments", "list", "--ref", "msg_ref", "--json")
	if code != 1 || stderr != "" || len(gateway.intents) != 1 ||
		gateway.intents[0] != mail.MessageReadIntentAttachments ||
		!strings.Contains(output, `"content_source":"emlx_partial"`) ||
		!strings.Contains(output, `"content_complete":false`) ||
		!strings.Contains(output, `"missing_parts":["2"]`) ||
		!strings.Contains(output, `"attachments":[`) {
		t.Fatalf("code = %d, stderr = %q, intents = %v, output = %s", code, stderr, gateway.intents, output)
	}
}

func slicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
