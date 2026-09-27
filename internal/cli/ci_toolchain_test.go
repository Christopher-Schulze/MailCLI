package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestCIWorkflowRestoresPinnedToolchainAfterSetup(t *testing.T) {
	workflow := readRepositoryFile(t, ".github/workflows/ci.yml")
	marker := "      - name: restore pinned Go toolchain\n        run: |\n"
	before, after, found := strings.Cut(workflow, marker)
	if !found || !strings.Contains(before, "uses: actions/setup-go@") || !strings.Contains(after, "- name: install verification tools") {
		t.Fatal("toolchain restoration must follow setup-go and precede verification tools")
	}
	if strings.Contains(before, "go install ") || strings.Contains(before, "run: go build") || strings.Contains(before, "run: scripts/tests/test.sh") {
		t.Fatal("Go consumers must run after toolchain restoration")
	}
	body, _, found := strings.Cut(after, "\n      - ")
	if !found {
		t.Fatal("restoration must be a separate step before Go consumers")
	}
	pin := regexp.MustCompile(`(?m)^go ([0-9]+\.[0-9]+\.[0-9]+)$`).FindStringSubmatch(readRepositoryFile(t, "go.mod"))
	if len(pin) != 2 {
		t.Fatal("missing exact go.mod pin")
	}
	for _, test := range []struct {
		name     string
		override string
		valid    bool
	}{
		{name: "setup-go override", override: "local", valid: true},
		{name: "existing exact pin", override: "go" + pin[1], valid: true},
		{name: "unrelated conflict", override: "go0.0.0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := filepath.Join(t.TempDir(), "runner-env")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "bash", "-eu", "-c", body)
			command.Dir = repositoryRoot(t)
			command.Env = append(os.Environ(), "GOTOOLCHAIN="+test.override, "GITHUB_ENV="+environment)
			output, err := command.CombinedOutput()
			if !test.valid {
				if err == nil || !strings.Contains(string(output), "Conflicting GOTOOLCHAIN") {
					t.Fatalf("conflicting override was accepted: output=%s err=%v", output, err)
				}
				if _, err := os.Stat(environment); !os.IsNotExist(err) {
					t.Fatalf("failed restoration published runner environment: %v", err)
				}
				return
			}
			if err != nil || !strings.Contains(string(output), "go_toolchain=go"+pin[1]+"\n") {
				t.Fatalf("restoration failed actual toolchain check: output=%s err=%v", output, err)
			}
			persisted, err := os.ReadFile(environment)
			if err != nil || string(persisted) != "GOTOOLCHAIN=go"+pin[1]+"\n" {
				t.Fatalf("subsequent steps lose exact pin: environment=%q err=%v", persisted, err)
			}
		})
	}
}
