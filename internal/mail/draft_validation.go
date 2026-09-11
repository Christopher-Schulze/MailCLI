package mail

import (
	"context"
	stdmail "net/mail"
	"strings"
	"time"

	"mailcli/internal/mailref"
)

func prepareDraft(request CreateDraftRequest) (Draft, error) {
	return prepareDraftWithObserver(request, nil)
}

func prepareDraftWithObserver(request CreateDraftRequest, observer draftContentObserver) (Draft, error) {
	return prepareDraftWithAttachmentsObserver(request, nil, observer)
}

func prepareDraftWithAttachmentsObserver(
	request CreateDraftRequest,
	previous []DraftAttachment,
	observer draftContentObserver,
) (Draft, error) {
	return prepareDraftWithAttachmentsObserverContext(context.Background(), request, previous, observer)
}

func prepareDraftWithAttachmentsObserverContext(
	ctx context.Context,
	request CreateDraftRequest,
	previous []DraftAttachment,
	observer draftContentObserver,
) (Draft, error) {
	if err := ctx.Err(); err != nil {
		return Draft{}, err
	}
	if request.Kind == "" {
		request.Kind = DraftKindNew
	}
	if request.Kind != DraftKindNew && request.Kind != DraftKindReply && request.Kind != DraftKindForward {
		return Draft{}, validationError("draft kind must be new, reply, or forward")
	}
	if request.Kind != DraftKindNew && request.SourceRef == "" {
		return Draft{}, validationError("reply and forward drafts require a source message ref")
	}
	if request.Kind != DraftKindNew {
		ref, err := mailref.DecodeMessage(request.SourceRef)
		if err != nil || !ref.IsStoreBound() {
			return Draft{}, validationError("reply and forward drafts require a store-bound source message ref")
		}
		if err := validateThreadSource(request.SourceMessageID, request.SourceReferences); err != nil {
			return Draft{}, err
		}
		canonicalReferences, err := canonicalThreadReferences(request.SourceReferences, request.SourceMessageID)
		if err != nil {
			return Draft{}, err
		}
		request.SourceReferences = canonicalReferences
	}
	if request.Kind == DraftKindNew && len(request.Input.To)+len(request.Input.CC)+len(request.Input.BCC) == 0 {
		return Draft{}, validationError("new drafts require at least one recipient")
	}
	if request.Kind == DraftKindForward && len(request.Input.To)+len(request.Input.CC)+len(request.Input.BCC) == 0 {
		return Draft{}, validationError("forward drafts require at least one explicit recipient")
	}
	if strings.ContainsAny(request.Input.AccountRef, "\r\n\x00") {
		return Draft{}, validationError("account ref contains control characters")
	}
	if err := validateDraftLimits(request.Input); err != nil {
		return Draft{}, err
	}
	if err := validateDraftAddresses(request.Input); err != nil {
		return Draft{}, err
	}
	content, err := prepareDraftContentWithObserver(ctx, request.Input.BodyFormat, request.Input.Body, observer)
	if err != nil {
		return Draft{}, err
	}
	if err := ctx.Err(); err != nil {
		return Draft{}, err
	}
	attachments, err := fingerprintAttachmentsWithPreviousContext(ctx, request.Input.Attachments, previous)
	if err != nil {
		return Draft{}, err
	}
	if err := ctx.Err(); err != nil {
		return Draft{}, err
	}
	ref, err := newDraftReference()
	if err != nil {
		return Draft{}, err
	}
	now := time.Now().UTC()
	return Draft{
		Ref: ref, Kind: request.Kind, SourceRef: request.SourceRef,
		AccountRef:      request.Input.AccountRef,
		ReplyAll:        request.ReplyAll,
		SourceMessageID: request.SourceMessageID, SourceReferences: request.SourceReferences,
		From: request.Input.From,
		To:   nonNilRecipients(request.Input.To), CC: nonNilRecipients(request.Input.CC),
		BCC: nonNilRecipients(request.Input.BCC), Subject: request.Input.Subject,
		Body: content.Plain, BodyFormat: content.Format,
		BodySource: content.Source, BodyHTML: content.HTML, Attachments: attachments,
		ContentDiagnostics: content.Diagnostics,
		CreatedAt:          now, UpdatedAt: now,
	}, nil
}

// validateThreadSource keeps malformed or control-containing source threading
// values out of drafts before the composer serializes them.
func validateThreadSource(sourceMessageID, sourceReferences string) error {
	if strings.ContainsAny(sourceMessageID, "\r\n") {
		return validationError("source message id contains control characters")
	}
	// Raw check: strings.Fields would treat CR/LF as whitespace and hide them.
	if strings.ContainsAny(sourceReferences, "\r\n") {
		return validationError("source references contain control characters")
	}
	_, err := canonicalThreadReferences(sourceReferences, sourceMessageID)
	return err
}

func validateDraftLimits(input DraftInput) error {
	return validateDraftResourceLimits(
		len(input.Subject), len(input.Body), len(input.To)+len(input.CC)+len(input.BCC), len(input.Attachments),
	)
}

func validateStoredDraftLimits(draft Draft) error {
	bodyBytes := max(len(draft.Body), len(draft.BodySource), len(draft.BodyHTML))
	return validateDraftResourceLimits(
		len(draft.Subject), bodyBytes, len(draft.To)+len(draft.CC)+len(draft.BCC), len(draft.Attachments),
	)
}

func validateDraftResourceLimits(subjectBytes int, bodyBytes int, recipients int, attachments int) error {
	if subjectBytes > MaximumDraftSubjectBytes {
		return validationError("draft subject exceeds 64 KiB")
	}
	if bodyBytes > MaximumDraftBodyBytes {
		return validationError("draft body exceeds 4 MiB")
	}
	if recipients > MaximumDraftRecipients {
		return validationError("draft exceeds 200 total recipients")
	}
	if attachments > MaximumDraftAttachments {
		return validationError("draft exceeds 100 attachments")
	}
	return nil
}

func validateDraftAddresses(input DraftInput) error {
	if input.From != "" {
		if _, err := stdmail.ParseAddress(input.From); err != nil {
			return validationError("invalid from address")
		}
	}
	seen := make(map[string]struct{})
	for _, group := range [][]Recipient{input.To, input.CC, input.BCC} {
		for _, recipient := range group {
			normalized, err := recipientAddressKey(recipient)
			if err != nil {
				return validationError("invalid recipient address")
			}
			if _, duplicate := seen[normalized]; duplicate {
				return validationError("duplicate recipient address")
			}
			seen[normalized] = struct{}{}
		}
	}
	return nil
}
