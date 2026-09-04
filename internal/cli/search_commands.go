package cli

import (
	"context"
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
	jsonOutput := defineSearchFlags(flags, &query, allowText)
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}

	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	page, err := service.SearchMessages(operationCtx, query)
	if err != nil {
		return failCommand(command, *jsonOutput, err, stdout, stderr)
	}
	normalizeSearchCoverage(&page)
	if *jsonOutput {
		return writeSuccess(stdout, command, responseData{Page: searchResponsePage(&page)})
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
	flags.IntVar(&query.Limit, "limit", mail.DefaultPageLimit, "page size")
	flags.StringVar(&query.Cursor, "cursor", "", "pagination cursor")
	if allowText {
		flags.IntVar(&query.MaxMessages, "max-messages", mail.DefaultSearchMaxMessages, "maximum messages for body search")
		flags.Int64Var(&query.MaxBytes, "max-bytes", mail.DefaultSearchMaxBytes, "maximum RFC bytes for body search")
	}
	addOptionalBoolFlag(flags, "read", "read status", &query.Read)
	addOptionalBoolFlag(flags, "flagged", "flagged status", &query.Flagged)
	addOptionalBoolFlag(flags, "attachment", "attachment presence", &query.HasAttachment)
	return flags.Bool("json", false, "emit JSON")
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
			stdout, "Coverage: %s, complete=%t, scanned=%d/%d, bytes=%d\n",
			page.Coverage.Backend, page.Coverage.Complete,
			page.Coverage.ScannedMessages, page.Coverage.CandidateMessages, page.Coverage.ScannedBytes,
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
		stdout, "coverage\t%s\tcorpus_complete=%t\tscanned=%d/%d\tbytes=%d\n",
		page.Coverage.Backend, page.Coverage.Complete,
		page.Coverage.ScannedMessages, page.Coverage.CandidateMessages, page.Coverage.ScannedBytes,
	)
}

func normalizeSearchCoverage(page *mail.SearchPage) {
	if page.Coverage.Backend == "emlx_stream" &&
		page.Coverage.ScannedMessages+page.Coverage.CatalogProvenMessages < page.Coverage.CandidateMessages {
		page.Coverage.Complete = false
	}
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
