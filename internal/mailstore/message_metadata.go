package mailstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	"github.com/emersion/go-message"
	messageMail "github.com/emersion/go-message/mail"
	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

func (c *Client) EnrichMessage(ctx context.Context, ref string, request mail.MessageEnrichmentRequest) (mail.MessageSummary, error) {
	var summary mail.MessageSummary
	if request.Threading {
		// No full-source fallback: production transports implement headers.
		local, err := c.store.GetMessageWithIntent(ctx, ref, mail.MessageReadIntentHeaders)
		if err == nil {
			summary.MessageID = local.Summary.MessageID
			mail.ApplyThreadingHeaders(&summary, local.Headers)
		} else if safeTargetedFallback(err) {
			if _, ok := c.send.ImapClient().(transport.MessageHeaderFetcher); ok {
				headers, _, remoteErr := c.hydrateMessageHeaders(ctx, ref)
				if remoteErr == nil {
					summary.MessageID = headers.MessageID
					mail.ApplyThreadingHeaders(&summary, headers.Raw)
				}
			}
		}
	}
	if request.Excerpt {
		data, complete, source := c.excerptSource(ctx, ref)
		summary.ExcerptSource = source
		if source != mail.ExcerptSourceUnavailable {
			text, parsed := excerptText(ctx, data)
			summary.Excerpt = mail.BuildExcerpt(text, request.ExcerptLength)
			summary.ExcerptComplete = complete && parsed
		}
	}
	return summary, ctx.Err()
}

func (c *Client) excerptSource(ctx context.Context, ref string) ([]byte, bool, mail.ExcerptSource) {
	if c.store == nil {
		return nil, false, mail.ExcerptSourceUnavailable
	}
	data, complete, localPartial, err := c.store.readExcerptSource(ctx, ref)
	if !excerptNeedsRemote(err, localPartial) {
		if err != nil {
			return nil, false, mail.ExcerptSourceUnavailable
		}
		return data, complete, mail.ExcerptSourceLocal
	}
	fetcher, supported := c.send.ImapClient().(transport.MessagePrefixFetcher)
	if supported {
		target, targetErr := c.resolveImapTarget(ctx, ref)
		if targetErr == nil {
			fetchCtx, cancel := hydrationFetchContext(ctx, mail.MaximumExcerptSourceBytes)
			defer cancel()
			remote, remoteErr := fetcher.FetchMessagePrefix(fetchCtx, target.cfg, target.imapMailbox, target.uid, target.uidvalidity, mail.MaximumExcerptSourceBytes)
			if remoteErr == nil && int64(len(remote)) <= mail.MaximumExcerptSourceBytes {
				return remote, int64(len(remote)) < mail.MaximumExcerptSourceBytes, mail.ExcerptSourceIMAPPartial
			}
		}
	}
	if err == nil {
		return data, complete, mail.ExcerptSourceLocal
	}
	return nil, false, mail.ExcerptSourceUnavailable
}

// excerptNeedsRemote allows an IMAP partial fetch only when local content is
// missing. A complete local source larger than the cap already provides the
// same bounded prefix an IMAP partial fetch would return.
func excerptNeedsRemote(localErr error, localPartial bool) bool {
	if localErr != nil {
		return safeTargetedFallback(localErr)
	}
	return localPartial
}

func (s *Store) readExcerptSource(ctx context.Context, ref string) (data []byte, complete bool, localPartial bool, resultErr error) {
	_, source, err := s.openMessageSource(ctx, ref)
	if err != nil {
		return nil, false, false, err
	}
	defer joinCloseError(&resultErr, source, "excerpt source")
	reader := mimeContextReader{ctx: ctx, reader: s.measuredSourceReader(source)}
	data, err = io.ReadAll(io.LimitReader(reader, mail.MaximumExcerptSourceBytes))
	return data, !source.partial && source.length <= mail.MaximumExcerptSourceBytes, source.partial, err
}

// Input, decoded text and part count are independently bounded. Parsing uses
// original decoded text so the exact signature delimiter is not normalized away.
func excerptText(ctx context.Context, data []byte) (string, bool) {
	reader, err := messageMail.CreateReader(bytes.NewReader(data))
	if err != nil || reader == nil {
		return "", false
	}
	var plain, html strings.Builder
	remaining := mail.MaximumExcerptSourceBytes
	for count := 0; count < 64; count++ {
		if ctx.Err() != nil {
			return preferredExcerptText(plain.String(), html.String()), false
		}
		part, partErr := reader.NextPart()
		if errors.Is(partErr, io.EOF) {
			return preferredExcerptText(plain.String(), html.String()), true
		}
		if partErr != nil || part == nil {
			return preferredExcerptText(plain.String(), html.String()), false
		}
		header, inline := part.Header.(*messageMail.InlineHeader)
		if !inline {
			continue
		}
		kind, _, typeErr := header.ContentType()
		if typeErr != nil {
			return preferredExcerptText(plain.String(), html.String()), false
		}
		if kind != "text/plain" && kind != "text/html" {
			continue
		}
		text, readErr := io.ReadAll(io.LimitReader(part.Body, remaining))
		remaining -= int64(len(text))
		if kind == "text/plain" {
			plain.Write(text)
		} else {
			html.Write(text)
		}
		if readErr != nil || remaining == 0 {
			return preferredExcerptText(plain.String(), html.String()), false
		}
	}
	return preferredExcerptText(plain.String(), html.String()), false
}

func preferredExcerptText(plain, html string) string {
	if plain != "" {
		return plain
	}
	return mail.HTMLToPlainText([]byte(html))
}

// Keep charset registration supplied by the existing go-message import path.
var _ = message.IsUnknownCharset
