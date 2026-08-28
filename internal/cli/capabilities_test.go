package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"testing"
)

func TestCapabilitiesJSONContract(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := Run(context.Background(), nil, []string{"capabilities", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if !response.OK || response.Command != "capabilities" || response.Data.Capabilities == nil {
		t.Fatalf("response = %+v", response)
	}
	manifest := response.Data.Capabilities
	if manifest.SchemaVersion != capabilitySchemaVersion || manifest.Name != name || manifest.Version != version {
		t.Fatalf("manifest identity = %+v", manifest)
	}
	if manifest.Limits.RawMIMESend || !manifest.Limits.RawMIMERead || manifest.Limits.OwnsMailIndex || manifest.Limits.BackgroundProcess {
		t.Fatalf("manifest limits = %+v", manifest.Limits)
	}
	if manifest.Limits.MaximumPageSize != 25 || manifest.Limits.MaximumDraftInputBytes != 16*1024*1024 {
		t.Fatalf("manifest bounds = %+v", manifest.Limits)
	}
}

func TestCapabilityCommandInventory(t *testing.T) {
	want := []string{
		"capabilities", "version", "doctor", "accounts.list", "mailboxes.list", "mailboxes.resolve",
		"messages.list", "messages.filter", "messages.search", "messages.get", "messages.raw",
		"attachments.list", "attachments.save", "drafts.create", "drafts.list", "drafts.inspect",
		"drafts.update", "drafts.save", "drafts.open", "drafts.send", "drafts.reconcile", "drafts.discard",
		"messages.reply", "messages.forward", "messages.mark", "messages.move", "messages.copy",
		"messages.delete", "sync",
	}
	manifest := capabilities()
	got := make([]string, 0, len(manifest.Commands))
	seen := make(map[string]struct{}, len(manifest.Commands))
	for _, command := range manifest.Commands {
		if _, exists := seen[command.ID]; exists {
			t.Fatalf("duplicate command ID %q", command.ID)
		}
		seen[command.ID] = struct{}{}
		got = append(got, command.ID)
		if command.EffectClass == "" || command.Confirmation == "" || command.StoreDependency == "" ||
			command.MailAppDependency == "" || len(command.ResultStates) == 0 {
			t.Fatalf("incomplete capability = %+v", command)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("command IDs = %q, want %q", got, want)
	}
	send := manifest.Commands[slices.Index(got, "drafts.send")]
	if send.EffectClass != "external-effect" || send.Confirmation != "required-flag" ||
		!slices.Equal(send.ResultStates, []string{"sent_store_observed", "accepted_by_mail", "outcome_unknown"}) {
		t.Fatalf("drafts.send capability = %+v", send)
	}
}

func TestCapabilitiesNeedNoMailService(t *testing.T) {
	for _, args := range [][]string{{"capabilities"}, {"capabilities", "--json"}, {"--json", "capabilities"}} {
		if RequiresMailService(args) {
			t.Fatalf("RequiresMailService(%q) = true", args)
		}
	}
}
