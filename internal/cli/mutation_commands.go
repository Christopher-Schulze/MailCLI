package cli

import (
	"context"
	"io"
	"strings"
	"time"

	"mailcli/internal/mail"
)

func runMessageMark(
	ctx context.Context,
	service *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	flags := newFlagSet("messages mark", stderr)
	ref := flags.String("ref", "", "message ref")
	request := mail.MarkMessageRequest{}
	addOptionalBoolFlag(flags, "read", "read status", &request.Read)
	addOptionalBoolFlag(flags, "flagged", "flagged status", &request.Flagged)
	addOptionalBoolFlag(flags, "junk", "junk status", &request.Junk)
	allowDraft := flags.Bool("allow-draft", false, "allow mutation when the source message is a draft")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	request.Ref = *ref
	request.AllowDraftMutation = *allowDraft
	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	message, err := service.MarkMessage(operationCtx, request)
	if err != nil {
		return failCommand("messages.mark", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "messages.mark", responseData{MessageState: &message})
	}
	writeFormat(stdout, "%s\tread=%t\tflagged=%t\tjunk=%t\n", message.Ref, message.Read, message.Flagged, message.Junk)
	return 0
}

func runMessageTransfer(
	ctx context.Context,
	service *mail.Service,
	copyMessage bool,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	command := "messages.move"
	if copyMessage {
		command = "messages.copy"
	}
	flags := newFlagSet(strings.ReplaceAll(command, ".", " "), stderr)
	ref := flags.String("ref", "", "message ref")
	mailboxRef := flags.String("mailbox", "", "destination mailbox ref")
	// Copy never alters the source draft, so the draft-mutation guard (and its flag)
	// applies to move only.
	var allowDraft *bool
	if !copyMessage {
		allowDraft = flags.Bool("allow-draft", false, "allow moving a source message that is a draft")
	}
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	request := mail.TransferMessageRequest{
		Ref: *ref, DestinationMailbox: *mailboxRef, Copy: copyMessage,
	}
	if allowDraft != nil {
		request.AllowDraftMutation = *allowDraft
	}
	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	message, err := service.TransferMessage(operationCtx, request)
	if err != nil {
		return failCommand(command, *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, command, responseData{MessageState: &message})
	}
	writeFormat(stdout, "%s\t%s\n", message.Ref, message.MailboxRef)
	return 0
}

func runMessageDelete(
	ctx context.Context,
	service *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	flags := newFlagSet("messages delete", stderr)
	ref := flags.String("ref", "", "message ref")
	confirm := flags.Bool("confirm", false, "confirm Mail.app deletion behavior")
	allowDraft := flags.Bool("allow-draft", false, "allow deleting a source message that is a draft")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if !*confirm {
		return failCommand("messages.delete", *jsonOutput, confirmationRequired("message delete"), stdout, stderr)
	}
	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	result, err := service.DeleteMessage(operationCtx, mail.DeleteMessageRequest{
		Ref: *ref, AllowDraftMutation: *allowDraft,
	})
	if err != nil {
		return failCommand("messages.delete", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "messages.delete", responseData{DeleteResult: &result})
	}
	writeFormat(stdout, "deleted\t%s\n", result.MessageRef)
	return 0
}

func runSync(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("sync", stderr)
	accountRef := flags.String("account", "", "account ref; omit to check all mail")
	checkOnly := flags.Bool("check", false, "check server vs local message counts over IMAP without launching Mail.app")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	operationCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if *checkOnly {
		checkResult, err := service.SyncCheck(operationCtx, *accountRef)
		if err != nil {
			return failCommand("sync", *jsonOutput, err, stdout, stderr)
		}
		if *jsonOutput {
			return writeSuccess(stdout, "sync", responseData{SyncCheck: &checkResult})
		}
		writeLine(stdout, "account\tmailbox\tlocal\tserver\tdelta\tunseen")
		for _, mbx := range checkResult.Mailboxes {
			writeFormat(stdout, "%s\t%s\t%d\t%d\t%+d\t%d\n",
				mbx.AccountRef, mbx.Name, mbx.LocalMessages, mbx.ServerMessages, mbx.Delta, mbx.Unseen)
		}
		if len(checkResult.Failures) > 0 {
			writeLine(stdout, "failures")
			writeLine(stdout, "account\tmailbox\tcode\tmessage")
			for _, failure := range checkResult.Failures {
				writeFormat(stdout, "%s\t%s\t%s\t%s\n",
					failure.Account, failure.Mailbox, failure.Code, failure.Message)
			}
		}
		return 0
	}

	result, err := service.Sync(operationCtx, *accountRef)
	if err != nil {
		return failCommand("sync", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "sync", responseData{SyncResult: &result})
	}
	writeLine(stdout, "mail synchronization triggered")
	return 0
}
