package cli

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestUpdateReporterTerminalPolicyAndActionEvidence(t *testing.T) {
	for _, test := range []struct {
		name, term string
		noColor    bool
		enabled    bool
		terminal   bool
		failure    bool
		animated   bool
		colored    bool
	}{
		{name: "terminal success", term: "xterm-256color", enabled: true, terminal: true, animated: true, colored: true},
		{name: "terminal failure", term: "xterm", enabled: true, terminal: true, failure: true, animated: true, colored: true},
		{name: "color opt-out", term: "xterm", enabled: true, terminal: true, noColor: true, animated: true},
		{name: "dumb terminal", term: "dumb", enabled: true, terminal: true},
		{name: "redirected output", term: "xterm", enabled: true},
		{name: "JSON disabled reporter", term: "xterm", terminal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("TERM", test.term)
			t.Setenv("NO_COLOR", "")
			if !test.noColor {
				if err := os.Unsetenv("NO_COLOR"); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			reporter := newUpdateReporter(&output, test.enabled, test.terminal)
			cause := errors.New("fixture verification failed")
			calls := 0
			err := reporter.step("Verifying release", func() error {
				calls++
				if test.failure {
					return cause
				}
				return nil
			})
			if calls != 1 || reporter.animated != test.animated || reporter.colored != test.colored ||
				(test.failure && err != cause) || (!test.failure && err != nil) {
				t.Fatalf("reporter policy/action changed: %+v calls=%d error=%v", reporter, calls, err)
			}
			text := output.String()
			if !test.enabled {
				if text != "" {
					t.Fatalf("disabled reporter emitted text: %q", text)
				}
				return
			}
			if strings.Contains(text, "\x1b[36m") != test.colored || strings.Contains(text, "\r") != test.animated {
				t.Fatalf("terminal controls leaked or missing: %q", text)
			}
			if !test.animated && text != "Verifying release...\n" {
				t.Fatalf("plain progress changed: %q", text)
			}
			if test.animated && (!strings.Contains(text, "  ") || strings.Contains(text, "✗") != test.failure || strings.Contains(text, "✓") == test.failure) {
				t.Fatalf("action outcome was misreported: %q", text)
			}
		})
	}
}

func TestUpdateReporterOutcomePresentation(t *testing.T) {
	available := true
	for _, test := range []struct {
		name, plain, terminal string
		result                updateResult
	}{
		{name: "verified update", result: updateResult{CurrentVersion: "1.5.0", LatestVersion: "1.5.1", Updated: true}, plain: "Updated mailcli from 1.5.0 to 1.5.1.\n", terminal: "Updated mailcli  1.5.0 → 1.5.1"},
		{name: "available only", result: updateResult{CurrentVersion: "1.5.0", LatestVersion: "1.5.1", UpdateAvailable: &available}, plain: "Update available: mailcli 1.5.0 -> 1.5.1; run `mailcli update` to install it.\n", terminal: "run `mailcli update`"},
		{name: "current", result: updateResult{CurrentVersion: "1.5.1", LatestVersion: "1.5.1"}, plain: "Already up to date (mailcli 1.5.1).\n", terminal: "Already up to date (mailcli 1.5.1)."},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("TERM", "xterm")
			t.Setenv("NO_COLOR", "1")
			for _, terminal := range []bool{false, true} {
				var output bytes.Buffer
				newUpdateReporter(&output, true, terminal).result(test.result)
				if !terminal && output.String() != test.plain || terminal && (!strings.Contains(output.String(), test.terminal) || strings.Contains(output.String(), "\x1b")) {
					t.Fatalf("terminal=%t outcome=%q", terminal, output.String())
				}
			}
		})
	}
}
