package mailstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
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
	mailboxLoads      singleflight.Group
	mutationMu        sync.Mutex
	copyAttempts      map[string]transport.MutationEvidence
	excerpts          excerptCache
}

type accountBindingSnapshotContextKey struct{}

type accountBindingSnapshot struct {
	client           *Client
	once             sync.Once
	bindings         mail.AccountBindingFile
	err              error
	mailboxRecordsMu sync.Mutex
	mailboxRecords   map[string]accountMailboxRecordCacheEntry
}

type accountMailboxRecordCacheEntry struct {
	records []mailboxRecord
	err     error
}

// WithAccountBindingSnapshot scopes one lazily loaded binding document to an invocation context.
func (c *Client) WithAccountBindingSnapshot(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil {
		return ctx
	}
	if snapshot, ok := ctx.Value(accountBindingSnapshotContextKey{}).(*accountBindingSnapshot); ok && snapshot.client == c {
		return ctx
	}
	return context.WithValue(ctx, accountBindingSnapshotContextKey{}, &accountBindingSnapshot{client: c})
}

func (c *Client) accountBindingsForResolution(ctx context.Context) (mail.AccountBindingFile, error) {
	if snapshot, ok := ctx.Value(accountBindingSnapshotContextKey{}).(*accountBindingSnapshot); ok && snapshot.client == c {
		snapshot.once.Do(func() {
			snapshot.bindings, snapshot.err = c.loadAccountBindingsForResolution()
		})
		return snapshot.bindings, snapshot.err
	}
	return c.loadAccountBindingsForResolution()
}

func accountMailboxRecordsCache(ctx context.Context, store *Store) (*accountBindingSnapshot, bool) {
	if ctx == nil {
		return nil, false
	}
	snapshot, ok := ctx.Value(accountBindingSnapshotContextKey{}).(*accountBindingSnapshot)
	return snapshot, ok && snapshot.client.store == store
}

func (c *Client) loadAccountBindingsForResolution() (mail.AccountBindingFile, error) {
	bindingStore := c.send.AccountBindings
	if bindingStore == nil && c.store != nil {
		bindingStore = c.store.accountBindings
	}
	if bindingStore == nil {
		return mail.AccountBindingFile{Version: mail.AccountBindingVersion, Bindings: []mail.AccountBinding{}}, nil
	}
	return bindingStore.LoadAccountBindings()
}

func NewClient(ctx context.Context, fallback mail.FallbackGateway, config Config, send mail.SendTransport) *Client {
	started := time.Now()
	if config.AccountBindings == nil && send.AccountBindings != nil {
		config.AccountBindings = send.AccountBindings
	}
	store, err := Open(ctx, config)
	if store != nil && send.AccountBindings == nil {
		send.AccountBindings = store.accountBindings
	}
	return &Client{
		store: store, storeErr: err, fallback: fallback, send: send,
		storeOpenDuration: time.Since(started),
		mailboxCache:      make(map[string]mailboxCacheEntry),
		copyAttempts:      make(map[string]transport.MutationEvidence),
		excerpts:          excerptCache{dir: config.ExcerptCacheDirectory},
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
	if profile, known := c.StoreProfile(); known {
		checks = append(checks, mail.Check{
			Name: "mail-store-profile", Status: "pass", Detail: storeProfileDetail(profile),
		})
	}
	return mail.DiagnosticReport{Checks: checks}, timings
}

// StoreProfile reports whether the opened Envelope Index carries the exact
// verified profile or an unverified writer build whose format generation and
// required capabilities still verify.
func (c *Client) StoreProfile() (mail.StoreProfile, bool) {
	if c == nil || c.store == nil {
		return mail.StoreProfile{}, false
	}
	capability := c.store.capability
	profile := mail.StoreProfile{
		State:                     mail.StoreProfileVerified,
		FrameworkVersion:          capability.FrameworkVersion,
		SupportedFrameworkVersion: supportedFrameworkVersion,
	}
	if !capability.ProfileVerified {
		profile.State = mail.StoreProfileUnverified
		profile.Code = mail.StoreProfileUnverifiedCode
	}
	return profile, true
}

func storeProfileDetail(profile mail.StoreProfile) string {
	if profile.Unverified() {
		return fmt.Sprintf(
			"profile unverified: framework %s differs from verified %s; schema capabilities verified",
			profile.FrameworkVersion, profile.SupportedFrameworkVersion,
		)
	}
	return "profile verified"
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
	if c.fallback == nil || nestedErrorCode(c.storeErr) == "mail_store_not_initialized" {
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
	if nestedErrorCode(c.storeErr) == "mail_store_not_initialized" {
		return nil, c.storeErr
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
	if nestedErrorCode(c.storeErr) == "mail_store_not_initialized" {
		return mail.MessagePage{}, c.storeErr
	}
	if !strings.HasPrefix(request.MailboxRef, "mbx_") || request.AccountRef != "" {
		return mail.MessagePage{}, operationErrorWithCause("safe_message_listing_unavailable",
			"unified inbox and mailbox selectors require the supported local Mail store; no Apple Events global scan was attempted", c.readUnavailableError())
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
		if nestedErrorCode(c.storeErr) == "mail_store_not_initialized" {
			return mail.SearchPage{}, c.storeErr
		}
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
	if mailref.IsServerRef(ref) {
		if openDraft {
			return mail.Message{}, &mail.ValidationError{
				Code: "invalid_reference", Message: "a server ref cannot open a draft; use the local ref once Mail.app has synced the message",
			}
		}
		return c.readServerMessage(ctx, ref)
	}
	if err := c.unavailableMessageRefError(ref); err != nil {
		return mail.Message{}, err
	}
	var local mail.Message
	hasLocal := false
	var localErr error
	if c.store != nil {
		localCtx, cancelLocal := localReadOrResolveContext(ctx)
		local, localErr = c.store.GetMessage(localCtx, ref)
		cancelLocal()
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
		rawSource, size, summary, rawErr := c.hydrateMessageSource(ctx, ref)
		if rawErr == nil {
			if size > 0 {
				message, err := messageFromRawReader(ctx, local, summary, rawSource)
				return message, errors.Join(err, rawSource.Close())
			}
			rawErr = rawSource.Close()
		}
		// IMAP fallback failed. Preserve both local and remote causes so the
		// caller sees the full picture.
		if rawErr != nil && hasLocal {
			remoteErr := typedHydrationFailure(rawErr)
			local.Hydration = messageHydrationDiagnostic(ctx, local, remoteErr)
			return local, newHydrationError("read message", incompleteMessageCause(local), remoteErr)
		}
		if rawErr != nil && localErr != nil {
			return mail.Message{}, newHydrationError("read message", localErr, rawErr)
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

func messageFromRawFallback(ctx context.Context, base mail.Message, summary mail.MessageSummary, raw string) (mail.Message, error) {
	return messageFromRawReader(ctx, base, summary, strings.NewReader(raw))
}

func messageFromRawReader(ctx context.Context, base mail.Message, summary mail.MessageSummary, raw io.ReadSeeker) (mail.Message, error) {
	// A completed source is replayable local data. Keep its Close ownership
	// with the caller; context-aware reads need no separate close watcher.
	document, err := parseMIMEDocumentWithContext(ctx, mimeContextReader{ctx: ctx, reader: raw}, false, false, false)
	if err != nil {
		return mail.Message{}, err
	}
	if _, err := raw.Seek(0, io.SeekStart); err != nil {
		return mail.Message{}, err
	}
	headers, err := readRawHeaders(mimeContextReader{ctx: ctx, reader: raw})
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
	if mailref.IsServerRef(ref) {
		return c.rawServerSource(ctx, ref)
	}
	if err := c.unavailableMessageRefError(ref); err != nil {
		return "", err
	}
	var localErr error
	if c.store != nil {
		localCtx, cancelLocal := localReadOrResolveContext(ctx)
		raw, err := c.store.GetRawSource(localCtx, ref)
		cancelLocal()
		if err == nil {
			return raw, nil
		}
		if !safeTargetedFallback(err) {
			return "", err
		}
		localErr = err
	}
	if c.send.ImapClient() != nil {
		source, size, _, rawErr := c.hydrateMessageSource(ctx, ref)
		if rawErr != nil {
			return "", newHydrationError("read raw source", localErr, rawErr)
		}
		raw, rawErr := readHydratedRawSource(ctx, source, size)
		if rawErr != nil {
			return "", newHydrationError("read raw source", localErr, rawErr)
		}
		if size > 0 {
			return raw, nil
		}
	}
	if localErr != nil {
		return "", localErr
	}
	return "", c.readUnavailableError()
}

func (c *Client) WriteRawSource(ctx context.Context, ref string, writer io.Writer) error {
	if mailref.IsServerRef(ref) {
		return c.writeServerRawSource(ctx, ref, writer)
	}
	if err := c.unavailableMessageRefError(ref); err != nil {
		return err
	}
	var localErr error
	if c.store != nil {
		localCtx, cancelLocal := localReadOrResolveContext(ctx)
		err := c.store.WriteRawSource(localCtx, ref, writer)
		cancelLocal()
		if err == nil {
			return nil
		}
		if !safeTargetedFallback(err) {
			return err
		}
		localErr = err
	}
	if c.send.ImapClient() != nil {
		source, size, _, rawErr := c.hydrateMessageSource(ctx, ref)
		if rawErr != nil {
			return newHydrationError("write raw source", localErr, rawErr)
		}
		if size > 0 {
			return errors.Join(copyHydratedSource(ctx, writer, source, size), source.Close())
		}
		if err := source.Close(); err != nil {
			return err
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
	_, err := c.SaveAttachmentToWithEvidence(ctx, messageRef, attachmentID, outputPath)
	return err
}

func (c *Client) SaveAttachmentToWithEvidence(
	ctx context.Context,
	messageRef string,
	attachmentID string,
	outputPath string,
) (mail.AttachmentEvidence, error) {
	if mailref.IsServerRef(messageRef) {
		return c.saveServerAttachment(ctx, messageRef, attachmentID, outputPath)
	}
	if err := c.unavailableMessageRefError(messageRef); err != nil {
		return mail.AttachmentEvidence{}, err
	}
	var localErr error
	if c.store != nil {
		localCtx, cancelLocal := localReadOrResolveContext(ctx)
		evidence, err := c.store.saveAttachmentToWithEvidence(localCtx, messageRef, attachmentID, outputPath)
		if err == nil {
			cancelLocal()
			return evidence, nil
		}
		if !safeTargetedFallback(err) {
			cancelLocal()
			return mail.AttachmentEvidence{}, err
		}
		localErr = err
		// Local materialization beats hydration: if Mail stored the
		// attachment as an external file, copying it needs no IMAP traffic
		// even when the .emlx source is partial or missing.
		evidence, saved, materializedErr := c.store.saveMaterializedAttachmentWithEvidence(
			localCtx, messageRef, attachmentID, outputPath,
		)
		cancelLocal()
		if materializedErr != nil {
			return mail.AttachmentEvidence{}, materializedErr
		}
		if saved {
			return evidence, nil
		}
	}
	if c.send.ImapClient() != nil {
		source, size, _, rawErr := c.hydrateMessageSource(ctx, messageRef)
		if rawErr != nil {
			return mail.AttachmentEvidence{}, newHydrationError("save attachment", localErr, rawErr)
		}
		if size > 0 {
			evidence, err := extractMIMEAttachmentWithEvidence(mimeContextReader{ctx: ctx, reader: source}, attachmentID, outputPath)
			return evidence, errors.Join(err, source.Close())
		}
		if err := source.Close(); err != nil {
			return mail.AttachmentEvidence{}, err
		}
	}
	if localErr != nil {
		return mail.AttachmentEvidence{}, localErr
	}
	return mail.AttachmentEvidence{}, c.readUnavailableError()
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
	document, err := parseMIMEDocumentWithContext(ctx, source.Reader(), false, true, false)
	if err != nil {
		return nil, err
	}
	if document.budget != nil && document.budget.error() != nil {
		return nil, operationError(
			"forward_source_incomplete", "forward source MIME budget prevented complete attachment coverage",
		)
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
		if err := c.unavailableMessageRefError(ref); err != nil {
			return mail.ThreadSource{}, err
		}
		return mail.ThreadSource{}, c.readUnavailableError()
	}
	localCtx, cancelLocal := localReadOrResolveContext(ctx)
	_, source, localErr := c.store.openMessageSource(localCtx, ref)
	var headers sourceHeaders
	if localErr == nil {
		headers, localErr = sourceHeadersFromReader(mimeContextReader{ctx: localCtx, reader: source.Reader()})
		localErr = errors.Join(localErr, source.Close())
	}
	cancelLocal()
	if localErr != nil {
		_, hasHeaderFetcher := c.send.ImapClient().(transport.MessageHeaderFetcher)
		if !safeTargetedFallback(localErr) || !hasHeaderFetcher {
			return mail.ThreadSource{}, localErr
		}
		var summary mail.MessageSummary
		var remoteErr error
		headers, summary, remoteErr = c.hydrateMessageHeaders(ctx, ref)
		if remoteErr != nil {
			return mail.ThreadSource{}, newHydrationError("read thread source headers", localErr, remoteErr)
		}
		if summary.MessageID != "" && headers.MessageID != summary.MessageID {
			return mail.ThreadSource{}, &transport.TransportError{
				Code: transport.CodeIMAPMessageUIDMismatch, Message: "IMAP thread source Message-ID differs from the bound source",
			}
		}
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
		Subject:             headers.Subject,
		From:                headers.From,
		FromParseError:      headers.FromError,
		ReplyTo:             headers.ReplyTo,
		ReplyToParseError:   headers.ReplyToError,
		To:                  headers.To,
		CC:                  headers.CC,
		MessageID:           messageID,
		References:          headers.References,
		ReferencesPresent:   headers.ReferencesPresent,
		InReplyTo:           headers.InReplyTo,
		InReplyToPresent:    headers.InReplyToPresent,
		RecipientParseError: errors.Join(headers.ToError, headers.CCError),
	}, nil
}

// unavailableMessageRefError reports a malformed message ref as caller input
// before the unavailable store is reported, so an agent fixes the ref instead
// of asking the user for Full Disk Access.
func (c *Client) unavailableMessageRefError(ref string) error {
	if c.store != nil {
		return nil
	}
	if _, err := mailref.DecodeMessage(ref); err != nil {
		return &mail.ValidationError{Code: "invalid_reference", Message: "invalid message ref; use a current ref from a listing"}
	}
	return nil
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
	if nestedErrorCode(c.storeErr) == "mail_store_not_initialized" {
		return c.storeErr
	}
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
