package mailstore

import (
	"context"
	"fmt"
	"io"
	"strings"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

func (s *Store) GetMessage(ctx context.Context, ref string) (result mail.Message, resultErr error) {
	resolved, source, err := s.openMessageSource(ctx, ref)
	if err != nil {
		return mail.Message{}, err
	}
	defer joinCloseError(&resultErr, source, "message source")
	headers, err := readRawHeaders(source.Reader())
	if err != nil {
		return mail.Message{}, err
	}
	document, err := parseMIMEDocument(source.Reader(), source.partial, false, false)
	if err != nil {
		return mail.Message{}, err
	}
	attachments, err := s.messageAttachments(ctx, resolved, source, document.Parts)
	if err != nil {
		return mail.Message{}, err
	}
	mailboxRef, err := mailref.EncodeMailbox(
		resolved.Reference.AccountID, resolved.Reference.MailboxPath,
	)
	if err != nil {
		return mail.Message{}, err
	}
	summary, err := mapMessageSummary(
		resolved.Record, mailboxRef, resolved.Reference.AccountID, resolved.Reference.MailboxPath,
		s.storeUUID,
	)
	if err != nil {
		return mail.Message{}, err
	}
	summary.MessageID = document.MessageID
	summary.AttachmentCount = len(attachments)
	return mail.Message{
		Summary: summary, ReplyTo: document.ReplyTo,
		To: document.To, CC: document.CC, BCC: document.BCC,
		Headers: headers, Content: document.Content,
		ContentSource: sourceKind(source.partial), ContentComplete: document.Complete,
		MissingParts: document.MissingParts, Attachments: attachments,
	}, nil
}

func (s *Store) GetRawSource(ctx context.Context, ref string) (result string, resultErr error) {
	var output strings.Builder
	if err := s.WriteRawSource(ctx, ref, &output); err != nil {
		return "", err
	}
	return output.String(), nil
}

func (s *Store) WriteRawSource(ctx context.Context, ref string, writer io.Writer) (resultErr error) {
	_, source, err := s.openMessageSource(ctx, ref)
	if err != nil {
		return err
	}
	defer joinCloseError(&resultErr, source, "raw message source")
	if source.partial {
		return operationError(
			"raw_source_partial",
			"the local EMLX source is partial; exact raw source requires a targeted Mail.app fallback",
		)
	}
	if source.length < 0 || source.length > mail.MaximumRawSourceBytes {
		return operationError("raw_source_too_large", "raw RFC message source exceeds 64 MiB")
	}
	if _, err := io.CopyN(writer, source.Reader(), source.length); err != nil {
		return fmt.Errorf("stream RFC message source: %w", err)
	}
	return nil
}

func sourceKind(partial bool) string {
	if partial {
		return "emlx_partial"
	}
	return "emlx_full"
}
