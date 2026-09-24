package mailstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync/atomic"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

type readMetrics struct {
	sourceBytes       atomic.Int64
	retainedBodyBytes atomic.Int64
}

type sourceByteReader struct {
	reader io.Reader
	bytes  *atomic.Int64
}

func (r sourceByteReader) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	r.bytes.Add(int64(count))
	return count, err
}

func (s *Store) measuredSourceReader(source *emlxSource) io.Reader {
	reader := source.Reader()
	if s.readMetrics == nil {
		return reader
	}
	return sourceByteReader{reader: reader, bytes: &s.readMetrics.sourceBytes}
}

func (s *Store) recordRetainedBody(content string) {
	if s.readMetrics != nil {
		s.readMetrics.retainedBodyBytes.Add(int64(len(content)))
	}
}

// GetMessageWithIntent keeps the legacy full read available and routes
// projection callers to bounded header or MIME metadata paths.
func (s *Store) GetMessageWithIntent(
	ctx context.Context,
	ref string,
	intent mail.MessageReadIntent,
) (mail.Message, error) {
	switch intent {
	case mail.MessageReadIntentFull:
		return s.GetMessage(ctx, ref)
	case mail.MessageReadIntentIndex, mail.MessageReadIntentHeaders:
		return s.getMessageHeaders(ctx, ref)
	case mail.MessageReadIntentAttachments:
		return s.getMessageAttachments(ctx, ref)
	default:
		return mail.Message{}, operationError("invalid_argument", "message read intent is invalid")
	}
}

func (s *Store) getMessageHeaders(ctx context.Context, ref string) (result mail.Message, resultErr error) {
	resolved, source, err := s.openMessageSource(ctx, ref)
	if err != nil {
		return mail.Message{}, err
	}
	defer joinCloseError(&resultErr, source, "message source")
	headers, err := sourceHeadersFromReader(s.measuredSourceReader(source))
	if err != nil {
		return mail.Message{}, err
	}
	summary, err := s.messageSummary(resolved)
	if err != nil {
		return mail.Message{}, err
	}
	summary.MessageID = headers.MessageID
	return mail.Message{
		Summary: summary, ReplyTo: headers.ReplyToText,
		To: headers.To, CC: headers.CC, BCC: headers.BCC,
		Headers: headers.Raw, ContentSource: sourceKind(source.partial),
	}, nil
}

func (s *Store) getMessageAttachments(ctx context.Context, ref string) (result mail.Message, resultErr error) {
	resolved, source, err := s.openMessageSource(ctx, ref)
	if err != nil {
		return mail.Message{}, err
	}
	defer joinCloseError(&resultErr, source, "message source")
	document, err := parseMIMEDocumentWithContextAndRetention(
		ctx, s.measuredSourceReader(source), source.partial, false, false, false,
	)
	if err != nil {
		return mail.Message{}, err
	}
	s.recordRetainedBody(document.Content)
	attachments, err := s.messageAttachments(ctx, resolved, source, document.Parts)
	if err != nil {
		return mail.Message{}, err
	}
	summary, err := s.messageSummary(resolved)
	if err != nil {
		return mail.Message{}, err
	}
	summary.MessageID = document.MessageID
	summary.AttachmentCount = len(attachments)
	return mail.Message{
		Summary: summary, ReplyTo: document.ReplyTo,
		To: document.To, CC: document.CC, BCC: document.BCC,
		ContentSource: sourceKind(source.partial), ContentComplete: document.Complete,
		MissingParts: document.MissingParts, Attachments: attachments,
	}, nil
}

func (s *Store) messageSummary(resolved resolvedMessage) (mail.MessageSummary, error) {
	mailboxRef, err := mailref.EncodeMailbox(
		resolved.Reference.AccountID, resolved.Reference.MailboxPath,
	)
	if err != nil {
		return mail.MessageSummary{}, fmt.Errorf("encode message mailbox reference: %w", err)
	}
	return mapMessageSummary(
		resolved.Record, mailboxRef, resolved.Reference.AccountID, resolved.Reference.MailboxPath,
		s.storeUUID,
	)
}

func (c *Client) GetMessageWithIntent(
	ctx context.Context,
	ref string,
	intent mail.MessageReadIntent,
) (mail.Message, error) {
	if intent == mail.MessageReadIntentFull {
		return c.readMessage(ctx, ref, false)
	}
	if c.store == nil {
		return mail.Message{}, c.readUnavailableError()
	}
	switch intent {
	case mail.MessageReadIntentIndex, mail.MessageReadIntentHeaders:
		return c.getMessageHeaders(ctx, ref)
	case mail.MessageReadIntentAttachments:
		return c.getMessageAttachments(ctx, ref)
	default:
		return mail.Message{}, operationError("invalid_argument", "message read intent is invalid")
	}
}

func (c *Client) getMessageHeaders(ctx context.Context, ref string) (mail.Message, error) {
	local, localErr := c.store.GetMessageWithIntent(ctx, ref, mail.MessageReadIntentHeaders)
	if localErr == nil {
		return local, nil
	}
	if !safeTargetedFallback(localErr) || c.send.ImapClient() == nil {
		return local, localErr
	}
	source, _, summary, remoteErr := c.hydrateMessageSource(ctx, ref, false)
	if remoteErr != nil {
		return local, newHydrationError("read message headers", localErr, remoteErr)
	}
	message, parseErr := messageFromRawHeaders(ctx, local, summary, source)
	closeErr := source.Close()
	if parseErr != nil {
		return local, newHydrationError("read message headers", localErr, errors.Join(parseErr, closeErr))
	}
	if closeErr != nil {
		return mail.Message{}, closeErr
	}
	return message, nil
}

func messageFromRawHeaders(
	ctx context.Context,
	base mail.Message,
	summary mail.MessageSummary,
	source io.Reader,
) (mail.Message, error) {
	headers, err := sourceHeadersFromReader(mimeContextReader{ctx: ctx, reader: source})
	if err != nil {
		return mail.Message{}, err
	}
	base.Summary = summary
	base.Summary.MessageID = headers.MessageID
	base.ReplyTo = headers.ReplyToText
	base.To, base.CC, base.BCC = headers.To, headers.CC, headers.BCC
	base.Headers = headers.Raw
	base.ContentSource = "imap_raw"
	return base, nil
}

func (c *Client) getMessageAttachments(ctx context.Context, ref string) (mail.Message, error) {
	local, localErr := c.store.GetMessageWithIntent(ctx, ref, mail.MessageReadIntentAttachments)
	hasLocal := localErr == nil
	if localErr == nil && local.ContentComplete {
		return local, nil
	}
	if localErr == nil && c.send.ImapClient() == nil {
		return local, nil
	}
	if localErr != nil && !safeTargetedFallback(localErr) {
		return mail.Message{}, localErr
	}
	if c.send.ImapClient() == nil {
		return mail.Message{}, localErr
	}
	source, size, summary, remoteErr := c.hydrateMessageSource(ctx, ref, false)
	if remoteErr == nil {
		if size > 0 {
			message, parseErr := messageFromRawReaderWithIntent(ctx, local, summary, source, mail.MessageReadIntentAttachments)
			return message, errors.Join(parseErr, source.Close())
		}
		remoteErr = source.Close()
	}
	if remoteErr != nil && hasLocal {
		remoteCause := typedHydrationFailure(remoteErr)
		local.Hydration = messageHydrationDiagnostic(ctx, local, remoteCause)
		return local, newHydrationError("read message attachments", incompleteMessageCause(local), remoteCause)
	}
	if remoteErr != nil && localErr != nil {
		return mail.Message{}, newHydrationError("read message attachments", localErr, remoteErr)
	}
	if hasLocal {
		return local, nil
	}
	if localErr != nil {
		return mail.Message{}, localErr
	}
	return mail.Message{}, c.readUnavailableError()
}

func messageFromRawReaderWithIntent(
	ctx context.Context,
	base mail.Message,
	summary mail.MessageSummary,
	source io.ReadSeeker,
	intent mail.MessageReadIntent,
) (mail.Message, error) {
	if intent != mail.MessageReadIntentAttachments {
		return messageFromRawReader(ctx, base, summary, source)
	}
	document, err := parseMIMEDocumentWithContextAndRetention(
		ctx, mimeContextReader{ctx: ctx, reader: source}, false, false, false, false,
	)
	if err != nil {
		return mail.Message{}, err
	}
	identifiers := make([]string, 0, len(document.Parts))
	for identifier := range document.Parts {
		identifiers = append(identifiers, identifier)
	}
	sort.Strings(identifiers)
	attachments := make([]mail.Attachment, 0, len(identifiers))
	for _, identifier := range identifiers {
		part := document.Parts[identifier]
		attachment := mail.Attachment{
			ID: identifier, Name: part.Name,
			Size: part.Size, SizeKnown: part.Complete, Downloaded: part.Complete,
		}
		if part.MIMEType != "" {
			mediaType := part.MIMEType
			attachment.MIMEType = &mediaType
		}
		attachments = append(attachments, attachment)
	}
	base.Summary = summary
	base.Summary.MessageID = document.MessageID
	base.Summary.AttachmentCount = len(attachments)
	base.ReplyTo, base.To, base.CC, base.BCC = document.ReplyTo, document.To, document.CC, document.BCC
	base.Content = ""
	base.ContentSource = "imap_raw"
	base.ContentComplete = document.Complete
	base.MissingParts = append([]string(nil), document.MissingParts...)
	base.Attachments = attachments
	return base, nil
}
