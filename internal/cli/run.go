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
	version       = "1.0.0"
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
	Name            string                `json:"name,omitempty"`
	Version         string                `json:"version,omitempty"`
	Checks          []mail.Check          `json:"checks,omitempty"`
	Accounts        *[]mail.Account       `json:"accounts,omitempty"`
	Mailboxes       *[]mail.Mailbox       `json:"mailboxes,omitempty"`
	Mailbox         *mail.Mailbox         `json:"mailbox,omitempty"`
	Page            *responsePage         `json:"page,omitempty"`
	Message         *mail.Message         `json:"message,omitempty"`
	MessageState    *mail.MessageSummary  `json:"message_state,omitempty"`
	RawSource       *string               `json:"raw_source,omitempty"`
	Attachments     *[]mail.Attachment    `json:"attachments,omitempty"`
	ContentSource   string                `json:"content_source,omitempty"`
	ContentComplete *bool                 `json:"content_complete,omitempty"`
	MissingParts    *[]string             `json:"missing_parts,omitempty"`
	SavedAttachment *mail.SavedAttachment `json:"saved_attachment,omitempty"`
	Draft           *mail.Draft           `json:"draft,omitempty"`
	Drafts          *[]mail.Draft         `json:"drafts,omitempty"`
	SavedDraft      *mail.SavedDraft      `json:"saved_draft,omitempty"`
	SendResult      *mail.SendResult      `json:"send_result,omitempty"`
	DeleteResult    *mail.DeleteResult    `json:"delete_result,omitempty"`
	SyncResult      *mail.SyncResult      `json:"sync_result,omitempty"`
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
) int {
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
	errorCode := "invalid_argument"
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

func (w *countingWriter) Write(payload []byte) (int, error) {
	written, err := w.writer.Write(payload)
	w.written += int64(written)
	return written, err
}

func runCommand(
	ctx context.Context,
	mailService *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	switch args[0] {
	case "help", "--help", "-h":
		writeHelp(stdout)
		return 0
	case "version", "--version":
		return runVersion(args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(ctx, mailService, args[1:], stdout, stderr)
	case "accounts":
		return runAccounts(ctx, mailService, args[1:], stdout, stderr)
	case "mailboxes":
		return runMailboxes(ctx, mailService, args[1:], stdout, stderr)
	case "messages":
		return runMessages(ctx, mailService, args[1:], stdout, stderr)
	case "attachments":
		return runAttachments(ctx, mailService, args[1:], stdout, stderr)
	case "drafts":
		return runDrafts(ctx, mailService, args[1:], stdout, stderr)
	case "sync":
		return runSync(ctx, mailService, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		writeHelp(stderr)
		return 2
	}
}

func normalizeGlobalJSON(args []string) ([]string, bool) {
	requested := false
	for _, argument := range args {
		if argument == "--json" {
			requested = true
			break
		}
	}
	if len(args) == 0 || args[0] != "--json" {
		return args, requested
	}

	normalized := make([]string, 0, len(args)-1)
	normalized = append(normalized, args[1:]...)
	if len(normalized) > 0 {
		normalized = append(normalized, "--json")
	}
	return normalized, true
}

func runVersion(args []string, stdout io.Writer, stderr io.Writer) int {
	if helpOnly(args) {
		fmt.Fprintln(stdout, "usage: mailcli version [--json]")
		return 0
	}
	jsonOutput, err := parseBooleanFlags(args, "--json")
	if err != nil {
		fmt.Fprintln(stderr, err)
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

	fmt.Fprintf(stdout, "%s %s\n", name, version)
	return 0
}

func runDoctor(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	if helpOnly(args) {
		fmt.Fprintln(stdout, "usage: mailcli doctor [--json] [--live]")
		return 0
	}
	flags, err := parseBooleanFlags(args, "--json", "--live")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	operationCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	report := service.Probe(operationCtx, flags["--live"])
	healthy := mail.IsHealthy(report)
	if flags["--json"] {
		response := envelope{
			SchemaVersion: schemaVersion,
			OK:            healthy,
			Command:       "doctor",
			Data:          responseData{Checks: report.Checks},
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
			fmt.Fprintf(stdout, "%-28s %-7s %s\n", check.Name, check.Status, check.Detail)
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
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
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
		case "accounts", "attachments", "drafts", "mailboxes", "messages":
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
	fmt.Fprint(writer, `MailCLI controls the locally configured macOS Mail app.

Usage:
  mailcli doctor [--json] [--live]
  mailcli accounts list [--json]
  mailcli mailboxes list [--account REF] [--json]
  mailcli mailboxes resolve --account REF --path SEGMENT [--path SEGMENT] [--json]
  mailcli messages list --mailbox REF [--limit N] [--cursor CURSOR] [--json]
  mailcli messages filter [filters] [--limit N] [--cursor CURSOR] [--json]
  mailcli messages search [filters] [--limit N] [--cursor CURSOR] [--json]
  mailcli messages get --ref REF [--json]
  mailcli messages raw --ref REF [--json]
  mailcli attachments list --message REF [--json]
  mailcli attachments save --message REF --attachment ID --output ABSOLUTE_PATH [--json]
  mailcli drafts create --input FILE|- [--json]
  mailcli drafts list [--json]
  mailcli drafts inspect --ref REF [--json]
  mailcli drafts update --ref REF --input FILE|- [--json]
  mailcli drafts save --ref REF [--json]
  mailcli drafts open --message REF [--json]
  mailcli drafts send --ref REF --confirm [--json]
  mailcli drafts reconcile --ref REF [--json]
  mailcli drafts discard --ref REF --confirm [--json]
  mailcli messages reply --message REF --input FILE|- [--all] [--json]
  mailcli messages forward --message REF --input FILE|- [--json]
  mailcli messages mark --ref REF [--read true|false] [--flagged true|false] [--junk true|false] [--json]
  mailcli messages move --ref REF --mailbox REF [--json]
  mailcli messages copy --ref REF --mailbox REF [--json]
  mailcli messages delete --ref REF --confirm [--json]
  mailcli sync [--account REF] [--json]
  mailcli version [--json]
  mailcli help

Commands:
  doctor   Verify macOS ARM64, Mail.app, osascript, and optional Apple Events access.
  accounts List enabled Mail.app accounts and sender identities.
  mailboxes Recursively list or resolve exact mailbox paths.
  messages List, filter, search, read, or return raw source for messages.
  attachments List or save received message attachments without overwriting files.
  drafts    Manage review drafts and save or open drafts through Mail.app.
  sync      Ask Mail.app to check all mail or synchronize one account.
  version  Print the CLI version.

Flags:
  --json   Emit a stable machine-readable JSON envelope.
  --live   Run the Mail.app Automation permission probe; macOS may request permission.

Run 'mailcli <command> --help' for command-specific flags.
`)
}
