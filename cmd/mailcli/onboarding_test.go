package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

func TestFreshHomeExplainsSetupWithoutCreatingState(t *testing.T) {
	for _, test := range []struct {
		name      string
		emptyRoot bool
		args      []string
	}{
		{name: "Mail never opened", args: []string{"doctor"}},
		{name: "Mail root without a generation", emptyRoot: true, args: []string{"doctor"}},
		{name: "account discovery", args: []string{"accounts", "list"}},
		{name: "mailbox discovery", args: []string{"mailboxes", "list"}},
		{name: "inbox listing", args: []string{"messages", "list"}},
		{name: "metadata filtering", args: []string{"messages", "filter", "--read", "false"}},
		{name: "body search", args: []string{"messages", "search", "--query", "invoice"}},
		{name: "server discovery", args: []string{"messages", "new"}},
		{name: "sync observation", args: []string{"sync", "--check"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			if test.emptyRoot {
				if err := os.MkdirAll(filepath.Join(home, "Library", "Mail"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			var before []string
			if err := filepath.WalkDir(home, func(path string, _ os.DirEntry, err error) error {
				before = append(before, path)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"mailcli"}, test.args...)
			stdout, stderr, code := runWithArgsAndStderrUsing(t, append(args, "--json"), run)
			var result struct {
				OK    bool `json:"ok"`
				Error struct {
					Code     string                 `json:"code"`
					Guidance mail.OperationGuidance `json:"guidance"`
				} `json:"error"`
				Next struct {
					Do, Why string
				} `json:"next"`
			}
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatalf("invalid envelope: %v, stdout=%s", err, stdout)
			}
			if code != 1 || stderr != "" || result.OK || result.Error.Code != "mail_store_not_initialized" || result.Next.Do != "ask_user" {
				t.Fatalf("first-use result exit=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			if result.Error.Guidance.EffectCertainty != mail.EffectNone || result.Error.Guidance.ReplayAllowed || !strings.Contains(result.Next.Why, "Mail.app") || !strings.Contains(result.Next.Why, "doctor --json") {
				t.Fatalf("unsafe or unhelpful first-use guidance: %s", stdout)
			}
			var after []string
			if err := filepath.WalkDir(home, func(path string, _ os.DirEntry, err error) error {
				after = append(after, path)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if strings.Join(before, "\n") != strings.Join(after, "\n") {
				t.Fatalf("command created first-use state: before=%v after=%v", before, after)
			}
		})
	}
}
