package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

func TestReconciledDiscoveryDocumentationMatchesRuntime(t *testing.T) {
	for _, path := range []string{"README.md", "docs/documentation.md"} {
		content := readRepositoryFile(t, path)
		for _, required := range []string{"schema_ref.resolve", "--schemas"} {
			if !strings.Contains(content, required) {
				t.Errorf("%s omits scoped schema retrieval %s", path, required)
			}
		}
	}
	code, output, response := captureCapabilitiesJSON(t, "--for", "messages.get", "--json")
	if code != 0 || response.Data.Capabilities == nil || len(response.Data.Capabilities.Commands) != 1 {
		t.Fatalf("discovery code=%d output=%s", code, output)
	}
	resolveCapabilitySchema(t, response.Data.Capabilities.Commands[0])
}

func TestProjectedPagesExposeOnlyAvailableContinuation(t *testing.T) {
	for _, cursor := range []string{"", "continuation"} {
		for _, selector := range []string{"subject", "all"} {
			fields := map[string]struct{}{selector: {}}
			pages := []json.RawMessage{
				*projectMessageListPage(mail.MessagePage{NextCursor: cursor}, fields),
				*projectSearchPage(mail.SearchPage{NextCursor: cursor}, fields),
			}
			for index, page := range pages {
				var result map[string]json.RawMessage
				if err := json.Unmarshal(page, &result); err != nil {
					t.Fatal(err)
				}
				value, present := result["next_cursor"]
				if present != (cursor != "") || present && string(value) != `"`+cursor+`"` {
					t.Fatalf("page=%d selector=%s cursor=%q output=%s", index, selector, cursor, page)
				}
			}
		}
	}
}

func TestStateOnlyDraftRecoveryUsesDocumentedMetadataView(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(testGateway{}, filepath.Join(t.TempDir(), "drafts"))
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), service, []string{
		"drafts", "create", "--to", "recipient@example.com", "--body", "retained review body", "--json",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("create code=%d output=%s stderr=%s", code, &stdout, &stderr)
	}
	var created envelope
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Data.Draft == nil || created.Data.Draft.Ref == "" {
		t.Fatalf("create omitted draft identity: %s", &stdout)
	}
	ref := created.Data.Draft.Ref
	stdout.Reset()
	stderr.Reset()
	recovery := draftInspectRecovery(ref, false)
	args := append([]string{"drafts", "inspect"}, recovery.Args...)
	if code := Run(context.Background(), service, args, &stdout, &stderr); code != 0 {
		t.Fatalf("recovery code=%d output=%s stderr=%s", code, &stdout, &stderr)
	}
	var result struct {
		Data struct {
			Draft      map[string]json.RawMessage `json:"draft"`
			Projection struct {
				View string `json:"view"`
			} `json:"projection"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Data.Projection.View != outputViewMetadata || string(result.Data.Draft["ref"]) != `"`+ref+`"` || result.Data.Draft["body"] != nil {
		t.Fatalf("state recovery changed view or exposed body: %s", &stdout)
	}
	content := readRepositoryFile(t, "docs/documentation.md")
	if !strings.Contains(content, "State-only draft recovery uses the default metadata view") {
		t.Fatal("documentation differs from executed recovery view")
	}
	guide := readRepositoryFile(t, "skills/mailcli/references/output-and-recovery.md")
	if !strings.Contains(guide, "(state "+result.Data.Projection.View+", conflicts/completed full)") {
		t.Fatal("portable recovery guide differs from executed recovery view")
	}
}
