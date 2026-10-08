package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
	"mailcli/internal/transport/imapclient"
)

func mustCapabilities(t testing.TB) capabilityManifest {
	t.Helper()
	manifest, err := capabilities()
	if err != nil {
		t.Fatalf("capabilities() error = %v", err)
	}
	return manifest
}

func mustCommandCapability(t testing.TB, contract commandContract) commandCapability {
	t.Helper()
	capability, err := commandCapabilityFor(contract)
	if err != nil {
		t.Fatalf("commandCapabilityFor(%q) error = %v", contract.ID, err)
	}
	return capability
}

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
	if response.SchemaVersion != schemaVersion {
		t.Fatalf("response schema version = %d, want %d", response.SchemaVersion, schemaVersion)
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
	if manifest.Limits.MaximumPageSize != mail.MaximumPageLimit || manifest.Limits.MaximumDraftInputBytes != 16*1024*1024 {
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
		manifest.DraftSavePolicy.SafeRecoveryCommand != "mailcli drafts reconcile --ref <DRAFT_REF> --json" {
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
	for _, field := range []string{`"mail_app_dependency"`, `"credential_dependencies"`, `"network_dependencies"`} {
		if bytes.Contains(stdout.Bytes(), []byte(field)) {
			t.Fatalf("serialized capabilities retain removed field %s", field)
		}
	}
}

func TestCapabilityCommandInventory(t *testing.T) {
	want := []string{
		"capabilities", "version", "update", "doctor", "batch", "accounts.list", "mailboxes.list", "mailboxes.resolve",
		"messages.list", "messages.filter", "messages.search", "messages.get", "messages.raw", "messages.state",
		"messages.thread", "messages.new",
		"attachments.list", "attachments.save", "drafts.create", "drafts.list", "drafts.inspect",
		"drafts.preview", "drafts.edit", "drafts.handoff", "drafts.update", "drafts.open",
		"drafts.adopt", "drafts.send", "send.setup", "drafts.reconcile", "drafts.discard", "drafts.prune",
		"messages.reply", "messages.forward", "messages.mark", "messages.move", "messages.copy",
		"messages.delete", "sync", "drafts.handoff-reconcile",
	}
	manifest := mustCapabilities(t)
	got := make([]string, 0, len(manifest.Commands))
	seen := make(map[string]struct{}, len(manifest.Commands))
	for _, command := range manifest.Commands {
		if _, exists := seen[command.ID]; exists {
			t.Fatalf("duplicate command ID %q", command.ID)
		}
		seen[command.ID] = struct{}{}
		got = append(got, command.ID)
		if command.EffectClass == "" || command.Confirmation == "" || command.StoreDependency == "" ||
			command.Dependencies == nil || len(command.ResultStates) == 0 {
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
	wantReasons := make([]string, 0, len(mail.DirectOpsSupportReasons))
	for _, reason := range mail.DirectOpsSupportReasons {
		wantReasons = append(wantReasons, string(reason))
	}
	if !slices.Equal(manifest.Limits.DirectOpsSupportReasons, wantReasons) {
		t.Fatalf("direct ops support reasons = %+v, want %+v", manifest.Limits.DirectOpsSupportReasons, wantReasons)
	}
	if manifest.Limits.SearchPaginationConsistency != mail.SearchConsistencyBestEffort ||
		!manifest.Limits.SearchCursorDetectsIndexDrift ||
		manifest.Limits.SearchCandidateCountDefault != "observed_lower_bound" ||
		!manifest.Limits.SearchExactCountBounded {
		t.Fatalf("search pagination capability = %+v", manifest.Limits)
	}
	if manifest.Limits.IMAPConnectionsPerAccount != imapclient.DefaultMaxConnectionsPerAccount ||
		manifest.Limits.MaximumIMAPConnectionsPerAccount != imapclient.MaximumConnectionsPerAccount ||
		manifest.Limits.MaximumIMAPListResponseBytes != imapclient.MaxListOperationResponseBytes ||
		manifest.Limits.MaximumIMAPListResponseLines != imapclient.MaxListOperationResponseLines ||
		manifest.Limits.MaximumIMAPListMailboxes != imapclient.MaxListOperationMailboxes ||
		!slices.Equal(manifest.Limits.IMAPConcurrentReadOperations, []string{"LIST", "STATUS", "SEARCH", "FETCH"}) ||
		!slices.Equal(manifest.Limits.IMAPExclusiveOperations, []string{"APPEND", "STORE", "COPY", "MOVE", "DELETE"}) ||
		!reflect.DeepEqual(manifest.Limits.IMAPOperationContract, imapclient.OperationContracts()) {
		t.Fatalf("IMAP concurrency capability = %+v", manifest.Limits)
	}
	for _, test := range []struct {
		id           string
		resultStates []string
	}{
		{id: "messages.filter", resultStates: []string{"complete", "partial", "search_cursor_stale", "search_index_changed", "search_count_limit_exceeded"}},
		{id: "messages.search", resultStates: []string{"complete", "partial", "search_cursor_stale", "search_index_changed", "search_count_limit_exceeded", "search_budget_too_small"}},
	} {
		command := manifest.Commands[slices.Index(got, test.id)]
		if !slices.Equal(command.ResultStates, test.resultStates) {
			t.Fatalf("%s result states = %+v, want %+v", test.id, command.ResultStates, test.resultStates)
		}
	}
	send := manifest.Commands[slices.Index(got, "drafts.send")]
	if send.EffectClass != "smtp-send" || send.Confirmation != "required-flag" ||
		send.StoreDependency != "draft-store" ||
		!slices.Equal(send.ResultStates, []string{"sent", "sent_mirror_pending"}) {
		t.Fatalf("drafts.send capability = %+v", send)
	}
	setup := manifest.Commands[slices.Index(got, "send.setup")]
	if setup.EffectClass != "keychain-write" || setup.Confirmation != "none" ||
		setup.StoreDependency != "mail-store-if-no-account" ||
		!slices.Equal(setup.ResultStates, []string{"stored", "removed"}) {
		t.Fatalf("send.setup capability = %+v", setup)
	}
	for _, id := range []string{"messages.reply", "messages.forward"} {
		command := manifest.Commands[slices.Index(got, id)]
		if command.StoreDependency != "mail-store" {
			t.Fatalf("%s capability = %+v", id, command)
		}
	}
	syncCommand := manifest.Commands[slices.Index(got, "sync")]
	if !slices.Equal(syncCommand.ResultStates, []string{"triggered", "checked_complete", "checked_incomplete"}) {
		t.Fatalf("sync result states = %+v", syncCommand.ResultStates)
	}
	open := manifest.Commands[slices.Index(got, "drafts.open")]
	if open.EffectClass != "read" || open.StoreDependency != "mail-store" ||
		!slices.Equal(open.ResultStates, []string{"complete", "partial"}) {
		t.Fatalf("drafts.open capability = %+v", open)
	}
	adopt := manifest.Commands[slices.Index(got, "drafts.adopt")]
	if adopt.EffectClass != "local-write" || adopt.StoreDependency != "draft-store+mail-store" ||
		!slices.Equal(adopt.ResultStates, []string{"created"}) {
		t.Fatalf("drafts.adopt capability = %+v", adopt)
	}
	reconcile := manifest.Commands[slices.Index(got, "drafts.reconcile")]
	if reconcile.EffectClass != "local-write+imap-write" ||
		reconcile.StoreDependency != "draft-store+mail-store-if-baseline" ||
		!slices.Equal(reconcile.ResultStates, []string{
			"sent_store_observed", "accepted_by_mail", "sent", "sent_mirror_pending", "outcome_unknown", "native_draft_observed", "draft_save_outcome_unknown",
		}) {
		t.Fatalf("drafts.reconcile capability = %+v", reconcile)
	}
	handoff := manifest.Commands[slices.Index(got, "drafts.handoff")]
	if !slices.Equal(handoff.ResultStates, []string{"handed_off", "not_handed_off", "unknown"}) {
		t.Fatalf("drafts.handoff result states = %+v", handoff.ResultStates)
	}
	handoffReconcile := manifest.Commands[slices.Index(got, "drafts.handoff-reconcile")]
	if handoffReconcile.EffectClass != "local-write" || handoffReconcile.Confirmation != "required-flag" ||
		handoffReconcile.StoreDependency != "draft-store" ||
		!slices.Equal(handoffReconcile.ResultStates, []string{"confirmed_opened", "confirmed_failed"}) {
		t.Fatalf("drafts.handoff-reconcile capability = %+v", handoffReconcile)
	}
}

// The audited Apple Events paths are account/message-list fallback, default sync
// and live doctor; message/raw/attachment detail and drafts.open hydrate via IMAP,
// while visible handoff uses AppKit and scripted compose writes stay disabled.
func TestCapabilityDependenciesMatchAuditedInventory(t *testing.T) {
	want := map[string][]commandDependency{
		"capabilities": {},
		"version":      {},
		"update":       {{Kind: dependencyKindNetwork, Target: dependencyTargetGitHubRelease, Condition: dependencyConditionAlways}},
		"doctor":       {{Kind: dependencyKindApp, Target: dependencyTargetMailApp, Condition: dependencyConditionIfDoctorLive}},
		"batch": {
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfBatchItemRequiresIMAP},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfBatchItemRequiresIMAP},
		},
		"accounts.list":     {{Kind: dependencyKindApp, Target: dependencyTargetMailApp, Condition: dependencyConditionIfLocalStoreUnavailable}},
		"mailboxes.list":    {},
		"mailboxes.resolve": {},
		"messages.list": {
			{Kind: dependencyKindApp, Target: dependencyTargetMailApp, Condition: dependencyConditionIfLocalStoreUnavailable},
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfEnrichmentSourceIncomplete},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfEnrichmentSourceIncomplete},
		},
		"messages.filter": enrichmentDependencies,
		"messages.search": enrichmentDependencies,
		"messages.get": {
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfLocalSourceIncomplete},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfLocalSourceIncomplete},
		},
		"messages.raw": {
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfLocalSourceIncomplete},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfLocalSourceIncomplete},
		},
		"messages.state": {
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionAlways},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionAlways},
		},
		"messages.thread": {},
		"messages.new": {
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionAlways},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionAlways},
		},
		"attachments.list": {
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfLocalSourceIncomplete},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfLocalSourceIncomplete},
		},
		"attachments.save": {
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfLocalAttachmentBytesUnavailable},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfLocalAttachmentBytesUnavailable},
		},
		"drafts.create":  {},
		"drafts.list":    {},
		"drafts.inspect": {},
		"drafts.preview": {},
		"drafts.edit":    {{Kind: dependencyKindApp, Target: dependencyTargetEditor, Condition: dependencyConditionAlways}},
		"drafts.handoff": {{Kind: dependencyKindApp, Target: dependencyTargetSystemComposeService, Condition: dependencyConditionAlways}},
		"drafts.update":  {},
		"drafts.open": {
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfLocalSourceIncomplete},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfLocalSourceIncomplete},
		},
		"drafts.adopt": {
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfLocalSourceIncomplete},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfLocalSourceIncomplete},
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfLocalAttachmentBytesUnavailable},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfLocalAttachmentBytesUnavailable},
		},
		"drafts.send": {
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfSend},
			{Kind: dependencyKindNetwork, Target: dependencyTargetSMTP, Condition: dependencyConditionIfSend},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfSMTPAccepted},
		},
		"send.setup": {{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionAlways}},
		"drafts.reconcile": {
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfTransportClaimNeedsIMAPReconciliation},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfTransportClaimNeedsIMAPReconciliation},
		},
		"drafts.discard":   {},
		"drafts.prune":     {},
		"messages.reply":   {},
		"messages.forward": {},
		"messages.mark": {
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionAlways},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionAlways},
		},
		"messages.move": {
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionAlways},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionAlways},
		},
		"messages.copy": {
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionAlways},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionAlways},
		},
		"messages.delete": {
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionAlways},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionAlways},
		},
		"sync": {
			{Kind: dependencyKindCredential, Target: dependencyTargetKeychain, Condition: dependencyConditionIfSyncCheck},
			{Kind: dependencyKindNetwork, Target: dependencyTargetIMAP, Condition: dependencyConditionIfSyncCheck},
			{Kind: dependencyKindApp, Target: dependencyTargetMailApp, Condition: dependencyConditionIfSyncDefault},
		},
		"drafts.handoff-reconcile": {},
	}

	validKinds := map[dependencyKind]struct{}{
		dependencyKindNetwork: {}, dependencyKindCredential: {}, dependencyKindApp: {},
	}
	validTargets := map[dependencyTarget]struct{}{
		dependencyTargetIMAP: {}, dependencyTargetSMTP: {}, dependencyTargetGitHubRelease: {},
		dependencyTargetKeychain: {}, dependencyTargetMailApp: {}, dependencyTargetEditor: {},
		dependencyTargetSystemComposeService: {},
	}
	validConditions := map[dependencyCondition]struct{}{
		dependencyConditionAlways: {}, dependencyConditionIfLocalSourceIncomplete: {},
		dependencyConditionIfEnrichmentSourceIncomplete:      {},
		dependencyConditionIfLocalAttachmentBytesUnavailable: {}, dependencyConditionIfLocalStoreUnavailable: {},
		dependencyConditionIfBatchItemRequiresIMAP: {}, dependencyConditionIfSyncCheck: {},
		dependencyConditionIfSyncDefault: {}, dependencyConditionIfDoctorLive: {},
		dependencyConditionIfSend: {}, dependencyConditionIfSMTPAccepted: {},
		dependencyConditionIfTransportClaimNeedsIMAPReconciliation: {},
	}
	manifest := mustCapabilities(t)
	seen := make(map[string]struct{}, len(manifest.Commands))
	for _, command := range manifest.Commands {
		if _, ok := seen[command.ID]; ok {
			t.Fatalf("duplicate capability ID %q", command.ID)
		}
		seen[command.ID] = struct{}{}
		expected, ok := want[command.ID]
		if !ok {
			t.Fatalf("capability %q has no audited dependency expectation", command.ID)
		}
		if command.Dependencies == nil || !reflect.DeepEqual(command.Dependencies, expected) {
			t.Fatalf("%s dependencies = %+v, want %+v", command.ID, command.Dependencies, expected)
		}
		seenDependencies := make(map[commandDependency]struct{}, len(command.Dependencies))
		for _, dependency := range command.Dependencies {
			if _, ok := validKinds[dependency.Kind]; !ok {
				t.Errorf("%s has unknown dependency kind %q", command.ID, dependency.Kind)
			}
			if _, ok := validTargets[dependency.Target]; !ok {
				t.Errorf("%s has unknown dependency target %q", command.ID, dependency.Target)
			}
			if _, ok := validConditions[dependency.Condition]; !ok {
				t.Errorf("%s has unknown dependency condition %q", command.ID, dependency.Condition)
			}
			if _, duplicate := seenDependencies[dependency]; duplicate {
				t.Errorf("%s repeats dependency %+v", command.ID, dependency)
			}
			seenDependencies[dependency] = struct{}{}
			switch dependency.Kind {
			case dependencyKindNetwork:
				if dependency.Target != dependencyTargetIMAP && dependency.Target != dependencyTargetSMTP && dependency.Target != dependencyTargetGitHubRelease {
					t.Errorf("%s network dependency has incompatible target %q", command.ID, dependency.Target)
				}
			case dependencyKindCredential:
				if dependency.Target != dependencyTargetKeychain {
					t.Errorf("%s credential dependency has incompatible target %q", command.ID, dependency.Target)
				}
			case dependencyKindApp:
				if dependency.Target != dependencyTargetMailApp && dependency.Target != dependencyTargetEditor && dependency.Target != dependencyTargetSystemComposeService {
					t.Errorf("%s app dependency has incompatible target %q", command.ID, dependency.Target)
				}
			}
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("audited command count = %d, manifest count = %d", len(want), len(seen))
	}
	for id := range want {
		if _, ok := seen[id]; !ok {
			t.Errorf("audited command %q is missing from the manifest", id)
		}
	}
}

func TestCommandCapabilityResultStatesAreIndependent(t *testing.T) {
	want := []string{"updated", "up_to_date"}
	for _, id := range []string{"update", "drafts.edit", "drafts.update", "messages.mark"} {
		var contract *commandContract
		for index := range commandContracts {
			if commandContracts[index].ID == id {
				contract = &commandContracts[index]
				break
			}
		}
		if contract == nil {
			t.Fatalf("command contract %q not found", id)
		}
		first := mustCommandCapability(t, *contract)
		if !slices.Equal(first.ResultStates, want) {
			t.Fatalf("%s result states = %+v, want %+v", id, first.ResultStates, want)
		}
		first.ResultStates[0] = "mutated"
		second := mustCommandCapability(t, *contract)
		if !slices.Equal(second.ResultStates, want) {
			t.Fatalf("%s later result states = %+v, want %+v", id, second.ResultStates, want)
		}
	}
}

func TestCommandCapabilityDependenciesAreIndependent(t *testing.T) {
	var contract *commandContract
	for index := range commandContracts {
		if commandContracts[index].ID == "messages.get" {
			contract = &commandContracts[index]
			break
		}
	}
	if contract == nil || len(contract.dependencies) == 0 {
		t.Fatal("messages.get dependency contract is missing")
	}
	first := mustCommandCapability(t, *contract)
	first.Dependencies[0].Condition = "mutated"
	second := mustCommandCapability(t, *contract)
	if second.Dependencies[0].Condition != dependencyConditionIfLocalSourceIncomplete {
		t.Fatalf("later dependency contract = %+v, want independent source value", second.Dependencies[0])
	}
}

func TestCapabilityContractsMatchDispatchRequirements(t *testing.T) {
	manifest := mustCapabilities(t)
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
		want := mustCommandCapability(t, contract)
		if got := manifest.Commands[index]; !reflect.DeepEqual(got, want) {
			t.Fatalf("manifest command %d = %+v, contract = %+v", index, got, want)
		}
		index++
	}
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"drafts", "open", "--ref", "ref"}, want: "read"},
		{args: []string{"drafts", "reconcile", "--ref", "draft"}, want: "local-write+imap-write"},
	} {
		contract, _ := commandContractForArgs(test.args)
		if contract == nil {
			t.Fatalf("commandContractForArgs(%q) = nil", test.args)
		}
		if contract.effectClass != test.want {
			t.Fatalf("%s effect_class = %q, want %q", contract.ID, contract.effectClass, test.want)
		}
	}
}

func TestNonReadCapabilitiesUseEffectfulFailureGuidance(t *testing.T) {
	for _, contract := range commandContracts {
		if contract.effectClass == "read" {
			continue
		}
		t.Run(contract.ID, func(t *testing.T) {
			guidance := mail.GuidanceForError(contract.ID, &mail.OperationError{
				Code: "operation_timeout", Message: "injected unclassified timeout",
			})
			if guidance.EffectCertainty != mail.EffectUnknown ||
				guidance.Retryability != mail.RetryObserveRequired || guidance.ReplayAllowed ||
				guidance.Recovery.Action != mail.RecoveryInspect {
				t.Fatalf("%s failure guidance = %+v", contract.ID, guidance)
			}
		})
	}
}

func TestCapabilitiesErrorsLooksUpOnlyTheRequestedCodes(t *testing.T) {
	code, output, response := captureCapabilitiesJSON(t, "--errors", "draft_busy", "--json")
	manifest := response.Data.Capabilities
	if code != 0 || !response.OK || manifest == nil || len(manifest.ErrorCodes) != 1 || manifest.ErrorCodes[0].Code != "draft_busy" {
		t.Fatalf("exit=%d response=%+v", code, response)
	}
	entry := manifest.ErrorCodes[0]
	if entry.Meaning == "" || len(entry.Guidance) == 0 || len(entry.Commands) == 0 {
		t.Fatalf("entry lacks meaning, commands or guidance: %+v", entry)
	}
	if len(manifest.Commands) != 0 || manifest.OutputDefinitions != nil {
		t.Fatalf("a code lookup carries %d command contracts and %d shared definitions", len(manifest.Commands), len(manifest.OutputDefinitions))
	}
	if len(output) > 2048 {
		t.Fatalf("a single code lookup is %d bytes, want at most 2048", len(output))
	}
	_, twoOutput, two := captureCapabilitiesJSON(t, "--errors", "imap_quota_exceeded, draft_busy", "--json")
	entries := two.Data.Capabilities.ErrorCodes
	if len(entries) != 2 || entries[0].Code != "imap_quota_exceeded" || entries[1].Code != "draft_busy" || len(twoOutput) > 4096 {
		t.Fatalf("two-code lookup = %+v (%d bytes)", entries, len(twoOutput))
	}
}

func TestCapabilitiesErrorsRestrictsCommandsWithFor(t *testing.T) {
	code, _, response := captureCapabilitiesJSON(t, "--for", "messages.get", "--errors", "imap_quota_exceeded", "--json")
	entries := response.Data.Capabilities.ErrorCodes
	if code != 0 || len(entries) != 1 || !slices.Equal(entries[0].Commands, []string{"messages.get"}) {
		t.Fatalf("exit=%d entries=%+v", code, entries)
	}
	for _, group := range entries[0].Guidance {
		if !slices.Equal(group.Commands, []string{"messages.get"}) {
			t.Fatalf("guidance group covers %v, want only messages.get", group.Commands)
		}
	}
	if code, _, response := captureCapabilitiesJSON(t, "--for", "version", "--errors", "draft_busy", "--json"); code != 2 || response.Error == nil ||
		response.Error.Code != "invalid_argument" || !strings.Contains(response.Error.Message, "draft_busy") {
		t.Fatalf("a code the selected command cannot emit: exit=%d response=%+v", code, response)
	}
}

func TestCapabilitiesErrorsRejectsUnknownCodesAndConflictingFlags(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown code":      {"--errors", "no_such_code", "--json"},
		"one unknown among": {"--errors", "draft_busy,no_such_code", "--json"},
		"empty entry":       {"--errors", "draft_busy,,operation_failed", "--json"},
		"with outputs":      {"--errors", "draft_busy", "--outputs", "--json"},
		"with limits":       {"--errors", "draft_busy", "--limits", "--json"},
		"with schemas":      {"--for", "messages.get", "--errors", "draft_busy", "--schemas", "--json"},
	} {
		code, _, response := captureCapabilitiesJSON(t, args...)
		if code != 2 || response.OK || response.Error == nil || response.Error.Code != "invalid_argument" {
			t.Errorf("%s: exit=%d response=%+v", name, code, response)
		}
	}
	if _, _, response := captureCapabilitiesJSON(t, "--errors", "no_such_code", "--json"); response.Error == nil || !strings.Contains(response.Error.Message, "no_such_code") {
		t.Errorf("the message does not name the unknown code: %+v", response.Error)
	}
}

func TestCapabilitiesErrorsHumanOutputNamesCodeMeaningAndNext(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), nil, []string{"capabilities", "--errors", "draft_busy"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	for _, want := range []string{"draft_busy", "Another operation holds the draft lock", "next=check_state"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("human output lacks %q: %q", want, stdout.String())
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
