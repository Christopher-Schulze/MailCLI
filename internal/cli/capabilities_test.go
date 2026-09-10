package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
	"mailcli/internal/transport/imapclient"
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
	if !reflect.DeepEqual(manifest.Limits.SupportedProviders, transport.SupportedProviders()) ||
		manifest.Limits.UnsupportedProviderCode != transport.CodeUnsupportedProvider ||
		manifest.Limits.ProviderSupportDescription != transport.ProviderSupportDescription() {
		t.Fatalf("provider support = %+v", manifest.Limits)
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
	if manifest.DraftSavePolicy.NewNativeSave != "rejected_before_mail_contact" ||
		manifest.DraftSavePolicy.LegacyClaimHandling != "reconcile_only" ||
		manifest.DraftSavePolicy.SafeRecoveryCommand != "mailcli drafts save --ref <DRAFT_REF> --json" {
		t.Fatalf("draft save policy = %+v", manifest.DraftSavePolicy)
	}
	if !manifest.SyncCheckPolicy.IncompleteIsSuccessfulResult ||
		manifest.SyncCheckPolicy.DefaultIncompleteExitCode != 0 ||
		manifest.SyncCheckPolicy.RequireCompleteFlag != "--require-complete" ||
		manifest.SyncCheckPolicy.RequireCompleteIncompleteExitCode != syncCheckIncompleteExitCode {
		t.Fatalf("sync check policy = %+v", manifest.SyncCheckPolicy)
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"require_complete_incomplete_exit_code":3`)) {
		t.Fatalf("serialized sync check policy = %s", stdout.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"imap_operation_contract":[{"operation":"LIST"`)) {
		t.Fatalf("serialized IMAP operation contract = %s", stdout.String())
	}
}

func TestCapabilityCommandInventory(t *testing.T) {
	want := []string{
		"capabilities", "version", "update", "doctor", "batch", "accounts.list", "mailboxes.list", "mailboxes.resolve",
		"messages.list", "messages.filter", "messages.search", "messages.get", "messages.raw",
		"attachments.list", "attachments.save", "drafts.create", "drafts.list", "drafts.inspect",
		"drafts.preview", "drafts.edit", "drafts.handoff", "drafts.update", "drafts.save", "drafts.open",
		"drafts.send", "send.setup", "drafts.reconcile", "drafts.discard", "drafts.prune",
		"messages.reply", "messages.forward", "messages.mark", "messages.move", "messages.copy",
		"messages.delete", "sync", "drafts.handoff-reconcile",
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
	accounts := manifest.Commands[slices.Index(got, "accounts.list")]
	if !slices.Equal(accounts.ResultStates, []string{"complete", "partial", "bounded_identity_coverage"}) {
		t.Fatalf("accounts.list result states = %+v", accounts.ResultStates)
	}
	if manifest.Limits.SenderIdentityScanLimit != mail.DefaultSenderIdentityScanLimit ||
		manifest.Limits.MaximumSenderIdentityScanLimit != mail.MaximumSenderIdentityScanLimit ||
		!slices.Contains(manifest.Limits.SenderIdentityCoverageStates, string(mail.SenderIdentityCoverageStateBounded)) {
		t.Fatalf("sender identity coverage capability = %+v", manifest.Limits)
	}
	if manifest.Limits.SearchPaginationConsistency != mail.SearchConsistencyBestEffort ||
		!manifest.Limits.SearchCursorDetectsIndexDrift ||
		manifest.Limits.SearchCandidateCountDefault != "observed_lower_bound" ||
		!manifest.Limits.SearchExactCountBounded {
		t.Fatalf("search pagination capability = %+v", manifest.Limits)
	}
	if manifest.Limits.IMAPConnectionsPerAccount != imapclient.DefaultMaxConnectionsPerAccount ||
		manifest.Limits.MaximumIMAPConnectionsPerAccount != imapclient.MaximumConnectionsPerAccount ||
		!slices.Equal(manifest.Limits.IMAPConcurrentReadOperations, []string{"LIST", "STATUS", "SEARCH", "FETCH"}) ||
		!slices.Equal(manifest.Limits.IMAPExclusiveOperations, []string{"APPEND", "STORE", "COPY", "MOVE", "DELETE"}) ||
		!reflect.DeepEqual(manifest.Limits.IMAPOperationContract, imapclient.OperationContracts()) {
		t.Fatalf("IMAP concurrency capability = %+v", manifest.Limits)
	}
	for _, id := range []string{"messages.filter", "messages.search"} {
		command := manifest.Commands[slices.Index(got, id)]
		if !slices.Equal(command.ResultStates, []string{
			"complete", "partial", "search_cursor_stale", "search_index_changed", "search_count_limit_exceeded",
		}) {
			t.Fatalf("%s result states = %+v", id, command.ResultStates)
		}
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
		if command.StoreDependency != "mail-store" || command.MailAppDependency != "none" {
			t.Fatalf("%s capability = %+v", id, command)
		}
	}
	syncCommand := manifest.Commands[slices.Index(got, "sync")]
	if !slices.Equal(syncCommand.ResultStates, []string{"triggered", "checked_complete", "checked_incomplete"}) {
		t.Fatalf("sync result states = %+v", syncCommand.ResultStates)
	}
	open := manifest.Commands[slices.Index(got, "drafts.open")]
	if open.EffectClass != "read" || open.StoreDependency != "mail-store" || open.MailAppDependency != "none" ||
		!slices.Equal(open.ResultStates, []string{"complete", "partial"}) {
		t.Fatalf("drafts.open capability = %+v", open)
	}
	reconcile := manifest.Commands[slices.Index(got, "drafts.reconcile")]
	if reconcile.EffectClass != "local-write+imap-write" ||
		reconcile.StoreDependency != "draft-store+mail-store-if-baseline" || reconcile.MailAppDependency != "none" ||
		!slices.Equal(reconcile.ResultStates, []string{
			"sent_store_observed", "accepted_by_mail", "sent", "sent_mirror_pending", "outcome_unknown",
		}) {
		t.Fatalf("drafts.reconcile capability = %+v", reconcile)
	}
	handoff := manifest.Commands[slices.Index(got, "drafts.handoff")]
	if !slices.Equal(handoff.ResultStates, []string{"confirmed_opened", "confirmed_failed", "outcome_unknown", "canceled_before_dispatch"}) {
		t.Fatalf("drafts.handoff result states = %+v", handoff.ResultStates)
	}
	handoffReconcile := manifest.Commands[slices.Index(got, "drafts.handoff-reconcile")]
	if handoffReconcile.EffectClass != "local-write" || handoffReconcile.Confirmation != "required-flag" ||
		handoffReconcile.StoreDependency != "draft-store" || handoffReconcile.MailAppDependency != "none" ||
		!slices.Equal(handoffReconcile.ResultStates, []string{"confirmed_opened", "confirmed_failed"}) {
		t.Fatalf("drafts.handoff-reconcile capability = %+v", handoffReconcile)
	}
}

// TestCapabilityMailAppDependencies pins every declared Mail.app dependency to an
// audited value so label drift cannot reintroduce undeclared automation surfaces.
// Evidence for each value lives in docs/tasks/done/027-correct-stale-mail-app-capability-labels.md.
func TestCapabilityMailAppDependencies(t *testing.T) {
	want := map[string]string{
		"capabilities":             "none",
		"version":                  "none",
		"update":                   "none",
		"doctor":                   "optional-automation",
		"batch":                    "none",
		"accounts.list":            "fallback-automation",
		"mailboxes.list":           "none",
		"mailboxes.resolve":        "none",
		"messages.list":            "fallback-automation",
		"messages.filter":          "none",
		"messages.search":          "none",
		"messages.get":             "none",
		"messages.raw":             "none",
		"attachments.list":         "none",
		"attachments.save":         "none",
		"drafts.create":            "none",
		"drafts.list":              "none",
		"drafts.inspect":           "none",
		"drafts.preview":           "none",
		"drafts.edit":              "none",
		"drafts.handoff":           "system-compose-service",
		"drafts.update":            "none",
		"drafts.save":              "none",
		"drafts.open":              "none",
		"drafts.send":              "none",
		"send.setup":               "none",
		"drafts.reconcile":         "none",
		"drafts.discard":           "none",
		"drafts.prune":             "none",
		"messages.reply":           "none",
		"messages.forward":         "none",
		"messages.mark":            "none",
		"messages.move":            "none",
		"messages.copy":            "none",
		"messages.delete":          "none",
		"sync":                     "optional",
		"drafts.handoff-reconcile": "none",
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

func TestCapabilityContractsMatchDispatchRequirements(t *testing.T) {
	manifest := capabilities()
	published := 0
	for _, contract := range commandContracts {
		if commandIsPublished(contract) {
			published++
		}
	}
	if len(manifest.Commands) != published {
		t.Fatalf("manifest command count = %d, published contract count = %d", len(manifest.Commands), published)
	}
	index := 0
	for _, contract := range commandContracts {
		if !commandIsPublished(contract) {
			continue
		}
		if got := manifest.Commands[index]; !reflect.DeepEqual(got, commandCapabilityFor(contract)) {
			t.Fatalf("manifest command %d = %+v, contract = %+v", index, got, commandCapabilityFor(contract))
		}
		index++
	}
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"drafts", "open", "--message", "ref"}, want: "read"},
		{args: []string{"drafts", "reconcile", "--ref", "draft"}, want: "local-write+imap-write"},
	} {
		contract, _ := commandContractForArgs(test.args)
		if contract == nil {
			t.Fatalf("commandContractForArgs(%q) = nil", test.args)
		}
		if contractTextString(contract.metadata[metadataEffectClass]) != test.want {
			t.Fatalf("%s effect_class = %q, want %q", contract.ID, contractTextString(contract.metadata[metadataEffectClass]), test.want)
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
