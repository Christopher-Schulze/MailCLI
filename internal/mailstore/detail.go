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
	document, err := parseMIMEDocumentWithContext(ctx, source.Reader(), source.partial, false, false)
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
	source, err := s.openRawSource(ctx, ref)
	if err != nil {
		return "", err
	}
	var output strings.Builder
	output.Grow(int(source.length))
	if err := writeRawMessageSource(ctx, &output, source); err != nil {
		return "", err
	}
	return output.String(), nil
}

func (s *Store) openRawSource(ctx context.Context, ref string) (*emlxSource, error) {
	_, source, err := s.openMessageSource(ctx, ref)
	if err != nil {
		return nil, err
	}
	if source.partial {
		err = operationError(
			"raw_source_partial",
			"the local EMLX source is partial; exact raw source requires a complete message source",
		)
	} else if source.length < 0 || source.length > mail.MaximumRawSourceBytes {
		err = operationError("raw_source_too_large", "raw RFC message source exceeds 64 MiB")
	}
	if err != nil {
		joinCloseError(&err, source, "raw message source")
		return nil, err
	}
	return source, nil
}

func (s *Store) WriteRawSource(ctx context.Context, ref string, writer io.Writer) error {
	source, err := s.openRawSource(ctx, ref)
	if err != nil {
		return err
	}
	return writeRawMessageSource(ctx, writer, source)
}

func writeRawMessageSource(ctx context.Context, writer io.Writer, source *emlxSource) (resultErr error) {
	defer joinCloseError(&resultErr, source, "raw message source")
	if _, err := io.CopyN(writer, mimeContextReader{ctx: ctx, reader: source.Reader()}, source.length); err != nil {
		return fmt.Errorf("stream RFC message source: %w", err)
	}
	return ctx.Err()
}

func sourceKind(partial bool) string {
	if partial {
		return "emlx_partial"
	}
	return "emlx_full"
}
