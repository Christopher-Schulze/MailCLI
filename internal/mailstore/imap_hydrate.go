package mailstore

import (
	"context"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

// hydrateMessage resolves the IMAP target and fetches the complete raw RFC
// 5322 source. It also returns the summary derived from the local store
// record, so the hydration fallback can fill metadata the raw message alone
// cannot provide (ref, subject, sender, dates, flags, mailbox).
func (c *Client) hydrateMessage(ctx context.Context, messageRef string, enforceRawCap bool) ([]byte, mail.MessageSummary, error) {
	resolveCtx, cancelResolve := localReadContext(ctx)
	target, err := c.resolveImapTarget(resolveCtx, messageRef)
	cancelResolve()
	if err != nil {
		return nil, mail.MessageSummary{}, typedHydrationFailure(err)
	}

	imapOp := c.send.ImapClient()
	if imapOp == nil {
		return nil, mail.MessageSummary{}, &transport.TransportError{
			Code:    transport.CodeIMAPFetchFailed,
			Message: "IMAP operator is not configured",
		}
	}

	bound := rawFetchBound(enforceRawCap)
	fetchCtx, cancelFetch := hydrationFetchContext(ctx, bound)
	defer cancelFetch()
	raw, err := imapOp.FetchMessage(fetchCtx, target.cfg, target.imapMailbox, target.uid, target.uidvalidity, bound)
	return raw, target.summary, typedHydrationFailure(err)
}

// HydrateMessageBytes fetches the complete raw RFC 5322 source of a message over IMAP.
func (c *Client) HydrateMessageBytes(ctx context.Context, messageRef string, enforceRawCap bool) ([]byte, error) {
	raw, _, err := c.hydrateMessage(ctx, messageRef, enforceRawCap)
	return raw, err
}

// rawFetchBound applies the same source-size bound to raw-source and content
// hydration, including replayable spools, so no literal bypasses the local limit.
func rawFetchBound(_ bool) int64 {
	return mail.MaximumRawSourceBytes
}
