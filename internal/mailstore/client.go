package mailstore

import (
	"context"
	"errors"
	"strconv"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

type Client struct {
	store    *Store
	storeErr error
	fallback mail.Gateway
}

func NewClient(ctx context.Context, fallback mail.Gateway, config Config) *Client {
	store, err := Open(ctx, config)
	return &Client{store: store, storeErr: err, fallback: fallback}
}

func (c *Client) Close() error {
	if c.store == nil {
		return nil
	}
	return c.store.Close()
}

func (c *Client) Probe(ctx context.Context, live bool) mail.DiagnosticReport {
	checks := c.platformChecks(ctx, live)
	if c.storeErr != nil {
		code := "mail_store_unavailable"
		var typed interface{ ErrorCode() string }
		if errors.As(c.storeErr, &typed) {
			code = typed.ErrorCode()
		}
		checks = append(checks, mail.Check{
			Name: "mail-store-read", Status: "fail", Code: code, Detail: c.storeErr.Error(),
		})
		return mail.DiagnosticReport{Checks: checks}
	}
	checks = append(checks, mail.Check{
		Name: "mail-store-read", Status: "pass",
		Detail: "strict read-only WAL access; schema " + c.store.SchemaFingerprint(),
	})
	return mail.DiagnosticReport{Checks: checks}
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
	var localErr error
	if c.store != nil {
		message, err := c.store.GetMessage(ctx, ref)
		if err == nil && message.ContentComplete {
			return message, nil
		}
		if err == nil && c.fallback == nil {
			return message, nil
		}
		if err != nil && !safeTargetedFallback(err) {
			return mail.Message{}, err
		}
		localErr = err
	}
	if c.fallback == nil {
		if localErr != nil {
			return mail.Message{}, localErr
		}
		return mail.Message{}, c.readUnavailableError()
	}
	automationRef, err := c.automationMessageRef(ctx, ref)
	if err != nil {
		return mail.Message{}, err
	}
	return c.fallback.GetMessage(ctx, automationRef)
}

func (c *Client) OpenDraft(ctx context.Context, ref string) (mail.Message, error) {
	var localErr error
	if c.store != nil {
		message, err := c.store.GetMessage(ctx, ref)
		if err == nil && message.ContentComplete {
			return message, nil
		}
		if err == nil && c.fallback == nil {
			return message, nil
		}
		if err != nil && !safeTargetedFallback(err) {
			return mail.Message{}, err
		}
		localErr = err
	}
	if c.fallback == nil {
		if localErr != nil {
			return mail.Message{}, localErr
		}
		return mail.Message{}, c.readUnavailableError()
	}
	automationRef, err := c.automationMessageRef(ctx, ref)
	if err != nil {
		return mail.Message{}, err
	}
	return c.fallback.OpenDraft(ctx, automationRef)
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
	if c.fallback == nil {
		if localErr != nil {
			return "", localErr
		}
		return "", c.readUnavailableError()
	}
	automationRef, err := c.automationMessageRef(ctx, ref)
	if err != nil {
		return "", err
	}
	return c.fallback.GetRawSource(ctx, automationRef)
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
	}
	if c.fallback == nil {
		if localErr != nil {
			return localErr
		}
		return c.readUnavailableError()
	}
	automationRef, err := c.automationMessageRef(ctx, messageRef)
	if err != nil {
		return err
	}
	return c.fallback.SaveAttachmentTo(ctx, automationRef, attachmentID, outputPath)
}

func (c *Client) SaveDraft(ctx context.Context, draft mail.Draft) (mail.MessageSummary, error) {
	automationDraft, err := c.automationDraft(ctx, draft)
	if err != nil {
		return mail.MessageSummary{}, err
	}
	if c.fallback == nil {
		return mail.MessageSummary{}, c.writeUnavailableError()
	}
	var baseline sendBaseline
	baselineAvailable := false
	if c.store != nil {
		baseline, err = c.store.captureMailboxBaseline(
			ctx, mailboxAttributeDrafts, "draft_store_unavailable",
			"no active Drafts mailbox is available in the local Mail store",
		)
		baselineAvailable = err == nil
	}
	_, saveErr := c.fallback.SaveDraft(ctx, automationDraft)
	if baselineAvailable {
		observationCtx, cancel := context.WithTimeout(context.Background(), sendObservationWindow)
		defer cancel()
		observed, found, observationErr := c.store.observeMailboxCandidate(
			observationCtx, baseline, draft, false,
		)
		if observationErr == nil && found {
			return observed, nil
		}
	}
	if saveErr != nil {
		return mail.MessageSummary{}, saveErr
	}
	return mail.MessageSummary{}, operationError(
		"draft_outcome_unknown",
		"Mail.app accepted the native draft save, but the local Drafts store did not expose a unique result; the local review draft was retained",
	)
}

func (c *Client) SendDraft(ctx context.Context, draft mail.Draft) (mail.SendEvidence, error) {
	automationDraft, err := c.automationDraft(ctx, draft)
	if err != nil {
		return mail.SendEvidence{}, err
	}
	if c.fallback == nil {
		return mail.SendEvidence{}, c.writeUnavailableError()
	}
	baseline, err := c.sendBaseline(draft.PreparedSendBaseline)
	if err != nil {
		return mail.SendEvidence{}, err
	}
	evidence, sendErr := c.fallback.SendDraft(ctx, automationDraft)
	evidence.ObservationBaseline = c.store.exportSendBaseline(baseline)
	if !evidence.InvocationStarted {
		return evidence, sendErr
	}
	observationCtx, cancel := context.WithTimeout(context.Background(), sendObservationWindow)
	defer cancel()
	observed, found, observationErr := c.store.observeSent(observationCtx, baseline, draft)
	if observationErr == nil && found {
		evidence.SentStoreObserved = true
		evidence.ObservedMessageRef = observed.Ref
		return evidence, nil
	}
	return evidence, sendErr
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
	observed, found, err := c.store.observeSent(ctx, baseline, draft)
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

func (c *Client) sendBaseline(prepared *mail.SendObservationBaseline) (sendBaseline, error) {
	if c.store == nil {
		return sendBaseline{}, c.safeWriteUnavailableError()
	}
	if prepared == nil {
		return sendBaseline{}, operationError(
			"send_prepare_required", "send requires a durably persisted store observation baseline",
		)
	}
	return c.store.importSendBaseline(prepared)
}

func (c *Client) MarkMessage(
	ctx context.Context,
	request mail.MarkMessageRequest,
) (mail.MessageSummary, error) {
	if c.store == nil {
		return mail.MessageSummary{}, c.safeWriteUnavailableError()
	}
	storeRequest := request
	automationRef, err := c.automationWriteRef(ctx, request.Ref)
	if err != nil {
		return mail.MessageSummary{}, err
	}
	if c.fallback == nil {
		return mail.MessageSummary{}, c.writeUnavailableError()
	}
	request.Ref = automationRef
	_, mutationErr := c.fallback.MarkMessage(ctx, request)
	observationCtx, cancel := context.WithTimeout(context.Background(), mutationObservationWindow)
	defer cancel()
	observed, found, observationErr := c.store.observeMarkedMessage(
		observationCtx, storeRequest.Ref, storeRequest,
	)
	if observationErr == nil && found {
		return observed, nil
	}
	if mutationErr != nil {
		return mail.MessageSummary{}, mutationErr
	}
	return mail.MessageSummary{}, operationError(
		"mutation_not_observed", "Mail.app accepted the state change, but the local Mail store did not observe it",
	)
}

func (c *Client) TransferMessage(
	ctx context.Context,
	request mail.TransferMessageRequest,
) (mail.MessageSummary, error) {
	if c.store == nil {
		return mail.MessageSummary{}, c.safeWriteUnavailableError()
	}
	baseline, err := c.store.captureTransferBaseline(
		ctx, request.Ref, request.DestinationMailbox,
	)
	if err != nil {
		return mail.MessageSummary{}, err
	}
	automationRef, err := c.automationWriteRef(ctx, request.Ref)
	if err != nil {
		return mail.MessageSummary{}, err
	}
	if c.fallback == nil {
		return mail.MessageSummary{}, c.writeUnavailableError()
	}
	request.Ref = automationRef
	_, mutationErr := c.fallback.TransferMessage(ctx, request)
	observationCtx, cancel := context.WithTimeout(context.Background(), mutationObservationWindow)
	defer cancel()
	observed, found, observationErr := c.store.observeTransfer(
		observationCtx, baseline, request.Copy,
	)
	if observationErr == nil && found {
		return observed, nil
	}
	if mutationErr != nil {
		return mail.MessageSummary{}, mutationErr
	}
	return mail.MessageSummary{}, operationError(
		"mutation_not_observed", "Mail.app accepted the transfer, but the local Mail store did not observe it",
	)
}

func (c *Client) DeleteMessage(ctx context.Context, ref string) error {
	if c.store == nil {
		return c.safeWriteUnavailableError()
	}
	automationRef, err := c.automationWriteRef(ctx, ref)
	if err != nil {
		return err
	}
	if c.fallback == nil {
		return c.writeUnavailableError()
	}
	mutationErr := c.fallback.DeleteMessage(ctx, automationRef)
	observationCtx, cancel := context.WithTimeout(context.Background(), mutationObservationWindow)
	defer cancel()
	observed, observationErr := c.store.observeMessageRemovedFromMailbox(observationCtx, ref)
	if observationErr == nil && observed {
		return nil
	}
	if mutationErr != nil {
		return mutationErr
	}
	return operationError(
		"mutation_not_observed", "Mail.app accepted deletion, but the message remained in its original mailbox",
	)
}

func (c *Client) Sync(ctx context.Context, accountRef string) error {
	if c.fallback == nil {
		return c.writeUnavailableError()
	}
	return c.fallback.Sync(ctx, accountRef)
}

func (c *Client) automationDraft(ctx context.Context, draft mail.Draft) (mail.Draft, error) {
	if draft.SourceRef == "" {
		return draft, nil
	}
	ref, err := c.automationWriteRef(ctx, draft.SourceRef)
	if err != nil {
		return mail.Draft{}, err
	}
	draft.SourceRef = ref
	return draft, nil
}

func (c *Client) automationMessageRef(ctx context.Context, value string) (string, error) {
	ref, err := mailref.DecodeMessage(value)
	if err != nil {
		return "", operationError("invalid_reference", "invalid message ref: "+err.Error())
	}
	if !ref.IsStoreBound() {
		return value, nil
	}
	if c.store == nil {
		return "", c.readUnavailableError()
	}
	resolved, err := c.store.resolveMessage(ctx, value)
	if err != nil {
		return "", err
	}
	return mailref.EncodeMessage(mailref.Message{
		AccountID:       resolved.PhysicalLocation.AccountID,
		MailboxPath:     resolved.PhysicalLocation.VisiblePath,
		LibraryID:       strconv.FormatInt(resolved.Record.RowID, 10),
		ExpectedSubject: resolved.Record.Subject,
	})
}

func (c *Client) automationWriteRef(ctx context.Context, value string) (string, error) {
	ref, err := mailref.DecodeMessage(value)
	if err != nil {
		return "", operationError("invalid_reference", "invalid message ref: "+err.Error())
	}
	if c.store != nil && !ref.IsStoreBound() {
		return "", operationError(
			"invalid_reference",
			"message write requires a store-bound ref; list or search the message again",
		)
	}
	return c.automationMessageRef(ctx, value)
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
