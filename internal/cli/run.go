package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

const (
	name          = "mailcli"
	version       = "1.4.0"
	schemaVersion = 1
)

type envelope struct {
	SchemaVersion int          `json:"schema_version"`
	OK            bool         `json:"ok"`
	Command       string       `json:"command"`
	Data          responseData `json:"data"`
	Error         *errorData   `json:"error"`
}

// responseData is the flat per-command payload union. The field set is
// deliberately one struct: MarshalJSON shadows message/draft/attachments with
// projected views, and the envelope layer injects cross-cutting fields.
// commandDataFields in response_fields.go documents which command owns each
// JSON field; keep that table updated when adding or reassigning fields.
type responseData struct {
	Name                     string                       `json:"name,omitempty"`
	Version                  string                       `json:"version,omitempty"`
	Capabilities             *capabilityManifest          `json:"capabilities,omitempty"`
	Checks                   []mail.Check                 `json:"checks,omitempty"`
	Timings                  []mail.DiagnosticTiming      `json:"timings,omitempty"`
	Accounts                 *[]mail.Account              `json:"accounts,omitempty"`
	Complete                 *bool                        `json:"complete,omitempty"`
	IdentityCoverageComplete *bool                        `json:"identity_coverage_complete,omitempty"`
	Mailboxes                *[]mail.Mailbox              `json:"mailboxes,omitempty"`
	Mailbox                  *mail.Mailbox                `json:"mailbox,omitempty"`
	Page                     *json.RawMessage             `json:"page,omitempty"`
	Message                  *mail.Message                `json:"message,omitempty"`
	MessageState             *mail.MessageSummary         `json:"message_state,omitempty"`
	State                    *mail.MessageState           `json:"state,omitempty"`
	Thread                   *mail.MessageThread          `json:"thread,omitempty"`
	RawSource                *string                      `json:"raw_source,omitempty"`
	Attachments              *[]mail.Attachment           `json:"attachments,omitempty"`
	Projection               *projectionInfo              `json:"projection,omitempty"`
	ContentExport            *mail.ContentExport          `json:"content_export,omitempty"`
	ContentSource            string                       `json:"content_source,omitempty"`
	ContentComplete          *bool                        `json:"content_complete,omitempty"`
	MissingParts             *[]string                    `json:"missing_parts,omitempty"`
	SavedAttachment          *mail.SavedAttachment        `json:"saved_attachment,omitempty"`
	Draft                    *mail.Draft                  `json:"draft,omitempty"`
	DraftPreview             *draftPreview                `json:"draft_preview,omitempty"`
	DraftHandoff             *draftHandoffResult          `json:"draft_handoff,omitempty"`
	HandoffReconcile         *mail.HandoffReconcileResult `json:"handoff_reconcile,omitempty"`
	Drafts                   *[]draftListEntry            `json:"drafts,omitempty"`
	PruneResult              *mail.PruneDraftsResult      `json:"prune,omitempty"`
	SavedDraft               *mail.SavedDraft             `json:"saved_draft,omitempty"`
	SendResult               *mail.SendResult             `json:"send_result,omitempty"`
	SendReceipt              *mail.SendReceipt            `json:"send_receipt,omitempty"`
	SendSetup                *sendSetupResult             `json:"send_setup,omitempty"`
	PartialEffects           []sendSetupPartialEffect     `json:"partial_effects,omitempty"`
	DeleteResult             *mail.DeleteResult           `json:"delete_result,omitempty"`
	SyncResult               *mail.SyncResult             `json:"sync_result,omitempty"`
	SyncCheck                *mail.SyncCheckResult        `json:"sync_check,omitempty"`
	BatchResult              *mail.BatchResult            `json:"batch_result,omitempty"`
	StoreProfile             *mail.StoreProfile           `json:"store_profile,omitempty"`
	Finalization             *finalizationData            `json:"finalization,omitempty"`
	UpdateResult             *updateResult                `json:"update_result,omitempty"`
	RequiredBytes            *int64                       `json:"required_bytes,omitempty"`
	LimitBytes               *int64                       `json:"limit_bytes,omitempty"`
	Measured                 string                       `json:"measured,omitempty"`
	serialization            *serializedProjection        `json:"-"`
	draftMutationCompleted   bool                         `json:"-"`
	draftRef                 string                       `json:"-"`
	searchRecoveryArgs       []string                     `json:"-"`
	draftListRecovery        *mail.RecoveryGuidance       `json:"-"`
}

func rawResponsePage(value any) *json.RawMessage {
	payload, err := marshalCLIJSON(value)
	if err == nil {
		return (*json.RawMessage)(&payload)
	}
	return nil
}

func messageResponsePage(page *mail.MessagePage) *json.RawMessage {
	return rawResponsePage(page)
}

func searchResponsePage(page *mail.SearchPage) *json.RawMessage {
	return rawResponsePage(page)
}

type errorData struct {
	Code                  string                          `json:"code"`
	Message               string                          `json:"message"`
	Guidance              *mail.OperationGuidance         `json:"guidance"`
	Data                  *unknownSubcommandData          `json:"data,omitempty"`
	IMAPRejection         *transport.IMAPCommandRejection `json:"imap_rejection,omitempty"`
	ValidSubcommands      []string                        `json:"valid_subcommands,omitempty"`
	RequiredBytes         *int64                          `json:"required_bytes,omitempty"`
	Limit                 *transport.ResourceLimit        `json:"limit,omitempty"`
	ObservedAtLeast       *int64                          `json:"observed_at_least,omitempty"`
	DraftRevisionConflict *mail.DraftRevisionConflict     `json:"draft_revision_conflict,omitempty"`
	DraftEditor           *draftEditorEvidence            `json:"draft_editor,omitempty"`
	UnclaimedSpool        *mail.UnclaimedSpoolObservation `json:"unclaimed_spool,omitempty"`
	outputSize            *outputSizeEvidence             `json:"-"`
}

type unknownSubcommandData struct {
	Requested string   `json:"requested"`
	Choices   []string `json:"choices"`
}

func newErrorData(command string, data responseData, err error) *errorData {
	code := errorCode(err)
	if code == transport.CodeIMAPResourceLimitExceeded {
		code = transport.ErrorCode(err)
	}
	guidance := guidanceForResponse(command, data, err)
	if command == "send.setup" {
		guidance = sendSetupErrorGuidance(guidance, data, err)
	}
	if command == "update" {
		result := data.UpdateResult
		if result == nil || result.FailedPhase == "" {
			guidance.EffectCertainty = mail.EffectNone
		} else {
			certainty := mail.EffectUnknown
			if result.failureCertainty == string(mail.EffectComplete) {
				certainty = mail.EffectComplete
			}
			guidance = mail.OperationGuidance{
				Phase: mail.OperationPhaseExecution, EffectCertainty: certainty,
				Retryability: mail.RetryObserveRequired,
				Recovery: mail.RecoveryGuidance{
					Action: mail.RecoveryObserve, Command: "version", Args: []string{"--json"},
				},
			}
		}
	}
	var conflict *mail.DraftRevisionConflict
	if errors.As(err, &conflict) && conflict.Ref != "" {
		guidance.Recovery = draftInspectRecovery(conflict.Ref, true)
	}
	var editor *draftEditorError
	var editorEvidence *draftEditorEvidence
	if errors.As(err, &editor) {
		editorEvidence = &editor.evidence
		guidance.ReplayAllowed, guidance.Retryability = false, mail.RetryObserveRequired
		guidance.Recovery = draftInspectRecovery(editor.evidence.Ref, true)
		if !editor.updateAttempted {
			guidance.Phase, guidance.EffectCertainty = mail.OperationPhaseExecution, mail.EffectNone
		}
	}
	var requiredBytes *int64
	var limit *transport.ResourceLimit
	var observedAtLeast *int64
	var resourceError *transport.TransportError
	if transport.IsResourceLimitExceeded(err) && errors.As(err, &resourceError) && resourceError.Limit != nil {
		limit = resourceError.Limit
		observedAtLeast = &resourceError.ObservedAtLeast
	}
	var budgetError interface{ RequiredBytes() int64 }
	if errorCode(err) == "search_budget_too_small" && errors.As(err, &budgetError) {
		value := budgetError.RequiredBytes()
		requiredBytes = &value
	}
	var outputSize *outputSizeEvidence
	var oversized *outputTooLargeError
	if errors.As(err, &oversized) {
		evidence := oversized.sizeEvidence()
		outputSize = &evidence
	}
	var unclaimedSpool *mail.UnclaimedSpoolObservation
	var operation *mail.OperationError
	if errors.As(err, &operation) {
		unclaimedSpool = operation.UnclaimedSpool
	}
	imapRejection, _ := transport.TaggedIMAPRejection(err)
	return &errorData{
		Code: code, Message: publicFailureMessage(err), Guidance: &guidance, IMAPRejection: imapRejection,
		RequiredBytes: requiredBytes, DraftRevisionConflict: conflict, DraftEditor: editorEvidence,
		Limit: limit, ObservedAtLeast: observedAtLeast, UnclaimedSpool: unclaimedSpool,
		outputSize: outputSize,
	}
}

func draftInspectRecovery(ref string, includeFullView bool) mail.RecoveryGuidance {
	args := []string{"--ref", ref}
	if includeFullView {
		args = append(args, "--view", outputViewFull)
	}
	args = append(args, "--json")
	return mail.RecoveryGuidance{Action: mail.RecoveryInspect, Command: "drafts.inspect", Args: args}
}

func guidanceForResponse(command string, data responseData, err error) mail.OperationGuidance {
	guidance := mail.GuidanceForError(command, err)
	if command == "drafts.list" && errorCode(err) == "output_too_large" && data.draftListRecovery != nil {
		guidance.Recovery = *data.draftListRecovery
	}
	if command == "messages.search" && errorCode(err) == "search_budget_too_small" && len(data.searchRecoveryArgs) > 0 {
		guidance.Recovery.Command = command
		guidance.Recovery.Args = data.searchRecoveryArgs
	}
	if command == "drafts.send" {
		var operation *mail.OperationError
		if errors.As(err, &operation) && operation.DraftRef != "" && operation.UnclaimedSpool != nil {
			guidance = mail.OperationGuidance{
				Phase: mail.OperationPhaseExecution, EffectCertainty: mail.EffectNone,
				Retryability: mail.RetryUserInputRequired, ReplayAllowed: false,
				Recovery: draftInspectRecovery(operation.DraftRef, false),
			}
			guidance.Recovery.Instruction = "SMTP was not contacted. Inspect this draft and confirm that no send claim remains. Remove only the reported path after confirming the draft lock is free and its current type, owner UID, and mode still match this observation. If it is a symlink, unlink only the link and never its target; then retry the send explicitly."
		}
	}
	if command == "drafts.send" && transport.IsSMTPSourceInvalid(err) && data.draftRef != "" {
		guidance.Recovery = draftInspectRecovery(data.draftRef, false)
	}
	if data.draftMutationCompleted && errorCode(err) == "output_too_large" {
		guidance.Phase, guidance.EffectCertainty = mail.OperationPhaseExecution, mail.EffectComplete
		guidance.Retryability, guidance.ReplayAllowed = mail.RetryObserveRequired, false
		guidance.Recovery = mail.RecoveryGuidance{Action: mail.RecoveryObserve}
		if data.Draft != nil && data.Draft.Ref != "" {
			guidance.Recovery = draftInspectRecovery(data.Draft.Ref, true)
		}
	}
	if errorCode(err) == "draft_busy" {
		var operation *mail.OperationError
		if errors.As(err, &operation) && operation.DraftRef != "" {
			guidance.Recovery = draftInspectRecovery(operation.DraftRef, false)
		}
	}
	if errorCode(err) == "account_binding_stale" && guidance.Phase != mail.OperationPhaseRead {
		guidance.Recovery = mail.RecoveryGuidance{
			Action: mail.RecoveryObserve, Command: "accounts.list", Args: []string{"--json"},
		}
	}
	if result := data.DraftHandoff; handoffNeedsReconciliation(result, err) {
		guidance.Phase = mail.OperationPhaseExecution
		guidance.EffectCertainty = mail.EffectUnknown
		guidance.Retryability = mail.RetryObserveRequired
		guidance.ReplayAllowed = false
		guidance.Recovery = mail.RecoveryGuidance{
			Action: mail.RecoveryReconcile, Command: "drafts.handoff-reconcile",
			Args:        []string{"--ref", result.DraftRef, "--attempt", result.AttemptID, "--confirm", "--json"},
			OperationID: result.AttemptID,
		}
	}
	if result := data.SendResult; result != nil && result.AttemptID != "" {
		guidance.Recovery.Action, guidance.Recovery.OperationID = mail.RecoveryReconcile, result.AttemptID
		if result.DraftRef != "" {
			guidance.Recovery.Command = "drafts.reconcile"
			guidance.Recovery.Args = []string{"--ref", result.DraftRef, "--json"}
		}
		guidance.ReplayAllowed, guidance.Retryability = false, mail.RetryObserveRequired
		switch result.Outcome {
		case mail.SendOutcomeMirrorPending:
			guidance.Phase, guidance.EffectCertainty = mail.OperationPhaseMirror, mail.EffectPartial
		case mail.SendOutcomeSent, mail.SendOutcomeObserved:
			guidance.Phase, guidance.EffectCertainty, guidance.Recovery.Action = mail.OperationPhaseCleanup, mail.EffectComplete, mail.RecoveryInspect
			if result.DraftRef != "" {
				guidance.Recovery = draftInspectRecovery(result.DraftRef, false)
				guidance.Recovery.OperationID = result.AttemptID
			}
		default:
			guidance.Phase = mail.OperationPhaseSubmission
			if result.SubmissionAccepted || result.AcceptedByMail {
				guidance.EffectCertainty = mail.EffectPartial
			} else {
				guidance.EffectCertainty = mail.EffectUnknown
			}
		}
	}
	if message := data.Message; message != nil && message.Hydration != nil && guidance.EffectCertainty == mail.EffectNone {
		guidance.Phase, guidance.EffectCertainty = mail.OperationPhaseHydration, mail.EffectNone
		if message.Summary.Ref != "" && (guidance.Recovery.Action == mail.RecoveryRetry || guidance.Recovery.Action == mail.RecoveryCorrect) &&
			(command == "messages.get" || command == "drafts.open") {
			guidance.Recovery.Command = command
			guidance.Recovery.Args = []string{"--ref", message.Summary.Ref, "--json"}
		}
	}
	return guidance
}

func handoffNeedsReconciliation(result *draftHandoffResult, err error) bool {
	if result == nil || result.AttemptID == "" || !result.DispatchStarted {
		return false
	}
	if result.Outcome == mail.HandoffOutcomeUnknown || result.SnapshotsRetained {
		return true
	}
	var cleanupErr *mail.OperationError
	if errors.As(err, &cleanupErr) {
		return cleanupErr.ErrorCode() == "handoff_attachment_cleanup_failed" || cleanupErr.ErrorCode() == "handoff_claim_cleanup_failed"
	}
	return false
}

const (
	initializationFailureCode = "initialization_failed"
	finalizationFailureCode   = "finalization_failed"
	serializationFailureCode  = "serialization_failed"
)

type finalizationData struct {
	State string     `json:"state"`
	Error *errorData `json:"error"`
}

func Run(
	ctx context.Context,
	mailService *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	args, jsonOutput := NormalizeGlobalJSON(args)
	trackedStdout := &errorTrackingWriter{writer: stdout}
	trackedStderr := &errorTrackingWriter{writer: stderr}
	stdout = trackedStdout
	stderr = trackedStderr

	if profile, known := mailService.StoreProfile(); known {
		invocationStoreProfile = &profile
		defer func() { invocationStoreProfile = nil }()
		if !jsonOutput && profile.Unverified() {
			writeFormat(
				stderr,
				"warning: %s: Mail store framework %s differs from verified %s; reads proceed on verified schema capabilities\n",
				mail.StoreProfileUnverifiedCode, profile.FrameworkVersion, profile.SupportedFrameworkVersion,
			)
		}
	}

	code := 0
	if len(args) == 0 {
		if jsonOutput {
			if WriteFailureEnvelope(stdout, "", "invalid_argument", "command is required") != 0 {
				return 1
			}
			code = 2
		} else {
			writeHelp(stdout)
		}
	} else if !jsonOutput {
		code = runCommand(ctx, mailService, args, stdout, stderr)
	} else {
		code = runJSONCommand(ctx, mailService, args, stdout, stderr)
	}
	if trackedStdout.err != nil || trackedStderr.err != nil {
		return 1
	}
	return code
}

// JSONOutputRequested reports whether args request the global JSON output mode.
func JSONOutputRequested(args []string) bool {
	_, requested := NormalizeGlobalJSON(args)
	return requested
}

// InitializationErrorCode returns a typed startup code or the startup fallback.
func InitializationErrorCode(err error) string {
	code := errorCode(err)
	if code == "operation_failed" {
		return initializationFailureCode
	}
	return code
}

// FinalizeJSON writes one command envelope after merging any resource teardown failure.
func FinalizeJSON(writer io.Writer, args []string, payload []byte, code int, cleanupErr error) int {
	normalized, _ := NormalizeGlobalJSON(args)
	command := AttemptedCommand(normalized)
	if helpOnly(normalized) || (len(normalized) > 1 && helpOnly(normalized[1:])) {
		if writeEnvelopeBytes(writer, payload) != 0 {
			return 1
		}
		return code
	}
	var value envelope
	if err := json.Unmarshal(payload, &value); err != nil || value.SchemaVersion <= 0 {
		failureCode := serializationFailureCode
		message := "invalid JSON"
		if cleanupErr != nil {
			failureCode = finalizationFailureCode
			message = "JSON cleanup: " + cleanupErr.Error()
		}
		WriteFailureEnvelope(writer, command, failureCode, message)
		return 1
	}
	if cleanupErr != nil {
		failureErr := &commandError{code: finalizationFailureCode, message: "close Mail: " + cleanupErr.Error()}
		failure := newErrorData(command, value.Data, failureErr)
		value.Data.Finalization = &finalizationData{State: "failed", Error: failure}
		if value.OK || value.Error == nil {
			value.OK = false
			value.Error = failure
		}
		if writeJSON(writer, value) != 0 {
			return 1
		}
		if code == 0 || code == 3 {
			return 1
		}
		return code
	}
	if writeEnvelopeBytes(writer, payload) != 0 {
		return 1
	}
	return code
}

func RequiresMailService(args []string) bool {
	args, _ = NormalizeGlobalJSON(args)
	if len(args) == 0 || helpOnly(args[1:]) {
		return false
	}
	contract, commandArgs := commandContractForArgs(args)
	return contract != nil && !helpOnly(commandArgs) && commandRequiresMailService(*contract, commandArgs)
}

func RequiresSignalContext(args []string) bool {
	args, _ = NormalizeGlobalJSON(args)
	if len(args) == 0 || helpOnly(args[1:]) {
		return false
	}
	contract, commandArgs := commandContractForArgs(args)
	if contract == nil || helpOnly(commandArgs) {
		return false
	}
	return commandNeedsSignalFor(*contract) || commandRequiresMailService(*contract, commandArgs)
}

func RequiresMainThread(args []string) bool {
	args, _ = NormalizeGlobalJSON(args)
	if len(args) == 0 || helpOnly(args[1:]) {
		return false
	}
	contract, commandArgs := commandContractForArgs(args)
	return contract != nil && !helpOnly(commandArgs) && commandNeedsMainThreadFor(*contract)
}

func draftReconcileCommandRequired(args []string) bool {
	if len(args) == 0 || helpOnly(args) {
		return false
	}
	ref, found := draftRefArgument(args)
	if !found || ref == "" {
		return true
	}
	return mail.DraftReconcileRequiresMailStore(ref)
}

func draftRefArgument(args []string) (string, bool) {
	flags := newFlagSet("drafts reconcile", io.Discard)
	ref := flags.String("ref", "", "draft ref")
	flags.Bool("json", false, "emit JSON")
	normalized, state := normalizeReferenceArguments(flags, args)
	if err := flags.Parse(normalized); err != nil {
		return "", false
	}
	if err := referenceArgumentError(flags, state); err != nil {
		return "", false
	}
	return *ref, true
}

func runJSONCommand(
	ctx context.Context,
	mailService *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	if errorCode, message, command, validSubcommands, requested := jsonSubcommandFailure(args); errorCode != "" {
		var details *unknownSubcommandData
		if requested != "" {
			details = &unknownSubcommandData{Requested: requested, Choices: validSubcommands}
		}
		if writeFailureEnvelope(stdout, command, errorCode, message, validSubcommands, details) != 0 {
			return 1
		}
		return 2
	}
	commandOutput := countingWriter{writer: stdout}
	commandError := commandDiagnosticBuffer{processOutput: stderr}
	code := runCommand(ctx, mailService, args, &commandOutput, &commandError)
	if code == 0 || commandOutput.written > 0 {
		if _, err := io.Copy(stderr, &commandError); err != nil {
			return 1
		}
		return code
	}
	message := firstOutputLine(commandError.String())
	if message == "" {
		message = "command failed"
	}
	// Classify error by exit code: code 2 = usage error, code 1 = runtime error.
	errorCode := "operation_failed"
	if code == 2 {
		errorCode = "invalid_argument"
	}
	if strings.Contains(message, "unknown command") ||
		(strings.HasPrefix(message, "unknown ") && strings.Contains(message, " command ")) {
		errorCode = "unknown_command"
	}
	if WriteFailureEnvelope(stdout, AttemptedCommand(args), errorCode, message) != 0 {
		return 1
	}
	return code
}

func jsonSubcommandFailure(args []string) (string, string, string, []string, string) {
	if len(args) == 0 {
		return "", "", "", nil, ""
	}
	validSubcommands := commandFamilyChoices(args[0])
	if len(validSubcommands) == 0 {
		return "", "", "", nil, ""
	}
	if len(args) == 1 || args[1] == "--json" {
		return "invalid_argument", args[0] + " requires a subcommand", args[0], validSubcommands, ""
	}
	if isHelpArgument(args[1]) {
		return "", "", "", nil, ""
	}
	for _, validSubcommand := range validSubcommands {
		if args[1] == validSubcommand {
			return "", "", "", nil, ""
		}
	}
	return "unknown_command", fmt.Sprintf("unknown %s command %q", args[0], args[1]),
		args[0], validSubcommands, args[1]
}

type countingWriter struct {
	writer  io.Writer
	written int64
}

type errorTrackingWriter struct {
	writer io.Writer
	err    error
}

func (w *errorTrackingWriter) Write(payload []byte) (int, error) {
	written, err := w.writer.Write(payload)
	if err == nil && written != len(payload) {
		err = io.ErrShortWrite
	}
	if err != nil && w.err == nil {
		w.err = err
	}
	return written, err
}

func (w *errorTrackingWriter) recordWriteError(err error) {
	if w.err == nil {
		w.err = err
	}
}

func writeRaw(writer io.Writer, values ...any) {
	if _, err := fmt.Fprint(writer, values...); err != nil {
		recordWriteError(writer, err)
	}
}

func writeLine(writer io.Writer, values ...any) {
	if _, err := fmt.Fprintln(writer, values...); err != nil {
		recordWriteError(writer, err)
	}
}

func writeFormat(writer io.Writer, format string, values ...any) {
	if _, err := fmt.Fprintf(writer, format, values...); err != nil {
		recordWriteError(writer, err)
	}
}

func recordWriteError(writer io.Writer, err error) {
	if recorder, ok := writer.(interface{ recordWriteError(error) }); ok {
		recorder.recordWriteError(err)
	}
}

func (w *countingWriter) Write(payload []byte) (int, error) {
	written, err := w.writer.Write(payload)
	w.written += int64(written)
	return written, err
}

type commandRunner func(context.Context, *mail.Service, []string, io.Writer, io.Writer) int

// invocationStoreProfile carries the opened Mail-store profile into every
// envelope written by this invocation. It is set once by Run before dispatch
// and cleared on return; the CLI is a single-shot process and tests invoke Run
// serially, so no command can observe another invocation's profile.
var invocationStoreProfile *mail.StoreProfile

func runCommand(
	ctx context.Context,
	mailService *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	if len(args) == 0 || isHelpArgument(args[0]) {
		writeHelp(stdout)
		return 0
	}
	if args[0] == "--version" {
		args = append([]string(nil), args...)
		args[0] = "version"
	}
	if family := commandFamilyContract(args[0]); family != nil {
		return family.familyHandler(ctx, mailService, args[1:], stdout, stderr)
	}
	contract, commandArgs := commandContractForArgs(args)
	if contract != nil {
		return contract.handler(ctx, mailService, commandArgs, stdout, stderr)
	}
	if len(commandFamilyChoices(args[0])) > 0 {
		return runCommandFamily(ctx, mailService, args[0], args[1:], stdout, stderr, nil)
	}
	writeFormat(stderr, "unknown command %q\nRun 'mailcli help' to list available commands.\n", args[0])
	return 2
}

func runCommandFamily(
	ctx context.Context,
	service *mail.Service,
	family string,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	handlerOverride commandRunner,
) int {
	choices := commandFamilyChoices(family)
	if len(choices) == 0 {
		writeFormat(stderr, "unknown command %q\nRun 'mailcli help' to list available commands.\n", family)
		return 2
	}
	if len(args) == 0 {
		if commandFamilyShowsHelpWhenEmpty(family) {
			writeCommandFamilyUsage(stdout, family, choices)
			return 0
		}
		writeCommandFamilyUsage(stderr, family, choices)
		return 2
	}
	if isHelpArgument(args[0]) {
		writeCommandFamilyUsage(stdout, family, choices)
		return 0
	}
	contract := commandContractForFamily(family, args[0])
	if contract == nil {
		writeFormat(stderr, "unknown %s command %q\n", family, args[0])
		return 2
	}
	handler := contract.handler
	if handlerOverride != nil {
		handler = handlerOverride
	}
	return handler(ctx, service, args[1:], stdout, stderr)
}

func writeCommandFamilyUsage(writer io.Writer, family string, choices []string) {
	if family == "send" {
		writeFormat(
			writer,
			"Usage:\n  mailcli send %s --from <email> [--account <ref>] [--credential-account <email>] [--smtp-host <host> --smtp-port <port>] [--imap-host <host> --imap-port <port>] [--remove] [--json]\n\n%s\n",
			strings.Join(choices, "|"),
			transport.ProviderSupportDescription(),
		)
		return
	}
	choiceText := strings.Join(choices, "|")
	if len(choices) > 1 {
		choiceText = "<" + choiceText + ">"
	}
	writeFormat(writer, "Usage:\n  mailcli %s %s [options]\n", family, choiceText)
}

func runCapabilitiesCommand(_ context.Context, _ *mail.Service, args []string, stdout, stderr io.Writer) int {
	return runCapabilities(args, stdout, stderr)
}

func runSendCommandFamily(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
	return runCommandFamily(ctx, service, "send", args, stdout, stderr, nil)
}

func runVersionCommand(_ context.Context, _ *mail.Service, args []string, stdout, stderr io.Writer) int {
	return runVersion(args, stdout, stderr)
}

func runUpdateCommand(ctx context.Context, _ *mail.Service, args []string, stdout, stderr io.Writer) int {
	return runUpdate(ctx, args, stdout, stderr)
}

func runDraftInspectCommand(_ context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
	return runDraftInspect(service, args, stdout, stderr)
}

func runDraftPreviewCommand(_ context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
	return runDraftPreview(service, args, stdout, stderr)
}

func runAttachmentsSaveCommand(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
	return runAttachmentsSave(ctx, service, args, stdout, stderr)
}

func runMessageMove(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
	return runMessageTransfer(ctx, service, false, args, stdout, stderr)
}

func runMessageCopy(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
	return runMessageTransfer(ctx, service, true, args, stdout, stderr)
}

// NormalizeGlobalJSON moves a global --json flag to the command tail.
func NormalizeGlobalJSON(args []string) ([]string, bool) {
	if normalized, requested, handled := normalizeReferenceGlobalJSON(args); handled {
		return normalized, requested
	}
	requested := false
	normalized := make([]string, 0, len(args)+1)
	for index, argument := range args {
		switch argument {
		case "--json":
			requested = true
			if index == 0 || !strings.HasPrefix(args[index-1], "-") {
				continue
			}
		}
		normalized = append(normalized, argument)
	}
	if !requested {
		return args, false
	}
	if len(normalized) == 0 {
		return nil, true
	}
	if len(normalized) < len(args) || normalized[len(normalized)-1] != "--json" {
		normalized = append(normalized, "--json")
	}
	return normalized, true
}

func normalizeReferenceGlobalJSON(args []string) ([]string, bool, bool) {
	contract, commandEnd, found := referenceCommandForArgs(args)
	if !found {
		return nil, false, false
	}
	flagArity, ok := referenceGlobalJSONFlagArity(contract)
	if !ok {
		return nil, false, false
	}
	beforeDelimiter, afterDelimiter, requested := separateReferenceGlobalJSON(args, commandEnd, flagArity)
	if !requested {
		return args, false, true
	}
	beforeDelimiter = append(beforeDelimiter, "--json")
	return append(beforeDelimiter, afterDelimiter...), true, true
}

func referenceCommandForArgs(args []string) (*commandContract, int, bool) {
	commandArgs := make([]string, 0, len(args))
	for _, argument := range args {
		if argument != "--json" {
			commandArgs = append(commandArgs, argument)
		}
	}
	contract, _ := commandContractForArgs(commandArgs)
	if contract == nil {
		return nil, 0, false
	}
	parts := strings.Split(contract.ID, ".")
	commandEnd := referenceCommandEnd(args, parts)
	return contract, commandEnd, commandEnd > 0
}

func referenceCommandEnd(args, parts []string) int {
	matched := 0
	for index, argument := range args {
		if argument == "--json" {
			continue
		}
		if argument == parts[matched] {
			matched++
			if matched == len(parts) {
				return index + 1
			}
		}
	}
	return 0
}

func referenceGlobalJSONFlagArity(contract *commandContract) (map[string]bool, bool) {
	var schema struct {
		Flags []struct {
			Name       string `json:"name"`
			TakesValue bool   `json:"takes_value"`
		} `json:"flags"`
		PositionalArguments []string `json:"positional_arguments"`
	}
	if err := json.Unmarshal(schemaForCommand(contract.ID), &schema); err != nil {
		return nil, false
	}
	flagArity := make(map[string]bool, len(schema.Flags))
	hasReferenceFlag := false
	for _, option := range schema.Flags {
		flagArity[strings.TrimPrefix(option.Name, "--")] = option.TakesValue
		hasReferenceFlag = hasReferenceFlag || option.Name == "--ref"
	}
	hasReferenceOperand := slices.Contains(schema.PositionalArguments, "REF")
	return flagArity, hasReferenceFlag && hasReferenceOperand
}

func separateReferenceGlobalJSON(args []string, commandEnd int, flagArity map[string]bool) ([]string, []string, bool) {
	beforeDelimiter := make([]string, 0, len(args)+1)
	afterDelimiter := make([]string, 0, 1)
	requested, parsingOptions := false, true
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if index < commandEnd {
			if argument == "--json" {
				requested = true
				continue
			}
			beforeDelimiter = append(beforeDelimiter, argument)
			continue
		}
		if !parsingOptions {
			afterDelimiter = append(afterDelimiter, argument)
			continue
		}
		if argument == "--" {
			parsingOptions = false
			afterDelimiter = append(afterDelimiter, argument)
			continue
		}
		beforeDelimiter = append(beforeDelimiter, argument)
		if argument == "" || argument == "-" || argument[0] != '-' {
			continue
		}
		name, _, hasValue := flagArgument(argument)
		if takesValue, known := flagArity[name]; known && takesValue && !hasValue && index+1 < len(args) {
			beforeDelimiter = append(beforeDelimiter, args[index+1])
			index++
			continue
		}
		if argument == "--json" {
			beforeDelimiter = beforeDelimiter[:len(beforeDelimiter)-1]
			requested = true
		}
	}
	return beforeDelimiter, afterDelimiter, requested
}

func runVersion(args []string, stdout io.Writer, stderr io.Writer) int {
	if helpOnly(args) {
		writeLine(stdout, "Usage:\n  mailcli version [--json]")
		return 0
	}
	jsonOutput, err := parseBooleanFlags(args, "--json")
	if err != nil {
		writeLine(stderr, err)
		return 2
	}

	if jsonOutput["--json"] {
		return writeJSON(stdout, envelope{
			SchemaVersion: schemaVersion,
			OK:            true,
			Command:       "version",
			Data:          responseData{Name: name, Version: version},
		})
	}

	writeFormat(stdout, "%s %s\n", name, version)
	return 0
}

func runDoctor(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	if helpOnly(args) {
		writeLine(stdout, "Usage:\n  mailcli doctor [--json] [--live] [--diagnostics]")
		return 0
	}
	flags, err := parseBooleanFlags(args, "--json", "--live", "--diagnostics")
	if err != nil {
		writeLine(stderr, err)
		return 2
	}

	operationCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	var report mail.DiagnosticReport
	var timings []mail.DiagnosticTiming
	if flags["--diagnostics"] {
		report, timings = service.ProbeWithDiagnostics(operationCtx, flags["--live"])
	} else {
		report = service.Probe(operationCtx, flags["--live"])
	}
	healthy := mail.IsHealthy(report)
	cancel()
	if flags["--json"] {
		response := envelope{
			SchemaVersion: schemaVersion,
			OK:            healthy,
			Command:       "doctor",
			Data:          responseData{Checks: report.Checks, Timings: timings},
		}
		if !healthy {
			code := "environment_unhealthy"
			for _, check := range report.Checks {
				if check.Status == "fail" && check.Code != "" {
					code = check.Code
					break
				}
			}
			response.Error = newErrorData("doctor", response.Data, &commandError{
				code: code, message: "one or more required MailCLI checks failed",
			})
		}
		if code := writeJSON(stdout, response); code != 0 {
			return code
		}
	} else {
		for _, check := range report.Checks {
			writeFormat(stdout, "%-28s %-7s %s\n", check.Name, check.Status, check.Detail)
		}
		for _, timing := range timings {
			writeFormat(stdout, "%-28s %7.2f ms\n", timing.Phase, timing.Milliseconds)
		}
	}

	if !healthy {
		return 1
	}
	return 0
}

func parseBooleanFlags(args []string, allowed ...string) (map[string]bool, error) {
	values := make(map[string]bool, len(allowed))
	for _, flag := range allowed {
		values[flag] = false
	}

	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			return nil, fmt.Errorf("unexpected argument %q", arg)
		}
		if _, exists := values[arg]; !exists {
			return nil, fmt.Errorf("unknown flag %q", arg)
		}
		values[arg] = true
	}

	return values, nil
}

func writeJSON(writer io.Writer, value envelope) int {
	if value.Data.StoreProfile == nil && invocationStoreProfile != nil {
		value.Data.StoreProfile = invocationStoreProfile
	}
	payload, err := marshalEnvelope(value)
	if err != nil {
		return 1
	}
	return writeEnvelopeBytes(writer, payload)
}

//go:noinline
func WriteFailureEnvelope(writer io.Writer, command string, code string, message string) int {
	return writeFailureEnvelope(writer, command, code, message, nil, nil)
}

func writeFailureEnvelope(
	writer io.Writer,
	command string,
	code string,
	message string,
	validSubcommands []string,
	details *unknownSubcommandData,
) int {
	failure := newErrorData(command, responseData{}, &commandError{code: code, message: message})
	failure.ValidSubcommands = validSubcommands
	failure.Data = details
	return writeJSON(writer, envelope{
		SchemaVersion: schemaVersion,
		OK:            false,
		Command:       command,
		Data:          responseData{},
		Error:         failure,
	})
}

// AttemptedCommand returns the command identifier represented by normalized args.
func AttemptedCommand(args []string) string {
	if len(args) == 0 {
		return ""
	}
	command := strings.TrimLeft(args[0], "-")
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		switch command {
		case "accounts", "attachments", "batch", "drafts", "mailboxes", "messages", "send":
			return command + "." + args[1]
		}
	}
	return command
}

func firstOutputLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func isHelpArgument(argument string) bool {
	return argument == "help" || argument == "--help" || argument == "-h"
}

//go:noinline
func helpOnly(args []string) bool {
	found := false
	for _, argument := range args {
		if isHelpArgument(argument) {
			found = true
			continue
		}
		if argument != "--json" {
			return false
		}
	}
	return found
}

func writeHelp(writer io.Writer) {
	writeRaw(writer, `MailCLI
Local Apple Mail access for the shell and coding agents.

Usage:
  mailcli <command> [flags]
Commands:
`)
	for _, contract := range commandRootContracts() {
		command := strings.SplitN(contract.ID, ".", 2)[0]
		writeFormat(writer, "  %-13s %s\n", command, contract.helpDescription)
	}
	writeLine(writer, "  help          Show this command overview")
	writeRaw(writer, `
Mail 16 scripted draft save remains disabled; visible handoff never sends.
Direct SMTP send and IMAP mutations work without Mail.app: run 'mailcli send setup' once.
`)
	writeFormat(writer, "%s\nRun 'mailcli <command> --help' for focused usage and flags.\n", transport.ProviderSupportDescription())
}
