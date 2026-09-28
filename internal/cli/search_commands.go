package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"mailcli/internal/mail"
)

func runMessagesSearch(
	ctx context.Context,
	service *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	return runMessagesQuery(ctx, service, "messages.search", true, args, stdout, stderr)
}

func runMessagesFilter(
	ctx context.Context,
	service *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	return runMessagesQuery(ctx, service, "messages.filter", false, args, stdout, stderr)
}

func runMessagesQuery(
	ctx context.Context,
	service *mail.Service,
	command string,
	allowText bool,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	flags := newFlagSet(strings.ReplaceAll(command, ".", " "), stderr)
	query := mail.Query{}
	enrichment := addMessageEnrichmentFlags(flags, true)
	jsonOutput := defineSearchFlags(flags, &query, allowText)
	fields := flags.String("fields", "", "comma-separated page fields; use all for the complete page")
	maxOutputBytes := flags.Int64("max-bytes", defaultJSONOutputBytes, "maximum JSON response bytes")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if err := validatePageLimit(query.Limit); err != nil {
		return failCommand(command, *jsonOutput, err, stdout, stderr)
	}
	if err := validateMessageEnrichment(*enrichment); err != nil {
		return failCommand(command, *jsonOutput, err, stdout, stderr)
	}
	if err := validateOutputByteLimit(*maxOutputBytes); err != nil {
		return failCommand(command, *jsonOutput, err, stdout, stderr)
	}
	pageFields, projection, err := pageProjectionOptions(flags, projectionTargetSearchPage, *fields)
	if err != nil {
		return failCommand(command, *jsonOutput, err, stdout, stderr)
	}

	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	page, err := service.SearchMessages(operationCtx, query)
	if err != nil {
		if command == "messages.search" && errorCode(err) == "search_budget_too_small" {
			data := responseData{searchRecoveryArgs: buildSearchRecoveryArgs(query, *fields, *jsonOutput, err)}
			return failCommandWithData(command, *jsonOutput, data, err, stdout, stderr)
		}
		return failCommand(command, *jsonOutput, err, stdout, stderr)
	}
	summaries := make([]*mail.MessageSummary, len(page.Messages))
	for index := range page.Messages {
		summaries[index] = &page.Messages[index].Summary
	}
	if err := enrichSummaries(operationCtx, service, summaries, *enrichment); err != nil {
		return failCommand(command, *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		pageData := searchResponsePage(&page)
		if projection != nil {
			pageData = projectSearchPage(page, pageFields)
		}
		return writeBoundedListSuccess(stdout, command, responseData{Page: pageData, Projection: projection},
			*maxOutputBytes, listOutputRecovery(command))
	}
	writeSearchResults(stdout, page)
	return 0
}

func defineSearchFlags(flags *flag.FlagSet, query *mail.Query, allowText bool) *bool {
	if allowText {
		flags.StringVar(&query.Text, "query", "", "full-text terms")
	}
	flags.StringVar(&query.Sender, "sender", "", "sender substring")
	flags.StringVar(&query.Recipient, "recipient", "", "recipient substring")
	flags.StringVar(&query.Subject, "subject", "", "subject substring")
	flags.StringVar(&query.After, "after", "", "received at or after RFC3339 or YYYY-MM-DD")
	flags.StringVar(&query.Before, "before", "", "received before RFC3339 or YYYY-MM-DD")
	flags.StringVar(&query.AccountRef, "account", "", "account ref")
	flags.StringVar(&query.MailboxRef, "mailbox", "", "mailbox ref")
	flags.IntVar(&query.Limit, "limit", mail.DefaultPageLimit, "page size (1-200)")
	flags.StringVar(&query.Cursor, "cursor", "", "pagination cursor")
	flags.BoolVar(&query.ExactCount, "exact-count", false, "request a bounded exact candidate total")
	if allowText {
		flags.IntVar(&query.MaxMessages, "max-messages", mail.DefaultSearchMaxMessages, "maximum messages for body search")
		flags.Int64Var(&query.MaxBytes, "max-scan-bytes", mail.DefaultSearchMaxBytes, "maximum RFC bytes for body search")
	}
	addOptionalBoolFlag(flags, "read", "read status", &query.Read)
	addOptionalBoolFlag(flags, "flagged", "flagged status", &query.Flagged)
	addOptionalBoolFlag(flags, "attachment", "attachment presence", &query.HasAttachment)
	return flags.Bool("json", false, "emit JSON")
}

func buildSearchRecoveryArgs(query mail.Query, fields string, jsonOutput bool, err error) []string {
	var sized interface{ RequiredBytes() int64 }
	if !errors.As(err, &sized) {
		return nil
	}
	requiredBytes := sized.RequiredBytes()
	if requiredBytes <= query.MaxBytes || requiredBytes > mail.MaximumSearchMaxBytes {
		return nil
	}
	args := make([]string, 0, 34)
	args = appendSearchStringArg(args, "--query", query.Text)
	args = appendSearchStringArg(args, "--sender", query.Sender)
	args = appendSearchStringArg(args, "--recipient", query.Recipient)
	args = appendSearchStringArg(args, "--subject", query.Subject)
	args = appendSearchStringArg(args, "--after", query.After)
	args = appendSearchStringArg(args, "--before", query.Before)
	args = appendSearchStringArg(args, "--account", query.AccountRef)
	args = appendSearchStringArg(args, "--mailbox", query.MailboxRef)
	args = append(args, "--limit", strconv.Itoa(query.Limit))
	args = appendSearchStringArg(args, "--cursor", query.Cursor)
	if query.ExactCount {
		args = append(args, "--exact-count")
	}
	args = append(args, "--max-messages", strconv.Itoa(query.MaxMessages))
	args = append(args, "--max-scan-bytes", strconv.FormatInt(requiredBytes, 10))
	args = appendSearchBoolArg(args, "--read", query.Read)
	args = appendSearchBoolArg(args, "--flagged", query.Flagged)
	args = appendSearchBoolArg(args, "--attachment", query.HasAttachment)
	args = appendSearchStringArg(args, "--fields", fields)
	if jsonOutput {
		args = append(args, "--json")
	}
	return args
}

func appendSearchStringArg(args []string, name, value string) []string {
	if value == "" {
		return args
	}
	return append(args, name, value)
}

func appendSearchBoolArg(args []string, name string, value *bool) []string {
	if value == nil {
		return args
	}
	return append(args, name, strconv.FormatBool(*value))
}

func writeSearchResults(stdout io.Writer, page mail.SearchPage) {
	rows := make([][]string, 0, len(page.Messages))
	for _, result := range page.Messages {
		message := result.Summary
		rows = append(rows, []string{message.Ref, message.DateReceived, message.Sender, message.Subject, result.Snippet})
	}
	if writeTerminalTable(stdout, []string{"REF", "RECEIVED", "FROM", "SUBJECT", "MATCH"}, rows) {
		if page.NextCursor != "" {
			writeFormat(stdout, "\nNext cursor: %s\n", page.NextCursor)
		}
		writeFormat(
			stdout, "Coverage: %s, consistency=%s, revision=%s, complete=%t, scanned=%d, candidates=%d, candidates_exact=%t, catalog_proven=%d, bytes=%d\n",
			page.Coverage.Backend, page.Coverage.Consistency, page.Coverage.IndexRevision, page.Coverage.Complete,
			page.Coverage.ScannedMessages, page.Coverage.CandidateMessages,
			page.Coverage.CandidateMessagesExact,
			page.Coverage.CatalogProvenMessages, page.Coverage.ScannedBytes,
		)
		return
	}
	for _, result := range page.Messages {
		message := result.Summary
		writeFormat(
			stdout, "%s\t%s\t%s\t%s\t%s\n",
			message.Ref, message.DateReceived, oneLine(message.Sender), oneLine(message.Subject), oneLine(result.Snippet),
		)
	}
	if page.NextCursor != "" {
		writeFormat(stdout, "next_cursor\t%s\n", page.NextCursor)
	}
	writeFormat(
		stdout, "coverage\t%s\tconsistency=%s\trevision=%s\tcorpus_complete=%t\tscanned=%d\tcandidates=%d\tcandidates_exact=%t\tcatalog_proven=%d\tbytes=%d\n",
		page.Coverage.Backend, page.Coverage.Consistency, page.Coverage.IndexRevision, page.Coverage.Complete,
		page.Coverage.ScannedMessages, page.Coverage.CandidateMessages,
		page.Coverage.CandidateMessagesExact,
		page.Coverage.CatalogProvenMessages, page.Coverage.ScannedBytes,
	)
}

func addOptionalBoolFlag(flags flagDefiner, name string, usage string, destination **bool) {
	flags.Func(name, usage+" (true|false)", func(value string) error {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("%s must be true or false", name)
		}
		*destination = &parsed
		return nil
	})
}

type flagDefiner interface {
	Func(name string, usage string, function func(string) error)
}
