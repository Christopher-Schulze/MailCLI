package mailstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"

	messageMail "github.com/emersion/go-message/mail"
	"golang.org/x/sync/errgroup"
	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

// enrichmentConcurrency bounds the per-row local reads and IMAP identity
// resolutions of one page.
const enrichmentConcurrency = 4

// excerptInput is one row's excerpt source before parsing. err is the first
// read or fetch failure, kept even when a partial local source stays usable.
type excerptInput struct {
	data     []byte
	complete bool
	source   mail.ExcerptSource
	err      error
}

// remoteExcerpt is a row whose excerpt needs the IMAP text prefix.
type remoteExcerpt struct {
	index  int
	target imapTarget
}

type remoteExcerptMailbox struct {
	accountID   string
	mailbox     string
	uidvalidity uint32
}

// EnrichMessages reads reply metadata for a page of refs, one summary per ref
// in order. Local work runs per row with bounded concurrency; rows that need
// an IMAP excerpt are fetched with one UID FETCH per account and mailbox.
// Failures are reported per row in enrichment_error.
func (c *Client) EnrichMessages(ctx context.Context, refs []string, request mail.MessageEnrichmentRequest) ([]mail.MessageSummary, error) {
	summaries := make([]mail.MessageSummary, len(refs))
	inputs := make([]excerptInput, len(refs))
	var remote []remoteExcerpt
	var remoteMu sync.Mutex
	var rows errgroup.Group
	rows.SetLimit(enrichmentConcurrency)
	for index, ref := range refs {
		rows.Go(func() error {
			if request.Threading {
				c.enrichThreading(ctx, ref, &summaries[index])
			}
			if request.Excerpt {
				input, target, needsRemote := c.localExcerpt(ctx, ref)
				inputs[index] = input
				if needsRemote {
					remoteMu.Lock()
					remote = append(remote, remoteExcerpt{index: index, target: target})
					remoteMu.Unlock()
				}
			}
			return nil
		})
	}
	if err := rows.Wait(); err != nil {
		return nil, err
	}
	if request.Excerpt {
		if err := c.fetchRemoteExcerpts(ctx, remote, inputs); err != nil {
			return nil, err
		}
		for index := range summaries {
			applyExcerpt(ctx, &summaries[index], inputs[index], request.ExcerptLength)
		}
	}
	return summaries, ctx.Err()
}

// noteEnrichmentFailure reports the first local or remote failure per row
// instead of hiding it behind threading_complete:false or
// excerpt_source:unavailable.
func noteEnrichmentFailure(summary *mail.MessageSummary, err error) {
	if err != nil && summary.EnrichmentError == "" {
		summary.EnrichmentError = hydrationErrorCode(err)
	}
}

func (c *Client) enrichThreading(ctx context.Context, ref string, summary *mail.MessageSummary) {
	// No full-source fallback: production transports implement headers.
	local, err := c.store.GetMessageWithIntent(ctx, ref, mail.MessageReadIntentHeaders)
	switch {
	case err == nil:
		summary.MessageID = local.Summary.MessageID
		mail.ApplyThreadingHeaders(summary, local.Headers)
	case !safeTargetedFallback(err):
		noteEnrichmentFailure(summary, err)
	default:
		if _, ok := c.send.ImapClient().(transport.MessageHeaderFetcher); !ok {
			noteEnrichmentFailure(summary, err)
		} else if headers, _, remoteErr := c.hydrateMessageHeaders(ctx, ref); remoteErr != nil {
			noteEnrichmentFailure(summary, remoteErr)
		} else {
			summary.MessageID = headers.MessageID
			mail.ApplyThreadingHeaders(summary, headers.Raw)
		}
	}
}

// localExcerpt reads the bounded local source. When local content is missing
// and an excerpt fetcher exists, it also resolves the IMAP target; the
// returned input is then the fallback if the remote fetch fails.
func (c *Client) localExcerpt(ctx context.Context, ref string) (excerptInput, imapTarget, bool) {
	unavailable := excerptInput{source: mail.ExcerptSourceUnavailable}
	if c.store == nil {
		return unavailable, imapTarget{}, false
	}
	data, complete, localPartial, err := c.store.readExcerptSource(ctx, ref)
	fallback := excerptInput{data: data, complete: complete, source: mail.ExcerptSourceLocal, err: err}
	if err != nil {
		fallback = excerptInput{source: mail.ExcerptSourceUnavailable, err: err}
	}
	if !excerptNeedsRemote(err, localPartial) {
		return fallback, imapTarget{}, false
	}
	if _, supported := c.send.ImapClient().(transport.MessageExcerptFetcher); !supported {
		return fallback, imapTarget{}, false
	}
	target, targetErr := c.resolveImapTarget(ctx, ref)
	if targetErr != nil {
		fallback.err = targetErr
		return fallback, imapTarget{}, false
	}
	return fallback, target, true
}

// fetchRemoteExcerpts replaces the input of every remote row with its IMAP
// text prefix, fetching each account mailbox once. A failed fetch keeps the
// local fallback and records the failure on the affected rows only.
func (c *Client) fetchRemoteExcerpts(ctx context.Context, remote []remoteExcerpt, inputs []excerptInput) error {
	fetcher, supported := c.send.ImapClient().(transport.MessageExcerptFetcher)
	if !supported || len(remote) == 0 {
		return nil
	}
	slices.SortFunc(remote, func(a, b remoteExcerpt) int { return a.index - b.index })
	var order []remoteExcerptMailbox
	groups := make(map[remoteExcerptMailbox][]remoteExcerpt)
	for _, row := range remote {
		key := remoteExcerptMailbox{accountID: row.target.accountID, mailbox: row.target.imapMailbox, uidvalidity: row.target.uidvalidity}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], row)
	}
	var mailboxes errgroup.Group
	mailboxes.SetLimit(enrichmentConcurrency)
	for _, key := range order {
		rows := groups[key]
		mailboxes.Go(func() error {
			uids := make([]uint32, len(rows))
			for index, row := range rows {
				uids[index] = row.target.uid
			}
			fetchCtx, cancel := hydrationFetchContext(ctx, 2*mail.IMAPExcerptTextBytes*int64(len(rows)))
			defer cancel()
			target := rows[0].target
			fetched, err := fetcher.FetchMessageExcerpts(fetchCtx, target.cfg, target.imapMailbox, target.uidvalidity, uids, mail.IMAPExcerptTextBytes)
			for _, row := range rows {
				source, found := fetched[row.target.uid]
				switch {
				case err != nil:
					inputs[row.index].err = err
				case !found:
					inputs[row.index].err = &transport.TransportError{Code: transport.CodeIMAPMessageNotFound, Message: "message not returned by the IMAP excerpt FETCH"}
				default:
					inputs[row.index] = excerptInput{data: source.Source, complete: source.Complete, source: mail.ExcerptSourceIMAPPartial}
				}
			}
			return nil
		})
	}
	return mailboxes.Wait()
}

func applyExcerpt(ctx context.Context, summary *mail.MessageSummary, input excerptInput, length int) {
	noteEnrichmentFailure(summary, input.err)
	summary.ExcerptSource = input.source
	if input.source == mail.ExcerptSourceUnavailable {
		return
	}
	text, parsed := excerptText(ctx, input.data)
	summary.Excerpt = mail.BuildExcerpt(text, length)
	summary.ExcerptComplete = input.complete && parsed
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
