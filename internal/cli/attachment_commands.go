package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"mailcli/internal/mail"
)

type attachmentSaver interface {
	SaveAttachment(context.Context, mail.SaveAttachmentRequest) (mail.SavedAttachment, error)
}

func runAttachments(
	ctx context.Context,
	service *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	return runCommandFamily(ctx, service, "attachments", args, stdout, stderr, nil)
}

func runAttachmentsList(
	ctx context.Context,
	service *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	flags := newFlagSet("attachments list", stderr)
	ref := flags.String("ref", "", "message ref")
	cursor := flags.String("cursor", "", "pagination cursor")
	limit := flags.Int("limit", mail.DefaultPageLimit, "page size (1-200)")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	outputFlags := addOutputFlags(flags, projectionTargetAttachment, outputViewMetadata, false)
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if err := validatePageLimit(*limit); err != nil {
		return failCommand("attachments.list", *jsonOutput, err, stdout, stderr)
	}
	output, err := outputFlags.options(projectionTargetAttachment)
	if err != nil {
		return failCommand("attachments.list", *jsonOutput, err, stdout, stderr)
	}
	scope := listCursorScope("message", *ref)
	if err := validateCatalogCursorScope(*cursor, "attachments.list", scope); err != nil {
		return failCatalogCursor("attachments.list", *jsonOutput, stdout, stderr)
	}
	operationCtx, cancel := hydrationReadContext(ctx)
	defer cancel()
	message, readErr := service.GetMessageWithIntent(operationCtx, *ref, mail.MessageReadIntentAttachments)
	if readErr != nil && message.ContentSource == "" && len(message.Attachments) == 0 {
		if *jsonOutput {
			return failProjectedEmpty("attachments.list", true, output, readErr, stdout, stderr)
		}
		return failCommand("attachments.list", false, readErr, stdout, stderr)
	}
	attachments, nextCursor, err := paginateCatalog(message.Attachments, *limit, *cursor, "attachments.list", scope,
		func(attachment mail.Attachment) string { return attachment.ID })
	if err != nil {
		if errorCode(err) == "invalid_cursor" {
			return failCatalogCursor("attachments.list", *jsonOutput, stdout, stderr)
		}
		return failCommand("attachments.list", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		complete := message.ContentComplete
		missing := message.MissingParts
		recovery := listOutputRecovery("attachments.list")
		data := responseData{
			Attachments: &attachments, ContentSource: message.ContentSource,
			ContentComplete: &complete, MissingParts: &missing,
			Page:         rawResponsePage(catalogPageMetadata{Limit: *limit, NextCursor: nextCursor}),
			listRecovery: &recovery,
		}
		if readErr != nil {
			return writeProjectedFailure(stdout, "attachments.list", data, output, readErr, false)
		}
		return writeProjectedSuccess(stdout, "attachments.list", data, output)
	}
	rows := make([][]string, 0, len(attachments))
	for _, attachment := range attachments {
		size := "unknown"
		if attachment.SizeKnown {
			size = fmt.Sprintf("%d", attachment.Size)
		}
		rows = append(rows, []string{attachment.ID, size, fmt.Sprint(attachment.Downloaded), attachment.Name})
	}
	if writeTerminalTable(stdout, []string{"ID", "BYTES", "DOWNLOADED", "NAME"}, rows) {
		writeFormat(
			stdout, "\nContent: source=%s, complete=%t, missing=%s\n",
			message.ContentSource, message.ContentComplete, oneLine(strings.Join(message.MissingParts, ",")),
		)
		if nextCursor != "" {
			writeFormat(stdout, "Next cursor: %s\n", nextCursor)
		}
		if readErr != nil {
			writeLine(stderr, oneLine(readErr.Error()))
			return commandExitCodeFor("attachments.list", readErr, false)
		}
		return 0
	}
	for _, attachment := range attachments {
		size := "unknown"
		if attachment.SizeKnown {
			size = fmt.Sprintf("%d", attachment.Size)
		}
		writeFormat(
			stdout, "%s\tsize=%s\tsize_known=%t\tdownloaded=%t\t%s\n",
			attachment.ID, size, attachment.SizeKnown, attachment.Downloaded, oneLine(attachment.Name),
		)
	}
	writeFormat(
		stdout, "content\tsource=%s\tcomplete=%t\tmissing=%s\n",
		message.ContentSource, message.ContentComplete, oneLine(strings.Join(message.MissingParts, ",")),
	)
	if nextCursor != "" {
		writeFormat(stdout, "next_cursor\t%s\n", nextCursor)
	}
	if readErr != nil {
		writeLine(stderr, oneLine(readErr.Error()))
		return commandExitCodeFor("attachments.list", readErr, false)
	}
	return 0
}

func runAttachmentsSave(
	ctx context.Context,
	service attachmentSaver,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	flags := newFlagSet("attachments save", stderr)
	ref := flags.String("ref", "", "message ref")
	attachmentID := flags.String("attachment", "", "attachment id")
	outputPath := flags.String("output", "", "absolute non-existing output path")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	operationCtx, cancel := hydrationReadContext(ctx)
	defer cancel()
	saved, err := service.SaveAttachment(operationCtx, mail.SaveAttachmentRequest{
		MessageRef: *ref, AttachmentID: *attachmentID, OutputPath: *outputPath,
	})
	if err != nil {
		data := responseData{}
		if saved.Path != "" {
			data.SavedAttachment = &saved
		}
		return failCommandWithData("attachments.save", *jsonOutput, data, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "attachments.save", responseData{SavedAttachment: &saved})
	}
	writeFormat(stdout, "%s\t%d\t%s\n", saved.Path, saved.Size, saved.SHA256)
	return 0
}
