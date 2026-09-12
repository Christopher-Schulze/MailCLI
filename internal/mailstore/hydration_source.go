package mailstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

type byteHydrationSource struct{ *bytes.Reader }

func (byteHydrationSource) Close() error { return nil }

func (c *Client) hydrateMessageSource(ctx context.Context, messageRef string, enforceRawCap bool) (io.ReadSeekCloser, int64, mail.MessageSummary, error) {
	target, err := c.resolveImapTarget(ctx, messageRef)
	if err != nil {
		return nil, 0, mail.MessageSummary{}, err
	}
	operator := c.send.ImapClient()
	if operator == nil {
		return nil, 0, target.summary, &transport.TransportError{Code: transport.CodeIMAPFetchFailed, Message: "IMAP operator is not configured"}
	}
	bound := rawFetchBound(enforceRawCap)
	var source io.ReadSeekCloser
	var size int64
	if streaming, ok := operator.(transport.StreamingFetcher); ok {
		source, size, err = streaming.FetchMessageReader(ctx, target.cfg, target.imapMailbox, target.uid, target.uidvalidity, bound)
	} else {
		var raw []byte
		raw, err = operator.FetchMessage(ctx, target.cfg, target.imapMailbox, target.uid, target.uidvalidity, bound)
		source, size = byteHydrationSource{bytes.NewReader(raw)}, int64(len(raw))
	}
	if err == nil && (source == nil || size < 0 || size > bound) {
		err = &transport.TransportError{Code: transport.CodeIMAPFetchFailed, Message: "IMAP returned an invalid bounded source"}
	}
	if err != nil {
		if source != nil {
			err = errors.Join(err, source.Close())
		}
		return nil, 0, target.summary, err
	}
	return source, size, target.summary, nil
}

func copyHydratedSource(ctx context.Context, writer io.Writer, source io.Reader, size int64) error {
	count, err := io.Copy(writer, mimeContextReader{ctx: ctx, reader: io.LimitReader(source, size+1)})
	if err != nil {
		return err
	}
	if count != size {
		return fmt.Errorf("IMAP source size changed: read %d, expected %d", count, size)
	}
	return ctx.Err()
}

func readHydratedRawSource(ctx context.Context, source io.ReadSeekCloser, size int64) (result string, resultErr error) {
	defer joinCloseError(&resultErr, source, "IMAP source")
	var output strings.Builder
	output.Grow(int(size))
	if err := copyHydratedSource(ctx, &output, source, size); err != nil {
		return "", err
	}
	return output.String(), nil
}
