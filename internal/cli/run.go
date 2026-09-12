package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

const (
	name          = "mailcli"
	version       = "1.3.0"
	schemaVersion = 1
)

type envelope struct {
	SchemaVersion int          `json:"schema_version"`
	OK            bool         `json:"ok"`
	Command       string       `json:"command"`
	Data          responseData `json:"data"`
	Error         *errorData   `json:"error"`
}

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
	DeleteResult             *mail.DeleteResult           `json:"delete_result,omitempty"`
	SyncResult               *mail.SyncResult             `json:"sync_result,omitempty"`
	SyncCheck                *mail.SyncCheckResult        `json:"sync_check,omitempty"`
	BatchResult              *mail.BatchResult            `json:"batch_result,omitempty"`
	Finalization             *finalizationData            `json:"finalization,omitempty"`
	UpdateResult             *updateResult                `json:"update_result,omitempty"`
	serialization            *serializedProjection        `json:"-"`
	draftMutationCompleted   bool                         `json:"-"`
}

func rawResponsePage(value any) *json.RawMessage {
	payload, err := json.Marshal(value)
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
	Code                  string                      `json:"code"`
	Message               string                      `json:"message"`
	Guidance              *mail.OperationGuidance     `json:"guidance"`
	DraftRevisionConflict *mail.DraftRevisionConflict `json:"draft_revision_conflict,omitempty"`
	DraftEditor           *draftEditorEvidence        `json:"draft_editor,omitempty"`
}

func newErrorData(command string, data responseData, err error) *errorData {
	guidance := guidanceForResponse(command, data, err)
	var conflict *mail.DraftRevisionConflict
	if errors.As(err, &conflict) {
		guidance.Recovery = mail.RecoveryGuidance{
			Action: mail.RecoveryInspect, Command: "drafts.inspect",
			Args: []string{"--ref", conflict.Ref, "--view", "full", "--json"},
		}
	}
	var editor *draftEditorError
	var editorEvidence *draftEditorEvidence
	if errors.As(err, &editor) {
		editorEvidence = &editor.evidence
		guidance.ReplayAllowed, guidance.Retryability = false, mail.RetryObserveRequired
		guidance.Recovery = mail.RecoveryGuidance{Action: mail.RecoveryInspect, Command: "drafts.inspect",
			Args: []string{"--ref", editor.evidence.Ref, "--view", "full", "--json"}}
		if !editor.updateAttempted {
			guidance.Phase, guidance.EffectCertainty = mail.OperationPhaseExecution, mail.EffectNone
		}
	}
	return &errorData{Code: errorCode(err), Message: err.Error(), Guidance: &guidance, DraftRevisionConflict: conflict, DraftEditor: editorEvidence}
}

func guidanceForResponse(command string, data responseData, err error) mail.OperationGuidance {
	guidance := mail.GuidanceForError(command, err)
	if data.draftMutationCompleted && data.Draft != nil && data.Draft.Ref != "" && errorCode(err) == "output_too_large" {
		guidance.Phase, guidance.EffectCertainty = mail.OperationPhaseExecution, mail.EffectComplete
		guidance.Retryability, guidance.ReplayAllowed = mail.RetryObserveRequired, false
		guidance.Recovery = mail.RecoveryGuidance{
			Action: mail.RecoveryInspect, Command: "drafts.inspect",
			Args: []string{"--ref", data.Draft.Ref, "--view", "full", "--json"},
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
				guidance.Recovery.Command = "drafts.inspect"
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
	if message := data.Message; message != nil && message.Hydration != nil {
		guidance.Phase, guidance.EffectCertainty = mail.OperationPhaseHydration, mail.EffectNone
		if message.Summary.Ref != "" && (guidance.Recovery.Action == mail.RecoveryRetry || guidance.Recovery.Action == mail.RecoveryCorrect) &&
			(command == "messages.get" || command == "drafts.open") {
			guidance.Recovery.Command = command
			argument := "--ref"
			if command == "drafts.open" {
				argument = "--message"
			}
			guidance.Recovery.Args = []string{argument, message.Summary.Ref, "--json"}
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
	for index, argument := range args {
		if argument == "--ref" {
			if index+1 >= len(args) {
				return "", true
			}
			return args[index+1], true
		}
		if strings.HasPrefix(argument, "--ref=") {
			return strings.TrimPrefix(argument, "--ref="), true
		}
	}
	return "", false
}

func runJSONCommand(
	ctx context.Context,
	mailService *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
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

type commandSpec struct {
	run commandRunner
}

var commandRegistry = map[string]commandSpec{
	"help": {run: func(_ context.Context, _ *mail.Service, _ []string, stdout, _ io.Writer) int {
		writeHelp(stdout)
		return 0
	}},
	"--help": {run: func(_ context.Context, _ *mail.Service, _ []string, stdout, _ io.Writer) int {
		writeHelp(stdout)
		return 0
	}},
	"-h": {run: func(_ context.Context, _ *mail.Service, _ []string, stdout, _ io.Writer) int {
		writeHelp(stdout)
		return 0
	}},
	"version": {run: func(_ context.Context, _ *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runVersion(args, stdout, stderr)
	}},
	"--version": {run: func(_ context.Context, _ *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runVersion(args, stdout, stderr)
	}},
	"update": {run: func(ctx context.Context, _ *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runUpdate(ctx, args, stdout, stderr)
	}},
	"capabilities": {run: func(_ context.Context, _ *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runCapabilities(args, stdout, stderr)
	}},
	"doctor": {run: func(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runDoctor(ctx, service, args, stdout, stderr)
	}},
	"batch": {run: func(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runBatch(ctx, service, args, stdout, stderr)
	}},
	"accounts": {run: func(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runAccounts(ctx, service, args, stdout, stderr)
	}},
	"mailboxes": {run: func(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runMailboxes(ctx, service, args, stdout, stderr)
	}},
	"messages": {run: func(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runMessages(ctx, service, args, stdout, stderr)
	}},
	"attachments": {run: func(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runAttachments(ctx, service, args, stdout, stderr)
	}},
	"drafts": {run: func(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runDrafts(ctx, service, args, stdout, stderr)
	}},
	"send": {run: func(_ context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
		if service == nil {
			return runSend(args, stdout, stderr)
		}
		return runSendWithBindings(args, stdout, stderr, service.InvalidateCredentials, service.AccountBindingStore())
	}},
	"sync": {run: func(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runSync(ctx, service, args, stdout, stderr)
	}},
}

func runCommand(
	ctx context.Context,
	mailService *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	spec, ok := commandRegistry[args[0]]
	if !ok {
		writeFormat(stderr, "unknown command %q\nRun 'mailcli help' to list available commands.\n", args[0])
		return 2
	}
	return spec.run(ctx, mailService, args[1:], stdout, stderr)
}

// NormalizeGlobalJSON moves a global --json flag to the command tail.
func NormalizeGlobalJSON(args []string) ([]string, bool) {
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
	payload, err := marshalEnvelope(value)
	if err != nil {
		return 1
	}
	return writeEnvelopeBytes(writer, payload)
}

//go:noinline
func WriteFailureEnvelope(writer io.Writer, command string, code string, message string) int {
	return writeJSON(writer, envelope{
		SchemaVersion: schemaVersion,
		OK:            false,
		Command:       command,
		Data:          responseData{},
		Error:         newErrorData(command, responseData{}, &commandError{code: code, message: message}),
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
  accounts      List configured accounts and sender identities
  mailboxes     List and resolve exact mailbox paths
  messages      List, search, read, reply, forward, and organize messages
  attachments   List and save received attachments
  batch         Execute bounded explicit reads, attachment saves, and marks
  drafts        Create, preview, edit, hand off, and prune drafts
  send          Store or remove app-specific SMTP send credentials
  sync          Synchronize with Mail.app or check server status over IMAP (--check)
  update        Check GitHub and install the latest verified release
  doctor        Verify the local MailCLI environment
  capabilities  Print the machine-readable command contract
  version       Print the installed version
  help          Show this command overview

Mail 16 scripted draft save remains disabled; visible handoff never sends.
Direct SMTP send and IMAP mutations work without Mail.app: run 'mailcli send setup' once.
`)
	writeFormat(writer, "%s\nRun 'mailcli <command> --help' for focused usage and flags.\n", transport.ProviderSupportDescription())
}
