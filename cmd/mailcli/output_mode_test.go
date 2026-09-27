package main

import (
	"encoding/json"
	"strings"
	"testing"

	"mailcli/internal/mailstore"
)

func TestRunOutputModeBeforeInitialization(t *testing.T) {
	for _, test := range []struct {
		name, environment string
		args              []string
		wantJSON          bool
		wantCode          int
		wantError         string
	}{
		{"pipe version", "", []string{"version"}, true, 0, ""},
		{"environment human", "human", []string{"version"}, false, 0, ""},
		{"explicit human", "json", []string{"--human", "version"}, false, 0, ""},
		{"explicit json", "human", []string{"--json", "version"}, true, 0, ""},
		{"default error", "", []string{"unknown"}, true, 2, "unknown_command"},
		{"human error", "human", []string{"unknown"}, false, 2, ""},
		{"invalid environment", "invalid", []string{"version"}, true, 2, "invalid_argument"},
		{"conflict", "human", []string{"version", "--json", "--human"}, true, 2, "invalid_argument"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("MAILCLI_OUTPUT", test.environment)
			factoryCalls := 0
			stdout, stderr, code := runWithArgsAndStderrUsing(t, append([]string{"mailcli"}, test.args...), func() int {
				return runWithConfigFactory(nil, func() *invocationTransport {
					factoryCalls++
					return &invocationTransport{}
				}, func() (mailstore.Config, error) {
					t.Fatal("store configuration requested")
					return mailstore.Config{}, nil
				})
			})
			if code != test.wantCode {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if test.wantJSON {
				var response struct {
					OK      bool                   `json:"ok"`
					Command string                 `json:"command"`
					Error   *struct{ Code string } `json:"error"`
				}
				if err := json.Unmarshal([]byte(stdout), &response); err != nil || stderr != "" {
					t.Fatalf("stdout=%q stderr=%q decode=%v", stdout, stderr, err)
				}
				if test.wantCode == 0 && (!response.OK || response.Command != "version" || response.Error != nil) {
					t.Fatalf("success envelope=%+v", response)
				}
				if test.wantError != "" && (response.OK || response.Error == nil || response.Error.Code != test.wantError) {
					t.Fatalf("failure envelope=%+v", response)
				}
			} else if test.wantCode == 0 && !strings.HasPrefix(stdout, "mailcli ") {
				t.Fatalf("human stdout=%q", stdout)
			} else if test.wantCode != 0 && (stdout != "" || !strings.Contains(stderr, "unknown command")) {
				t.Fatalf("human error stdout=%q stderr=%q", stdout, stderr)
			}
			wantFactories := 1
			if test.wantError == "invalid_argument" {
				wantFactories = 0
			}
			if factoryCalls != wantFactories {
				t.Fatalf("transport factories=%d, want %d", factoryCalls, wantFactories)
			}
		})
	}
}
