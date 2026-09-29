package mailstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

// Server refs name a message by account, mailbox path, UIDVALIDITY and UID and
// are read over IMAP only; they never touch the local store's message rows
// except to look for the local counterpart of the same message.

const serverRefStalenessNote = "read state comes from the IMAP server"

func (c *Client) resolveReadTarget(ctx context.Context, ref string) (imapTarget, error) {
	if mailref.IsServerRef(ref) {
		return c.resolveServerTarget(ctx, ref)
	}
	return c.resolveImapTarget(ctx, ref)
}

func (c *Client) resolveServerTarget(ctx context.Context, value string) (imapTarget, error) {
	server, err := mailref.DecodeServer(value)
	if err != nil {
		return imapTarget{}, &mail.ValidationError{Code: "invalid_reference", Message: fmt.Sprintf("invalid server ref: %v", err)}
	}
	if c.store == nil {
		return imapTarget{}, c.readUnavailableError()
	}
	imapOp := c.send.ImapClient()
	if imapOp == nil {
		return imapTarget{}, &transport.TransportError{Code: transport.CodeIMAPFetchFailed, Message: "IMAP operator is not configured"}
	}
	email, cfg, err := c.imapConfigForAccountID(ctx, server.AccountID)
	if err != nil {
		return imapTarget{}, err
	}
	boxes, err := c.getOrLoadMailboxes(ctx, imapOp, cfg, email)
	if err != nil {
		return imapTarget{}, err
	}
	imapMailbox, err := transport.ResolveMailboxPath(boxes, server.MailboxPath)
	if err != nil {
		return imapTarget{}, err
	}
	return imapTarget{
		cfg: cfg, imapMailbox: imapMailbox, uid: server.UID, uidvalidity: server.UIDValidity,
		duplicateMatches: 1, accountID: server.AccountID, summary: mail.MessageSummary{Ref: value},
	}, nil
}

// readServerMessage reads the complete message of a server ref.
func (c *Client) readServerMessage(ctx context.Context, ref string) (mail.Message, error) {
	return c.parseServerSource(ctx, ref, mail.MessageReadIntentFull)
}

// getServerMessageWithIntent serves the bounded read intents of a server ref;
// a server message has no local index row.
func (c *Client) getServerMessageWithIntent(ctx context.Context, ref string, intent mail.MessageReadIntent) (mail.Message, error) {
	switch intent {
	case mail.MessageReadIntentFull, mail.MessageReadIntentAttachments:
		return c.parseServerSource(ctx, ref, intent)
	case mail.MessageReadIntentHeaders:
		headers, summary, err := c.hydrateMessageHeaders(ctx, ref)
		if err != nil {
			return mail.Message{}, err
		}
		return c.finishServerMessage(ctx, ref, messageFromHeaders(mail.Message{}, summary, headers)), nil
	default:
		return mail.Message{}, &mail.ValidationError{
			Code: "invalid_reference", Message: "a server ref has no local index row; use a local ref once Mail.app has synced the message",
		}
	}
}

func (c *Client) parseServerSource(ctx context.Context, ref string, intent mail.MessageReadIntent) (mail.Message, error) {
	source, size, summary, err := c.hydrateMessageSource(ctx, ref)
	if err != nil {
		return mail.Message{}, err
	}
	if size <= 0 {
		return mail.Message{}, errors.Join(serverMessageNotFound(), source.Close())
	}
	message, parseErr := messageFromRawReaderWithIntent(ctx, mail.Message{}, summary, source, intent)
	if err := errors.Join(parseErr, source.Close()); err != nil {
		return mail.Message{}, err
	}
	return c.finishServerMessage(ctx, ref, message), nil
}

func serverMessageNotFound() error {
	return &transport.TransportError{Code: transport.CodeIMAPMessageNotFound, Message: "the server returned no message for this server ref"}
}

// finishServerMessage completes the summary a raw source cannot give: the
// decoded subject, sender and date from the headers, the server's read
// state, and the local ref when Mail.app already holds the message.
func (c *Client) finishServerMessage(ctx context.Context, ref string, message mail.Message) mail.Message {
	row := mail.NewMessageFromHeader(ref, []byte(message.Headers), true)
	message.Summary.Ref = ref
	message.Summary.Subject, message.Summary.Sender, message.Summary.DateSent = row.Subject, row.Sender, row.DateSent
	message.Summary.LocalRef = c.localRefForServerRef(ctx, ref)
	if target, err := c.resolveServerTarget(ctx, ref); err == nil {
		if reader, supported := c.send.ImapClient().(transport.FlagStateReader); supported {
			state, flagErr := reader.FetchFlags(ctx, target.cfg, target.imapMailbox, target.uid, target.uidvalidity)
			if flagErr == nil && !state.Missing {
				for _, flag := range state.Flags {
					switch strings.ToLower(flag) {
					case `\seen`:
						message.Summary.Read = true
					case `\flagged`:
						message.Summary.Flagged = true
					case `\deleted`:
						message.Summary.Deleted = true
					}
				}
				message.Summary.StalenessNote = serverRefStalenessNote
				return message
			}
		}
	}
	message.Summary.StalenessNote = "server read state unavailable; read is not verified"
	return message
}

// localRefForServerRef returns the store-bound ref of the same message when
// the local index already has it, otherwise an empty string.
func (c *Client) localRefForServerRef(ctx context.Context, value string) string {
	if c.store == nil {
		return ""
	}
	server, err := mailref.DecodeServer(value)
	if err != nil {
		return ""
	}
	localCtx, cancel := localReadOrResolveContext(ctx)
	defer cancel()
	ref, err := c.store.localRefForServerMessage(localCtx, server)
	if err != nil {
		return ""
	}
	return ref
}

// rawServerSource reads the exact RFC source of a server ref.
func (c *Client) rawServerSource(ctx context.Context, ref string) (string, error) {
	source, size, _, err := c.hydrateMessageSource(ctx, ref)
	if err != nil {
		return "", err
	}
	if size <= 0 {
		return "", errors.Join(serverMessageNotFound(), source.Close())
	}
	return readHydratedRawSource(ctx, source, size)
}

func (c *Client) writeServerRawSource(ctx context.Context, ref string, writer io.Writer) error {
	source, size, _, err := c.hydrateMessageSource(ctx, ref)
	if err != nil {
		return err
	}
	if size <= 0 {
		return errors.Join(serverMessageNotFound(), source.Close())
	}
	return errors.Join(copyHydratedSource(ctx, writer, source, size), source.Close())
}

func (c *Client) saveServerAttachment(
	ctx context.Context, ref string, attachmentID string, outputPath string,
) (mail.AttachmentEvidence, error) {
	source, size, _, err := c.hydrateMessageSource(ctx, ref)
	if err != nil {
		return mail.AttachmentEvidence{}, err
	}
	if size <= 0 {
		return mail.AttachmentEvidence{}, errors.Join(serverMessageNotFound(), source.Close())
	}
	evidence, saveErr := extractMIMEAttachmentWithEvidence(mimeContextReader{ctx: ctx, reader: source}, attachmentID, outputPath)
	return evidence, errors.Join(saveErr, source.Close())
}

// localRefForServerMessage finds the local row that holds the server message
// (same account, mailbox and server UID, not deleted) and returns its ref.
func (s *Store) localRefForServerMessage(ctx context.Context, server mailref.Server) (string, error) {
	records, err := s.mailboxRecords(ctx)
	if err != nil {
		return "", err
	}
	mailbox, found := findMailboxRecord(records, server.AccountID, server.MailboxPath)
	if !found {
		return "", nil
	}
	if validity, validityErr := s.mailboxUIDValidity(ctx, mailbox.Location); validityErr == nil && validity != 0 && validity != server.UIDValidity {
		return "", nil
	}
	record, err := s.messageRecordByServerUID(ctx, mailbox.RowID, server.UID)
	if err != nil || record == nil {
		return "", err
	}
	mailboxRef, err := mailref.EncodeMailbox(server.AccountID, server.MailboxPath)
	if err != nil {
		return "", err
	}
	summary, err := mapMessageSummary(*record, mailboxRef, server.AccountID, server.MailboxPath, s.storeUUID)
	if err != nil {
		return "", err
	}
	return summary.Ref, nil
}
