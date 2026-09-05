package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"mailcli/internal/mail"
)

const readTimeout = 60 * time.Second

type codedError interface {
	error
	ErrorCode() string
}

func runAccounts(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) > 0 && isHelpArgument(args[0]) {
		writeLine(stdout, "Usage:\n  mailcli accounts list [--json]")
		return 0
	}
	if len(args) == 0 || args[0] != "list" {
		writeLine(stderr, "Usage:\n  mailcli accounts list [--json]")
		return 2
	}
	flags := newFlagSet("accounts list", stderr)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args[1:], stdout, stderr); code >= 0 {
		return code
	}

	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	accounts, err := service.ListAccounts(operationCtx)
	if err != nil {
		return failCommand("accounts.list", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "accounts.list", responseData{Accounts: &accounts})
	}
	rows := make([][]string, 0, len(accounts))
	for _, account := range accounts {
		emailList := strings.Join(account.EmailAddresses, ",")
		if account.State == "degraded" {
			emailList = "degraded: " + account.DegradedReason
		}
		rows = append(rows, []string{account.Ref, account.Name, emailList})
	}
	if writeTerminalTable(stdout, []string{"REF", "ACCOUNT", "EMAIL ADDRESSES"}, rows) {
		for _, account := range accounts {
			if account.State == "degraded" {
				writeFormat(stdout, "warning: account %s is degraded: %s\n", account.Ref, account.DegradedReason)
			}
		}
		return 0
	}
	for _, account := range accounts {
		line := fmt.Sprintf("%s\t%s\t%s", account.Ref, oneLine(account.Name), strings.Join(account.EmailAddresses, ","))
		if account.State == "degraded" {
			line += "\tdegraded: " + account.DegradedReason
		}
		writeFormat(stdout, "%s\n", line)
	}
	return 0
}

func runMailboxes(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		writeLine(stdout, "Usage:\n  mailcli mailboxes <list|resolve> [options]")
		return 0
	}
	if args[0] == "resolve" {
		return runMailboxResolve(ctx, service, args[1:], stdout, stderr)
	}
	if args[0] != "list" {
		writeLine(stderr, "Usage:\n  mailcli mailboxes <list|resolve> [options]")
		return 2
	}
	flags := newFlagSet("mailboxes list", stderr)
	accountRef := flags.String("account", "", "scope to an account ref")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args[1:], stdout, stderr); code >= 0 {
		return code
	}

	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	mailboxes, err := service.ListMailboxes(operationCtx, mail.ListMailboxesRequest{AccountRef: *accountRef})
	if err != nil {
		return failCommand("mailboxes.list", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "mailboxes.list", responseData{Mailboxes: &mailboxes})
	}
	rows := make([][]string, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		rows = append(rows, []string{mailbox.Ref, strings.Join(mailbox.Path, "/"), fmt.Sprint(mailbox.UnreadCount)})
	}
	if writeTerminalTable(stdout, []string{"REF", "MAILBOX", "UNREAD"}, rows) {
		return 0
	}
	for _, mailbox := range mailboxes {
		writeFormat(stdout, "%s\t%s\t%d\n", mailbox.Ref, strings.Join(mailbox.Path, "/"), mailbox.UnreadCount)
	}
	return 0
}

func runMailboxResolve(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("mailboxes resolve", stderr)
	accountRef := flags.String("account", "", "account ref")
	var path stringListFlag
	flags.Var(&path, "path", "exact mailbox path segment; repeat for nested folders")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if *accountRef == "" {
		return failCommand("mailboxes.resolve", *jsonOutput, invalidDraftInput("missing required --account"), stdout, stderr)
	}
	if len(path) == 0 {
		return failCommand("mailboxes.resolve", *jsonOutput, invalidDraftInput("missing required --path"), stdout, stderr)
	}
	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	mailbox, err := service.ResolveMailbox(operationCtx, *accountRef, path)
	if err != nil {
		return failCommand("mailboxes.resolve", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "mailboxes.resolve", responseData{Mailbox: &mailbox})
	}
	writeFormat(stdout, "%s\t%s\t%d\n", mailbox.Ref, strings.Join(mailbox.Path, "/"), mailbox.UnreadCount)
	return 0
}

type stringListFlag []string

func (values *stringListFlag) String() string {
	return strings.Join(*values, "/")
}

func (values *stringListFlag) Set(value string) error {
	if value == "" {
		return fmt.Errorf("mailbox path segment must not be empty")
	}
	*values = append(*values, value)
	return nil
}

func runMessages(
	ctx context.Context,
	mailService *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	if len(args) == 0 {
		writeLine(stderr, "Usage:\n  mailcli messages <list|filter|search|get|raw|reply|forward|mark|move|copy|delete> [options]")
		return 2
	}
	switch args[0] {
	case "help", "--help", "-h":
		writeLine(stdout, "Usage:\n  mailcli messages <list|filter|search|get|raw|reply|forward|mark|move|copy|delete> [options]")
		return 0
	case "list":
		return runMessagesList(ctx, mailService, args[1:], stdout, stderr)
	case "filter":
		return runMessagesFilter(ctx, mailService, args[1:], stdout, stderr)
	case "search":
		return runMessagesSearch(ctx, mailService, args[1:], stdout, stderr)
	case "get":
		return runMessagesGet(ctx, mailService, args[1:], stdout, stderr)
	case "raw":
		return runMessagesRaw(ctx, mailService, args[1:], stdout, stderr)
	case "reply":
		return runMessageReply(ctx, mailService, args[1:], stdout, stderr)
	case "forward":
		return runMessageForward(ctx, mailService, args[1:], stdout, stderr)
	case "mark":
		return runMessageMark(ctx, mailService, args[1:], stdout, stderr)
	case "move":
		return runMessageTransfer(ctx, mailService, false, args[1:], stdout, stderr)
	case "copy":
		return runMessageTransfer(ctx, mailService, true, args[1:], stdout, stderr)
	case "delete":
		return runMessageDelete(ctx, mailService, args[1:], stdout, stderr)
	default:
		writeFormat(stderr, "unknown messages command %q\n", args[0])
		return 2
	}
}

func runMessagesList(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("messages list", stderr)
	mailboxRef := flags.String("mailbox", "", "mailbox ref")
	cursor := flags.String("cursor", "", "pagination cursor")
	limit := flags.Int("limit", mail.DefaultPageLimit, "page size")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}

	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	page, err := service.ListMessages(operationCtx, mail.ListMessagesRequest{
		MailboxRef: *mailboxRef, Cursor: *cursor, Limit: *limit,
	})
	if err != nil {
		return failCommand("messages.list", *jsonOutput, err, stdout, stderr)
	}
	return writeMessagePage(stdout, "messages.list", page, *jsonOutput)
}

func runMessagesGet(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("messages get", stderr)
	ref := flags.String("ref", "", "message ref")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if *ref == "" {
		return failCommand("messages.get", *jsonOutput, invalidDraftInput("missing required --ref"), stdout, stderr)
	}

	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	message, err := service.GetMessage(operationCtx, *ref)
	if err != nil {
		return failCommand("messages.get", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "messages.get", responseData{Message: &message})
	}
	if err := writeMessage(stdout, message); err != nil {
		return 1
	}
	return 0
}

func runMessagesRaw(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("messages raw", stderr)
	ref := flags.String("ref", "", "message ref")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if *ref == "" {
		return failCommand("messages.raw", *jsonOutput, invalidDraftInput("missing required --ref"), stdout, stderr)
	}

	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	if *jsonOutput {
		raw, err := service.GetRawSource(operationCtx, *ref)
		if err != nil {
			return failCommand("messages.raw", true, err, stdout, stderr)
		}
		return writeSuccess(stdout, "messages.raw", responseData{RawSource: &raw})
	}
	if err := service.WriteRawSource(operationCtx, *ref, stdout); err != nil {
		return failCommand("messages.raw", false, err, stdout, stderr)
	}
	return 0
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	return flags
}

func parseFlags(flags *flag.FlagSet, args []string, stdout io.Writer, stderr io.Writer) int {
	if helpOnly(args) {
		writeFlagUsage(flags, stdout)
		return 0
	}
	flags.SetOutput(io.Discard)
	err := flags.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		writeFlagUsage(flags, stdout)
		return 0
	}
	if err != nil || flags.NArg() != 0 {
		if err != nil {
			writeLine(stderr, err)
		} else {
			writeFormat(stderr, "unexpected argument %q\n", flags.Arg(0))
		}
		writeFlagUsage(flags, stderr)
		return 2
	}
	return -1
}

func writeFlagUsage(flags *flag.FlagSet, writer io.Writer) {
	type optionHelp struct {
		synopsis    string
		description string
	}
	options := make([]optionHelp, 0, flags.NFlag()+1)
	width := 0
	flags.VisitAll(func(option *flag.Flag) {
		valueName, description := flag.UnquoteUsage(option)
		valueName, description = polishedFlagValue(option.Name, valueName, description)
		synopsis := "--" + option.Name
		if valueName != "" {
			synopsis += " <" + valueName + ">"
		}
		description = capitalizeHelp(description)
		if value := visibleFlagDefault(option); value != "" {
			description += " (default: " + value + ")"
		}
		options = append(options, optionHelp{synopsis: synopsis, description: description})
		width = max(width, len(synopsis))
	})
	helpSynopsis := "-h, --help"
	options = append(options, optionHelp{synopsis: helpSynopsis, description: "Show command help"})
	width = max(width, len(helpSynopsis))

	writeFormat(writer, "Usage:\n  mailcli %s [options]\n\nOptions:\n", flags.Name())
	for _, option := range options {
		writeFormat(writer, "  %-*s  %s\n", width, option.synopsis, option.description)
	}
}

func polishedFlagValue(name string, inferred string, description string) (string, string) {
	if strings.Contains(description, " (true|false)") {
		return "true|false", strings.Replace(description, " (true|false)", "", 1)
	}
	switch name {
	case "account", "mailbox", "message", "ref":
		return "ref", description
	case "attachment":
		return "id", description
	case "after", "before":
		return "date", description
	case "cursor":
		return "token", description
	case "input", "output":
		return "path", description
	case "limit", "max-messages":
		return "number", description
	case "max-bytes":
		return "bytes", description
	case "path":
		return "segment", description
	case "query", "recipient", "sender", "subject":
		return "text", description
	default:
		return inferred, description
	}
}

func visibleFlagDefault(option *flag.Flag) string {
	switch option.DefValue {
	case "", "0", "false":
		return ""
	case "-":
		return "standard input"
	}
	if option.Name == "max-bytes" && option.DefValue == "4294967296" {
		return "4 GiB"
	}
	return option.DefValue
}

func capitalizeHelp(value string) string {
	if value == "" || value[0] < 'a' || value[0] > 'z' {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func failCommand(command string, jsonOutput bool, err error, stdout io.Writer, stderr io.Writer) int {
	return failCommandWithData(command, jsonOutput, responseData{}, err, stdout, stderr)
}

func failCommandWithData(
	command string,
	jsonOutput bool,
	data responseData,
	err error,
	stdout io.Writer,
	stderr io.Writer,
) int {
	if !jsonOutput {
		writeLine(stderr, err)
		return commandExitCode(err)
	}
	code := "operation_failed"
	var typed codedError
	if errors.As(err, &typed) {
		code = typed.ErrorCode()
	}
	writeJSON(stdout, envelope{
		SchemaVersion: schemaVersion,
		OK:            false,
		Command:       command,
		Data:          data,
		Error:         &errorData{Code: code, Message: err.Error()},
	})
	return commandExitCode(err)
}

// commandExitCode maps error codes to exit codes:
// usage errors (invalid_argument, invalid_input, missing_required,
// unknown_command) return 2; all other errors return 1.
func commandExitCode(err error) int {
	var typed codedError
	if errors.As(err, &typed) {
		switch typed.ErrorCode() {
		case "invalid_argument", "invalid_input", "missing_required",
			"unknown_command":
			return 2
		}
	}
	return 1
}

func writeSuccess(stdout io.Writer, command string, data responseData) int {
	return writeJSON(stdout, envelope{
		SchemaVersion: schemaVersion,
		OK:            true,
		Command:       command,
		Data:          data,
	})
}

func writeMessagePage(stdout io.Writer, command string, page mail.MessagePage, jsonOutput bool) int {
	if jsonOutput {
		return writeSuccess(stdout, command, responseData{Page: messageResponsePage(&page)})
	}
	rows := make([][]string, 0, len(page.Messages))
	for _, message := range page.Messages {
		rows = append(rows, []string{message.Ref, message.DateReceived, message.Sender, message.Subject})
	}
	if writeTerminalTable(stdout, []string{"REF", "RECEIVED", "FROM", "SUBJECT"}, rows) {
		if page.NextCursor != "" {
			writeFormat(stdout, "\nNext cursor: %s\n", page.NextCursor)
		}
		return 0
	}
	for _, message := range page.Messages {
		writeFormat(stdout, "%s\t%s\t%s\t%s\n", message.Ref, message.DateReceived, oneLine(message.Sender), oneLine(message.Subject))
	}
	if page.NextCursor != "" {
		writeFormat(stdout, "next_cursor\t%s\n", page.NextCursor)
	}
	return 0
}

func writeMessage(stdout io.Writer, message mail.Message) error {
	if _, err := fmt.Fprintf(stdout, "Ref: %s\n", message.Summary.Ref); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "From: %s\n", message.Summary.Sender); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "To: %s\n", formatRecipients(message.To)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "CC: %s\n", formatRecipients(message.CC)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "BCC: %s\n", formatRecipients(message.BCC)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Subject: %s\n", message.Summary.Subject); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Date received: %s\n", message.Summary.DateReceived); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Attachments: %d\n", len(message.Attachments)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Content source: %s\n", message.ContentSource); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Content complete: %t\n", message.ContentComplete); err != nil {
		return err
	}
	if len(message.MissingParts) > 0 {
		if _, err := fmt.Fprintf(stdout, "Missing parts: %s\n", strings.Join(message.MissingParts, ", ")); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(stdout); err != nil {
		return err
	}
	if _, err := stdout.Write([]byte(message.Content)); err != nil {
		return err
	}
	return nil
}

func formatRecipients(recipients []mail.Recipient) string {
	values := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		if recipient.Name == "" {
			values = append(values, recipient.Address)
			continue
		}
		values = append(values, fmt.Sprintf("%s <%s>", recipient.Name, recipient.Address))
	}
	return strings.Join(values, ", ")
}

func oneLine(value string) string {
	return strings.Map(func(char rune) rune {
		if unicode.IsControl(char) || unicode.Is(unicode.Zl, char) || unicode.Is(unicode.Zp, char) {
			return ' '
		}
		return char
	}, value)
}
