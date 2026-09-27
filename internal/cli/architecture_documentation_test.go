package cli

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// The transport classifier is the one named cross-package extension point;
// private draft implementation filenames are not architectural contracts.
const architectureExtensionPoint = "internal/transport/classification.go"

func validateArchitectureSourceReferences(content string) error {
	pattern := regexp.MustCompile(`\b(?:[A-Za-z0-9_.-]+/)*[A-Za-z0-9_-]+\.go\b`)
	for _, path := range pattern.FindAllString(content, -1) {
		if path != architectureExtensionPoint {
			return fmt.Errorf("architecture contains an implementation filename: %s", path)
		}
	}
	return nil
}

func TestArchitectureDocumentationUsesResponsibilities(t *testing.T) {
	documentation := readRepositoryFile(t, "docs/documentation.md")
	start := strings.Index(documentation, "\n## Architecture\n")
	end := strings.Index(documentation, "\n## Platform and freshness boundaries\n")
	if start < 0 || end <= start {
		t.Fatal("architecture section boundaries are missing")
	}
	architecture := documentation[start:end]
	if err := validateArchitectureSourceReferences(architecture); err != nil {
		t.Fatal(err)
	}
	for _, fact := range []string{
		"bounded reference-ordered pagination and directory revision checks",
		"streaming metadata reads and skipped-string validation",
		"pure input, content, address, and resource validation",
		"bounded JSON state files and atomic publication",
		"references, leases, and mutation cleanup",
		"send/save claim encoding, validation, and transitions",
		"attachment fingerprints and snapshot checks",
		"envelope and Sent-message identity fingerprints",
		"replayable MIME spools and transport adapters",
		"direct SMTP/IMAP delivery helpers and recipient normalization",
		"send and reconciliation orchestration",
		"retained historical native-save reconciliation",
		"stale-draft, orphan-lock, and orphan-claim/spool/snapshot cleanup",
		"shared cancellation and lock-wait classification",
		"those helpers do not call back into CLI, store",
	} {
		if !strings.Contains(architecture, fact) {
			t.Errorf("architecture lost responsibility or boundary %q", fact)
		}
	}
	extension := readRepositoryFile(t, architectureExtensionPoint)
	if !strings.Contains(architecture, architectureExtensionPoint) ||
		!strings.Contains(extension, "func IsTransientReadFailure(err error) bool") {
		t.Fatal("allowlisted transport classification extension point is missing")
	}
}

func TestArchitectureSourceReferencesRejectPrivateCatalogues(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		valid   bool
	}{
		{name: "package and symbol", content: "internal/mail Gateway SendTransport", valid: true},
		{name: "extension point", content: architectureExtensionPoint, valid: true},
		{name: "documentation URL", content: "https://support.google.com/mail", valid: true},
		{name: "private filename", content: "draft_store.go owns drafts"},
		{name: "private path", content: "internal/mail/draft_store.go"},
		{name: "basename is not allowlisted", content: "classification.go"},
		{name: "adjacent extension file", content: "internal/transport/classification_test.go"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateArchitectureSourceReferences(test.content); (err == nil) != test.valid {
				t.Fatalf("source reference validation = %v, want valid=%t", err, test.valid)
			}
		})
	}
}
