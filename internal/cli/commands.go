package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

const (
	readTimeout          = mail.LocalReadTimeout
	resolveTimeout       = mail.LocalReadTimeout
	hydrationParseMargin = 10 * time.Second
)

func hydrationReadBudget() time.Duration {
	return readTimeout + resolveTimeout + transport.TransferCommandBudget +
		transport.TransferBudgetForSize(mail.MaximumRawSourceBytes) + hydrationParseMargin
}

func hydrationReadContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, hydrationReadBudget())
}

type codedError interface {
	error
	ErrorCode() string
}

func runAccounts(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	return runCommandFamily(ctx, service, "accounts", args, stdout, stderr, nil)
}

func runAccountsList(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("accounts list", stderr)
	cursor := flags.String("cursor", "", "pagination cursor")
	limit := flags.Int("limit", mail.DefaultPageLimit, "page size (1-200)")
	maxBytes := flags.Int64("max-bytes", defaultJSONOutputBytes, "maximum JSON response bytes")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if err := validatePageLimit(*limit); err != nil {
		return failCommand("accounts.list", *jsonOutput, err, stdout, stderr)
	}
	if err := validateOutputByteLimit(*maxBytes); err != nil {
		return failCommand("accounts.list", *jsonOutput, err, stdout, stderr)
	}
	scope := listCursorScope("accounts")
	if err := validateCatalogCursorScope(*cursor, "accounts.list", scope); err != nil {
		return failCatalogCursor("accounts.list", *jsonOutput, stdout, stderr)
	}

	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	catalog, err := service.ListAccountCatalog(operationCtx)
	if err != nil {
		return failCommand("accounts.list", *jsonOutput, err, stdout, stderr)
	}
	allAccounts := catalog.Accounts
	complete := catalog.Complete && accountCatalogComplete(allAccounts)
	identityCoverageComplete := accountIdentityCoverageComplete(allAccounts)
	accounts, nextCursor, err := paginateCatalog(allAccounts, *limit, *cursor, "accounts.list", scope,
		func(account mail.Account) string { return account.Ref })
	if err != nil {
		if errorCode(err) == "invalid_cursor" {
			return failCatalogCursor("accounts.list", *jsonOutput, stdout, stderr)
		}
		return failCommand("accounts.list", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeBoundedListSuccess(stdout, "accounts.list", responseData{
			Accounts: &accounts, Complete: &complete,
			IdentityCoverageComplete: &identityCoverageComplete,
			Page:                     rawResponsePage(catalogPageMetadata{Limit: *limit, NextCursor: nextCursor}),
		}, *maxBytes, listOutputRecovery("accounts.list"))
	}
	rows := make([][]string, 0, len(accounts))
	for _, account := range accounts {
		emailList := strings.Join(account.EmailAddresses, ",")
		if account.State == "degraded" {
			emailList = "degraded: " + account.DegradedReason
		}
		rows = append(rows, []string{account.Ref, account.Name, emailList, identityCoverageSummary(account.IdentityCoverage), directOpsSummary(account)})
	}
	if writeTerminalTable(stdout, []string{"REF", "ACCOUNT", "EMAIL ADDRESSES", "IDENTITY COVERAGE", "DIRECT OPS"}, rows) {
		writeAccountCatalogStatus(stdout, complete, identityCoverageComplete)
		writeAccountCatalogWarnings(stdout, accounts)
		return 0
	}
	for _, account := range accounts {
		line := fmt.Sprintf(
			"%s\t%s\t%s\t%s\t%s",
			account.Ref, oneLine(account.Name), strings.Join(account.EmailAddresses, ","),
			identityCoverageSummary(account.IdentityCoverage), directOpsSummary(account),
		)
		if account.State == "degraded" {
			line += "\tdegraded: " + account.DegradedReason
		}
		writeFormat(stdout, "%s\n", line)
	}
	writeAccountCatalogStatus(stdout, complete, identityCoverageComplete)
	writeAccountCatalogWarnings(stdout, accounts)
	return 0
}

func accountCatalogComplete(accounts []mail.Account) bool {
	for _, account := range accounts {
		if account.State == "degraded" {
			return false
		}
	}
	return true
}

func accountIdentityCoverageComplete(accounts []mail.Account) bool {
	for _, account := range accounts {
		switch account.IdentityCoverage.State {
		case mail.SenderIdentityCoverageStateComplete,
			mail.SenderIdentityCoverageStateNoValidSender,
			mail.SenderIdentityCoverageStateNoSentMailbox,
			mail.SenderIdentityCoverageStateConfigured,
			mail.SenderIdentityCoverageStateNotApplicable:
			continue
		case mail.SenderIdentityCoverageStateBounded,
			mail.SenderIdentityCoverageStateNotObserved,
			mail.SenderIdentityCoverageStateUnavailable:
			return false
		default:
			return false
		}
	}
	return true
}

func identityCoverageSummary(coverage mail.SenderIdentityCoverage) string {
	state := string(coverage.State)
	if state == "" {
		state = string(mail.SenderIdentityCoverageStateUnavailable)
	}
	if coverage.Limit == 0 {
		return state
	}
	more := ""
	if coverage.MoreAvailable {
		more = "+"
	}
	return fmt.Sprintf("%s:%d/%d%s", state, coverage.ObservedMessages, coverage.Limit, more)
}

func directOpsSummary(account mail.Account) string {
	if !account.DirectOpsSupported {
		return "no:" + string(mail.DirectOpsReasonUnsupportedProvider)
	}
	reason := string(account.DirectOpsReason)
	if reason == "" {
		reason = string(mail.DirectOpsReasonProviderSupported)
	}
	return "yes:" + reason
}

func writeAccountCatalogStatus(writer io.Writer, complete bool, identityCoverageComplete bool) {
	writeFormat(writer, "complete\t%t\n", complete)
	writeFormat(writer, "identity_coverage_complete\t%t\n", identityCoverageComplete)
}

func writeAccountCatalogWarnings(writer io.Writer, accounts []mail.Account) {
	for _, account := range accounts {
		if account.State != "degraded" {
			continue
		}
		writeFormat(
			writer,
			"warning: account %s is degraded: %s; remediation: %s\n",
			account.Ref, account.DegradedReason, account.DegradedRemediation,
		)
	}
	for _, account := range accounts {
		if account.IdentityCoverage.State != mail.SenderIdentityCoverageStateBounded &&
			account.IdentityCoverage.State != mail.SenderIdentityCoverageStateNotObserved {
			continue
		}
		writeFormat(
			writer,
			"warning: account %s sender identity coverage is %s; observed %d of configured %d Sent messages and more history is available\n",
			account.Ref, account.IdentityCoverage.State,
			account.IdentityCoverage.ObservedMessages, account.IdentityCoverage.Limit,
		)
	}
}

func runMailboxes(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	return runCommandFamily(ctx, service, "mailboxes", args, stdout, stderr, nil)
}

func runMailboxesList(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("mailboxes list", stderr)
	accountRef := flags.String("account", "", "scope to an account ref")
	cursor := flags.String("cursor", "", "pagination cursor")
	limit := flags.Int("limit", mail.DefaultPageLimit, "page size (1-200)")
	maxBytes := flags.Int64("max-bytes", defaultJSONOutputBytes, "maximum JSON response bytes")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if err := validatePageLimit(*limit); err != nil {
		return failCommand("mailboxes.list", *jsonOutput, err, stdout, stderr)
	}
	if err := validateOutputByteLimit(*maxBytes); err != nil {
		return failCommand("mailboxes.list", *jsonOutput, err, stdout, stderr)
	}
	scope := listCursorScope("account", *accountRef)
	if err := validateCatalogCursorScope(*cursor, "mailboxes.list", scope); err != nil {
		return failCatalogCursor("mailboxes.list", *jsonOutput, stdout, stderr)
	}

	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	mailboxes, err := service.ListMailboxes(operationCtx, mail.ListMailboxesRequest{AccountRef: *accountRef})
	if err != nil {
		return failCommand("mailboxes.list", *jsonOutput, err, stdout, stderr)
	}
	mailboxes, nextCursor, err := paginateCatalog(mailboxes, *limit, *cursor, "mailboxes.list", scope,
		func(mailbox mail.Mailbox) string { return mailbox.Ref })
	if err != nil {
		if errorCode(err) == "invalid_cursor" {
			return failCatalogCursor("mailboxes.list", *jsonOutput, stdout, stderr)
		}
		return failCommand("mailboxes.list", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeBoundedListSuccess(stdout, "mailboxes.list", responseData{
			Mailboxes: &mailboxes,
			Page:      rawResponsePage(catalogPageMetadata{Limit: *limit, NextCursor: nextCursor}),
		}, *maxBytes, listOutputRecovery("mailboxes.list"))
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
	return runCommandFamily(ctx, mailService, "messages", args, stdout, stderr, nil)
}

func runMessagesList(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("messages list", stderr)
	enrichment := addMessageEnrichmentFlags(flags, true)
	mailboxRef := flags.String("mailbox", "", "mailbox ref, role, or exact path; omit for unified inbox")
	accountRef := flags.String("account", "", "scope to an account ref")
	cursor := flags.String("cursor", "", "pagination cursor")
	limit := flags.Int("limit", mail.DefaultPageLimit, "page size (1-200)")
	maxBytes := flags.Int64("max-bytes", defaultJSONOutputBytes, "maximum JSON response bytes")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	fields := flags.String("fields", "", "comma-separated page fields; use all for the complete page")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if err := validatePageLimit(*limit); err != nil {
		return failCommand("messages.list", *jsonOutput, err, stdout, stderr)
	}
	if err := validateMessageEnrichment(*enrichment); err != nil {
		return failCommand("messages.list", *jsonOutput, err, stdout, stderr)
	}
	if err := validateOutputByteLimit(*maxBytes); err != nil {
		return failCommand("messages.list", *jsonOutput, err, stdout, stderr)
	}
	pageFields, projection, err := pageProjectionOptions(flags, projectionTargetListPage, *fields)
	if err != nil {
		return failCommand("messages.list", *jsonOutput, err, stdout, stderr)
	}

	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	page, err := service.ListMessages(operationCtx, mail.ListMessagesRequest{
		MailboxRef: *mailboxRef, AccountRef: *accountRef, Cursor: *cursor, Limit: *limit,
	})
	if err != nil {
		return failCommand("messages.list", *jsonOutput, err, stdout, stderr)
	}
	summaries := make([]*mail.MessageSummary, len(page.Messages))
	for index := range page.Messages {
		summaries[index] = &page.Messages[index]
	}
	if err := enrichSummaries(ctx, operationCtx, service, summaries, *enrichment); err != nil {
		return failCommand("messages.list", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput && projection != nil {
		return writeBoundedListSuccess(stdout, "messages.list", responseData{
			Page: projectMessageListPage(page, pageFields), Projection: projection,
		}, *maxBytes, listOutputRecovery("messages.list"))
	}
	if *jsonOutput {
		return writeBoundedListSuccess(stdout, "messages.list", responseData{
			Page: messageResponsePage(&page),
		}, *maxBytes, listOutputRecovery("messages.list"))
	}
	return writeMessagePage(stdout, "messages.list", page, *jsonOutput)
}

func runMessageThread(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("messages thread", stderr)
	ref := flags.String("ref", "", "message ref")
	limit := flags.Int("limit", mail.DefaultPageLimit, "page size (1-200)")
	cursor := flags.String("cursor", "", "next or previous cursor from this thread")
	fields := flags.String("fields", "", "comma-separated message fields")
	maxBytes := flags.Int64("max-bytes", defaultJSONOutputBytes, "maximum JSON response bytes")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if err := validatePageLimit(*limit); err != nil {
		return failCommand("messages.thread", *jsonOutput, err, stdout, stderr)
	}
	if err := validateOutputByteLimit(*maxBytes); err != nil {
		return failCommand("messages.thread", *jsonOutput, err, stdout, stderr)
	}
	selectedFields, projection, projectionErr := pageProjectionOptions(flags, projectionTargetListPage, *fields)
	if projectionErr != nil {
		return failCommand("messages.thread", *jsonOutput, projectionErr, stdout, stderr)
	}

	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	thread, err := service.MessageThread(operationCtx, mail.MessageThreadRequest{
		Ref: *ref, Limit: *limit, Cursor: *cursor,
	})
	if err != nil {
		return failCommand("messages.thread", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		data := responseData{Thread: &thread, Projection: projection}
		if projection != nil {
			data = projectedMessageThreadData(data, thread, selectedFields)
		}
		return writeBoundedListSuccess(stdout, "messages.thread", data, *maxBytes,
			listOutputRecovery("messages.thread"))
	}
	writeFormat(stdout, "conversation_id\t%d\ttruncated\t%t\n", thread.ConversationID, thread.Truncated)
	rows := make([][]string, 0, len(thread.Messages))
	for _, message := range thread.Messages {
		rows = append(rows, []string{message.Ref, message.DateReceived, message.Sender, message.Subject})
	}
	if !writeTerminalTable(stdout, []string{"REF", "RECEIVED", "FROM", "SUBJECT"}, rows) {
		for _, message := range thread.Messages {
			writeFormat(stdout, "%s\t%s\t%s\t%s\n", message.Ref, message.DateReceived, oneLine(message.Sender), oneLine(message.Subject))
		}
	}
	if thread.NextCursor != "" {
		writeFormat(stdout, "\nNext cursor: %s\n", thread.NextCursor)
	}
	if thread.PrevCursor != "" {
		writeFormat(stdout, "\nPrevious cursor: %s\n", thread.PrevCursor)
	}
	return 0
}

func runMessagesGet(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("messages get", stderr)
	enrichment := addMessageEnrichmentFlags(flags, false)
	ref := flags.String("ref", "", "message ref")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	links := flags.String("links", string(mail.LinkModeFull), "URLs in the plain body: full, host (long URLs become <host>) or none")
	outputFlags := addOutputFlags(flags, projectionTargetMessage, defaultMessageOutputView, true)
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	output, err := outputFlags.options(projectionTargetMessage)
	if err != nil {
		return failCommand("messages.get", *jsonOutput, err, stdout, stderr)
	}
	if output.links, err = mail.ParseLinkMode(*links); err != nil {
		return failCommand("messages.get", *jsonOutput, err, stdout, stderr)
	}
	if err := validateMessageEnrichment(*enrichment); err != nil {
		return failCommand("messages.get", *jsonOutput, err, stdout, stderr)
	}
	if *ref == "" {
		return failProjectedEmpty("messages.get", *jsonOutput, output,
			invalidDraftInput("missing required --ref"), stdout, stderr)
	}

	operationCtx, cancel := hydrationReadContext(ctx)
	output.readRecoveryArgs = append([]string(nil), args...)
	defer cancel()
	intent := mail.MessageReadIntentFull
	if *jsonOutput {
		if output.fieldsProvided || output.view == outputViewMetadata {
			output.omitUnselectedMessageState = true
		}
		intent = messageReadIntentForProjection(output)
	}
	message, err := service.GetMessageWithIntent(operationCtx, *ref, intent)
	if err != nil {
		return failMessageRead("messages.get", *jsonOutput, message, err, stdout, stderr, output)
	}
	enrichment.Threading = true
	if message.Headers != "" {
		mail.ApplyThreadingHeaders(&message.Summary, message.Headers)
		enrichment.Threading = false
	}
	enrichment.Excerpt = output.includes("excerpt") || output.includes("excerpt_complete") || output.includes("excerpt_source")
	message.Summary, err = service.EnrichMessage(operationCtx, message.Summary, *enrichment)
	if err != nil {
		return failMessageRead("messages.get", *jsonOutput, message, err, stdout, stderr, output)
	}
	var exported *mail.ContentExport
	if output.exportPath != "" {
		value, exportErr := exportMessageBody(message, output.exportPath)
		if exportErr != nil {
			return failProjectedMessage("messages.get", *jsonOutput, message, output, exportErr, stdout, stderr)
		}
		exported = &value
	}
	if *jsonOutput {
		data := responseData{Message: &message, ContentExport: exported}
		return writeProjectedSuccess(stdout, "messages.get", data, output)
	}
	if output.exportPath != "" {
		writeFormat(stdout, "%s\t%d\t%s\n", exported.Path, exported.Size, exported.SHA256)
		return 0
	}
	message.Content = mail.ShortenLinks(message.Content, output.links)
	if err := writeMessage(stdout, message); err != nil {
		return 1
	}
	return 0
}

func failMessageRead(
	command string,
	jsonOutput bool,
	message mail.Message,
	err error,
	stdout io.Writer,
	stderr io.Writer,
	options ...outputOptions,
) int {
	var output outputOptions
	if len(options) > 0 {
		output = options[0]
	}
	if message.Hydration == nil {
		if !jsonOutput {
			writeLine(stderr, oneLine(publicFailureMessage(err)))
			return commandExitCodeFor(command, err, false)
		}
		if jsonOutput && output.target == projectionTargetMessage {
			if messageHasRecoveryData(message) {
				return writeProjectedFailure(stdout, command, responseData{Message: &message}, output, err, false)
			}
			return failProjectedEmpty(command, true, output, err, stdout, stderr)
		}
		return failCommand(command, jsonOutput, err, stdout, stderr)
	}
	safeErr := hydrationCommandError(message.Hydration, err)
	if jsonOutput {
		if output.target == projectionTargetMessage {
			return failProjectedMessage(command, true, message, output, safeErr, stdout, stderr)
		}
		return failCommandWithData(command, true, responseData{Message: &message}, safeErr, stdout, stderr)
	}
	if writeErr := writeMessage(stdout, message); writeErr != nil {
		return 1
	}
	writeLine(stderr, oneLine(safeErr.Error()))
	return commandExitCodeFor(command, safeErr, false)
}

func messageHasRecoveryData(message mail.Message) bool {
	return message.Summary.Ref != "" || message.Summary.MessageID != "" || message.Content != "" ||
		message.Headers != "" || message.ContentSource != "" || message.Hydration != nil ||
		len(message.To) > 0 || len(message.CC) > 0 || len(message.BCC) > 0 || len(message.Attachments) > 0
}

func hydrationCommandError(diagnostic *mail.HydrationDiagnostic, fallback error) error {
	code := "hydration_failed"
	if diagnostic != nil && diagnostic.Remote != nil && diagnostic.Remote.Code != "" {
		code = diagnostic.Remote.Code
	}
	if code == "hydration_failed" {
		var typed codedError
		if errors.As(fallback, &typed) && typed.ErrorCode() != "" {
			code = typed.ErrorCode()
		}
	}
	message := "message content is incomplete"
	if diagnostic != nil {
		switch code {
		case transport.CodeIMAPTimeout, "operation_timeout":
			message = "message content is incomplete; hydration timed out; no external mutation was attempted"
		case "operation_canceled":
			message = "message content is incomplete; hydration was canceled; no external mutation was attempted"
		}
		if diagnostic.Remediation != "" {
			message += "; " + diagnostic.Remediation
		}
	}
	if transport.IsSubmissionOutcomeUnknown(fallback) || transport.IsMutationOutcomeUnknown(fallback) || transport.IsMirrorOutcomeUncertain(fallback) {
		code = transport.ErrorCode(fallback)
	}
	return &commandError{code: code, message: message, cause: fallback}
}

func publicFailureMessage(err error) string {
	if transport.IsTLSVerificationFailure(err) {
		return "IMAP TLS certificate verification failed; correct certificate trust or the configured hostname before retrying"
	}
	var verification *tls.CertificateVerificationError
	if errors.As(err, &verification) {
		return "operation failed with an IMAP TLS verification cause; inspect retained outcome evidence and correct TLS configuration before retrying"
	}
	return err.Error()
}

func runMessagesRaw(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("messages raw", stderr)
	ref := flags.String("ref", "", "message ref")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	outputFlags := addOutputFlags(flags, projectionTargetRaw, defaultRawOutputView, true)
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	output, err := outputFlags.options(projectionTargetRaw)
	if err != nil {
		return failCommand("messages.raw", *jsonOutput, err, stdout, stderr)
	}
	if *ref == "" {
		return failProjectedEmpty("messages.raw", *jsonOutput, output,
			invalidDraftInput("missing required --ref"), stdout, stderr)
	}

	operationCtx, cancel := hydrationReadContext(ctx)
	defer cancel()
	if output.exportPath != "" {
		exported, exportErr := mail.WriteExclusiveContent(output.exportPath, func(writer io.Writer) error {
			return service.WriteRawSource(operationCtx, *ref, writer)
		})
		if exportErr != nil {
			if *jsonOutput {
				return failProjectedEmpty("messages.raw", true, output, exportErr, stdout, stderr)
			}
			return failCommand("messages.raw", false, exportErr, stdout, stderr)
		}
		if *jsonOutput {
			data := responseData{ContentExport: &exported}
			return writeProjectedSuccess(stdout, "messages.raw", data, output)
		}
		writeFormat(stdout, "%s\t%d\t%s\n", exported.Path, exported.Size, exported.SHA256)
		return 0
	}
	if *jsonOutput {
		raw, err := service.GetRawSource(operationCtx, *ref)
		if err != nil {
			return failProjectedEmpty("messages.raw", true, output, err, stdout, stderr)
		}
		return writeProjectedSuccess(stdout, "messages.raw", responseData{RawSource: &raw}, output)
	}
	if err := service.WriteRawSource(operationCtx, *ref, stdout); err != nil {
		return failCommand("messages.raw", false, err, stdout, stderr)
	}
	return 0
}

//go:noinline
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
	referenceState := referenceArgumentState{}
	if flags.Lookup("ref") != nil {
		args, referenceState = normalizeReferenceArguments(flags, args)
	}
	flags.SetOutput(io.Discard)
	err := flags.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		writeFlagUsage(flags, stdout)
		return 0
	}
	if err != nil {
		writeLine(stderr, flagParseError(flags, err))
		writeFlagUsage(flags, stderr)
		return 2
	}
	if flags.Lookup("ref") != nil {
		if err := referenceArgumentError(flags, referenceState); err != nil {
			jsonOutput := flags.Lookup("json")
			return failCommand(
				strings.ReplaceAll(flags.Name(), " ", "."),
				jsonOutput != nil && jsonOutput.Value.String() == "true",
				err, stdout, stderr,
			)
		}
		return -1
	}
	if flags.NArg() != 0 {
		writeFormat(stderr, "unexpected argument %q\n", flags.Arg(0))
		writeFlagUsage(flags, stderr)
		return 2
	}
	return -1
}

// flagParseError rewrites the flag package's single-dash messages so an agent
// sees the exact flag it wrote and the flags the command accepts.
func flagParseError(flags *flag.FlagSet, err error) error {
	const undefined = "flag provided but not defined: -"
	const needsValue = "flag needs an argument: -"
	message := err.Error()
	switch {
	case strings.HasPrefix(message, undefined):
		var valid []string
		flags.VisitAll(func(option *flag.Flag) { valid = append(valid, "--"+option.Name) })
		valid = append(valid, "--help")
		return fmt.Errorf("unknown flag --%s for %q; valid flags: %s",
			strings.TrimLeft(strings.TrimPrefix(message, undefined), "-"), flags.Name(), strings.Join(valid, ", "))
	case strings.HasPrefix(message, needsValue):
		return fmt.Errorf("flag --%s for %q needs a value", strings.TrimLeft(strings.TrimPrefix(message, needsValue), "-"), flags.Name())
	default:
		return err
	}
}

type referenceArgumentState struct {
	flagValues       []string
	missingFlagValue bool
}

func normalizeReferenceArguments(flags *flag.FlagSet, args []string) ([]string, referenceArgumentState) {
	normalized := make([]string, 0, len(args)+2)
	operands := make([]string, 0, 1)
	state := referenceArgumentState{}
	parsingOptions := true
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if !parsingOptions {
			operands = append(operands, argument)
			continue
		}
		if argument == "--" {
			parsingOptions = false
			continue
		}
		if argument == "" || argument == "-" || argument[0] != '-' {
			operands = append(operands, argument)
			continue
		}
		optionArgs, consumedThrough := normalizeReferenceOption(flags, args, index, &state)
		normalized = append(normalized, optionArgs...)
		index = consumedThrough
	}
	if len(operands) > 0 {
		normalized = append(normalized, "--")
		normalized = append(normalized, operands...)
	}
	return normalized, state
}

func normalizeReferenceOption(
	flags *flag.FlagSet,
	args []string,
	index int,
	state *referenceArgumentState,
) ([]string, int) {
	argument := args[index]
	name, value, hasValue := flagArgument(argument)
	option := flags.Lookup(name)
	if option == nil || !flagTakesValue(option.Value) {
		return []string{argument}, index
	}
	if hasValue {
		if name == "ref" {
			state.flagValues = append(state.flagValues, value)
		}
		return []string{argument}, index
	}
	if index+1 < len(args) {
		value = args[index+1]
		if name == "ref" {
			state.flagValues = append(state.flagValues, value)
		}
		return []string{argument, value}, index + 1
	}
	if name == "ref" {
		state.missingFlagValue = true
		return []string{"--ref="}, index
	}
	return []string{argument}, index
}

func flagArgument(argument string) (string, string, bool) {
	if strings.HasPrefix(argument, "--") {
		argument = argument[2:]
	} else {
		argument = argument[1:]
	}
	name, value, hasValue := strings.Cut(argument, "=")
	return name, value, hasValue
}

func flagTakesValue(value flag.Value) bool {
	booleanFlag, ok := value.(interface{ IsBoolFlag() bool })
	return !ok || !booleanFlag.IsBoolFlag()
}

func referenceArgumentError(flags *flag.FlagSet, state referenceArgumentState) error {
	if state.missingFlagValue {
		return &commandError{code: "invalid_argument", message: "--ref requires a value"}
	}
	for _, value := range state.flagValues {
		if value == "" {
			return &commandError{code: "invalid_argument", message: "--ref must not be empty"}
		}
	}
	if len(state.flagValues) > 1 {
		for _, value := range state.flagValues[1:] {
			if value != state.flagValues[0] {
				return &commandError{code: "invalid_argument", message: "repeated --ref values must match"}
			}
		}
	}
	operands := flags.Args()
	if len(operands) > 1 {
		return &commandError{code: "invalid_argument", message: "only one positional REF is allowed"}
	}
	if len(operands) == 1 && operands[0] == "" {
		return &commandError{code: "invalid_argument", message: "REF must not be empty"}
	}
	ref := flags.Lookup("ref")
	if len(operands) == 1 {
		if len(state.flagValues) > 0 && state.flagValues[0] != operands[0] {
			return &commandError{code: "invalid_argument", message: "--ref and positional REF must match"}
		}
		if len(state.flagValues) == 0 {
			if err := ref.Value.Set(operands[0]); err != nil {
				return &commandError{code: "invalid_argument", message: "invalid positional REF", cause: err}
			}
		}
	}
	if ref.Value.String() == "" {
		return &commandError{code: "invalid_argument", message: "missing required --ref or REF"}
	}
	return nil
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

	usage := flags.Name() + " [options]"
	if flags.Lookup("ref") != nil {
		usage = flags.Name() + " [REF] [options]"
	}
	writeFormat(writer, "Usage:\n  mailcli %s\n\nOptions:\n", usage)
	for _, option := range options {
		writeFormat(writer, "  %-*s  %s\n", width, option.synopsis, option.description)
	}
	if flags.Name() == "send setup" {
		writeFormat(writer, "\n%s\n", transport.ProviderSupportDescription())
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
	case "max-scan-bytes":
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
	if option.Name == "max-scan-bytes" && option.DefValue == "4294967296" {
		return "4 GiB"
	}
	if option.Name == "max-bytes" && option.DefValue == "1048576" {
		return "1 MiB"
	}
	return option.DefValue
}

func capitalizeHelp(value string) string {
	if value == "" || value[0] < 'a' || value[0] > 'z' {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

//go:noinline
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
	hasPartialEffects := len(data.PartialEffects) > 0 || data.draftMutationCompleted
	if !jsonOutput {
		writeLine(stderr, publicFailureMessage(err))
		return commandExitCodeFor(command, err, hasPartialEffects)
	}
	writeJSON(stdout, envelope{
		SchemaVersion: schemaVersion,
		OK:            false,
		Command:       command,
		Data:          data,
		Error:         newErrorData(command, data, err),
	})
	return commandExitCodeFor(command, err, hasPartialEffects)
}

// commandExitCode preserves the context-free exit-code contract for callers
// that do not carry a command name.
func commandExitCode(err error) int {
	return commandExitCodeFor("", err, false)
}

func commandExitCodeFor(command string, err error, hasPartialEffects bool) int {
	if hasPartialEffects || !isCallerInputError(err) {
		return 1
	}
	guidance := mail.GuidanceForError(command, err)
	if guidance.EffectCertainty != mail.EffectNone ||
		guidance.Retryability != mail.RetryUserInputRequired ||
		guidance.Recovery.Action != mail.RecoveryCorrect {
		return 1
	}
	return 2
}

func isCallerInputError(err error) bool {
	var typed codedError
	if !errors.As(err, &typed) {
		return false
	}
	switch code := typed.ErrorCode(); code {
	case "invalid_argument", "invalid_input", "missing_required", "unknown_command":
		return true
	case "invalid_reference", "invalid_cursor":
		var validation *mail.ValidationError
		return errors.As(err, &validation) && validation.ErrorCode() == code
	default:
		return false
	}
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
	if _, err := fmt.Fprintf(stdout, "Ref: %s\n", oneLine(message.Summary.Ref)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "From: %s\n", oneLine(message.Summary.Sender)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "To: %s\n", oneLine(formatRecipients(message.To))); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "CC: %s\n", oneLine(formatRecipients(message.CC))); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "BCC: %s\n", oneLine(formatRecipients(message.BCC))); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Subject: %s\n", oneLine(message.Summary.Subject)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Date received: %s\n", oneLine(message.Summary.DateReceived)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Attachments: %d\n", len(message.Attachments)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Content source: %s\n", oneLine(message.ContentSource)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Content complete: %t\n", message.ContentComplete); err != nil {
		return err
	}
	if len(message.MissingParts) > 0 {
		if _, err := fmt.Fprintf(stdout, "Missing parts: %s\n", oneLine(strings.Join(message.MissingParts, ", "))); err != nil {
			return err
		}
	}
	if message.Hydration != nil {
		if _, err := fmt.Fprintf(stdout, "Hydration state: %s\n", oneLine(string(message.Hydration.State))); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(stdout, "Hydration source: %s\n", oneLine(message.Hydration.AttemptedSource)); err != nil {
			return err
		}
		if cause := message.Hydration.Local; cause != nil {
			if _, err := fmt.Fprintf(stdout, "Hydration local: %s: %s\n", oneLine(cause.Code), oneLine(cause.Message)); err != nil {
				return err
			}
		}
		if cause := message.Hydration.Remote; cause != nil {
			if _, err := fmt.Fprintf(stdout, "Hydration remote: %s: %s\n", oneLine(cause.Code), oneLine(cause.Message)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(stdout, "Hydration remediation: %s\n", oneLine(message.Hydration.Remediation)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(stdout); err != nil {
		return err
	}
	if _, err := io.WriteString(stdout, terminalText(message.Content, true)); err != nil {
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
	return terminalText(value, false)
}

// terminalText follows the shared human-output policy; only body layout may
// retain LF and TAB. Raw source, exports, and JSON never use this boundary.
func terminalText(value string, multiline bool) string {
	return strings.Map(func(char rune) rune {
		if multiline && (char == '\n' || char == '\t') {
			return char
		}
		if unicode.IsControl(char) || unicode.Is(unicode.Zl, char) || unicode.Is(unicode.Zp, char) {
			return ' '
		}
		return char
	}, value)
}
