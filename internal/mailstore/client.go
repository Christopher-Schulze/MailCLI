package mailstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"mailcli/internal/mail"
)

type Client struct {
	store    *Store
	storeErr error
	// fallback serves explicitly documented Apple Events operations such as
	// live diagnostics, draft workflows, sync, and store-unavailable listing.
	// IMAP mutation target resolution must never consult it.
	fallback          mail.FallbackGateway
	send              mail.SendTransport
	storeOpenDuration time.Duration
	mailboxCacheMu    sync.Mutex
	mailboxCache      map[string]mailboxCacheEntry
}

func NewClient(ctx context.Context, fallback mail.FallbackGateway, config Config, send mail.SendTransport) *Client {
	started := time.Now()
	store, err := Open(ctx, config)
	return &Client{
		store: store, storeErr: err, fallback: fallback, send: send,
		storeOpenDuration: time.Since(started),
		mailboxCache:      make(map[string]mailboxCacheEntry),
	}
}

func (c *Client) ComposeWriteSupportError() error {
	capability, ok := c.fallback.(mail.ComposeWriteGate)
	if !ok {
		return nil
	}
	return capability.ComposeWriteSupportError()
}

func (c *Client) Close() error {
	c.invalidateMailboxCache()
	if c.store == nil {
		return nil
	}
	return c.store.Close()
}

func (c *Client) Probe(ctx context.Context, live bool) mail.DiagnosticReport {
	report, _ := c.ProbeWithDiagnostics(ctx, live)
	return report
}

func (c *Client) ProbeWithDiagnostics(
	ctx context.Context,
	live bool,
) (mail.DiagnosticReport, []mail.DiagnosticTiming) {
	platformStarted := time.Now()
	checks := c.platformChecks(ctx, live)
	timings := []mail.DiagnosticTiming{
		{Phase: "store_open", Milliseconds: float64(c.storeOpenDuration.Microseconds()) / 1000},
		{Phase: "platform_probe", Milliseconds: float64(time.Since(platformStarted).Microseconds()) / 1000},
	}
	if c.storeErr != nil {
		code := "mail_store_unavailable"
		var typed interface{ ErrorCode() string }
		if errors.As(c.storeErr, &typed) {
			code = typed.ErrorCode()
		}
		checks = append(checks, mail.Check{
			Name: "mail-store-read", Status: "fail", Code: code, Detail: c.storeErr.Error(),
		})
		return mail.DiagnosticReport{Checks: checks}, timings
	}
	checks = append(checks, mail.Check{
		Name: "mail-store-read", Status: "pass",
		Detail: "strict read-only WAL access; schema " + c.store.SchemaFingerprint(),
	})
	return mail.DiagnosticReport{Checks: checks}, timings
}

func (c *Client) platformChecks(ctx context.Context, live bool) []mail.Check {
	if c.fallback == nil {
		return nil
	}
	return c.fallback.Probe(ctx, live).Checks
}

func (c *Client) ListAccounts(ctx context.Context) ([]mail.Account, error) {
	if c.store != nil {
		return c.store.ListAccounts(ctx)
	}
	if c.fallback == nil {
		return nil, c.readUnavailableError()
	}
	return c.fallback.ListAccounts(ctx)
}

func (c *Client) ListMailboxes(
	ctx context.Context,
	request mail.ListMailboxesRequest,
) ([]mail.Mailbox, error) {
	if c.store != nil {
		return c.store.ListMailboxes(ctx, request)
	}
	return nil, operationError(
		"safe_mailbox_listing_unavailable",
		"safe mailbox listing requires the supported local Mail store; no recursive Apple Events scan will be attempted: "+c.readUnavailableError().Error(),
	)
}

func (c *Client) ListMessages(
	ctx context.Context,
	request mail.ListMessagesRequest,
) (mail.MessagePage, error) {
	if c.store != nil {
		return c.store.ListMessages(ctx, request)
	}
	if c.fallback == nil {
		return mail.MessagePage{}, c.readUnavailableError()
	}
	return c.fallback.ListMessages(ctx, request)
}

func (c *Client) SearchMessages(
	ctx context.Context,
	query mail.PreparedQuery,
) (mail.SearchPage, error) {
	if c.store == nil {
		return mail.SearchPage{}, operationError(
			"safe_search_unavailable",
			"safe search requires the supported local Mail store; no Apple Events global scan will be attempted: "+c.readUnavailableError().Error(),
		)
	}
	return c.store.SearchMessages(ctx, query)
}

func (c *Client) GetMessage(ctx context.Context, ref string) (mail.Message, error) {
	return c.readMessage(ctx, ref, false)
}

func (c *Client) OpenDraft(ctx context.Context, ref string) (mail.Message, error) {
	return c.readMessage(ctx, ref, true)
}

func (c *Client) readMessage(ctx context.Context, ref string, openDraft bool) (mail.Message, error) {
	var local mail.Message
	hasLocal := false
	var localErr error
	if c.store != nil {
		local, localErr = c.store.GetMessage(ctx, ref)
		hasLocal = localErr == nil
		if localErr == nil && local.ContentComplete {
			return local, nil
		}
		if localErr == nil && c.send.ImapClient() == nil {
			// No IMAP fallback available; return what we have.
			return local, nil
		}
		if localErr != nil && !safeTargetedFallback(localErr) {
			return mail.Message{}, localErr
		}
	}
	if c.send.ImapClient() != nil {
		rawBytes, summary, rawErr := c.hydrateMessage(ctx, ref, false)
		if rawErr == nil && len(rawBytes) > 0 {
			return messageFromRawFallback(local, summary, string(rawBytes))
		}
		// IMAP fallback failed. If we also have a local error, join both
		// so the caller sees the full picture.
		if rawErr != nil && localErr != nil {
			return mail.Message{}, errors.Join(localErr, rawErr)
		}
	}
	if hasLocal {
		// Local content is incomplete and no IMAP fallback succeeded.
		return local, nil
	}
	if localErr != nil {
		return mail.Message{}, localErr
	}
	return mail.Message{}, c.readUnavailableError()
}

func messageFromRawFallback(base mail.Message, summary mail.MessageSummary, raw string) (mail.Message, error) {
	document, err := parseMIMEDocument(strings.NewReader(raw), false, false, false)
	if err != nil {
		return mail.Message{}, err
	}
	headers, err := readRawHeaders(strings.NewReader(raw))
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
	base.ReplyTo = document.ReplyTo
	base.Summary.MessageID = document.MessageID
	base.Summary.AttachmentCount = len(attachments)
	base.To = document.To
	base.CC = document.CC
	base.BCC = document.BCC
	base.Headers = headers
	base.Content = document.Content
	base.ContentSource = "imap_raw"
	base.ContentComplete = document.Complete
	base.MissingParts = append([]string(nil), document.MissingParts...)
	base.Attachments = attachments
	return base, nil
}

func (c *Client) GetRawSource(ctx context.Context, ref string) (string, error) {
	var localErr error
	if c.store != nil {
		raw, err := c.store.GetRawSource(ctx, ref)
		if err == nil {
			return raw, nil
		}
		if !safeTargetedFallback(err) {
			return "", err
		}
		localErr = err
	}
	if c.send.ImapClient() != nil {
		rawBytes, rawErr := c.HydrateMessageBytes(ctx, ref, true)
		if rawErr != nil {
			return "", rawErr
		}
		if len(rawBytes) > 0 {
			return string(rawBytes), nil
		}
	}
	if localErr != nil {
		return "", localErr
	}
	return "", c.readUnavailableError()
}

func (c *Client) WriteRawSource(ctx context.Context, ref string, writer io.Writer) error {
	var localErr error
	if c.store != nil {
		err := c.store.WriteRawSource(ctx, ref, writer)
		if err == nil {
			return nil
		}
		if !safeTargetedFallback(err) {
			return err
		}
		localErr = err
	}
	if c.send.ImapClient() != nil {
		rawBytes, rawErr := c.HydrateMessageBytes(ctx, ref, true)
		if rawErr != nil {
			return rawErr
		}
		if len(rawBytes) > 0 {
			_, werr := writer.Write(rawBytes)
			return werr
		}
	}
	if localErr != nil {
		return localErr
	}
	return c.readUnavailableError()
}

func (c *Client) SaveAttachmentTo(
	ctx context.Context,
	messageRef string,
	attachmentID string,
	outputPath string,
) error {
	var localErr error
	if c.store != nil {
		err := c.store.SaveAttachmentTo(ctx, messageRef, attachmentID, outputPath)
		if err == nil {
			return nil
		}
		if !safeTargetedFallback(err) {
			return err
		}
		localErr = err
		// Local materialization beats hydration: if Mail stored the
		// attachment as an external file, copying it needs no IMAP traffic
		// even when the .emlx source is partial or missing.
		saved, materializedErr := c.store.saveMaterializedAttachment(ctx, messageRef, attachmentID, outputPath)
		if materializedErr != nil {
			return materializedErr
		}
		if saved {
			return nil
		}
	}
	if c.send.ImapClient() != nil {
		rawBytes, rawErr := c.HydrateMessageBytes(ctx, messageRef, true)
		if rawErr != nil {
			return rawErr
		}
		if len(rawBytes) > 0 {
			return extractMIMEAttachment(bytes.NewReader(rawBytes), attachmentID, outputPath)
		}
	}
	if localErr != nil {
		return localErr
	}
	return c.readUnavailableError()
}

func (c *Client) SaveDraft(ctx context.Context, _ mail.Draft) (mail.MessageSummary, error) {
	if err := c.ComposeWriteSupportError(); err != nil {
		return mail.MessageSummary{}, err
	}
	return mail.MessageSummary{}, c.safeWriteUnavailableError()
}

func (c *Client) ReconcileDraftSave(
	ctx context.Context,
	draft mail.Draft,
	attempt mail.DraftSaveAttempt,
) (mail.DraftSaveEvidence, error) {
	baseline, err := c.draftSaveBaseline(ctx, attempt.ObservationBaseline)
	if err != nil {
		return mail.DraftSaveEvidence{}, err
	}
	evidence := mail.DraftSaveEvidence{
		InvocationStarted: attempt.InvocationStarted, AcceptedByMail: attempt.AcceptedByMail,
		ObservationBaseline: c.store.exportSendBaseline(baseline),
		Materialized:        cloneSaveMaterialization(attempt.Materialized),
	}
	observationDraft, err := c.materializedObservationDraft(ctx, draft, attempt.Materialized)
	if err != nil {
		return evidence, err
	}
	observed, found, err := c.store.observeMailboxCandidate(ctx, baseline, observationDraft, false)
	if err != nil || !found {
		return evidence, err
	}
	evidence.InvocationStarted = true
	evidence.AcceptedByMail = true
	evidence.ObservedMessage = observed
	return evidence, nil
}

func (c *Client) draftSaveBaseline(
	ctx context.Context,
	prepared *mail.SendObservationBaseline,
) (sendBaseline, error) {
	if c.store == nil {
		return sendBaseline{}, c.safeWriteUnavailableError()
	}
	if prepared != nil {
		return c.store.importSendBaseline(prepared)
	}
	return c.store.captureMailboxBaseline(
		ctx, mailboxAttributeDrafts, "draft_store_unavailable",
		"no active Drafts mailbox is available in the local Mail store",
	)
}

func cloneSaveMaterialization(value *mail.SendMaterialization) *mail.SendMaterialization {
	if value == nil {
		return nil
	}
	clone := *value
	clone.To = append([]mail.Recipient(nil), value.To...)
	clone.CC = append([]mail.Recipient(nil), value.CC...)
	clone.BCC = append([]mail.Recipient(nil), value.BCC...)
	if value.Body != nil {
		body := *value.Body
		clone.Body = &body
	}
	return &clone
}

// SendDraft submits the draft over direct SMTP and mirrors it into the
// account's Sent mailbox without touching Mail.app automation. It performs
// no local claim handling and never resubmits after an accepted submission,
// even when the mirror fails; the partial transport evidence is returned
// alongside the mirror error.
func (c *Client) SendDraft(ctx context.Context, draft mail.Draft) (mail.TransportEvidence, error) {
	return mail.DeliverViaTransport(ctx, c.send, draft)
}

func (c *Client) PrepareSend(ctx context.Context, _ mail.Draft) (mail.SendObservationBaseline, error) {
	if c.store == nil {
		return mail.SendObservationBaseline{}, c.safeWriteUnavailableError()
	}
	baseline, err := c.store.captureSendBaseline(ctx)
	if err != nil {
		return mail.SendObservationBaseline{}, err
	}
	return *c.store.exportSendBaseline(baseline), nil
}

func (c *Client) ReconcileSend(
	ctx context.Context,
	draft mail.Draft,
	attempt mail.SendAttempt,
) (mail.SendEvidence, error) {
	if c.store == nil {
		return mail.SendEvidence{}, c.safeWriteUnavailableError()
	}
	baseline, err := c.store.importSendBaseline(attempt.ObservationBaseline)
	if err != nil {
		return mail.SendEvidence{}, err
	}
	observationDraft, err := c.materializedObservationDraft(ctx, draft, attempt.Materialized)
	if err != nil {
		return mail.SendEvidence{}, err
	}
	observed, found, err := c.store.observeSent(ctx, baseline, observationDraft)
	evidence := mail.SendEvidence{
		InvocationStarted:   attempt.InvocationStarted,
		AcceptedByMail:      attempt.AcceptedByMail,
		ObservationBaseline: c.store.exportSendBaseline(baseline),
	}
	if err != nil || !found {
		return evidence, err
	}
	evidence.InvocationStarted = true
	evidence.AcceptedByMail = true
	evidence.SentStoreObserved = true
	evidence.ObservedMessageRef = observed.Ref
	return evidence, nil
}

func (c *Client) materializedObservationDraft(
	ctx context.Context,
	draft mail.Draft,
	materialized *mail.SendMaterialization,
) (mail.Draft, error) {
	if materialized == nil {
		return mail.Draft{}, operationError(
			"send_materialization_missing",
			"Mail.app did not return the final native headers and body required for exact store observation",
		)
	}
	if materialized.Body == nil {
		return mail.Draft{}, operationError(
			"send_materialization_invalid",
			"Mail.app returned no final native body for exact store observation",
		)
	}
	result := draft
	result.From = materialized.From
	result.To = append([]mail.Recipient(nil), materialized.To...)
	result.CC = append([]mail.Recipient(nil), materialized.CC...)
	result.BCC = append([]mail.Recipient(nil), materialized.BCC...)
	result.Subject = materialized.Subject
	body := *materialized.Body
	result.ExpectedBody = &body
	if parsedAddress(result.From) == "" || len(result.To)+len(result.CC)+len(result.BCC) == 0 {
		return mail.Draft{}, operationError(
			"send_materialization_invalid", "Mail.app returned incomplete native send headers",
		)
	}
	if _, valid := draftRecipientAddressSets(result); !valid {
		return mail.Draft{}, operationError(
			"send_materialization_invalid", "Mail.app returned invalid or duplicate native recipients",
		)
	}
	if draft.Kind == mail.DraftKindForward {
		native, err := c.sourceAttachmentFingerprints(ctx, draft.SourceRef)
		if err != nil {
			return mail.Draft{}, err
		}
		result.Attachments = append(native, draft.Attachments...)
	}
	if materialized.AttachmentCount != len(result.Attachments) {
		if materialized.AttachmentCount < len(result.Attachments) {
			return mail.Draft{}, operationError(
				"send_materialization_invalid",
				fmt.Sprintf("Mail.app materialized %d attachments; at least %d reviewed or forwarded attachments are required", materialized.AttachmentCount, len(result.Attachments)),
			)
		}
	}
	count := materialized.AttachmentCount
	result.ExpectedAttachmentCount = &count
	return result, nil
}

func (c *Client) sourceAttachmentFingerprints(
	ctx context.Context,
	ref string,
) (result []mail.DraftAttachment, resultErr error) {
	if c.store == nil {
		return nil, c.safeWriteUnavailableError()
	}
	_, source, err := c.store.openMessageSource(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer joinCloseError(&resultErr, source, "forward source")
	if source.partial {
		return nil, operationError(
			"forward_source_incomplete", "forward source is partial; original attachments cannot be proven",
		)
	}
	document, err := parseMIMEDocument(source.Reader(), false, true, false)
	if err != nil {
		return nil, err
	}
	identifiers := make([]string, 0, len(document.Parts))
	for identifier := range document.Parts {
		identifiers = append(identifiers, identifier)
	}
	sort.Strings(identifiers)
	attachments := make([]mail.DraftAttachment, 0, len(identifiers))
	for _, identifier := range identifiers {
		part := document.Parts[identifier]
		if part.Name == "" || !part.Complete || part.Size < 0 || part.SHA256 == "" {
			return nil, operationError(
				"forward_source_incomplete", "forward source attachment bytes cannot be proven",
			)
		}
		attachments = append(attachments, mail.DraftAttachment{
			Path: part.Name, Size: part.Size, SHA256: part.SHA256,
		})
	}
	return attachments, nil
}

func (c *Client) rejectUnconfirmedDraftMutation(ctx context.Context, ref string, allowed bool) error {
	if allowed {
		return nil
	}
	isDraft, err := c.store.messageInSpecialMailbox(ctx, ref, mailboxAttributeDrafts)
	if err != nil {
		return err
	}
	if !isDraft {
		return nil
	}
	return operationError(
		"draft_mutation_confirmation_required",
		"source message is in Drafts; repeat with --allow-draft only after closing any editor for that draft",
	)
}

func (c *Client) Sync(ctx context.Context, accountRef string) error {
	if c.fallback == nil {
		return c.writeUnavailableError()
	}
	return c.fallback.Sync(ctx, accountRef)
}

// MessageThreadSource resolves the reply/forward derivation inputs from the
// source message's header block (header-only read, no body scan). The
// message's current membership is revalidated as part of the source open.
func (c *Client) MessageThreadSource(ctx context.Context, ref string) (mail.ThreadSource, error) {
	if c.store == nil {
		return mail.ThreadSource{}, c.readUnavailableError()
	}
	_, source, err := c.store.openMessageSource(ctx, ref)
	if err != nil {
		return mail.ThreadSource{}, err
	}
	defer func() { _ = source.Close() }()
	headers, err := sourceHeadersFromReader(source.Reader())
	if err != nil {
		return mail.ThreadSource{}, err
	}
	if headers.MessageID == "" {
		return mail.ThreadSource{}, operationError(
			"invalid_message_source", "source message has no Message-ID header",
		)
	}
	// The composer writes threading headers verbatim, so the source message
	// id travels in its bracketed msg-id form; the header reader returns the
	// bare id.
	messageID := headers.MessageID
	if messageID != "" && !strings.HasPrefix(messageID, "<") {
		messageID = "<" + messageID + ">"
	}
	return mail.ThreadSource{
		Subject:    headers.Subject,
		From:       headers.From,
		ReplyTo:    headers.ReplyTo,
		To:         headers.To,
		CC:         headers.CC,
		MessageID:  messageID,
		References: headers.References,
	}, nil
}

func (c *Client) readUnavailableError() error {
	if c.storeErr != nil {
		return c.storeErr
	}
	return operationError("mail_store_unavailable", "local Mail store is unavailable")
}

func (c *Client) writeUnavailableError() error {
	return operationError("mail_automation_unavailable", "Mail.app automation backend is unavailable")
}

func (c *Client) safeWriteUnavailableError() error {
	return operationError(
		"safe_write_unavailable",
		"verified message mutation requires the supported read-only local Mail store; no unverified write was attempted",
	)
}

func safeTargetedFallback(err error) bool {
	var typed *Error
	if !errors.As(err, &typed) {
		return false
	}
	switch typed.Code {
	case "raw_source_partial", "message_source_missing", "attachment_not_downloaded", "not_found",
		"store_bound_reference_required", "invalid_emlx":
		return true
	default:
		return false
	}
}
