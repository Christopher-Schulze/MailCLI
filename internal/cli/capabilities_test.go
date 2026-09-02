package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"testing"

	"mailcli/internal/mail"
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
	if !manifest.Limits.RawMIMESend || !manifest.Limits.RawMIMERead || manifest.Limits.OwnsMailIndex || manifest.Limits.BackgroundProcess {
		t.Fatalf("manifest limits = %+v", manifest.Limits)
	}
	if manifest.Limits.ComposeAttachmentWrite {
		t.Fatal("Mail 16 scripted compose attachment writes must not be advertised")
	}
	if manifest.Limits.ComposeWrite {
		t.Fatal("Mail 16 scripted compose writes must not be advertised")
	}
	if !manifest.Limits.VisibleComposeHandoff || !manifest.Limits.VisibleAttachmentHandoff {
		t.Fatal("visible system compose handoff must be advertised")
	}
	if manifest.Limits.SendTransport != "smtp" {
		t.Fatalf("send transport = %q, want smtp", manifest.Limits.SendTransport)
	}
	if manifest.Limits.MaximumPageSize != 25 || manifest.Limits.MaximumDraftInputBytes != 16*1024*1024 {
		t.Fatalf("manifest bounds = %+v", manifest.Limits)
	}
	if manifest.Limits.MaximumComposeBodyBytes != mail.MaximumComposeBodyBytes {
		t.Fatalf("maximum compose body bytes = %d", manifest.Limits.MaximumComposeBodyBytes)
	}
	if manifest.Limits.MaximumDraftSubjectBytes != mail.MaximumDraftSubjectBytes {
		t.Fatalf("maximum draft subject bytes = %d", manifest.Limits.MaximumDraftSubjectBytes)
	}
}

func TestCapabilityCommandInventory(t *testing.T) {
	want := []string{
		"capabilities", "version", "update", "doctor", "accounts.list", "mailboxes.list", "mailboxes.resolve",
		"messages.list", "messages.filter", "messages.search", "messages.get", "messages.raw",
		"attachments.list", "attachments.save", "drafts.create", "drafts.list", "drafts.inspect",
		"drafts.preview", "drafts.edit", "drafts.handoff", "drafts.update", "drafts.save", "drafts.open",
		"drafts.send", "send.setup", "drafts.reconcile", "drafts.discard", "drafts.prune",
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
	if send.EffectClass != "smtp-send" || send.Confirmation != "required-flag" ||
		send.StoreDependency != "draft-store" || send.MailAppDependency != "none" ||
		!slices.Equal(send.ResultStates, []string{"sent", "sent_mirror_pending"}) {
		t.Fatalf("drafts.send capability = %+v", send)
	}
	setup := manifest.Commands[slices.Index(got, "send.setup")]
	if setup.EffectClass != "keychain-write" || setup.Confirmation != "none" ||
		setup.StoreDependency != "none" || setup.MailAppDependency != "none" ||
		!slices.Equal(setup.ResultStates, []string{"stored", "removed"}) {
		t.Fatalf("send.setup capability = %+v", setup)
	}
	save := manifest.Commands[slices.Index(got, "drafts.save")]
	if save.EffectClass != "unsupported" || save.StoreDependency != "draft-store" || save.MailAppDependency != "none" ||
		!slices.Equal(save.ResultStates, []string{"compose_automation_unsupported"}) {
		t.Fatalf("drafts.save capability = %+v", save)
	}
	for _, id := range []string{"messages.reply", "messages.forward"} {
		command := manifest.Commands[slices.Index(got, id)]
		if command.StoreDependency != "draft-store" || command.MailAppDependency != "none" {
			t.Fatalf("%s capability = %+v", id, command)
		}
	}
}

// TestCapabilityMailAppDependencies pins every declared Mail.app dependency to an
// audited value so label drift cannot reintroduce undeclared automation surfaces.
// Evidence for each value lives in docs/tasks/done/027-correct-stale-mail-app-capability-labels.md.
func TestCapabilityMailAppDependencies(t *testing.T) {
	want := map[string]string{
		"capabilities":      "none",
		"version":           "none",
		"update":            "none",
		"doctor":            "optional-automation",
		"accounts.list":     "fallback-automation",
		"mailboxes.list":    "none",
		"mailboxes.resolve": "none",
		"messages.list":     "fallback-automation",
		"messages.filter":   "none",
		"messages.search":   "none",
		"messages.get":      "none",
		"messages.raw":      "none",
		"attachments.list":  "none",
		"attachments.save":  "none",
		"drafts.create":     "none",
		"drafts.list":       "none",
		"drafts.inspect":    "none",
		"drafts.preview":    "none",
		"drafts.edit":       "none",
		"drafts.handoff":    "system-compose-service",
		"drafts.update":     "none",
		"drafts.save":       "none",
		"drafts.open":       "fallback-automation",
		"drafts.send":       "none",
		"send.setup":        "none",
		"drafts.reconcile":  "none",
		"drafts.discard":    "none",
		"drafts.prune":      "none",
		"messages.reply":    "none",
		"messages.forward":  "none",
		"messages.mark":     "none",
		"messages.move":     "none",
		"messages.copy":     "none",
		"messages.delete":   "none",
		"sync":              "optional",
	}
	manifest := capabilities()
	seen := make(map[string]struct{}, len(manifest.Commands))
	for _, command := range manifest.Commands {
		seen[command.ID] = struct{}{}
		expected, audited := want[command.ID]
		if !audited {
			t.Fatalf("command %q has no audited mail_app_dependency expectation", command.ID)
		}
		if command.MailAppDependency != expected {
			t.Fatalf("%s mail_app_dependency = %q, want %q", command.ID, command.MailAppDependency, expected)
		}
	}
	for id := range want {
		if _, declared := seen[id]; !declared {
			t.Fatalf("audited command %q is missing from the manifest", id)
		}
	}
}

func TestCapabilitiesNeedNoMailService(t *testing.T) {
	for _, args := range [][]string{{"capabilities"}, {"capabilities", "--json"}, {"--json", "capabilities"}} {
		if RequiresMailService(args) {
			t.Fatalf("RequiresMailService(%q) = true", args)
		}
	}
}
