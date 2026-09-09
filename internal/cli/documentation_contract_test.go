package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOperationalDocumentationMatchesRuntimeContracts(t *testing.T) {
	artifacts := readOperationalDocumentation(t)
	help := renderTopLevelHelp(t)
	assertCapabilitySemantics(t, capabilities())
	assertDocumentationClaims(t, artifacts)
	assertHelpClaims(t, help)
}

func assertCapabilitySemantics(t *testing.T, manifest capabilityManifest) {
	t.Helper()
	send := findCapability(t, manifest, "drafts.send")
	if send.EffectClass != "smtp-send" || send.StoreDependency != "draft-store" ||
		send.MailAppDependency != "none" || !containsAll(send.ResultStates, "sent", "sent_mirror_pending") {
		t.Fatalf("drafts.send capability = %+v", send)
	}
	reconcile := findCapability(t, manifest, "drafts.reconcile")
	if reconcile.EffectClass != "local-write+imap-write" ||
		reconcile.StoreDependency != "draft-store+mail-store-if-baseline" ||
		reconcile.MailAppDependency != "none" ||
		!containsAll(reconcile.ResultStates, "sent", "sent_mirror_pending", "outcome_unknown") {
		t.Fatalf("drafts.reconcile capability = %+v", reconcile)
	}
}

func assertDocumentationClaims(t *testing.T, artifacts map[string]string) {
	t.Helper()
	for name, content := range artifacts {
		lower := strings.ToLower(content)
		for _, stale := range []string{
			"keep the last uid",
			"keeping the last uid",
			"keep the last-uid behavior",
			"keeping the last-uid behavior",
			"last verified uid",
		} {
			if strings.Contains(lower, stale) {
				t.Errorf("%s contains stale Message-ID selection claim %q", name, stale)
			}
		}
		for _, claim := range []struct {
			name string
			text string
		}{
			{name: "ambiguous identity", text: "imap_ambiguous_message_id"},
			{name: "partial content state", text: "content_complete"},
			{name: "partial content evidence", text: "missing_parts"},
			{name: "direct recovery", text: "drafts reconcile"},
			{name: "accepted SMTP evidence", text: "submission_accepted"},
			{name: "Sent-copy evidence", text: "sent_copy_observed"},
		} {
			if !strings.Contains(content, claim.text) {
				t.Errorf("%s omits %s contract (%q)", name, claim.name, claim.text)
			}
		}
	}
}

func assertHelpClaims(t *testing.T, help string) {
	t.Helper()
	for _, claim := range []string{
		"Mail.app",
		"Direct SMTP send and IMAP mutations work without Mail.app",
		"Mail 16 scripted draft save remains disabled",
	} {
		if !strings.Contains(help, claim) {
			t.Errorf("top-level help omits %q contract: %s", claim, help)
		}
	}
}

func readOperationalDocumentation(t *testing.T) map[string]string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate the contract test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	paths := []string{"README.md", "docs/documentation.md", "skills/mailcli/SKILL.md"}
	artifacts := make(map[string]string, len(paths))
	for _, path := range paths {
		content, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		artifacts[path] = string(content)
	}
	return artifacts
}

func renderTopLevelHelp(t *testing.T) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), newTestService(), []string{"help"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("top-level help code = %d, stderr = %q", code, stderr.String())
	}
	return stdout.String()
}

func findCapability(t *testing.T, manifest capabilityManifest, id string) commandCapability {
	t.Helper()
	for _, command := range manifest.Commands {
		if command.ID == id {
			return command
		}
	}
	t.Fatalf("capability %q is missing", id)
	return commandCapability{}
}

func containsAll(values []string, wanted ...string) bool {
	for _, value := range wanted {
		found := false
		for _, candidate := range values {
			if candidate == value {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
