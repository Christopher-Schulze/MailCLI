package mailstore

import (
	"context"
	"strings"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

// MarkMessage updates read, flagged, and junk status over IMAP.
func (c *Client) MarkMessage(ctx context.Context, request mail.MarkMessageRequest) (mail.MessageSummary, error) {
	if c.store == nil {
		return mail.MessageSummary{}, c.safeWriteUnavailableError()
	}
	if err := c.rejectUnconfirmedDraftMutation(ctx, request.Ref, request.AllowDraftMutation); err != nil {
		return mail.MessageSummary{}, err
	}

	target, err := c.resolveImapTargetForMutation(ctx, request.Ref)
	if err != nil {
		return mail.MessageSummary{}, err
	}

	imapOp := c.send.ImapClient()
	if imapOp == nil {
		return mail.MessageSummary{}, &transport.TransportError{
			Code:    transport.CodeIMAPMutationFailed,
			Message: "IMAP operator is not configured",
		}
	}

	var addFlags []string
	var removeFlags []string

	if request.Read != nil {
		if *request.Read {
			addFlags = append(addFlags, "\\Seen")
		} else {
			removeFlags = append(removeFlags, "\\Seen")
		}
	}
	if request.Flagged != nil {
		if *request.Flagged {
			addFlags = append(addFlags, "\\Flagged")
		} else {
			removeFlags = append(removeFlags, "\\Flagged")
		}
	}
	if request.Junk != nil {
		if *request.Junk {
			addFlags = append(addFlags, "$Junk")
			removeFlags = append(removeFlags, "$NotJunk")
		} else {
			addFlags = append(addFlags, "$NotJunk")
			removeFlags = append(removeFlags, "$Junk")
		}
	}

	ev, err := imapOp.SetFlags(ctx, target.cfg, target.imapMailbox, target.uid, target.uidvalidity, addFlags, removeFlags)
	if shouldRetryFlagMutation(err, ev) {
		retried, retryErr := c.resolveImapTargetForMutation(ctx, request.Ref)
		if retryErr != nil {
			return mail.MessageSummary{}, retryErr
		}
		ev, err = imapOp.SetFlags(ctx, retried.cfg, retried.imapMailbox, retried.uid, retried.uidvalidity, addFlags, removeFlags)
		target = retried
	}
	switch classifyFlagOutcome(ev, err, target) {
	case flagOutcomePropagate:
		return mail.MessageSummary{}, err
	case flagOutcomeForceUnknown:
		ev.Outcome, ev.FlagsState, ev.ActualFlags = transport.MutationOutcomeUnknown, transport.FlagObservationUnverified, nil
		err = &transport.MutationOutcomeError{
			Code: transport.CodeIMAPFlagsOutcomeUnknown, Evidence: ev, Err: err,
			Message: "IMAP STORE returned no complete flag observation for the resolved message; inspect server state before retrying",
		}
	}
	ev.DuplicateMatches = duplicateMatchEvidence(target.duplicateMatches)

	summary := target.summary
	if ev.FlagsState == transport.FlagObservationObserved {
		summary.Read, summary.Flagged, summary.Junk, summary.Deleted = false, false, false, false
		var notJunk bool
		for _, flag := range ev.ActualFlags {
			switch strings.ToLower(flag) {
			case "\\seen":
				summary.Read = true
			case "\\flagged":
				summary.Flagged = true
			case "$junk":
				summary.Junk = true
			case "$notjunk":
				notJunk = true
			case "\\deleted":
				summary.Deleted = true
			}
		}
		// RFC 9051 treats a conflicting pair as no definite classification.
		summary.Junk = summary.Junk && !notJunk
	}
	summary.ServerTruth = &mail.ServerMutationEvidence{
		OperationID:         ev.OperationID,
		Outcome:             mail.ServerMutationOutcome(ev.Outcome),
		SourceAccount:       ev.SourceAccount,
		Command:             ev.Command,
		ServerResponse:      ev.ServerResponse,
		Mailbox:             ev.Mailbox,
		UID:                 ev.UID,
		ExpectedUIDValidity: ev.ExpectedUIDValidity,
		UIDValidity:         ev.UIDValidity,
		DuplicateMatches:    ev.DuplicateMatches,
		FlagsState:          string(ev.FlagsState),
		ActualFlags:         append([]string(nil), ev.ActualFlags...),
		FlagsSource:         ev.FlagsSource,
	}
	summary.StalenessNote = "flags observed on IMAP server at command completion; concurrent clients may change them; local metadata updates on the next Mail.app sync"
	if ev.FlagsState != transport.FlagObservationObserved {
		summary.StalenessNote = "server flags are not verified; summary booleans retain local cached values"
	}
	return summary, err
}
