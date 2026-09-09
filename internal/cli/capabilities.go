package cli

import (
	"fmt"
	"io"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
	"mailcli/internal/transport/imapclient"
)

const capabilitySchemaVersion = 1

type capabilityManifest struct {
	SchemaVersion   int                 `json:"schema_version"`
	Name            string              `json:"name"`
	Version         string              `json:"version"`
	Commands        []commandCapability `json:"commands"`
	Limits          capabilityLimits    `json:"limits"`
	SyncCheckPolicy syncCheckPolicy     `json:"sync_check_policy"`
	DraftSavePolicy draftSavePolicy     `json:"draft_save_policy"`
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
	ID                string   `json:"id"`
	EffectClass       string   `json:"effect_class"`
	Confirmation      string   `json:"confirmation"`
	StoreDependency   string   `json:"store_dependency"`
	MailAppDependency string   `json:"mail_app_dependency"`
	ResultStates      []string `json:"result_states"`
}

type capabilityLimits struct {
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
	OutputProjection                 outputProjectionCapability     `json:"output_projection"`
	SenderIdentityScanLimit          int                            `json:"sender_identity_scan_limit"`
	MaximumSenderIdentityScanLimit   int                            `json:"maximum_sender_identity_scan_limit"`
	SenderIdentityCoverageStates     []string                       `json:"sender_identity_coverage_states"`
	SearchPaginationConsistency      string                         `json:"search_pagination_consistency"`
	SearchCursorDetectsIndexDrift    bool                           `json:"search_cursor_detects_index_drift"`
	SearchCandidateCountDefault      string                         `json:"search_candidate_count_default"`
	SearchExactCountBounded          bool                           `json:"search_exact_count_bounded"`
	IMAPConnectionsPerAccount        int                            `json:"imap_connections_per_account"`
	MaximumIMAPConnectionsPerAccount int                            `json:"maximum_imap_connections_per_account"`
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

func capabilities() capabilityManifest {
	imapContract := imapclient.OperationContracts()
	read := func(id, store, mailApp string, states ...string) commandCapability {
		return commandCapability{
			ID: id, EffectClass: "read", Confirmation: "none",
			StoreDependency: store, MailAppDependency: mailApp, ResultStates: states,
		}
	}
	write := func(id, class, confirmation, store, mailApp string, states ...string) commandCapability {
		return commandCapability{
			ID: id, EffectClass: class, Confirmation: confirmation,
			StoreDependency: store, MailAppDependency: mailApp, ResultStates: states,
		}
	}
	return capabilityManifest{
		SchemaVersion: capabilitySchemaVersion,
		Name:          name,
		Version:       version,
		Commands: []commandCapability{
			read("capabilities", "none", "none", "available"),
			read("version", "none", "none", "available"),
			write("update", "local-write", "none", "none", "none", "updated", "up_to_date"),
			read("doctor", "mail-store", "optional-automation", "healthy", "unhealthy"),
			read("accounts.list", "mail-store", "fallback-automation", "complete", "partial", "bounded_identity_coverage"),
			read("mailboxes.list", "mail-store", "none", "complete"),
			read("mailboxes.resolve", "mail-store", "none", "resolved"),
			read("messages.list", "mail-store", "fallback-automation", "complete"),
			read("messages.filter", "mail-store", "none", "complete", "partial", "search_cursor_stale", "search_index_changed", "search_count_limit_exceeded"),
			read("messages.search", "mail-store", "none", "complete", "partial", "search_cursor_stale", "search_index_changed", "search_count_limit_exceeded"),
			read("messages.get", "mail-store", "none", "complete", "partial"),
			read("messages.raw", "mail-store", "none", "complete"),
			read("attachments.list", "mail-store", "none", "complete", "partial"),
			write("attachments.save", "filesystem-write", "none", "mail-store", "none", "saved"),
			write("drafts.create", "local-write", "none", "draft-store", "none", "created"),
			read("drafts.list", "draft-store", "none", "complete"),
			read("drafts.inspect", "draft-store", "none", "complete"),
			read("drafts.preview", "draft-store", "none", "complete"),
			write("drafts.edit", "local-write", "none", "draft-store", "none", "updated"),
			write("drafts.handoff", "visible-compose", "none", "draft-store", "system-compose-service", "opened"),
			write("drafts.update", "local-write", "none", "draft-store", "none", "updated"),
			write("drafts.save", "unsupported", "none", "draft-store", "none", "compose_automation_unsupported"),
			read("drafts.open", "mail-store", "fallback-automation", "complete"),
			write("drafts.send", "smtp-send", "required-flag", "draft-store", "none", "sent", "sent_mirror_pending"),
			write("send.setup", "keychain-write", "none", "none", "none", "stored", "removed"),
			read("drafts.reconcile", "mail-and-draft-store", "none", "sent_store_observed", "accepted_by_mail", "outcome_unknown"),
			write("drafts.discard", "local-write", "required-flag", "draft-store", "none", "discarded"),
			write("drafts.prune", "local-write", "required-flag", "draft-store", "none", "listed", "pruned"),
			write("messages.reply", "local-write", "none", "mail-store", "none", "created"),
			write("messages.forward", "local-write", "none", "mail-store", "none", "created"),
			write("messages.mark", "imap-write", "draft-flag", "mail-store", "none", "updated"),
			write("messages.move", "imap-write", "draft-flag", "mail-store", "none", "moved"),
			write("messages.copy", "imap-write", "none", "mail-store", "none", "copied"),
			write("messages.delete", "imap-write", "required-and-draft-flags", "mail-store", "none", "deleted"),
			write("sync", "mail-write", "none", "mail-store", "optional", "triggered", "checked_complete", "checked_incomplete"),
		},
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
		},
		SyncCheckPolicy: syncCheckPolicy{
			IncompleteIsSuccessfulResult:      true,
			DefaultIncompleteExitCode:         0,
			RequireCompleteFlag:               "--require-complete",
			RequireCompleteIncompleteExitCode: syncCheckIncompleteExitCode,
		},
		DraftSavePolicy: draftSavePolicy{
			NewNativeSave:       "rejected_before_mail_contact",
			LegacyClaimHandling: "reconcile_only",
			SafeRecoveryCommand: "mailcli drafts save --ref <DRAFT_REF> --json",
		},
	}
}

func runCapabilities(args []string, stdout io.Writer, stderr io.Writer) int {
	if helpOnly(args) {
		writeLine(stdout, "Usage:\n  mailcli capabilities [--json]")
		return 0
	}
	flags, err := parseBooleanFlags(args, "--json")
	if err != nil {
		writeLine(stderr, err)
		return 2
	}
	manifest := capabilities()
	if flags["--json"] {
		return writeJSON(stdout, envelope{
			SchemaVersion: schemaVersion,
			OK:            true,
			Command:       "capabilities",
			Data:          responseData{Capabilities: &manifest},
		})
	}
	for _, command := range manifest.Commands {
		writeFormat(
			stdout, "%s\t%s\tconfirmation=%s\tstore=%s\tmail_app=%s\tstates=%s\n",
			command.ID, command.EffectClass, command.Confirmation, command.StoreDependency,
			command.MailAppDependency, fmt.Sprint(command.ResultStates),
		)
	}
	return 0
}
