package cli

import (
	"context"
	"fmt"
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
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	request.Ref = *ref
	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	message, err := service.MarkMessage(operationCtx, request)
	if err != nil {
		return failCommand("messages.mark", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "messages.mark", responseData{MessageState: &message})
	}
	fmt.Fprintf(stdout, "%s\tread=%t\tflagged=%t\tjunk=%t\n", message.Ref, message.Read, message.Flagged, message.Junk)
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
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	message, err := service.TransferMessage(operationCtx, mail.TransferMessageRequest{
		Ref: *ref, DestinationMailbox: *mailboxRef, Copy: copyMessage,
	})
	if err != nil {
		return failCommand(command, *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, command, responseData{MessageState: &message})
	}
	fmt.Fprintf(stdout, "%s\t%s\n", message.Ref, message.MailboxRef)
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
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if !*confirm {
		return failCommand("messages.delete", *jsonOutput, confirmationRequired("message delete"), stdout, stderr)
	}
	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	result, err := service.DeleteMessage(operationCtx, *ref)
	if err != nil {
		return failCommand("messages.delete", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "messages.delete", responseData{DeleteResult: &result})
	}
	fmt.Fprintf(stdout, "deleted\t%s\n", result.MessageRef)
	return 0
}

func runSync(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("sync", stderr)
	accountRef := flags.String("account", "", "account ref; omit to check all mail")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	operationCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := service.Sync(operationCtx, *accountRef)
	if err != nil {
		return failCommand("sync", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "sync", responseData{SyncResult: &result})
	}
	fmt.Fprintln(stdout, "mail synchronization triggered")
	return 0
}
