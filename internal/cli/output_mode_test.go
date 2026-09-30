package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestResolveOutputMode(t *testing.T) {
	for _, test := range []struct {
		name, environment string
		args, want        []string
		jsonOutput, fails bool
	}{
		{"pipe default", "", []string{"version"}, []string{"version", "--json"}, true, false},
		{"environment human", "human", []string{"version"}, []string{"version"}, false, false},
		{"environment json", "json", []string{"version"}, []string{"version", "--json"}, true, false},
		{"explicit json wins", "human", []string{"--json", "version"}, []string{"version", "--json"}, true, false},
		{"explicit human wins", "json", []string{"version", "--human"}, []string{"version"}, false, false},
		{"repeat human", "", []string{"--human", "version", "--human"}, []string{"version"}, false, false},
		{"json false", "json", []string{"--json=false", "version"}, []string{"version"}, false, false},
		{"human false", "human", []string{"--human=false", "version"}, []string{"version", "--json"}, true, false},
		{"invalid boolean", "", []string{"version", "--json=invalid"}, nil, true, true},
		{"conflict", "", []string{"version", "--human", "--json"}, nil, true, true},
		{"reverse conflict", "human", []string{"--json", "version", "--human"}, nil, true, true},
		{"invalid environment", "xml", []string{"version"}, nil, true, true},
		{"invalid overridden environment", "xml", []string{"--human", "version"}, nil, false, true},
		{"subject value", "human", []string{"drafts", "create", "--subject", "--json"}, []string{"drafts", "create", "--subject", "--json"}, false, false},
		{"human value", "", []string{"drafts", "create", "--subject", "--human"}, []string{"drafts", "create", "--subject", "--human", "--json"}, true, false},
		{"delimiter value", "", []string{"drafts", "create", "--subject", "--"}, []string{"drafts", "create", "--subject", "--", "--json"}, true, false},
		{"equal value", "human", []string{"drafts", "create", "--subject=--json"}, []string{"drafts", "create", "--subject=--json"}, false, false},
		{"between nouns", "human", []string{"messages", "--json", "get", "--ref", "msg_x"}, []string{"messages", "get", "--ref", "msg_x", "--json"}, true, false},
		{"between nouns boolean", "human", []string{"messages", "--json=true", "get", "--ref", "msg_x"}, []string{"messages", "get", "--ref", "msg_x", "--json"}, true, false},
		{"operand delimiter", "", []string{"messages", "get", "--", "--human"}, []string{"messages", "get", "--json", "--", "--human"}, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := slices.Clone(test.args)
			args, jsonOutput, err := ResolveOutputMode(test.args, &bytes.Buffer{}, test.environment)
			if (err != nil) != test.fails || jsonOutput != test.jsonOutput {
				t.Fatalf("args=%q json=%t error=%v", args, jsonOutput, err)
			}
			if !test.fails && !slices.Equal(args, test.want) {
				t.Fatalf("args=%q, want %q", args, test.want)
			}
			if !test.fails {
				_, requested := NormalizeGlobalJSON(args)
				if requested != jsonOutput {
					t.Fatalf("dispatcher mode=%t, selected mode=%t", requested, jsonOutput)
				}
			}
			if !slices.Equal(test.args, original) {
				t.Fatal("input arguments changed")
			}
		})
	}
}

func TestResolveOutputModeTerminal(t *testing.T) {
	_, terminal := openEditorTestTerminal(t)
	for _, test := range []struct {
		environment string
		args        []string
		wantJSON    bool
	}{
		{"", []string{"version"}, false},
		{"json", []string{"version"}, true},
		{"human", []string{"version", "--json"}, true},
		{"json", []string{"version", "--human"}, false},
	} {
		args, jsonOutput, err := ResolveOutputMode(test.args, terminal, test.environment)
		if err != nil || jsonOutput != test.wantJSON {
			t.Fatalf("args=%q environment=%q json=%t error=%v", args, test.environment, jsonOutput, err)
		}
	}
}

func TestOutputModePreservesEveryPublishedFlagValue(t *testing.T) {
	for _, contract := range commandContracts {
		arity := commandGlobalJSONFlagArity(&contract)
		for name, takesValue := range arity {
			if !takesValue {
				continue
			}
			args := append(strings.Split(contract.ID, "."), "--"+name, "--human")
			normalized, jsonOutput, err := ResolveOutputMode(args, &bytes.Buffer{}, "human")
			if err != nil || jsonOutput || !slices.Equal(normalized, args) {
				t.Fatalf("%s --%s: args=%q json=%t error=%v", contract.ID, name, normalized, jsonOutput, err)
			}
		}
	}
}

func TestDefaultJSONErrorsMatchExplicitJSONForEveryPublishedCommand(t *testing.T) {
	for _, contract := range commandContracts {
		if !contract.published {
			continue
		}
		t.Run(contract.ID, func(t *testing.T) {
			args := append(strings.Split(contract.ID, "."), "--invalid-output-mode-probe")
			normalized, jsonOutput, err := ResolveOutputMode(args, &bytes.Buffer{}, "")
			if err != nil || !jsonOutput {
				t.Fatalf("mode=%t error=%v", jsonOutput, err)
			}
			var automatic, explicit, stderr bytes.Buffer
			code := Run(context.Background(), newTestService(), normalized, &automatic, &stderr)
			explicitCode := Run(context.Background(), newTestService(), append(args, "--json"), &explicit, &stderr)
			var response envelope
			if code == 0 || code != explicitCode || stderr.Len() != 0 || !bytes.Equal(automatic.Bytes(), explicit.Bytes()) {
				t.Fatalf("code=%d explicit=%d automatic=%q explicit=%q stderr=%q", code, explicitCode, &automatic, &explicit, &stderr)
			}
			if err := json.Unmarshal(automatic.Bytes(), &response); err != nil || response.OK || response.Error == nil || response.Command != contract.ID {
				t.Fatalf("response=%+v decode=%v", response, err)
			}
		})
	}
}
