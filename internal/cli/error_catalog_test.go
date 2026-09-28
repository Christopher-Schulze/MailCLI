package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
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
