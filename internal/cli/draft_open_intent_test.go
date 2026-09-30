package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

type draftOpenIntentGateway struct {
	*projectionGateway
	intents []mail.MessageReadIntent
}

func (g *draftOpenIntentGateway) OpenDraftWithIntent(_ context.Context, _ string, intent mail.MessageReadIntent) (mail.Message, error) {
	g.intents = append(g.intents, intent)
	return g.message, g.getErr
}

func TestDraftOpenSelectsProjectionReadIntent(t *testing.T) {
	for _, test := range []struct {
		name      string
		options   []string
		intent    mail.MessageReadIntent
		fullCalls int
	}{
		{"metadata", []string{"--json"}, mail.MessageReadIntentHeaders, 0},
		{"summary", []string{"--fields", "summary", "--json"}, mail.MessageReadIntentHeaders, 0},
		{"headers", []string{"--fields", "header_fields", "--json"}, mail.MessageReadIntentHeaders, 0},
		{"attachments", []string{"--fields", "attachments", "--json"}, mail.MessageReadIntentAttachments, 0},
		{"content diagnostics", []string{"--fields", "content_complete", "--json"}, mail.MessageReadIntentAttachments, 0},
		{"plain", []string{"--view", "plain", "--json"}, "", 1},
		{"full", []string{"--view", "full", "--json"}, "", 1},
		{"human", []string{"--fields", "summary"}, "", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			gateway := &draftOpenIntentGateway{projectionGateway: &projectionGateway{message: projectionMessage()}}
			args := append([]string{"drafts", "open", "--ref", "msg_ref"}, test.options...)
			var stdout, stderr bytes.Buffer
			if Run(context.Background(), mail.NewService(gateway), args, &stdout, &stderr) != 0 || stderr.Len() != 0 {
				t.Fatalf("open failed: stdout=%s stderr=%s", &stdout, &stderr)
			}
			if gateway.openCalls != test.fullCalls || (test.intent == "" && len(gateway.intents) != 0) || (test.intent != "" && (len(gateway.intents) != 1 || gateway.intents[0] != test.intent)) {
				t.Fatalf("full=%d intents=%v; want full=%d intent=%s", gateway.openCalls, gateway.intents, test.fullCalls, test.intent)
			}
			if test.intent == mail.MessageReadIntentHeaders && (strings.Contains(stdout.String(), `"content_complete"`) || strings.Contains(stdout.String(), `"attachments"`)) {
				t.Fatalf("unselected MIME state leaked: %s", &stdout)
			}
		})
	}
}

func TestDraftOpenReadIntentValidationAndLegacyGateway(t *testing.T) {
	for _, test := range []struct {
		name, ref string
		intent    mail.MessageReadIntent
	}{
		{"empty ref", "", mail.MessageReadIntentHeaders},
		{"invalid intent", "msg_ref", "invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			gateway := &draftOpenIntentGateway{projectionGateway: &projectionGateway{message: projectionMessage()}}
			_, err := mail.NewService(gateway).OpenDraftWithIntent(context.Background(), test.ref, test.intent)
			if err == nil || errorCode(err) != "invalid_argument" || gateway.openCalls != 0 || len(gateway.intents) != 0 {
				t.Fatalf("validation dispatched: error=%v full=%d intents=%v", err, gateway.openCalls, gateway.intents)
			}
		})
	}
	gateway := &projectionGateway{message: projectionMessage()}
	message, err := mail.NewService(gateway).OpenDraftWithIntent(context.Background(), "msg_ref", mail.MessageReadIntentHeaders)
	if err != nil || gateway.openCalls != 1 || message.Content != gateway.message.Content {
		t.Fatalf("legacy draft semantics changed: message=%+v error=%v full=%d", message, err, gateway.openCalls)
	}
}
