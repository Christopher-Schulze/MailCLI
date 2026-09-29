package imapclient

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"mailcli/internal/transport"
)

// SetFlags adds and removes IMAP flags on a message.
func (c *Client) SetFlags(ctx context.Context, cfg transport.ImapConfig, mailbox string, uid uint32, expectedUIDValidity uint32, addFlags, removeFlags []string) (transport.MutationEvidence, error) {
	ev := transport.MutationEvidence{
		OperationID: transport.MutationOperationID("STORE", cfg.Username, mailbox, uid, expectedUIDValidity,
			"+FLAGS "+strings.Join(addFlags, " ")+" -FLAGS "+strings.Join(removeFlags, " ")),
		Outcome: transport.MutationOutcomeNotStarted, SourceAccount: cfg.Username,
		Command: "STORE", Mailbox: mailbox, UID: uid, ExpectedUIDValidity: expectedUIDValidity,
		FlagsState: transport.FlagObservationUnverified,
	}
	if err := validateMessageUID(uid); err != nil {
		return ev, err
	}
	if err := validateFlagChanges(addFlags, removeFlags); err != nil {
		return ev, err
	}
	ps, release, err := c.acquireMutation(ctx, cfg)
	if err != nil {
		return ev, err
	}
	defer release()

	info, err := c.ensureSelectedFresh(ctx, ps, mailbox)
	if err != nil {
		return ev, err
	}
	if err := checkUIDValidity(expectedUIDValidity, info.uidvalidity); err != nil {
		return ev, err
	}

	ev.UIDValidity = info.uidvalidity
	return c.setFlagsAndVerify(ctx, ps.sess, ev, addFlags, removeFlags, info.permissions)
}

// checkUIDValidity rejects an identity-sensitive operation before it runs when
// the mailbox was rebuilt or UIDVALIDITY is unknown: the stored UID may
// address a different message.
func checkUIDValidity(expected, observed uint32) error {
	if expected == 0 || observed == 0 {
		return &transport.TransportError{
			Code: transport.CodeIMAPUIDValidityUnknown,
			Message: fmt.Sprintf(
				"mailbox UIDVALIDITY is unknown (expected %d, observed %d); refusing the operation; refresh mailbox state and rerun the command",
				expected, observed,
			),
		}
	}
	if expected != 0 && observed != 0 && expected != observed {
		return &transport.TransportError{
			Code: "mailbox_uidvalidity_changed",
			Message: fmt.Sprintf(
				"mailbox was rebuilt between resolution and mutation (UIDVALIDITY %d -> %d); message moved or mailbox rebuilt; rerun the command",
				expected, observed,
			),
		}
	}
	return nil
}

func validateMessageUID(uid uint32) error {
	if uid != 0 {
		return nil
	}
	return &transport.TransportError{
		Code:    transport.CodeIMAPMessageUIDUnknown,
		Message: "message UID is unresolved; refusing the IMAP operation",
	}
}

func (c *Client) doCommandResponse(ctx context.Context, sess *session, cmd string) (string, string, error) {
	separator := strings.IndexByte(cmd, ' ')
	if separator <= 0 {
		return "", "", &transport.TransportError{
			Code:    transport.CodeIMAPInvalidValue,
			Message: "IMAP command has no tag separator",
		}
	}
	tag := cmd[:separator]
	if err := c.setDeadline(ctx, sess); err != nil {
		return "", "", wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP command deadline")
	}
	if err := c.writeLine(sess, cmd); err != nil {
		return "", "", wrapIOError(ctx, err, transport.CodeIMAPMutationFailed, "IMAP command write")
	}
	status, text, err := c.readFinal(ctx, sess, tag)
	if err != nil {
		return "", "", err
	}
	if status == "OK" {
		return status, text, nil
	}
	return status, text, &transport.TransportError{
		Code:    transport.CodeIMAPMutationFailed,
		Message: "IMAP command failed: " + status + " " + text,
	}
}

func (c *Client) CopyMessage(ctx context.Context, cfg transport.ImapConfig, srcMailbox string, uid uint32, expectedUIDValidity uint32, dstMailbox string) (transport.MutationEvidence, error) {
	ev, _, err := c.copyMessage(ctx, cfg, srcMailbox, uid, expectedUIDValidity, dstMailbox, "")
	return ev, err
}

// CopyMessageIfAbsent implements transport.GuardedCopier.
func (c *Client) CopyMessageIfAbsent(ctx context.Context, cfg transport.ImapConfig, srcMailbox string, uid uint32, expectedUIDValidity uint32, dstMailbox string, messageID string) (transport.MutationEvidence, transport.DestinationMatch, error) {
	return c.copyMessage(ctx, cfg, srcMailbox, uid, expectedUIDValidity, dstMailbox, messageID)
}

// destinationMatch looks for messageID in the destination on the mutation
// session; a message that is not there is not an error.
func (c *Client) destinationMatch(ctx context.Context, ps *pooledSession, dstMailbox, messageID string) (transport.DestinationMatch, error) {
	normalized, err := normalizeMessageID(messageID)
	if err != nil {
		return transport.DestinationMatch{}, err
	}
	uid, validity, count, err := c.searchUIDOnSession(ctx, ps, dstMailbox, messageID, normalized)
	if err != nil {
		if transport.ErrorCode(err) == transport.CodeIMAPMessageNotFound {
			return transport.DestinationMatch{}, nil
		}
		return transport.DestinationMatch{}, err
	}
	return transport.DestinationMatch{UID: uid, UIDValidity: validity, Count: count}, nil
}

func (c *Client) copyMessage(ctx context.Context, cfg transport.ImapConfig, srcMailbox string, uid uint32, expectedUIDValidity uint32, dstMailbox string, guardMessageID string) (transport.MutationEvidence, transport.DestinationMatch, error) {
	ev := transport.MutationEvidence{
		OperationID:         transport.MutationOperationID("COPY", cfg.Username, srcMailbox, uid, expectedUIDValidity, dstMailbox),
		Outcome:             transport.MutationOutcomeNotStarted,
		SourceAccount:       cfg.Username,
		Command:             "COPY",
		Mailbox:             srcMailbox,
		TargetMailbox:       dstMailbox,
		UID:                 uid,
		ExpectedUIDValidity: expectedUIDValidity,
	}
	var match transport.DestinationMatch
	if err := validateMessageUID(uid); err != nil {
		return ev, match, err
	}
	ps, release, err := c.acquireMutation(ctx, cfg)
	if err != nil {
		return ev, match, err
	}
	defer release()
	if guardMessageID != "" {
		match, err = c.destinationMatch(ctx, ps, dstMailbox, guardMessageID)
		if err != nil {
			return ev, transport.DestinationMatch{}, &transport.CopyGuardError{Err: err}
		}
		if match.Count > 0 {
			return ev, match, nil
		}
	}
	info, err := c.ensureSelectedFresh(ctx, ps, srcMailbox)
	if err != nil {
		return ev, match, err
	}
	if err := checkUIDValidity(expectedUIDValidity, info.uidvalidity); err != nil {
		return ev, match, err
	}
	ev.UIDValidity = info.uidvalidity

	quotedDestination, err := safeQuoteIMAP(dstMailbox)
	if err != nil {
		return ev, match, err
	}
	cmd := fmt.Sprintf("%s UID COPY %d %s", ps.sess.nextTag(), uid, quotedDestination)
	status, text, responseCodes, dispatched, err := c.doTransferCommandResponse(ctx, ps.sess, cmd, "COPY")
	if err != nil {
		failed, failure := transferCommandError(ev, status, text, dispatched, err)
		return failed, match, failure
	}
	ev.ServerResponse = joinIMAPResponse(status, text)
	if err := applyCopyUIDEvidence(&ev, responseCodes, uid); err != nil {
		ev.Outcome = transport.MutationOutcomeUnknown
		return ev, match, copyOutcomeUnknown(ev, err)
	}
	ev.Outcome = transport.MutationOutcomeCompleted
	return ev, match, nil
}

func (c *Client) doTransferCommandResponse(ctx context.Context, sess *session, cmd, operation string) (string, string, []string, bool, error) {
	separator := strings.IndexByte(cmd, ' ')
	if separator <= 0 {
		return "", "", nil, false, &transport.TransportError{
			Code:    transport.CodeIMAPInvalidValue,
			Message: "IMAP " + operation + " command has no tag separator",
		}
	}
	tag := cmd[:separator]
	if err := c.setDeadline(ctx, sess); err != nil {
		return "", "", nil, false, wrapIOError(ctx, err, transport.CodeIMAPTimeout, "IMAP "+operation+" deadline")
	}
	if err := c.writeLine(sess, cmd); err != nil {
		if transport.ErrorCode(err) == transport.CodeIMAPInvalidValue {
			return "", "", nil, false, err
		}
		return "", "", nil, true, wrapIOError(ctx, err, transport.CodeIMAPMutationFailed, "IMAP "+operation+" write")
	}
	status, text, responseCodes, err := c.readFinalWithCodes(
		ctx, sess, tag, transport.CodeIMAPMutationFailed, "IMAP "+operation+" final response read",
	)
	if err != nil {
		return status, text, responseCodes, true, err
	}
	if strings.EqualFold(status, "OK") {
		return status, text, responseCodes, true, nil
	}
	return status, text, responseCodes, true, &transport.TransportError{
		Code:    transport.CodeIMAPMutationFailed,
		Message: "IMAP " + operation + " failed: " + status + " " + text,
	}
}

func transferCommandError(
	evidence transport.MutationEvidence,
	status string,
	text string,
	dispatched bool,
	cause error,
) (transport.MutationEvidence, error) {
	if status != "" {
		evidence.ServerResponse = joinIMAPResponse(status, text)
		if strings.EqualFold(status, "NO") || strings.EqualFold(status, "BAD") {
			evidence.Outcome = transport.MutationOutcomeRejected
			return evidence, cause
		}
	}
	if !dispatched {
		evidence.Outcome = transport.MutationOutcomeNotStarted
		code := transport.ErrorCode(cause)
		if code == "" {
			code = transport.CodeIMAPMutationFailed
		}
		return evidence, &transport.MutationOutcomeError{
			Code:     code,
			Message:  "IMAP " + evidence.Command + " was not dispatched; no server-side effect occurred",
			Evidence: evidence,
			Err:      cause,
		}
	}
	evidence.Outcome = transport.MutationOutcomeUnknown
	if evidence.Command == "MOVE" {
		return evidence, moveOutcomeUnknown(evidence, cause)
	}
	return evidence, copyOutcomeUnknown(evidence, cause)
}

func parseCopyUIDResponse(responseCode string, sourceUID uint32) (uint32, uint32, error) {
	fields := strings.Fields(responseCode)
	if len(fields) != 4 || !strings.EqualFold(fields[0], "COPYUID") {
		return 0, 0, fmt.Errorf("malformed IMAP COPYUID response code %q", responseCode)
	}
	uidValidity, err := parsePositiveUIDValue(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("malformed IMAP COPYUID UIDVALIDITY: %w", err)
	}
	returnedSourceUID, err := parsePositiveUIDValue(fields[2])
	if err != nil {
		return 0, 0, fmt.Errorf("malformed IMAP COPYUID source UID: %w", err)
	}
	destinationUID, err := parsePositiveUIDValue(fields[3])
	if err != nil {
		return 0, 0, fmt.Errorf("malformed IMAP COPYUID destination UID: %w", err)
	}
	if returnedSourceUID != sourceUID {
		return 0, 0, fmt.Errorf(
			"IMAP COPYUID source UID %d does not match requested UID %d",
			returnedSourceUID, sourceUID,
		)
	}
	return uidValidity, destinationUID, nil
}

func applyCopyUIDEvidence(evidence *transport.MutationEvidence, responseCodes []string, sourceUID uint32) error {
	var responseCode string
	for _, candidate := range responseCodes {
		fields := strings.Fields(candidate)
		if len(fields) == 0 || !strings.EqualFold(fields[0], "COPYUID") {
			continue
		}
		if responseCode != "" {
			return fmt.Errorf("IMAP COPY returned multiple COPYUID response codes")
		}
		responseCode = candidate
	}
	if responseCode == "" {
		return nil
	}
	uidValidity, destinationUID, err := parseCopyUIDResponse(responseCode, sourceUID)
	evidence.CopyUIDResponse = responseCode
	if err != nil {
		return err
	}
	evidence.CopyUIDValidity = uidValidity
	evidence.CopySourceUID = sourceUID
	evidence.CopyDestinationUID = destinationUID
	evidence.DestinationUIDValidity = uidValidity
	evidence.DestinationUID = destinationUID
	return nil
}

func parsePositiveUIDValue(value string) (uint32, error) {
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("UID value %q is not a positive uint32", value)
	}
	return uint32(parsed), nil
}

func joinIMAPResponse(status, text string) string {
	if text == "" {
		return status
	}
	return status + " " + text
}

func copyOutcomeUnknown(ev transport.MutationEvidence, cause error) error {
	return &transport.MutationOutcomeError{
		Code: transport.CodeIMAPCopyOutcomeUnknown,
		Message: fmt.Sprintf(
			"IMAP COPY outcome is unknown for operation %s; reconcile the destination before retrying",
			ev.OperationID,
		),
		Evidence: ev,
		Err:      cause,
	}
}

func moveOutcomeUnknown(ev transport.MutationEvidence, cause error) error {
	return &transport.MutationOutcomeError{
		Code: transport.CodeIMAPMoveOutcomeUnknown,
		Message: fmt.Sprintf(
			"IMAP MOVE outcome is unknown for operation %s; reconcile source and destination before retrying",
			ev.OperationID,
		),
		Evidence: ev,
		Err:      cause,
	}
}

func appendMutationEffect(evidence *transport.MutationEvidence, effect string) {
	for _, existing := range evidence.CompletedEffects {
		if existing == effect {
			return
		}
	}
	evidence.CompletedEffects = append(evidence.CompletedEffects, effect)
}

// MoveMessage moves a message by UID to dstMailbox using native UID MOVE with COPY+EXPUNGE fallback.
func (c *Client) MoveMessage(ctx context.Context, cfg transport.ImapConfig, srcMailbox string, uid uint32, expectedUIDValidity uint32, dstMailbox string) (transport.MutationEvidence, error) {
	evidence := transport.MutationEvidence{
		OperationID:         transport.MutationOperationID("MOVE", cfg.Username, srcMailbox, uid, expectedUIDValidity, dstMailbox),
		Outcome:             transport.MutationOutcomeNotStarted,
		SourceAccount:       cfg.Username,
		Command:             "MOVE",
		Mailbox:             srcMailbox,
		TargetMailbox:       dstMailbox,
		UID:                 uid,
		ExpectedUIDValidity: expectedUIDValidity,
	}
	if err := validateMessageUID(uid); err != nil {
		return evidence, err
	}
	ps, release, err := c.acquireMutation(ctx, cfg)
	if err != nil {
		return evidence, err
	}
	defer release()
	return c.moveMessage(ctx, ps, srcMailbox, uid, expectedUIDValidity, dstMailbox)
}

func (c *Client) moveMessage(ctx context.Context, ps *pooledSession, srcMailbox string, uid uint32, expectedUIDValidity uint32, dstMailbox string) (transport.MutationEvidence, error) {
	ev := transport.MutationEvidence{
		OperationID:         transport.MutationOperationID("MOVE", ps.key.username, srcMailbox, uid, expectedUIDValidity, dstMailbox),
		Outcome:             transport.MutationOutcomeNotStarted,
		SourceAccount:       ps.key.username,
		Command:             "MOVE",
		Mailbox:             srcMailbox,
		TargetMailbox:       dstMailbox,
		UID:                 uid,
		ExpectedUIDValidity: expectedUIDValidity,
	}
	if err := validateMessageUID(uid); err != nil {
		return ev, err
	}
	info, err := c.ensureSelectedFresh(ctx, ps, srcMailbox)
	if err != nil {
		return ev, err
	}
	if err := checkUIDValidity(expectedUIDValidity, info.uidvalidity); err != nil {
		return ev, err
	}
	ev.UIDValidity = info.uidvalidity
	sess := ps.sess
	quotedDestination, err := safeQuoteIMAP(dstMailbox)
	if err != nil {
		return ev, err
	}

	tag := sess.nextTag()
	cmd := fmt.Sprintf("%s UID MOVE %d %s", tag, uid, quotedDestination)
	ev.Outcome = transport.MutationOutcomeAttempted
	status, text, _, dispatched, err := c.doTransferCommandResponse(ctx, sess, cmd, "MOVE")
	if err != nil {
		if !strings.EqualFold(status, "NO") && !strings.EqualFold(status, "BAD") {
			return transferCommandError(ev, status, text, dispatched, err)
		}
	}
	if strings.EqualFold(status, "OK") {
		ev.ServerResponse = joinIMAPResponse(status, text)
		ev.Outcome = transport.MutationOutcomeCompleted
		ev.CompletedEffects = []string{"move"}
		return ev, nil
	}
	if status != "NO" && status != "BAD" {
		ev.ServerResponse = joinIMAPResponse(status, text)
		ev.Outcome = transport.MutationOutcomeRejected
		return ev, &transport.TransportError{
			Code:    transport.CodeIMAPMutationFailed,
			Message: "IMAP MOVE failed without fallback permission: " + status + " " + text,
		}
	}

	// Fallback for servers without UID MOVE: COPY + STORE \\Deleted, then
	// prefer UID EXPUNGE. If it is unavailable, leave cleanup deferred so an
	// unscoped EXPUNGE cannot remove another client's deleted message.
	copyCmd := fmt.Sprintf("%s UID COPY %d %s", sess.nextTag(), uid, quotedDestination)
	copyStatus, copyText, responseCodes, copyDispatched, err := c.doTransferCommandResponse(ctx, sess, copyCmd, "COPY")
	if err != nil {
		return transferCommandError(ev, copyStatus, copyText, copyDispatched, err)
	}
	ev.ServerResponse = joinIMAPResponse(copyStatus, copyText)
	appendMutationEffect(&ev, "copy")
	if err := applyCopyUIDEvidence(&ev, responseCodes, uid); err != nil {
		ev.Outcome = transport.MutationOutcomeUnknown
		return ev, moveOutcomeUnknown(ev, err)
	}

	copyResponse := ev.ServerResponse
	// Track the flag phase independently; COPY is already a proven effect.
	ev.Outcome, ev.FlagsState, ev.ServerResponse = transport.MutationOutcomeNotStarted, transport.FlagObservationUnverified, ""
	ev, err = c.setFlagsAndVerify(ctx, sess, ev, []string{"\\Deleted"}, nil, info.permissions)
	flagResponse := ev.ServerResponse
	ev.ServerResponse, ev.Outcome = copyResponse, transport.MutationOutcomePartial
	if flagResponse != "" {
		ev.ServerResponse += "; source flag response: " + flagResponse
	}
	if err != nil {
		return ev, moveOutcomeUnknown(ev, err)
	}
	appendMutationEffect(&ev, "source_flag")

	uidExpungeCmd := fmt.Sprintf("%s UID EXPUNGE %d", sess.nextTag(), uid)
	uidExpungeStatus, _, uidExpungeErr := c.doCommandResponse(ctx, sess, uidExpungeCmd)
	if uidExpungeErr == nil {
		ev.ServerResponse += " (fallback UID EXPUNGE)"
		ev.Outcome = transport.MutationOutcomeCompleted
		ev.ExpungeBranch = "uid_expunge"
		appendMutationEffect(&ev, "uid_expunge")
		return ev, nil
	}
	if !strings.EqualFold(uidExpungeStatus, "NO") && !strings.EqualFold(uidExpungeStatus, "BAD") {
		ev.Outcome = transport.MutationOutcomeUnknown
		return ev, moveOutcomeUnknown(ev, uidExpungeErr)
	}

	deletedUIDs, err := c.doUIDSearchDeleted(ctx, sess, sess.nextTag())
	if err != nil {
		ev.Outcome = transport.MutationOutcomeUnknown
		return ev, moveOutcomeUnknown(ev, err)
	}
	foreignDeleted := 0
	for _, deletedUID := range deletedUIDs {
		if deletedUID != uid {
			foreignDeleted++
		}
	}
	ev.ServerResponse = fmt.Sprintf(
		"%s (moved + flagged deleted; expunge deferred because UID EXPUNGE is unsupported; other deleted messages present: %d)",
		ev.ServerResponse, foreignDeleted,
	)
	ev.Outcome = transport.MutationOutcomeCompleted
	ev.ExpungeBranch = "deferred"
	ev.ForeignDeletedCount = foreignDeleted
	appendMutationEffect(&ev, "cleanup_deferred")
	return ev, nil
}

// DeleteMessage moves a message by UID to the Trash mailbox discovered via special-use flags.
func (c *Client) DeleteMessage(ctx context.Context, cfg transport.ImapConfig, srcMailbox string, uid uint32, expectedUIDValidity uint32) (transport.MutationEvidence, error) {
	if err := validateMessageUID(uid); err != nil {
		return transport.MutationEvidence{}, err
	}
	ps, release, err := c.acquireMutation(ctx, cfg)
	if err != nil {
		return transport.MutationEvidence{}, err
	}
	defer release()

	mboxes, err := c.listMailboxes(ctx, ps)
	if err != nil {
		return transport.MutationEvidence{}, err
	}

	trashBox, err := transport.ResolveTrashMailbox(mboxes)
	if err != nil {
		return transport.MutationEvidence{}, err
	}
	if strings.EqualFold(srcMailbox, trashBox) {
		return transport.MutationEvidence{}, &transport.TransportError{
			Code:    transport.CodeMessageAlreadyTrashed,
			Message: "message is already in the Trash mailbox",
		}
	}

	ev, err := c.moveMessage(ctx, ps, srcMailbox, uid, expectedUIDValidity, trashBox)
	if err != nil {
		return ev, err
	}
	ev.Command = "DELETE"
	return ev, nil
}
