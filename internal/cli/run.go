package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"mailcli/internal/mail"
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
	Name            string                  `json:"name,omitempty"`
	Version         string                  `json:"version,omitempty"`
	Capabilities    *capabilityManifest     `json:"capabilities,omitempty"`
	Checks          []mail.Check            `json:"checks,omitempty"`
	Timings         []mail.DiagnosticTiming `json:"timings,omitempty"`
	Accounts        *[]mail.Account         `json:"accounts,omitempty"`
	Mailboxes       *[]mail.Mailbox         `json:"mailboxes,omitempty"`
	Mailbox         *mail.Mailbox           `json:"mailbox,omitempty"`
	Page            *responsePage           `json:"page,omitempty"`
	Message         *mail.Message           `json:"message,omitempty"`
	MessageState    *mail.MessageSummary    `json:"message_state,omitempty"`
	RawSource       *string                 `json:"raw_source,omitempty"`
	Attachments     *[]mail.Attachment      `json:"attachments,omitempty"`
	ContentSource   string                  `json:"content_source,omitempty"`
	ContentComplete *bool                   `json:"content_complete,omitempty"`
	MissingParts    *[]string               `json:"missing_parts,omitempty"`
	SavedAttachment *mail.SavedAttachment   `json:"saved_attachment,omitempty"`
	Draft           *mail.Draft             `json:"draft,omitempty"`
	DraftPreview    *draftPreview           `json:"draft_preview,omitempty"`
	DraftHandoff    *draftHandoffResult     `json:"draft_handoff,omitempty"`
	Drafts          *[]draftListEntry       `json:"drafts,omitempty"`
	PruneResult     *mail.PruneDraftsResult `json:"prune,omitempty"`
	SavedDraft      *mail.SavedDraft        `json:"saved_draft,omitempty"`
	SendResult      *mail.SendResult        `json:"send_result,omitempty"`
	SendSetup       *sendSetupResult        `json:"send_setup,omitempty"`
	DeleteResult    *mail.DeleteResult      `json:"delete_result,omitempty"`
	SyncResult      *mail.SyncResult        `json:"sync_result,omitempty"`
	SyncCheck       *mail.SyncCheckResult   `json:"sync_check,omitempty"`
	UpdateResult    *updateResult           `json:"update_result,omitempty"`
}

type responsePage struct {
	message *mail.MessagePage
	search  *mail.SearchPage
}

func (p responsePage) MarshalJSON() ([]byte, error) {
	if p.message != nil && p.search == nil {
		return json.Marshal(p.message)
	}
	if p.search != nil && p.message == nil {
		return json.Marshal(p.search)
	}
	return nil, fmt.Errorf("response page must contain exactly one page type")
}

func messageResponsePage(page *mail.MessagePage) *responsePage {
	return &responsePage{message: page}
}

func searchResponsePage(page *mail.SearchPage) *responsePage {
	return &responsePage{search: page}
}

type errorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func Run(
	ctx context.Context,
	mailService *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) (code int) {
	trackedStdout := &errorTrackingWriter{writer: stdout}
	trackedStderr := &errorTrackingWriter{writer: stderr}
	stdout = trackedStdout
	stderr = trackedStderr
	defer func() {
		if trackedStdout.err != nil || trackedStderr.err != nil {
			code = 1
		}
	}()

	args, jsonOutput := normalizeGlobalJSON(args)
	if len(args) == 0 {
		if jsonOutput {
			if writeFailureEnvelope(stdout, "", "invalid_argument", "command is required") != 0 {
				return 1
			}
			return 2
		}
		writeHelp(stdout)
		return 0
	}
	if !jsonOutput {
		return runCommand(ctx, mailService, args, stdout, stderr)
	}
	return runJSONCommand(ctx, mailService, args, stdout, stderr)
}

func RequiresMailService(args []string) bool {
	args, _ = normalizeGlobalJSON(args)
	if len(args) == 0 {
		return false
	}
	if helpOnly(args[1:]) {
		return false
	}
	spec, ok := commandRegistry[args[0]]
	return ok && spec.requiresMailService != nil && spec.requiresMailService(args[1:])
}

func RequiresSignalContext(args []string) bool {
	args, _ = normalizeGlobalJSON(args)
	if len(args) == 0 || helpOnly(args[1:]) {
		return false
	}
	spec, ok := commandRegistry[args[0]]
	if !ok {
		return false
	}
	return (spec.requiresSignal != nil && spec.requiresSignal(args[1:])) ||
		(spec.requiresMailService != nil && spec.requiresMailService(args[1:]))
}

func RequiresMainThread(args []string) bool {
	args, _ = normalizeGlobalJSON(args)
	if len(args) == 0 || helpOnly(args[1:]) {
		return false
	}
	spec, ok := commandRegistry[args[0]]
	return ok && spec.requiresMainThread != nil && spec.requiresMainThread(args[1:])
}

func serviceCommandRequired(args []string) bool {
	return !helpOnly(args)
}

func accountCommandRequired(args []string) bool {
	return len(args) > 0 && args[0] == "list" && !helpOnly(args[1:])
}

func mailboxCommandRequired(args []string) bool {
	return len(args) > 0 && (args[0] == "list" || args[0] == "resolve") && !helpOnly(args[1:])
}

func messageCommandRequired(args []string) bool {
	if len(args) == 0 || helpOnly(args[1:]) {
		return false
	}
	switch args[0] {
	case "reply", "forward", "list", "filter", "search", "get", "raw", "mark", "move", "copy", "delete":
		return true
	default:
		return false
	}
}

func attachmentCommandRequired(args []string) bool {
	return len(args) > 0 && (args[0] == "list" || args[0] == "save") && !helpOnly(args[1:])
}

func draftCommandRequired(args []string) bool {
	return len(args) > 0 && (args[0] == "open" || args[0] == "reconcile") && !helpOnly(args[1:])
}

func draftSignalRequired(args []string) bool {
	return len(args) > 0 && (args[0] == "edit" || args[0] == "handoff") && !helpOnly(args[1:])
}

func draftHandoffRequired(args []string) bool {
	return len(args) > 0 && args[0] == "handoff" && !helpOnly(args[1:])
}

func runJSONCommand(
	ctx context.Context,
	mailService *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	commandOutput := countingWriter{writer: stdout}
	var commandError bytes.Buffer
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
	if writeFailureEnvelope(stdout, attemptedCommand(args), errorCode, message) != 0 {
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
	run                 commandRunner
	requiresMailService func([]string) bool
	requiresSignal      func([]string) bool
	requiresMainThread  func([]string) bool
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
	}, requiresSignal: func([]string) bool { return true }},
	"capabilities": {run: func(_ context.Context, _ *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runCapabilities(args, stdout, stderr)
	}},
	"doctor": {run: func(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runDoctor(ctx, service, args, stdout, stderr)
	}, requiresMailService: serviceCommandRequired},
	"accounts": {run: func(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runAccounts(ctx, service, args, stdout, stderr)
	}, requiresMailService: accountCommandRequired},
	"mailboxes": {run: func(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runMailboxes(ctx, service, args, stdout, stderr)
	}, requiresMailService: mailboxCommandRequired},
	"messages": {run: func(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runMessages(ctx, service, args, stdout, stderr)
	}, requiresMailService: messageCommandRequired},
	"attachments": {run: func(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runAttachments(ctx, service, args, stdout, stderr)
	}, requiresMailService: attachmentCommandRequired},
	"drafts": {run: func(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runDrafts(ctx, service, args, stdout, stderr)
	}, requiresMailService: draftCommandRequired, requiresSignal: draftSignalRequired, requiresMainThread: draftHandoffRequired},
	"send": {run: func(_ context.Context, _ *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runSend(args, stdout, stderr)
	}},
	"sync": {run: func(ctx context.Context, service *mail.Service, args []string, stdout, stderr io.Writer) int {
		return runSync(ctx, service, args, stdout, stderr)
	}, requiresMailService: func([]string) bool { return true }, requiresSignal: func([]string) bool { return true }},
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

func normalizeGlobalJSON(args []string) ([]string, bool) {
	requested := false
	moved := false
	normalized := make([]string, 0, len(args)+1)
	for index, argument := range args {
		if argument != "--json" {
			normalized = append(normalized, argument)
			continue
		}
		requested = true
		if index > 0 && strings.HasPrefix(args[index-1], "-") {
			normalized = append(normalized, argument)
			continue
		}
		moved = true
	}
	if !requested {
		return args, false
	}
	if moved || len(normalized) == 0 || normalized[len(normalized)-1] != "--json" {
		normalized = append(normalized, "--json")
	}
	if len(normalized) == 1 && normalized[0] == "--json" {
		return nil, true
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
	defer cancel()
	var report mail.DiagnosticReport
	var timings []mail.DiagnosticTiming
	if flags["--diagnostics"] {
		report, timings = service.ProbeWithDiagnostics(operationCtx, flags["--live"])
	} else {
		report = service.Probe(operationCtx, flags["--live"])
	}
	healthy := mail.IsHealthy(report)
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
			response.Error = &errorData{
				Code:    code,
				Message: "one or more required MailCLI checks failed",
			}
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
	// Marshal to a buffer first so a partial JSON envelope is never written
	// to the output. If marshalling fails, the caller can still emit a
	// fallback error envelope.
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return 1
	}
	if _, err := writer.Write(buf.Bytes()); err != nil {
		return 1
	}
	return 0
}

func writeFailureEnvelope(writer io.Writer, command string, code string, message string) int {
	return writeJSON(writer, envelope{
		SchemaVersion: schemaVersion,
		OK:            false,
		Command:       command,
		Data:          responseData{},
		Error:         &errorData{Code: code, Message: message},
	})
}

func attemptedCommand(args []string) string {
	if len(args) == 0 {
		return ""
	}
	command := strings.TrimLeft(args[0], "-")
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		switch command {
		case "accounts", "attachments", "drafts", "mailboxes", "messages", "send":
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
Run 'mailcli <command> --help' for focused usage and flags.
`)
}
