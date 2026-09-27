package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestReferenceArgumentNormalization(t *testing.T) {
	for _, test := range []struct {
		name     string
		args     []string
		wantRef  string
		wantJSON bool
	}{
		{name: "flag", args: []string{"--ref", "msg_ref"}, wantRef: "msg_ref"},
		{name: "positional", args: []string{"msg_ref"}, wantRef: "msg_ref"},
		{name: "options after positional", args: []string{"msg_ref", "--limit", "4"}, wantRef: "msg_ref"},
		{name: "options before positional", args: []string{"--limit", "4", "msg_ref"}, wantRef: "msg_ref"},
		{name: "equal flag and positional", args: []string{"--ref", "msg_ref", "msg_ref"}, wantRef: "msg_ref"},
		{name: "repeated equal flags", args: []string{"--ref", "msg_ref", "--ref=msg_ref"}, wantRef: "msg_ref"},
		{name: "flag value begins with option prefix", args: []string{"--ref", "--literal"}, wantRef: "--literal"},
		{name: "flag value is option terminator token", args: []string{"--ref", "--"}, wantRef: "--"},
		{name: "flag value uses equals", args: []string{"--ref=--literal"}, wantRef: "--literal"},
		{name: "value option consumes option-like value", args: []string{"--label", "--literal", "msg_ref"}, wantRef: "msg_ref"},
		{name: "delimiter operand begins with option prefix", args: []string{"--", "--literal"}, wantRef: "--literal"},
		{name: "delimiter operand matches flag", args: []string{"--ref", "msg_ref", "--", "msg_ref"}, wantRef: "msg_ref"},
		{name: "boolean option after positional", args: []string{"msg_ref", "--json"}, wantRef: "msg_ref", wantJSON: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, ref, jsonOutput, stdout, stderr := parseReferenceTestArguments(test.args)
			if code != -1 || ref != test.wantRef || jsonOutput != test.wantJSON || stdout != "" || stderr != "" {
				t.Fatalf("parse = (%d, %q, %t, %q, %q), want ref=%q json=%t", code, ref, jsonOutput, stdout, stderr, test.wantRef, test.wantJSON)
			}
		})
	}
}

func TestReferenceArgumentErrorsAreTypedBeforeDispatch(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "missing", args: []string{}},
		{name: "empty flag", args: []string{"--ref="}},
		{name: "empty positional", args: []string{""}},
		{name: "missing flag value", args: []string{"--ref"}},
		{name: "conflicting flag and positional", args: []string{"--ref", "one", "two"}},
		{name: "conflicting repeated flags", args: []string{"--ref", "one", "--ref", "two"}},
		{name: "conflicting repeated flag and operand", args: []string{"--ref", "one", "--ref", "one", "two"}},
		{name: "excess operands", args: []string{"one", "two"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"--json"}, test.args...)
			code, _, _, stdout, stderr := parseReferenceTestArguments(args)
			if code != 2 || stderr != "" {
				t.Fatalf("parse code=%d stdout=%q stderr=%q, want JSON invalid_argument", code, stdout, stderr)
			}
			var got envelope
			if err := json.Unmarshal([]byte(stdout), &got); err != nil {
				t.Fatalf("decode error envelope %q: %v", stdout, err)
			}
			if got.OK || got.Error == nil || got.Error.Code != "invalid_argument" || got.Command != "messages.get" {
				t.Fatalf("error envelope = %+v, want messages.get invalid_argument", got)
			}
		})
	}
}

func TestNormalizeGlobalJSONHonorsReferenceArityAndDelimiter(t *testing.T) {
	for _, test := range []struct {
		name      string
		args      []string
		want      []string
		requested bool
	}{
		{name: "global before command", args: []string{"--json", "messages", "get", "msg_ref"}, want: []string{"messages", "get", "msg_ref", "--json"}, requested: true},
		{name: "global after reference", args: []string{"messages", "get", "msg_ref", "--json"}, want: []string{"messages", "get", "msg_ref", "--json"}, requested: true},
		{name: "reference flag consumes json-looking value", args: []string{"messages", "get", "--ref", "--json"}, want: []string{"messages", "get", "--ref", "--json"}},
		{name: "reference flag consumes terminator token", args: []string{"messages", "get", "--ref", "--", "--json"}, want: []string{"messages", "get", "--ref", "--", "--json"}, requested: true},
		{name: "draft subject consumes json-looking value", args: []string{"messages", "reply", "--subject", "--json", "--ref", "msg_ref"}, want: []string{"messages", "reply", "--subject", "--json", "--ref", "msg_ref"}},
		{name: "json after delimiter is reference", args: []string{"messages", "get", "--", "--json"}, want: []string{"messages", "get", "--", "--json"}},
		{name: "global json plus delimiter reference", args: []string{"--json", "messages", "get", "--", "--json"}, want: []string{"messages", "get", "--json", "--", "--json"}, requested: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, requested := NormalizeGlobalJSON(test.args)
			if requested != test.requested || !equalStrings(got, test.want) {
				t.Fatalf("NormalizeGlobalJSON(%q) = (%q, %t), want (%q, %t)", test.args, got, requested, test.want, test.requested)
			}
		})
	}
}

func TestReferenceRoutesAcceptCanonicalHelpAndRejectLegacyFlag(t *testing.T) {
	for _, command := range referenceRouteIDs() {
		args := append(strings.Split(command, "."), "--help")
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), newTestService(), args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
			t.Fatalf("%s help code=%d stdout=%q stderr=%q", command, code, stdout.String(), stderr.String())
		}
		wantUsage := "mailcli " + strings.ReplaceAll(command, ".", " ") + " [REF] [options]"
		if !strings.Contains(stdout.String(), wantUsage) || !strings.Contains(stdout.String(), "--ref <ref>") {
			t.Errorf("%s help = %q, want %q and --ref", command, stdout.String(), wantUsage)
		}
	}
	for _, args := range [][]string{
		{"messages", "reply", "--message", "msg_ref"},
		{"messages", "forward", "--message", "msg_ref"},
		{"attachments", "list", "--message", "msg_ref"},
		{"attachments", "save", "--message", "msg_ref"},
		{"drafts", "open", "--message", "msg_ref"},
		{"drafts", "adopt", "--message", "msg_ref"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), newTestService(), args, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "flag provided but not defined: -message") {
			t.Errorf("legacy args %q code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestReferenceConflictsFailBeforeEveryRouteDispatch(t *testing.T) {
	for _, command := range referenceRouteIDs() {
		t.Run(command, func(t *testing.T) {
			args := append(strings.Split(command, "."), "--ref", "one", "two", "--json")
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), nil, args, &stdout, &stderr)
			if code != 2 || stderr.Len() != 0 {
				t.Fatalf("args=%q code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
			}
			var got envelope
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("decode error envelope %q: %v", stdout.String(), err)
			}
			if got.OK || got.Error == nil || got.Error.Code != "invalid_argument" || got.Command != command {
				t.Fatalf("response = %+v, want %s invalid_argument", got, command)
			}
		})
	}
}

func referenceRouteIDs() []string {
	return []string{
		"messages.get", "messages.raw", "messages.state", "messages.thread", "messages.mark", "messages.move", "messages.copy", "messages.delete", "messages.reply", "messages.forward",
		"attachments.list", "attachments.save",
		"drafts.open", "drafts.adopt", "drafts.inspect", "drafts.update", "drafts.send", "drafts.discard", "drafts.reconcile", "drafts.handoff", "drafts.preview", "drafts.edit", "drafts.handoff-reconcile",
	}
}

func parseReferenceTestArguments(args []string) (int, string, bool, string, string) {
	flags := newFlagSet("messages get", io.Discard)
	ref := flags.String("ref", "", "message ref")
	flags.String("label", "", "test value option")
	flags.Int("limit", 10, "test numeric option")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	var stdout, stderr bytes.Buffer
	code := parseFlags(flags, args, &stdout, &stderr)
	return code, *ref, *jsonOutput, stdout.String(), stderr.String()
}

func TestNonReferenceParsersKeepPositionalRejection(t *testing.T) {
	flags := newFlagSet("messages list", io.Discard)
	flags.Bool("json", false, "emit JSON")
	var stdout, stderr bytes.Buffer
	if code := parseFlags(flags, []string{"msg_ref"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), `unexpected argument "msg_ref"`) {
		t.Fatalf("non-reference parse code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
