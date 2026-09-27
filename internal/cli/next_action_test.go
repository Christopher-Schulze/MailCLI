package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

func TestNextActionMapping(t *testing.T) {
	tests := []struct {
		name, command, code, want, recovery string
		err                                 error
		data                                responseData
		wait                                int
	}{
		{name: "validation", command: "messages.get", code: "missing_required", want: "fix_input"},
		{name: "fresh ref", command: "messages.get", code: "not_found", want: "fix_input"},
		{name: "environment", command: "messages.get", code: "mail_store_unavailable", want: "ask_user"},
		{name: "initialization", command: "messages.get", code: "initialization_failed", want: "ask_user"},
		{name: "permissions", command: "messages.get", err: fs.ErrPermission, want: "ask_user"},
		{name: "safe listing unavailable", command: "messages.list", code: "safe_message_listing_unavailable", want: "ask_user"},
		{name: "busy read", command: "messages.get", code: "mail_busy", want: "retry", wait: 1},
		{name: "busy mutation", command: "messages.move", code: transport.CodeIMAPAccountBusy, want: "retry", wait: 1},
		{name: "terminal", command: "messages.get", code: "unsafe_message_source", want: "stop"},
		{name: "read cancellation", command: "messages.get", err: context.Canceled, want: "stop"},
		{name: "unstarted batch cancellation", command: "batch", code: "batch_canceled", want: "stop"},
		{name: "handoff cancellation", command: "drafts.handoff", code: "handoff_canceled_before_dispatch", want: "stop"},
		{name: "uncertain canceled write", command: "messages.move", err: context.Canceled, want: "check_state"},
		{name: "send unknown", command: "drafts.send", err: &transport.SubmissionError{Stage: "final reply"}, want: "check_state", recovery: "drafts.reconcile", data: responseData{SendResult: &mail.SendResult{DraftRef: "draft_ref", AttemptID: "attempt", Outcome: mail.SendOutcomeUnknown}}},
		{name: "mirror partial", command: "drafts.send", code: "send_mirror_pending", want: "check_state", recovery: "drafts.reconcile", data: responseData{SendResult: &mail.SendResult{DraftRef: "draft_ref", AttemptID: "attempt", Outcome: mail.SendOutcomeMirrorPending}}},
		{name: "completed draft output", command: "drafts.update", code: "output_too_large", want: "check_state", recovery: "drafts.inspect", data: responseData{draftMutationCompleted: true, Draft: &mail.Draft{Ref: "draft_ref"}}},
		{name: "handoff observation", command: "drafts.handoff", code: "handoff_outcome_unknown", want: "check_state", recovery: "drafts.inspect", data: responseData{DraftHandoff: &draftHandoffResult{DraftRef: "draft_ref", AttemptID: "attempt", DispatchStarted: true, Outcome: draftHandoffUnknown}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.err
			if err == nil {
				err = &mail.OperationError{Code: test.code, Message: test.name}
			}
			value := envelope{SchemaVersion: schemaVersion, Command: test.command, Data: test.data, Error: newErrorData(test.command, test.data, err)}
			payload, err := marshalEnvelope(value)
			if err != nil {
				t.Fatal(err)
			}
			var decoded envelope
			if err := json.Unmarshal(payload, &decoded); err != nil {
				t.Fatal(err)
			}
			assertNextContract(t, decoded.Next)
			if decoded.Next.Do != test.want || decoded.Next.Command != test.recovery || decoded.Next.WaitSeconds != test.wait {
				t.Fatalf("next = %+v, want %s / %s / %d; guidance = %+v", decoded.Next, test.want, test.recovery, test.wait, decoded.Error.Guidance)
			}
			if decoded.Error.Guidance == nil || decoded.Error.Code != value.Error.Code {
				t.Fatalf("lost detailed error evidence: %s", payload)
			}
			reencoded, err := marshalEnvelope(decoded)
			if err != nil || !bytes.Equal(payload, reencoded) {
				t.Fatalf("next action changed across typed finalization: err=%v first=%s second=%s", err, payload, reencoded)
			}
		})
	}
}

func TestNextActionGuidanceTruthTable(t *testing.T) {
	for _, rule := range []struct {
		retryability mail.Retryability
		replay       bool
		want         string
	}{
		{mail.RetrySafe, true, "retry"},
		{mail.RetrySafe, false, "check_state"},
		{mail.RetryUserInputRequired, false, "fix_input"},
		{mail.RetryObserveRequired, false, "check_state"},
		{mail.RetryTerminal, false, "stop"},
	} {
		for _, effect := range []mail.EffectCertainty{mail.EffectNone, mail.EffectComplete, mail.EffectPartial, mail.EffectUnknown, ""} {
			guidance := mail.OperationGuidance{Phase: mail.OperationPhaseValidation, EffectCertainty: effect, Retryability: rule.retryability, ReplayAllowed: rule.replay}
			value := envelope{Error: &errorData{Code: "fixture_failure", Guidance: &guidance}}
			next := envelopeNextAction(value)
			want := rule.want
			if effect != mail.EffectNone {
				want = "check_state"
			}
			if next.Do != want {
				t.Fatalf("guidance=%+v next=%+v want=%s", guidance, next, want)
			}
		}
	}
}

func TestNextActionEveryPublishedFailureEnvelope(t *testing.T) {
	for _, command := range publishedCommandSchemaIDs() {
		t.Run(command, func(t *testing.T) {
			args := strings.Split(command, ".")
			args = append(args, "--unsupported-next-action-test-flag", "--json")
			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), newTestService(), args, &stdout, &stderr); code != 2 {
				t.Fatalf("invalid invocation exit=%d output=%s stderr=%s", code, &stdout, &stderr)
			}
			var value envelope
			if err := json.Unmarshal(stdout.Bytes(), &value); err != nil {
				t.Fatal(err)
			}
			assertNextContract(t, value.Next)
			if value.OK || value.Next.Do != "fix_input" || stderr.Len() != 0 {
				t.Fatalf("invalid invocation envelope=%s stderr=%s", &stdout, &stderr)
			}
		})
	}
}

func TestNextActionSkillContractBudget(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "skills", "mailcli", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(content), "## Error contract\n")
	if !found {
		t.Fatal("missing error contract")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	// The accepted TASK 516 source baseline (2209cfb8) contains 1467 bytes.
	if len(section)*100 > 1467*40 || len(strings.Split(strings.TrimSpace(section), "\n")) > 12 {
		t.Fatalf("error contract exceeds 60%% reduction or 12-line target: bytes=%d", len(section))
	}
	for _, action := range []string{"retry", "fix_input", "check_state", "ask_user", "stop"} {
		if !strings.Contains(section, "| `"+action+"` |") {
			t.Fatalf("missing next.do row %s", action)
		}
	}
}

func TestNextActionEverySourceErrorCode(t *testing.T) {
	codes := sourceErrorCodes(t)
	if len(codes) < 100 {
		t.Fatalf("error inventory unexpectedly incomplete: %d", len(codes))
	}
	for code := range codes {
		for _, command := range []string{"messages.get", "drafts.send", "messages.move", "batch", "update"} {
			t.Run(command+"/"+code, func(t *testing.T) {
				failure := newErrorData(command, responseData{}, &mail.OperationError{Code: code, Message: "source error"})
				value := envelope{SchemaVersion: schemaVersion, Command: command, Error: failure}
				next := envelopeNextAction(value)
				assertNextContract(t, next)
				guidance := failure.Guidance
				if next.Do == "retry" && (!guidance.ReplayAllowed || guidance.Retryability != mail.RetrySafe || guidance.EffectCertainty != mail.EffectNone || strings.Contains(code, "canceled")) {
					t.Fatalf("unsafe retry for %s: next=%+v guidance=%+v", code, next, guidance)
				}
				if guidance.EffectCertainty != mail.EffectNone && next.Do != "check_state" {
					t.Fatalf("effect evidence bypassed: next=%+v guidance=%+v", next, guidance)
				}
				if environmentFailure(code) && guidance.EffectCertainty == mail.EffectNone && guidance.Retryability == mail.RetryUserInputRequired && next.Do != "ask_user" {
					t.Fatalf("environment classified as input: %+v", next)
				}
			})
		}
	}
	t.Logf("tested %d source-declared error codes across five command contexts", len(codes))
}

// Discover declarations from production code rather than maintaining a stale
// catalog. Include typed constants, Code/code initializers, and literal returns
// in ErrorCode methods; dynamic codes use their existing safe fallback.
func sourceErrorCodes(t *testing.T) map[string]bool {
	t.Helper()
	codes := make(map[string]bool)
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		add := func(value ast.Expr) {
			literal, ok := value.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return
			}
			code, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			if code != "" {
				codes[code] = true
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.AssignStmt:
				for index, target := range node.Lhs {
					if name, ok := target.(*ast.Ident); ok && name.Name == "code" && index < len(node.Rhs) {
						add(node.Rhs[index])
					}
				}
			case *ast.CallExpr:
				if name, ok := node.Fun.(*ast.Ident); ok && (strings.HasSuffix(name.Name, "Failure") || strings.HasSuffix(name.Name, "Error")) && len(node.Args) > 0 {
					if literal, ok := node.Args[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
						value, err := strconv.Unquote(literal.Value)
						if err == nil && strings.Contains(value, "_") && strings.IndexFunc(value, func(char rune) bool { return char != '_' && (char < 'a' || char > 'z') }) < 0 {
							add(literal)
						}
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := node.Key.(*ast.Ident); ok && (key.Name == "Code" || key.Name == "code") {
					add(node.Value)
				}
			case *ast.ValueSpec:
				for index, name := range node.Names {
					if index < len(node.Values) && (strings.HasPrefix(name.Name, "Code") || strings.HasSuffix(name.Name, "FailureCode")) {
						add(node.Values[index])
					}
				}
			case *ast.FuncDecl:
				if node.Name.Name == "ErrorCode" && node.Body != nil {
					ast.Inspect(node.Body, func(child ast.Node) bool {
						if result, ok := child.(*ast.ReturnStmt); ok {
							for _, value := range result.Results {
								add(value)
							}
						}
						return true
					})
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return codes
}

func assertNextContract(t *testing.T, next *nextAction) {
	t.Helper()
	if next == nil || next.Why == "" || utf8.RuneCountInString(next.Why) > 120 {
		t.Fatalf("invalid next action: %+v", next)
	}
	switch next.Do {
	case "retry", "fix_input", "check_state", "ask_user", "stop":
	default:
		t.Fatalf("unknown next action: %+v", next)
	}
	if next.Command != "" {
		catalog, err := embeddedCommandSchemaCatalog()
		if err != nil {
			t.Fatal(err)
		}
		if catalog[next.Command] == nil || len(next.Args) == 0 {
			t.Fatalf("unpublished recovery: %+v", next)
		}
	}
}

func TestNextActionPendingAndFinalization(t *testing.T) {
	tests := []struct {
		name string
		data responseData
		want string
	}{
		{name: "completed", data: responseData{SendResult: &mail.SendResult{Outcome: mail.SendOutcomeSent}}},
		{name: "pending mirror", data: responseData{SendResult: &mail.SendResult{DraftRef: "draft_ref", AttemptID: "attempt", Outcome: mail.SendOutcomeMirrorPending}}, want: "check_state"},
		{name: "retained claim", data: responseData{Draft: &mail.Draft{Ref: "draft_ref", SendAttempt: &mail.SendAttempt{Outcome: mail.SendOutcomeUnknown}}}, want: "check_state"},
		{name: "incomplete coverage", data: responseData{SyncCheck: &mail.SyncCheckResult{Complete: false}}, want: "ask_user"},
		{name: "synchronization submitted", data: responseData{SyncResult: &mail.SyncResult{Triggered: true, AccountRef: "account_ref"}}, want: "check_state"},
		{name: "incomplete content", data: responseData{Message: &mail.Message{MissingParts: []string{"body"}}}, want: "ask_user"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := marshalEnvelope(envelope{SchemaVersion: schemaVersion, OK: true, Command: "drafts.send", Data: test.data})
			if err != nil {
				t.Fatal(err)
			}
			var decoded envelope
			if err := json.Unmarshal(payload, &decoded); err != nil {
				t.Fatal(err)
			}
			if test.want == "" {
				if decoded.Next != nil {
					t.Fatalf("completed result has next: %+v", decoded.Next)
				}
				return
			}
			assertNextContract(t, decoded.Next)
			if !decoded.OK || decoded.Next.Do != test.want {
				t.Fatalf("pending result = %s", payload)
			}
			var output bytes.Buffer
			if FinalizeJSON(&output, []string{"drafts", "send", "--json"}, payload, 0, errors.New("cleanup")) != 1 {
				t.Fatal("cleanup did not fail")
			}
			if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.OK || decoded.Next == nil || decoded.Next.Do != "check_state" || decoded.Data.Finalization == nil {
				t.Fatalf("cleanup lost uncertainty: %s", output.String())
			}
		})
	}
}

func TestNextActionRetainedHandoffUsesRunnableInspection(t *testing.T) {
	service, draft := createProjectionHandoffState(t, mail.HandoffOutcomeUnknown)
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), service, []string{"drafts", "inspect", "--ref", draft.Ref, "--fields", "ref", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("inspect exit=%d stderr=%s", code, &stderr)
	}
	var value envelope
	if err := json.Unmarshal(stdout.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	assertNextContract(t, value.Next)
	if value.Next.Do != "check_state" || value.Next.Command != "drafts.inspect" {
		t.Fatalf("handoff recovery = %+v", value.Next)
	}
	stdout.Reset()
	args := append(strings.Split(value.Next.Command, "."), value.Next.Args...)
	if code := Run(context.Background(), service, args, &stdout, &stderr); code != 0 {
		t.Fatalf("emitted recovery is not runnable: exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	if err := json.Unmarshal(stdout.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if !value.OK || value.Data.Draft == nil || value.Data.Draft.HandoffAttempt == nil || value.Data.Draft.HandoffAttempt.ID != draft.HandoffAttempt.ID || value.Data.Draft.HandoffAttempt.Outcome != mail.HandoffOutcomeUnknown {
		t.Fatalf("inspection lost or changed the retained claim: %s", &stdout)
	}
}
