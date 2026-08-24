package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"mailcli/internal/mail"
)

func runAttachments(
	ctx context.Context,
	service *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	if len(args) > 0 && isHelpArgument(args[0]) {
		fmt.Fprintln(stdout, "usage: mailcli attachments <list|save> [flags]")
		return 0
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: mailcli attachments <list|save> [flags]")
		return 2
	}
	if args[0] == "list" {
		return runAttachmentsList(ctx, service, args[1:], stdout, stderr)
	}
	if args[0] == "save" {
		return runAttachmentsSave(ctx, service, args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "unknown attachments command %q\n", args[0])
	return 2
}

func runAttachmentsList(
	ctx context.Context,
	service *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	flags := newFlagSet("attachments list", stderr)
	messageRef := flags.String("message", "", "message ref")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	message, err := service.GetMessage(operationCtx, *messageRef)
	if err != nil {
		return failCommand("attachments.list", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		complete := message.ContentComplete
		missing := message.MissingParts
		return writeSuccess(stdout, "attachments.list", responseData{
			Attachments: &message.Attachments, ContentSource: message.ContentSource,
			ContentComplete: &complete, MissingParts: &missing,
		})
	}
	for _, attachment := range message.Attachments {
		size := "unknown"
		if attachment.SizeKnown {
			size = fmt.Sprintf("%d", attachment.Size)
		}
		fmt.Fprintf(
			stdout, "%s\tsize=%s\tsize_known=%t\tdownloaded=%t\t%s\n",
			attachment.ID, size, attachment.SizeKnown, attachment.Downloaded, oneLine(attachment.Name),
		)
	}
	fmt.Fprintf(
		stdout, "content\tsource=%s\tcomplete=%t\tmissing=%s\n",
		message.ContentSource, message.ContentComplete, oneLine(strings.Join(message.MissingParts, ",")),
	)
	return 0
}

func runAttachmentsSave(
	ctx context.Context,
	service *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	flags := newFlagSet("attachments save", stderr)
	messageRef := flags.String("message", "", "message ref")
	attachmentID := flags.String("attachment", "", "attachment id")
	outputPath := flags.String("output", "", "absolute non-existing output path")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	operationCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	saved, err := service.SaveAttachment(operationCtx, mail.SaveAttachmentRequest{
		MessageRef: *messageRef, AttachmentID: *attachmentID, OutputPath: *outputPath,
	})
	if err != nil {
		return failCommand("attachments.save", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "attachments.save", responseData{SavedAttachment: &saved})
	}
	fmt.Fprintf(stdout, "%s\t%d\t%s\n", saved.Path, saved.Size, saved.SHA256)
	return 0
}
