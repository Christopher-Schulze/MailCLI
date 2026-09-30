package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
	"mailcli/internal/transport/imapclient"
)

const capabilitySchemaVersion = 2

type dependencyKind string

const (
	dependencyKindNetwork    dependencyKind = "network"
	dependencyKindCredential dependencyKind = "credential"
	dependencyKindApp        dependencyKind = "app"
)

type dependencyTarget string

const (
	dependencyTargetIMAP                 dependencyTarget = "imap"
	dependencyTargetSMTP                 dependencyTarget = "smtp"
	dependencyTargetGitHubRelease        dependencyTarget = "github-release"
	dependencyTargetKeychain             dependencyTarget = "keychain"
	dependencyTargetMailApp              dependencyTarget = "mail-app"
	dependencyTargetEditor               dependencyTarget = "editor"
	dependencyTargetSystemComposeService dependencyTarget = "system-compose-service"
)

type dependencyCondition string

const (
	dependencyConditionAlways                                  dependencyCondition = "always"
	dependencyConditionIfLocalSourceIncomplete                 dependencyCondition = "if-local-source-incomplete"
	dependencyConditionIfEnrichmentSourceIncomplete            dependencyCondition = "if-enrichment-source-incomplete"
	dependencyConditionIfLocalAttachmentBytesUnavailable       dependencyCondition = "if-local-attachment-bytes-unavailable"
	dependencyConditionIfLocalStoreUnavailable                 dependencyCondition = "if-local-store-unavailable"
	dependencyConditionIfBatchItemRequiresIMAP                 dependencyCondition = "if-batch-item-requires-imap"
	dependencyConditionIfSyncCheck                             dependencyCondition = "if-sync-check"
	dependencyConditionIfSyncDefault                           dependencyCondition = "if-sync-default"
	dependencyConditionIfDoctorLive                            dependencyCondition = "if-doctor-live"
	dependencyConditionIfSend                                  dependencyCondition = "if-send"
	dependencyConditionIfSMTPAccepted                          dependencyCondition = "if-smtp-accepted"
	dependencyConditionIfTransportClaimNeedsIMAPReconciliation dependencyCondition = "if-transport-claim-needs-imap-reconciliation"
)

type commandDependency struct {
	Kind      dependencyKind      `json:"kind"`
	Target    dependencyTarget    `json:"target"`
	Condition dependencyCondition `json:"condition"`
}

type capabilityManifest struct {
	SchemaVersion     int                   `json:"schema_version"`
	Name              string                `json:"name"`
	Version           string                `json:"version"`
	ContractSHA256    string                `json:"contract_sha256"`
	Commands          []commandCapability   `json:"commands"`
	Scope             string                `json:"scope,omitempty"`
	Limits            capabilityLimits      `json:"limits"`
	SyncCheckPolicy   *syncCheckPolicy      `json:"sync_check_policy,omitempty"`
	DraftSavePolicy   *draftSavePolicy      `json:"draft_save_policy,omitempty"`
	OutputDefinitions map[string]outputNode `json:"$defs,omitempty"`
	ErrorCodes        []errorCatalogEntry   `json:"error_codes,omitempty"`
	outputDataOnly    bool
}

type syncCheckPolicy struct {
	IncompleteIsSuccessfulResult      bool   `json:"incomplete_is_successful_result"`
	DefaultIncompleteExitCode         int    `json:"default_incomplete_exit_code"`
	RequireCompleteFlag               string `json:"require_complete_flag"`
	RequireCompleteIncompleteExitCode int    `json:"require_complete_incomplete_exit_code"`
}

type draftSavePolicy struct {
	NewNativeSave       string `json:"new_native_save"`
	LegacyClaimHandling string `json:"legacy_claim_handling"`
	SafeRecoveryCommand string `json:"safe_recovery_command"`
}

type commandCapability struct {
	ID              string                     `json:"id"`
	Audience        commandAudience            `json:"audience"`
	Schema          json.RawMessage            `json:"schema,omitempty"`
	SchemaRef       *capabilitySchemaReference `json:"schema_ref,omitempty"`
	EffectClass     string                     `json:"effect_class"`
	Confirmation    string                     `json:"confirmation"`
	StoreDependency string                     `json:"store_dependency"`
	Dependencies    []commandDependency        `json:"dependencies"`
	ResultStates    []string                   `json:"result_states"`
	LimitRefs       []string                   `json:"limit_refs"`
}

type capabilityLimits struct {
	selectedRefs                     []string
	Platform                         string                         `json:"platform"`
	Architecture                     string                         `json:"architecture"`
	OwnsMailIndex                    bool                           `json:"owns_mail_index"`
	BackgroundProcess                bool                           `json:"background_process"`
	RawMIMERead                      bool                           `json:"raw_mime_read"`
	RawMIMESend                      bool                           `json:"raw_mime_send"`
	ComposeWrite                     bool                           `json:"compose_write"`
	ComposeAttachmentWrite           bool                           `json:"compose_attachment_write"`
	VisibleComposeHandoff            bool                           `json:"visible_compose_handoff"`
	VisibleAttachmentHandoff         bool                           `json:"visible_attachment_handoff"`
	SendTransport                    string                         `json:"send_transport"`
	MutationTransport                string                         `json:"mutation_transport"`
	SupportedProviders               []transport.ProviderSupport    `json:"supported_providers"`
	UnsupportedProviderCode          string                         `json:"unsupported_provider_code"`
	ProviderSupportDescription       string                         `json:"provider_support_description"`
	MaximumPageSize                  int                            `json:"maximum_page_size"`
	MaximumDraftInputBytes           int                            `json:"maximum_draft_input_bytes"`
	MaximumDraftSubjectBytes         int                            `json:"maximum_draft_subject_bytes"`
	MaximumDraftBodyBytes            int                            `json:"maximum_draft_body_bytes"`
	MaximumDraftRecipients           int                            `json:"maximum_draft_recipients"`
	MaximumDraftAttachments          int                            `json:"maximum_draft_attachments"`
	MaximumDraftAttachmentBytes      int64                          `json:"maximum_draft_attachment_bytes"`
	MaximumComposeBodyBytes          int                            `json:"maximum_compose_body_bytes"`
	MaximumRawSourceBytes            int64                          `json:"maximum_raw_source_bytes"`
	BatchOperations                  []string                       `json:"batch_operations"`
	DefaultBatchConcurrency          int                            `json:"default_batch_concurrency"`
	MaximumBatchConcurrency          int                            `json:"maximum_batch_concurrency"`
	MaximumBatchItems                int                            `json:"maximum_batch_items"`
	MaximumBatchInputBytes           int                            `json:"maximum_batch_input_bytes"`
	OutputProjection                 outputProjectionCapability     `json:"output_projection"`
	SenderIdentityScanLimit          int                            `json:"sender_identity_scan_limit"`
	MaximumSenderIdentityScanLimit   int                            `json:"maximum_sender_identity_scan_limit"`
	SenderIdentityCoverageStates     []string                       `json:"sender_identity_coverage_states"`
	DirectOpsSupportReasons          []string                       `json:"direct_ops_support_reasons"`
	SearchPaginationConsistency      string                         `json:"search_pagination_consistency"`
	SearchCursorDetectsIndexDrift    bool                           `json:"search_cursor_detects_index_drift"`
	SearchCandidateCountDefault      string                         `json:"search_candidate_count_default"`
	SearchExactCountBounded          bool                           `json:"search_exact_count_bounded"`
	IMAPConnectionsPerAccount        int                            `json:"imap_connections_per_account"`
	MaximumIMAPConnectionsPerAccount int                            `json:"maximum_imap_connections_per_account"`
	MaximumIMAPListResponseBytes     int64                          `json:"maximum_imap_list_response_bytes"`
	MaximumIMAPListResponseLines     int                            `json:"maximum_imap_list_response_lines"`
	MaximumIMAPListMailboxes         int                            `json:"maximum_imap_list_mailboxes"`
	IMAPConcurrentReadOperations     []string                       `json:"imap_concurrent_read_operations"`
	IMAPExclusiveOperations          []string                       `json:"imap_exclusive_operations"`
	IMAPOperationContract            []imapclient.OperationContract `json:"imap_operation_contract"`
}

type outputProjectionCapability struct {
	ViewFlag                  string   `json:"view_flag"`
	FieldsFlag                string   `json:"fields_flag"`
	MaxBytesFlag              string   `json:"max_bytes_flag"`
	ExportFlag                string   `json:"export_flag"`
	MessageViews              []string `json:"message_views"`
	DraftViews                []string `json:"draft_views"`
	AttachmentViews           []string `json:"attachment_views"`
	RawViews                  []string `json:"raw_views"`
	MessageFields             []string `json:"message_fields"`
	DraftFields               []string `json:"draft_fields"`
	AttachmentFields          []string `json:"attachment_fields"`
	RawFields                 []string `json:"raw_fields"`
	DraftListFields           []string `json:"draft_list_fields"`
	DraftListCoreFields       []string `json:"draft_list_core_fields"`
	DraftListOptionalFields   []string `json:"draft_list_optional_fields"`
	ListPageFields            []string `json:"list_page_fields"`
	SearchPageFields          []string `json:"search_page_fields"`
	MessageDefaultView        string   `json:"message_default_view"`
	DraftDefaultView          string   `json:"draft_default_view"`
	AttachmentDefaultView     string   `json:"attachment_default_view"`
	RawDefaultView            string   `json:"raw_default_view"`
	DefaultJSONBytes          int64    `json:"default_json_bytes"`
	MaximumJSONBytes          int64    `json:"maximum_json_bytes"`
	MaximumContentExportBytes int64    `json:"maximum_content_export_bytes"`
	ExportCommands            []string `json:"export_commands"`
}

func imapOperationsWithConcurrency(
	contracts []imapclient.OperationContract,
	concurrency imapclient.OperationConcurrency,
) []string {
	operations := make([]string, 0, len(contracts))
	for _, contract := range contracts {
		if contract.Concurrency == concurrency {
			operations = append(operations, contract.Operation)
		}
	}
	return operations
}

func capabilities() (capabilityManifest, error) {
	commands, err := publishedCommandCapabilities()
	if err != nil {
		return capabilityManifest{}, err
	}
	imapContract := imapclient.OperationContracts()
	manifest := capabilityManifest{
		SchemaVersion: capabilitySchemaVersion,
		Name:          name,
		Version:       version,
		Commands:      commands,
		Limits: capabilityLimits{
			Platform: "darwin", Architecture: "arm64",
			OwnsMailIndex: false, BackgroundProcess: false,
			RawMIMERead: true, RawMIMESend: true, ComposeWrite: false,
			ComposeAttachmentWrite:      false,
			VisibleComposeHandoff:       true,
			VisibleAttachmentHandoff:    true,
			SendTransport:               "smtp",
			MutationTransport:           "imap",
			SupportedProviders:          transport.SupportedProviders(),
			UnsupportedProviderCode:     transport.CodeUnsupportedProvider,
			ProviderSupportDescription:  transport.ProviderSupportDescription(),
			MaximumPageSize:             mail.MaximumPageLimit,
			MaximumDraftInputBytes:      maximumDraftInputBytes,
			MaximumDraftSubjectBytes:    mail.MaximumDraftSubjectBytes,
			MaximumDraftBodyBytes:       mail.MaximumDraftBodyBytes,
			MaximumDraftRecipients:      mail.MaximumDraftRecipients,
			MaximumDraftAttachments:     mail.MaximumDraftAttachments,
			MaximumDraftAttachmentBytes: mail.MaximumDraftAttachmentBytes,
			MaximumComposeBodyBytes:     mail.MaximumComposeBodyBytes,
			MaximumRawSourceBytes:       mail.MaximumRawSourceBytes,
			BatchOperations: []string{
				string(mail.BatchOperationRead), string(mail.BatchOperationAttachmentSave), string(mail.BatchOperationMark),
				string(mail.BatchOperationMove), string(mail.BatchOperationCopy), string(mail.BatchOperationDelete),
			},
			DefaultBatchConcurrency: mail.DefaultBatchConcurrency,
			MaximumBatchConcurrency: mail.MaximumBatchConcurrency,
			MaximumBatchItems:       mail.MaximumBatchItems,
			MaximumBatchInputBytes:  mail.MaximumBatchInputBytes,
			OutputProjection: outputProjectionCapability{
				ViewFlag:                  "--view",
				FieldsFlag:                "--fields",
				MaxBytesFlag:              "--max-bytes",
				ExportFlag:                "--export",
				MessageViews:              []string{outputViewMetadata, outputViewPlain, outputViewFull},
				DraftViews:                []string{outputViewMetadata, outputViewPlain, outputViewFull},
				AttachmentViews:           []string{outputViewMetadata, outputViewFull},
				RawViews:                  []string{outputViewFull},
				MessageFields:             projectionFieldNames(projectionTargetMessage),
				DraftFields:               projectionFieldNames(projectionTargetDraft),
				AttachmentFields:          projectionFieldNames(projectionTargetAttachment),
				RawFields:                 projectionFieldNames(projectionTargetRaw),
				DraftListFields:           projectionFieldNames(projectionTargetDraftList),
				DraftListCoreFields:       projectionCoreFieldNames(projectionTargetDraftList),
				DraftListOptionalFields:   projectionOptionalFieldNames(projectionTargetDraftList),
				ListPageFields:            projectionFieldNames(projectionTargetListPage),
				SearchPageFields:          projectionFieldNames(projectionTargetSearchPage),
				MessageDefaultView:        defaultMessageOutputView,
				DraftDefaultView:          defaultDraftOutputView,
				AttachmentDefaultView:     outputViewMetadata,
				RawDefaultView:            defaultRawOutputView,
				DefaultJSONBytes:          defaultJSONOutputBytes,
				MaximumJSONBytes:          maximumJSONOutputBytes,
				MaximumContentExportBytes: maximumContentExportBytes,
				ExportCommands:            []string{"messages.get", "messages.raw", "drafts.inspect"},
			},
			SenderIdentityScanLimit:          mail.DefaultSenderIdentityScanLimit,
			MaximumSenderIdentityScanLimit:   mail.MaximumSenderIdentityScanLimit,
			SearchPaginationConsistency:      mail.SearchConsistencyBestEffort,
			SearchCursorDetectsIndexDrift:    true,
			SearchCandidateCountDefault:      "observed_lower_bound",
			SearchExactCountBounded:          true,
			IMAPConnectionsPerAccount:        imapclient.DefaultMaxConnectionsPerAccount,
			MaximumIMAPConnectionsPerAccount: imapclient.MaximumConnectionsPerAccount,
			MaximumIMAPListResponseBytes:     imapclient.MaxListOperationResponseBytes,
			MaximumIMAPListResponseLines:     imapclient.MaxListOperationResponseLines,
			MaximumIMAPListMailboxes:         imapclient.MaxListOperationMailboxes,
			IMAPConcurrentReadOperations: imapOperationsWithConcurrency(
				imapContract, imapclient.OperationConcurrencySharedAccount,
			),
			IMAPExclusiveOperations: imapOperationsWithConcurrency(
				imapContract, imapclient.OperationConcurrencyExclusiveAccount,
			),
			IMAPOperationContract: imapContract,
			SenderIdentityCoverageStates: []string{
				string(mail.SenderIdentityCoverageStateComplete),
				string(mail.SenderIdentityCoverageStateBounded),
				string(mail.SenderIdentityCoverageStateNotObserved),
				string(mail.SenderIdentityCoverageStateNoValidSender),
				string(mail.SenderIdentityCoverageStateNoSentMailbox),
				string(mail.SenderIdentityCoverageStateConfigured),
				string(mail.SenderIdentityCoverageStateUnavailable),
				string(mail.SenderIdentityCoverageStateNotApplicable),
			},
			DirectOpsSupportReasons: []string{
				string(mail.DirectOpsReasonProviderSupported),
				string(mail.DirectOpsReasonBindingHosts),
				string(mail.DirectOpsReasonUnsupportedProvider),
			},
		},
		SyncCheckPolicy: &syncCheckPolicy{
			IncompleteIsSuccessfulResult:      true,
			DefaultIncompleteExitCode:         0,
			RequireCompleteFlag:               "--require-complete",
			RequireCompleteIncompleteExitCode: syncCheckIncompleteExitCode,
		},
		DraftSavePolicy: &draftSavePolicy{
			NewNativeSave:       "rejected_before_mail_contact",
			LegacyClaimHandling: "reconcile_only",
			SafeRecoveryCommand: "mailcli drafts reconcile --ref <DRAFT_REF> --json",
		},
	}
	return manifest, nil
}

func capabilitiesForCommands(selected []string) (capabilityManifest, error) {
	manifest, err := capabilities()
	if err != nil {
		return capabilityManifest{}, err
	}
	selectedSet := make(map[string]struct{}, len(selected))
	for _, id := range selected {
		selectedSet[id] = struct{}{}
	}
	commands := make([]commandCapability, 0, len(selectedSet))
	for _, command := range manifest.Commands {
		if _, ok := selectedSet[command.ID]; ok {
			commands = append(commands, command)
		}
	}
	manifest.Commands = commands
	if _, ok := selectedSet["sync"]; !ok {
		manifest.SyncCheckPolicy = nil
	}
	if !slices.ContainsFunc(selected, func(id string) bool { return strings.HasPrefix(id, "drafts.") }) {
		manifest.DraftSavePolicy = nil
	}
	manifest.Limits.selectedRefs = []string{}
	for _, command := range commands {
		manifest.Limits.selectedRefs = append(manifest.Limits.selectedRefs, command.LimitRefs...)
	}
	return manifest, nil
}

func runCapabilities(args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("capabilities", stderr)
	var selectors repeatableStringFlag
	flags.Var(&selectors, "for", "command IDs, comma lists, or family.* wildcards to describe")
	limitsOnly := flags.Bool("limits", false, "print the full limit set without command contracts")
	includeSchemas := flags.Bool("schemas", false, "include complete parameter schemas with --for")
	includeOutputs := flags.Bool("outputs", false, "include schema.output trees and shared $defs; implies inline schemas")
	outputSchema := flags.Bool("output-schema", false, "include selected data output schemas and reachable $defs; requires --for; implies inline schemas")
	errorLookup := flags.String("errors", "", "look up these error codes (comma list) in the error catalog without the other contracts; --for restricts the commands")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	errorLookupProvided := false
	flags.Visit(func(option *flag.Flag) {
		errorLookupProvided = errorLookupProvided || option.Name == "errors"
	})
	if *outputSchema && (len(selectors) == 0 || *includeOutputs || *limitsOnly || errorLookupProvided) {
		return failCommand("capabilities", *jsonOutput, &commandError{
			code: "invalid_argument", message: "--output-schema requires --for and cannot be combined with --outputs, --errors or --limits",
		}, stdout, stderr)
	}
	if *errorLookup != "" {
		return runCapabilityErrors(*errorLookup, selectors, *limitsOnly || *includeSchemas || *includeOutputs, *jsonOutput, stdout, stderr)
	}
	if *includeSchemas && len(selectors) == 0 {
		return failCommand("capabilities", *jsonOutput, &commandError{
			code: "invalid_argument", message: "--schemas requires --for",
		}, stdout, stderr)
	}
	if len(selectors) > 0 {
		if len(selectors) != 1 || *limitsOnly {
			return failCommand("capabilities", *jsonOutput, &commandError{
				code: "invalid_argument", message: "supply --for once; --for and --limits are mutually exclusive",
			}, stdout, stderr)
		}
		selected, err := resolveCapabilityCommands(selectors[0])
		if err != nil {
			return failCommand("capabilities", *jsonOutput, err, stdout, stderr)
		}
		manifest, err := capabilitiesForCommands(selected)
		if err != nil {
			return failCommand("capabilities", *jsonOutput, err, stdout, stderr)
		}
		if len(selected) == 1 {
			manifest.Scope = selected[0]
		}
		manifest.outputDataOnly = *outputSchema
		if !*includeSchemas && !*includeOutputs && !*outputSchema {
			if err := referenceCapabilitySchemas(manifest.Commands); err != nil {
				return failCommand("capabilities", *jsonOutput, err, stdout, stderr)
			}
		}
		return writeCapabilitiesWithOutputs(stdout, stderr, *jsonOutput, manifest, *includeOutputs || *outputSchema)
	}
	manifest, err := capabilities()
	if err != nil {
		return failCommand("capabilities", *jsonOutput, err, stdout, stderr)
	}
	if *limitsOnly {
		manifest.Commands = []commandCapability{}
	}
	return writeCapabilitiesWithOutputs(stdout, stderr, *jsonOutput, manifest, *includeOutputs)
}

// runCapabilityErrors answers `capabilities --errors CODE[,CODE...]` with the
// catalog entries of those codes only: no command contracts, limits or shared
// output definitions, so a lookup costs about a kilobyte instead of the 64 KB
// of --outputs. --for restricts the commands inside each entry.
func runCapabilityErrors(spec string, selectors repeatableStringFlag, otherView bool, jsonOutput bool, stdout, stderr io.Writer) int {
	invalid := func(message string) int {
		return failCommand("capabilities", jsonOutput, &commandError{code: "invalid_argument", message: message}, stdout, stderr)
	}
	if otherView || len(selectors) > 1 {
		return invalid("--errors cannot be combined with --limits, --schemas or --outputs, and --for may be supplied once")
	}
	var codes []string
	for _, code := range strings.Split(spec, ",") {
		code = strings.TrimSpace(code)
		if code == "" {
			return invalid("--errors takes a comma-separated list of error codes without empty entries")
		}
		if !slices.Contains(codes, code) {
			codes = append(codes, code)
		}
	}
	catalog := errorCatalog()
	scoped := len(selectors) == 1
	if scoped {
		selected, err := resolveCapabilityCommands(selectors[0])
		if err != nil {
			return failCommand("capabilities", jsonOutput, err, stdout, stderr)
		}
		manifest, err := capabilitiesForCommands(selected)
		if err != nil {
			return failCommand("capabilities", jsonOutput, err, stdout, stderr)
		}
		catalog = errorCatalogFor(manifest.Commands)
	}
	entries := make([]errorCatalogEntry, 0, len(codes))
	for _, code := range codes {
		index := slices.IndexFunc(catalog, func(entry errorCatalogEntry) bool { return entry.Code == code })
		if index < 0 {
			if scoped {
				return invalid(fmt.Sprintf("error code %q is unknown or cannot be emitted by the selected commands", code))
			}
			return invalid(fmt.Sprintf("unknown error code %q", code))
		}
		entries = append(entries, catalog[index])
	}
	if !jsonOutput {
		for _, entry := range entries {
			writeFormat(stdout, "%s\t%s\n", entry.Code, entry.Meaning)
			for _, group := range entry.Guidance {
				recovery := ""
				if group.Recovery != nil {
					recovery = fmt.Sprintf("\trecovery=%s %s", group.Recovery.Command, strings.Join(group.Recovery.Args, " "))
				}
				writeFormat(stdout, "  commands=%s\tphase=%s\teffect=%s\tretryability=%s\treplay_allowed=%t\tnext=%s%s\n",
					strings.Join(group.Commands, ","), group.Phase, group.EffectCertainty, group.Retryability, group.ReplayAllowed, group.Next, recovery)
			}
		}
		return 0
	}
	manifest, err := capabilities()
	if err != nil {
		return failCommand("capabilities", true, err, stdout, stderr)
	}
	manifest.Commands = []commandCapability{}
	manifest.Limits.selectedRefs = []string{}
	manifest.SyncCheckPolicy, manifest.DraftSavePolicy = nil, nil
	digest, err := contractDigest()
	if err != nil {
		return failCommand("capabilities", true, err, stdout, stderr)
	}
	manifest.ContractSHA256, manifest.ErrorCodes = digest, entries
	return writeJSON(stdout, envelope{
		SchemaVersion: schemaVersion, OK: true, Command: "capabilities", Data: responseData{Capabilities: &manifest},
	})
}

func writeCapabilitiesWithOutputs(stdout, stderr io.Writer, jsonOutput bool, manifest capabilityManifest, outputs bool) int {
	digest, err := contractDigest()
	if err != nil {
		return failCommand("capabilities", jsonOutput, err, stdout, stderr)
	}
	manifest.ContractSHA256 = digest
	if outputs {
		if err := attachOutputSchemas(manifest.Commands); err != nil {
			return failCommand("capabilities", jsonOutput, err, stdout, stderr)
		}
		if !manifest.outputDataOnly {
			manifest.ErrorCodes = errorCatalogFor(manifest.Commands)
		}
	}
	return writeCapabilities(stdout, stderr, jsonOutput, manifest)
}

func writeCapabilities(stdout, stderr io.Writer, jsonOutput bool, manifest capabilityManifest) int {
	if jsonOutput {
		if err := publishOutputDefinitions(&manifest); err != nil {
			return failCommand("capabilities", true, err, stdout, stderr)
		}
		return writeJSON(stdout, envelope{
			SchemaVersion: schemaVersion,
			OK:            true,
			Command:       "capabilities",
			Data:          responseData{Capabilities: &manifest},
		})
	}
	for _, command := range manifest.Commands {
		writeFormat(
			stdout, "%s\t%s\taudience=%s\tconfirmation=%s\tstore=%s\tdependencies=%v\tstates=%s\n",
			command.ID, command.EffectClass, command.Audience, command.Confirmation, command.StoreDependency,
			command.Dependencies, fmt.Sprint(command.ResultStates),
		)
	}
	return 0
}
