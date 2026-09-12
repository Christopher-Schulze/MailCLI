package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

func TestDraftOutputLimitRetainsCompletedMutation(t *testing.T) {
	for _, command := range []string{"create", "update", "edit", "reply", "forward"} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			service := mail.NewServiceWithDraftRoot(&jsonInputGateway{}, root)
			args, previousRevision := draftEffectCommand(t, service, command)
			args = append(args, "--max-bytes", "1", "--json")
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), service, args, &stdout, &stderr)
			stored := onlyPersistedOutputDraft(t, service, root)
			if stored.Subject != "After" || stored.Body != "Body" || stored.Revision == "" || stored.Revision == previousRevision {
				t.Fatalf("mutation was not persisted correctly: %+v", stored)
			}
			var response envelope
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != 1 || stderr.Len() != 0 ||
				response.OK || response.Error == nil || response.Error.Guidance == nil || response.Data.Draft == nil {
				t.Fatalf("code=%d error=%v stdout=%s stderr=%s", code, err, &stdout, &stderr)
			}
			guidance := response.Error.Guidance
			if response.Error.Code != "output_too_large" || guidance.EffectCertainty != mail.EffectComplete ||
				guidance.Phase != mail.OperationPhaseExecution || guidance.ReplayAllowed || guidance.Retryability != mail.RetryObserveRequired ||
				guidance.Recovery.Action != mail.RecoveryInspect || guidance.Recovery.Command != "drafts.inspect" ||
				!slices.Equal(guidance.Recovery.Args, []string{"--ref", stored.Ref, "--view", "full", "--json"}) ||
				response.Data.Draft.Ref != stored.Ref || response.Data.Draft.Revision != stored.Revision {
				t.Fatalf("completed state lost: guidance=%+v data=%+v", guidance, response.Data.Draft)
			}
			if strings.Contains(response.Error.Message, "--export") || !strings.Contains(response.Error.Message, stored.Ref) {
				t.Fatalf("unsupported or unusable recovery text: %s", response.Error.Message)
			}
			stdout.Reset()
			inspectArgs := append([]string{"drafts", "inspect"}, guidance.Recovery.Args...)
			if code := Run(context.Background(), service, inspectArgs, &stdout, &stderr); code != 0 {
				t.Fatalf("recovery command failed: %d %s %s", code, &stdout, &stderr)
			}
			if after := onlyPersistedOutputDraft(t, service, root); after.Revision != stored.Revision {
				t.Fatal("inspection changed the persisted revision")
			}
		})
	}
}

func draftEffectCommand(t *testing.T, service *mail.Service, command string) ([]string, string) {
	t.Helper()
	args := []string{"drafts", command, "--to", "recipient@example.com", "--subject", "After", "--body", "Body"}
	if command == "reply" || command == "forward" {
		args[0] = "messages"
		return append(args, "--message", jsonInputSourceRef(t)), ""
	}
	if command == "create" {
		return args, ""
	}
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		To: []mail.Recipient{{Address: "recipient@example.com"}}, Subject: "Before", Body: "Body",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if command == "edit" {
		t.Setenv("MAILCLI_TEST_EDITOR", "1")
		return []string{"drafts", "edit", "--ref", draft.Ref, "--editor", os.Args[0],
			"--editor-arg=-test.run=TestDraftEditorHelperProcess", "--editor-arg=--"}, draft.Revision
	}
	return []string{"drafts", "update", "--ref", draft.Ref, "--expected-revision", draft.Revision, "--subject", "After"}, draft.Revision
}

func onlyPersistedOutputDraft(t *testing.T, service *mail.Service, root string) mail.Draft {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "draft_*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("expected exactly one persisted draft: %v %v", paths, err)
	}
	draft, err := service.GetDraft(strings.TrimSuffix(filepath.Base(paths[0]), ".json"))
	if err != nil {
		t.Fatal(err)
	}
	return draft
}

func TestDraftOutputLimitDoesNotInventCompletion(t *testing.T) {
	root := t.TempDir()
	service := mail.NewServiceWithDraftRoot(nil, root)
	args, revision := draftEffectCommand(t, service, "update")
	draft := onlyPersistedOutputDraft(t, service, root)
	for _, test := range []struct {
		name string
		args []string
		code int
	}{
		{"invalid input", []string{"drafts", "create", "--to", "recipient@example.com", "--json"}, 2},
		{"invalid budget", append(append([]string(nil), args...), "--max-bytes", "0", "--json"), 2},
		{"conflict", []string{"drafts", "update", "--ref", draft.Ref, "--expected-revision", "stale", "--subject", "After", "--max-bytes", "1", "--json"}, 1},
		{"inspect", []string{"drafts", "inspect", "--ref", draft.Ref, "--max-bytes", "1", "--json"}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			before, err := os.ReadFile(filepath.Join(root, draft.Ref+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), service, test.args, &stdout, &stderr)
			var response envelope
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != test.code || response.Error == nil ||
				response.Error.Guidance == nil || response.Error.Guidance.EffectCertainty != mail.EffectNone {
				t.Fatalf("false effect: code=%d error=%v output=%s stderr=%s", code, err, &stdout, &stderr)
			}
			after, err := os.ReadFile(filepath.Join(root, draft.Ref+".json"))
			if err != nil || !bytes.Equal(before, after) || onlyPersistedOutputDraft(t, service, root).Revision != revision {
				t.Fatalf("failed/read-only command mutated draft: %v", err)
			}
		})
	}
	var output bytes.Buffer
	if code := writeProjectedSuccess(&output, "drafts.update", responseData{Draft: &draft}, outputOptions{
		target: projectionTargetDraft, view: outputViewFull, maxBytes: 1,
	}); code != 1 || !strings.Contains(output.String(), `"effect_certainty":"none"`) {
		t.Fatalf("a ref alone invented completion: %d %s", code, &output)
	}
}

func TestDraftHumanAndBrokenOutputDoNotReplayMutation(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		for _, broken := range []bool{false, true} {
			root := t.TempDir()
			service := mail.NewServiceWithDraftRoot(nil, root)
			args := []string{"drafts", "create", "--to", "recipient@example.com", "--body", "Body", "--max-bytes", "1"}
			if jsonOutput {
				args = append(args, "--json")
			}
			var stdout, stderr bytes.Buffer
			code := 0
			if broken {
				code = Run(context.Background(), service, args, failingWriter{}, &stderr)
			} else {
				code = Run(context.Background(), service, args, &stdout, &stderr)
			}
			stored := onlyPersistedOutputDraft(t, service, root)
			wantCode := 0
			if broken || jsonOutput {
				wantCode = 1
			}
			if code != wantCode || (!broken && !strings.Contains(stdout.String(), stored.Ref)) || strings.Contains(stderr.String(), "--export") {
				t.Fatalf("json=%t broken=%t code=%d stdout=%s stderr=%s", jsonOutput, broken, code, &stdout, &stderr)
			}
		}
	}
}

func TestProjectionLimitSuggestsOnlySupportedExport(t *testing.T) {
	for _, test := range []struct {
		args   []string
		export bool
	}{
		{[]string{"messages", "get", "--ref", "msg_ref", "--view", "full"}, true},
		{[]string{"messages", "raw", "--ref", "msg_ref"}, true},
		{[]string{"drafts", "open", "--message", "msg_ref", "--view", "full"}, false},
	} {
		gateway := &projectionGateway{message: projectionMessage(), raw: "raw bytes"}
		args := append(test.args, "--max-bytes", "1", "--json")
		code, output, stderr := runProjectionCommand(t, gateway, args...)
		var response envelope
		if err := json.Unmarshal([]byte(output), &response); err != nil || code != 1 || stderr != "" || response.Error == nil {
			t.Fatalf("code=%d error=%v output=%s stderr=%s", code, err, output, stderr)
		}
		if response.Error.Code != "output_too_large" || strings.Contains(response.Error.Message, "--export") != test.export ||
			response.Error.Guidance == nil || response.Error.Guidance.EffectCertainty != mail.EffectNone {
			t.Fatalf("invalid read-only output guidance: %+v", response.Error)
		}
	}
}
