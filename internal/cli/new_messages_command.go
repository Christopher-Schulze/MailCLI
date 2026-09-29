package cli

import (
	"context"
	"io"
	"time"

	"mailcli/internal/mail"
)

const newMessagesTimeout = 30 * time.Second

func runMessagesNew(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("messages new", stderr)
	accountRef := flags.String("account", "", "account ref; omit to compare every account with credentials")
	mailboxRef := flags.String("mailbox", "", "mailbox ref, role, or exact path to compare with the server; default inbox")
	limit := flags.Int("limit", mail.DefaultNewMessagesLimit, "maximum new messages listed per mailbox (1-50)")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if *limit < 1 || *limit > mail.MaximumNewMessagesLimit {
		return failCommand("messages.new", *jsonOutput, &commandError{
			code: "invalid_argument", message: "--limit must be between 1 and 50",
		}, stdout, stderr)
	}
	operationCtx, cancel := context.WithTimeout(ctx, newMessagesTimeout)
	defer cancel()
	result, err := service.NewMessages(operationCtx, mail.NewMessagesRequest{
		AccountRef: *accountRef, MailboxRef: *mailboxRef, Limit: *limit,
	})
	if err != nil {
		return failCommand("messages.new", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "messages.new", responseData{NewMessages: &result})
	}
	writeFormat(stdout, "complete\t%t\tnew_count\t%d\n", result.Complete, result.NewCount)
	for _, mailbox := range result.Mailboxes {
		writeFormat(stdout, "mailbox\t%s\t%s\tserver=%d\tnew=%d\ttruncated=%t\t%s\n",
			mailbox.AccountRef, mailbox.Name, mailbox.ServerMessages, mailbox.NewCount, mailbox.Truncated, mailbox.State)
		rows := make([][]string, 0, len(mailbox.Messages))
		for _, message := range mailbox.Messages {
			state := "seen"
			if message.Unseen {
				state = "unseen"
			}
			rows = append(rows, []string{message.ServerRef, state, message.DateSent, message.Sender, message.Subject})
		}
		if !writeTerminalTable(stdout, []string{"SERVER REF", "STATE", "SENT", "FROM", "SUBJECT"}, rows) {
			for _, row := range rows {
				writeFormat(stdout, "%s\t%s\t%s\t%s\t%s\n", row[0], row[1], row[2], oneLine(row[3]), oneLine(row[4]))
			}
		}
	}
	if len(result.Skipped) > 0 {
		writeLine(stdout, "skipped")
		for _, skip := range result.Skipped {
			writeFormat(stdout, "%s\t%s\t%s\n", skip.Account, skip.Mailbox, skip.Reason)
		}
	}
	if len(result.Failures) > 0 {
		writeLine(stdout, "failures")
		for _, failure := range result.Failures {
			writeFormat(stdout, "%s\t%s\t%s\t%s\n", failure.Account, failure.Mailbox, failure.Code, failure.Message)
		}
	}
	return 0
}
