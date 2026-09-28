package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

var errorCodeLiteral = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)+$`)

// sourceErrorCodes discovers every error code literal in production code:
// Code/code fields, error constructors and failure helpers, code variables and
// constants, and ErrorCode method returns.
func sourceErrorCodes(t *testing.T) map[string][]string {
	t.Helper()
	codes := make(map[string][]string)
	fileSet := token.NewFileSet()
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return walkErr
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		add := func(value ast.Expr) {
			literal, ok := value.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return
			}
			code, err := strconv.Unquote(literal.Value)
			if err == nil && errorCodeLiteral.MatchString(code) {
				codes[code] = append(codes[code], fileSet.Position(literal.Pos()).String())
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.KeyValueExpr:
				if key, ok := node.Key.(*ast.Ident); ok && (key.Name == "Code" || key.Name == "code") {
					add(node.Value)
				}
			case *ast.CallExpr:
				name := ""
				switch function := node.Fun.(type) {
				case *ast.Ident:
					name = function.Name
				case *ast.SelectorExpr:
					name = function.Sel.Name
				}
				lower := strings.ToLower(name)
				if len(node.Args) > 0 && (strings.Contains(lower, "error") || strings.Contains(lower, "fail")) {
					add(node.Args[0])
				}
			case *ast.AssignStmt:
				for index, target := range node.Lhs {
					if name, ok := target.(*ast.Ident); ok && (name.Name == "code" || strings.HasSuffix(name.Name, "Code")) && index < len(node.Rhs) {
						add(node.Rhs[index])
					}
				}
			case *ast.ValueSpec:
				for index, name := range node.Names {
					if index < len(node.Values) && (strings.HasPrefix(name.Name, "Code") || strings.HasSuffix(name.Name, "Code")) {
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

func TestErrorCatalogDeclaresEveryEmittedCode(t *testing.T) {
	emitted := sourceErrorCodes(t)
	declared := make(map[string]bool, len(errorCodeDefinitions))
	for _, definition := range errorCodeDefinitions {
		if declared[definition.Code] {
			t.Errorf("duplicate catalog code %s", definition.Code)
		}
		declared[definition.Code] = true
	}
	for code, sites := range emitted {
		if !declared[code] {
			t.Errorf("emitted error code %s is not declared in errorCodeDefinitions (%s)", code, sites[0])
		}
	}
	for code := range declared {
		if _, ok := emitted[code]; !ok {
			t.Errorf("catalog declares %s, which production code no longer emits", code)
		}
	}
	if len(emitted) < 200 {
		t.Fatalf("error code inventory unexpectedly small: %d", len(emitted))
	}
}

func TestErrorCatalogEntriesAreComplete(t *testing.T) {
	published := make(map[string]bool, len(commandContracts))
	for _, contract := range commandContracts {
		published[contract.ID] = true
	}
	codes := make([]string, 0, len(errorCodeDefinitions))
	for _, entry := range errorCatalog() {
		codes = append(codes, entry.Code)
		if strings.TrimSpace(entry.Meaning) == "" || len(entry.Commands) == 0 || len(entry.Guidance) == 0 {
			t.Errorf("incomplete catalog entry %+v", entry)
		}
		covered := 0
		for _, command := range entry.Commands {
			if !published[command] {
				t.Errorf("%s names unpublished command %q", entry.Code, command)
			}
		}
		for _, guidance := range entry.Guidance {
			covered += len(guidance.Commands)
			if !slices.Contains([]string{"retry", "fix_input", "check_state", "ask_user", "stop"}, guidance.Next) {
				t.Errorf("%s has unknown next action %q", entry.Code, guidance.Next)
			}
		}
		if covered != len(entry.Commands) {
			t.Errorf("%s guidance covers %d of %d commands", entry.Code, covered, len(entry.Commands))
		}
	}
	if !sort.StringsAreSorted(codes) {
		t.Error("catalog codes must stay sorted")
	}
}

func TestErrorCatalogScopesStoreAndReferenceCodesToTheirCommands(t *testing.T) {
	storeAndRefCodes := []string{"not_found", "invalid_reference", "stale_reference", "unknown_command",
		"ambiguous_mail_store_generation", "mail_store_not_read_only", "mail_store_path_mismatch", "unsupported_mail_store_schema"}
	for _, command := range []string{"version", "capabilities", "update"} {
		for _, entry := range errorCatalogFor([]commandCapability{{ID: command}}) {
			if slices.Contains(storeAndRefCodes, entry.Code) {
				t.Errorf("%s claims it can emit %s", command, entry.Code)
			}
		}
	}
	for _, entry := range errorCatalogFor([]commandCapability{{ID: "version"}}) {
		if entry.Code == "output_too_large" {
			t.Error("version has no --max-bytes budget but lists output_too_large")
		}
	}
	for _, entry := range errorCatalogFor([]commandCapability{{ID: "messages.get"}}) {
		if slices.Contains(storeAndRefCodes, entry.Code) && entry.Code != "unknown_command" {
			return
		}
	}
	t.Error("messages.get lost its store and reference codes")
}

func TestErrorCatalogIsPublishedWithOutputs(t *testing.T) {
	_, _, plain := captureCapabilitiesJSON(t, "--json")
	if plain.Data.Capabilities.ErrorCodes != nil {
		t.Fatal("ordinary discovery must not include the error catalog")
	}
	_, _, full := captureCapabilitiesJSON(t, "--outputs", "--json")
	if len(full.Data.Capabilities.ErrorCodes) != len(errorCodeDefinitions) {
		t.Fatalf("full catalog has %d entries, want %d", len(full.Data.Capabilities.ErrorCodes), len(errorCodeDefinitions))
	}
	_, _, scoped := captureCapabilitiesJSON(t, "--for", "sync", "--outputs", "--json")
	entries := scoped.Data.Capabilities.ErrorCodes
	if len(entries) == 0 || len(entries) >= len(errorCodeDefinitions) {
		t.Fatalf("scoped catalog has %d entries", len(entries))
	}
	for _, entry := range entries {
		if !slices.Equal(entry.Commands, []string{"sync"}) {
			t.Fatalf("scoped entry %s lists %v", entry.Code, entry.Commands)
		}
	}
}

func catalogGroup(t *testing.T, code, command string) errorCatalogGuidance {
	t.Helper()
	for _, entry := range errorCatalog() {
		if entry.Code != code {
			continue
		}
		for _, group := range entry.Guidance {
			if slices.Contains(group.Commands, command) {
				return group
			}
		}
	}
	t.Fatalf("catalog has no group for %s on %s", code, command)
	return errorCatalogGuidance{}
}

func TestErrorCatalogNamesRecoveryCommands(t *testing.T) {
	inspect := &errorCatalogRecovery{Action: "inspect", Command: "drafts.inspect", Args: []string{"--ref", "REF", "--json"}}
	inspectFull := &errorCatalogRecovery{Action: "inspect", Command: "drafts.inspect", Args: []string{"--ref", "REF", "--view", "full", "--json"}}
	version := &errorCatalogRecovery{Action: "observe", Command: "version", Args: []string{"--json"}}
	accounts := &errorCatalogRecovery{Action: "observe", Command: "accounts.list", Args: []string{"--json"}}
	tests := []struct {
		code, command string
		want          *errorCatalogRecovery
	}{
		{"draft_revision_conflict", "drafts.update", inspectFull},
		{"draft_revision_conflict", "drafts.send", inspectFull},
		{"draft_revision_unavailable", "drafts.send", inspect},
		{"draft_busy", "drafts.update", inspect},
		{"draft_busy", "drafts.send", inspect},
		{"draft_busy", "drafts.create", nil},
		{"draft_busy", "drafts.list", nil},
		{"smtp_source_invalid", "drafts.send", inspect},
		{"update_install_failed", "update", version},
		{"operation_failed", "update", version},
		{"operation_timeout", "update", version},
		{"finalization_failed", "update", version},
		{"operation_failed", "send.setup", accounts},
		{"operation_timeout", "send.setup", accounts},
		{"finalization_failed", "send.setup", accounts},
		{"operation_failed", "messages.get", nil},
	}
	for _, test := range tests {
		got := catalogGroup(t, test.code, test.command)
		if !reflect.DeepEqual(got.Recovery, test.want) {
			t.Errorf("%s on %s recovery = %+v, want %+v", test.code, test.command, got.Recovery, test.want)
		}
		if got.Next != "check_state" || got.EffectCertainty != mail.EffectNone {
			t.Errorf("%s on %s next=%s effect=%s; a recovery command must keep check_state without a proven effect", test.code, test.command, got.Next, got.EffectCertainty)
		}
	}
}

func TestErrorCatalogRecoveryMatchesRuntime(t *testing.T) {
	const ref = "draft_runtime"
	fill := func(recovery *errorCatalogRecovery) []string {
		args := slices.Clone(recovery.Args)
		for index, arg := range args {
			if arg == "REF" {
				args[index] = ref
			}
		}
		return args
	}
	failures := map[string]error{
		"draft_busy":              &mail.OperationError{Code: "draft_busy", Message: "busy", DraftRef: ref},
		"draft_revision_conflict": &mail.DraftRevisionConflict{Ref: ref, ExpectedRevision: "a", CurrentRevision: "b"},
	}
	for code, failure := range failures {
		group := catalogGroup(t, code, "drafts.update")
		runtime := newErrorData("drafts.update", responseData{}, failure).Guidance.Recovery
		if runtime.Command != group.Recovery.Command || !slices.Equal(runtime.Args, fill(group.Recovery)) {
			t.Errorf("%s runtime recovery %s %v differs from the catalog template %s %v", code, runtime.Command, runtime.Args, group.Recovery.Command, fill(group.Recovery))
		}
	}
	update := newErrorData("update", responseData{UpdateResult: &updateResult{FailedPhase: updatePhaseInstaller}}, updateFailure("update_install_failed", "install release: failed")).Guidance.Recovery
	group := catalogGroup(t, "update_install_failed", "update")
	if update.Command != group.Recovery.Command || !slices.Equal(update.Args, group.Recovery.Args) {
		t.Errorf("update_install_failed runtime recovery %s %v differs from the catalog template %s %v", update.Command, update.Args, group.Recovery.Command, group.Recovery.Args)
	}
}

func TestErrorCatalogReclassifiedCodes(t *testing.T) {
	tests := []struct {
		code, command, next string
	}{
		{"account_binding_changed", "send.setup", "ask_user"},
		{"bridge_cleanup_failed", "doctor", "ask_user"},
		{"mail_access_gate_failed", "accounts.list", "ask_user"},
		{"mail_automation_unavailable", "messages.list", "ask_user"},
		{"special_use_mailbox_unresolved", "accounts.list", "ask_user"},
		{"imap_quota_exceeded", "messages.get", "fix_input"},
		{"imap_command_rejected", "messages.get", "stop"},
	}
	for _, test := range tests {
		group := catalogGroup(t, test.code, test.command)
		if group.Next != test.next || group.EffectCertainty != mail.EffectNone {
			t.Errorf("%s on %s next=%s effect=%s, want next=%s with no effect", test.code, test.command, group.Next, group.EffectCertainty, test.next)
		}
	}
}

func TestErrorCatalogLeavesOnlyNamedResidueWithoutRecovery(t *testing.T) {
	allowed := []string{"draft_busy", "hydration_failed", "operation_failed", "store_profile_unverified"}
	groups := 0
	for _, entry := range errorCatalog() {
		for _, group := range entry.Guidance {
			if group.EffectCertainty != mail.EffectNone || group.Next != "check_state" || group.Recovery != nil {
				continue
			}
			groups++
			if !slices.Contains(allowed, entry.Code) {
				t.Errorf("%s on %v is a no-effect check_state group without a recovery command", entry.Code, group.Commands)
			}
		}
	}
	if groups > 4 {
		t.Errorf("%d no-effect check_state groups without a recovery command; the budget is 4", groups)
	}
}
