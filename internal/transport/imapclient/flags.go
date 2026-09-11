package imapclient

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"mailcli/internal/transport"
)

const (
	maxFlagCount         = 1024
	maxFlagResponseCount = 1024
	maxFlagResponseBytes = 4 << 20
)

type flagObservation struct {
	sequence uint32
	flags    []string
	seenUID  bool
	observed bool
	missing  bool
}

type flagCommandResult struct {
	status      string
	response    string
	observation flagObservation
}

func validateFlagChanges(addFlags, removeFlags []string) error {
	if len(addFlags)+len(removeFlags) == 0 || len(addFlags)+len(removeFlags) > maxFlagCount {
		return &transport.TransportError{Code: transport.CodeIMAPInvalidValue, Message: "IMAP flag changes require between 1 and 1024 flags"}
	}
	for _, flags := range [][]string{addFlags, removeFlags} {
		for _, flag := range flags {
			if !validFlagAtom(flag) || strings.EqualFold(flag, "\\Recent") {
				return &transport.TransportError{Code: transport.CodeIMAPInvalidValue, Message: "IMAP flag changes contain an invalid or non-writable flag"}
			}
		}
	}
	for _, flag := range addFlags {
		if containsFlag(removeFlags, flag) {
			return &transport.TransportError{Code: transport.CodeIMAPInvalidValue, Message: "the same IMAP flag cannot be added and removed"}
		}
	}
	return nil
}

func validFlagAtom(flag string) bool {
	flag = strings.TrimPrefix(flag, "\\")
	if flag == "" {
		return false
	}
	for index := range len(flag) {
		if flag[index] <= 0x20 || flag[index] >= 0x7f || strings.ContainsRune("(){%*\"\\]", rune(flag[index])) {
			return false
		}
	}
	return true
}

func parseFlagListValue(value string, allowWildcard bool) ([]string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != '(' || value[len(value)-1] != ')' {
		return nil, errors.New("FETCH FLAGS value is not a flag list")
	}
	flags := strings.Fields(value[1 : len(value)-1])
	if len(flags) > maxFlagCount {
		return nil, errors.New("FETCH FLAGS exceeds the flag count limit")
	}
	for _, flag := range flags {
		if !validFlagAtom(flag) && (!allowWildcard || flag != "\\*") {
			return nil, errors.New("FETCH FLAGS contains an invalid flag atom")
		}
	}
	return flags, nil
}

func containsFlag(flags []string, wanted string) bool {
	for _, flag := range flags {
		if strings.EqualFold(flag, wanted) {
			return true
		}
	}
	return false
}

func flagsMatchChanges(flags, addFlags, removeFlags []string) bool {
	for _, flag := range addFlags {
		if !containsFlag(flags, flag) {
			return false
		}
	}
	for _, flag := range removeFlags {
		if containsFlag(flags, flag) {
			return false
		}
	}
	return true
}

func (c *Client) setFlagsAndVerify(ctx context.Context, sess *session, ev transport.MutationEvidence, addFlags, removeFlags []string, permissions flagPermissions) (transport.MutationEvidence, error) {
	requested := flagChanges{add: addFlags, remove: removeFlags}
	ev, needed, err := c.prepareFlagChanges(ctx, sess, ev, requested, &permissions)
	if err != nil {
		return ev, err
	}
	phases := [2]flagChanges{{add: needed.add}, {remove: needed.remove}}
	if requested.touchesJunk() {
		phases[0], phases[1] = phases[1], phases[0]
	}
	for _, phase := range phases {
		if len(phase.add)+len(phase.remove) == 0 {
			continue
		}
		if unsupported := permissions.unsupported(phase); unsupported != "" {
			return unsupportedFlagResult(ev, unsupported)
		}
		ev, err = c.performFlagChange(ctx, sess, ev, phase, &permissions)
		if err != nil {
			return ev, err
		}
	}
	return completeFlagResult(ev, addFlags, removeFlags)
}

func (c *Client) prepareFlagChanges(ctx context.Context, sess *session, ev transport.MutationEvidence, requested flagChanges, permissions *flagPermissions) (transport.MutationEvidence, flagChanges, error) {
	needed := requested
	if requested.touchesJunk() || permissions.unsupported(requested) != "" {
		ev.FlagsSource = "FETCH"
		result, err := c.fetchFlagResult(ctx, sess, ev.UID, ev.UIDValidity, permissions)
		ev.ServerResponse = result.response
		if err != nil {
			ev, err = flagPreflightFailure(ev, err)
			return ev, needed, err
		}
		ev = withFlagObservation(ev, result.observation)
		if ev.FlagsState != transport.FlagObservationObserved {
			code := transport.CodeIMAPResponseMalformed
			if ev.FlagsState == transport.FlagObservationMissing {
				code = transport.CodeIMAPMessageNotFound
			}
			ev, err = flagPreflightFailure(ev, &transport.TransportError{Code: code, Message: "target flags unavailable before STORE"})
			return ev, needed, err
		}
		needed = requested.pending(ev.ActualFlags)
	}
	if unsupported := permissions.unsupported(needed); unsupported != "" {
		updated, err := unsupportedFlagResult(ev, unsupported)
		return updated, needed, err
	}
	return ev, needed, nil
}

func (c *Client) performFlagChange(ctx context.Context, sess *session, ev transport.MutationEvidence, phase flagChanges, permissions *flagPermissions) (transport.MutationEvidence, error) {
	previouslyCompleted := ev.Outcome == transport.MutationOutcomePartial
	result, err := c.storeFlagChange(ctx, sess, &ev, phase, permissions)
	if err != nil {
		if result.status == "NO" || result.status == "BAD" {
			return c.rejectedFlagResult(ctx, sess, ev, previouslyCompleted, permissions, err)
		}
		if ev.Outcome == transport.MutationOutcomeNotStarted {
			return flagPreflightFailure(ev, err)
		}
		if ev.Outcome == transport.MutationOutcomePartial {
			return ev, &transport.MutationOutcomeError{Code: transport.CodeIMAPFlagsPartial, Evidence: ev, Err: err, Message: "a verified flag phase completed; the next phase was not dispatched"}
		}
		return unknownFlagResult(ev, err)
	}
	ev, err = c.verifyFlagResult(ctx, sess, ev, result.observation, phase.add, phase.remove, permissions)
	if err != nil {
		return ev, err
	}
	if unsupported := permissions.unsupported(phase); unsupported != "" {
		ev.Outcome = transport.MutationOutcomeUnknown
		return unsupportedFlagResult(ev, unsupported)
	}
	ev.Outcome = transport.MutationOutcomePartial
	return ev, nil
}

func (c *Client) storeFlagChange(ctx context.Context, sess *session, ev *transport.MutationEvidence, phase flagChanges, permissions *flagPermissions) (flagCommandResult, error) {
	if err := c.setDeadline(ctx, sess); err != nil {
		return flagCommandResult{}, err
	}
	operation, flags := "+FLAGS", phase.add
	if len(phase.remove) > 0 {
		operation, flags = "-FLAGS", phase.remove
	}
	tag := sess.nextTag()
	ev.Outcome, ev.FlagsSource = transport.MutationOutcomeAttempted, "STORE"
	ev.FlagsState, ev.ActualFlags = transport.FlagObservationUnverified, nil
	command := fmt.Sprintf("%s UID STORE %d %s (%s)", tag, ev.UID, operation, strings.Join(flags, " "))
	if err := c.writeLine(sess, command); err != nil {
		return flagCommandResult{}, err
	}
	result, err := c.readFlagResult(ctx, sess, tag, ev.UID, ev.UIDValidity, permissions)
	ev.ServerResponse = result.response
	return result, err
}

func (c *Client) rejectedFlagResult(ctx context.Context, sess *session, ev transport.MutationEvidence, previouslyCompleted bool, permissions *flagPermissions, cause error) (transport.MutationEvidence, error) {
	code := transport.CodeIMAPMutationFailed
	ev.Outcome, ev.FlagsSource = transport.MutationOutcomeRejected, "FETCH"
	if previouslyCompleted {
		code, ev.Outcome = transport.CodeIMAPFlagsPartial, transport.MutationOutcomePartial
	}
	result, err := c.fetchFlagResult(ctx, sess, ev.UID, ev.UIDValidity, permissions)
	if err == nil {
		ev = withFlagObservation(ev, result.observation)
	}
	return ev, &transport.MutationOutcomeError{
		Code: code, Evidence: ev, Err: errors.Join(cause, err),
		Message: "IMAP STORE was rejected; inspect the retained flag state before another mutation",
	}
}

func flagPreflightFailure(ev transport.MutationEvidence, cause error) (transport.MutationEvidence, error) {
	code := transport.ErrorCode(cause)
	if code == "" {
		code = transport.CodeIMAPFetchFailed
	}
	return ev, &transport.MutationOutcomeError{Code: code, Evidence: ev, Err: cause, Message: "IMAP flag operation stopped before STORE"}
}

func unsupportedFlagResult(ev transport.MutationEvidence, flag string) (transport.MutationEvidence, error) {
	return ev, &transport.MutationOutcomeError{
		Code: transport.CodeIMAPFlagsUnsupported, Evidence: ev,
		Message: fmt.Sprintf("IMAP flag %q cannot be changed permanently under the current PERMANENTFLAGS; inspect the retained state and mailbox permissions", flag),
	}
}

func (c *Client) fetchFlagResult(ctx context.Context, sess *session, uid, uidvalidity uint32, permissions *flagPermissions) (flagCommandResult, error) {
	if err := c.setDeadline(ctx, sess); err != nil {
		return flagCommandResult{}, err
	}
	tag := sess.nextTag()
	if err := c.writeLine(sess, fmt.Sprintf("%s UID FETCH %d (UID FLAGS)", tag, uid)); err != nil {
		return flagCommandResult{}, err
	}
	return c.readFlagResult(ctx, sess, tag, uid, uidvalidity, permissions)
}

func withFlagObservation(ev transport.MutationEvidence, observation flagObservation) transport.MutationEvidence {
	ev.ActualFlags, ev.FlagsState = nil, transport.FlagObservationUnverified
	if observation.missing || !observation.seenUID {
		ev.FlagsState = transport.FlagObservationMissing
	} else if observation.observed {
		ev.ActualFlags = append([]string{}, observation.flags...)
		ev.FlagsState = transport.FlagObservationObserved
	}
	return ev
}

func (c *Client) verifyFlagResult(ctx context.Context, sess *session, ev transport.MutationEvidence, observation flagObservation, addFlags, removeFlags []string, permissions *flagPermissions) (transport.MutationEvidence, error) {
	ev.FlagsSource = "STORE"
	if observation.missing {
		return missingFlagResult(ev)
	}
	if !observation.observed {
		ev.FlagsSource = "FETCH"
		result, err := c.fetchFlagResult(ctx, sess, ev.UID, ev.UIDValidity, permissions)
		if err != nil {
			return unknownFlagResult(ev, err)
		}
		observation = result.observation
	}
	ev = withFlagObservation(ev, observation)
	if ev.FlagsState == transport.FlagObservationMissing {
		return missingFlagResult(ev)
	}
	if ev.FlagsState != transport.FlagObservationObserved {
		return unknownFlagResult(ev, errors.New("target UID was returned without complete FLAGS proof"))
	}
	return completeFlagResult(ev, addFlags, removeFlags)
}

func completeFlagResult(ev transport.MutationEvidence, addFlags, removeFlags []string) (transport.MutationEvidence, error) {
	if !flagsMatchChanges(ev.ActualFlags, addFlags, removeFlags) {
		ev.Outcome = transport.MutationOutcomeObserved
		return ev, &transport.MutationOutcomeError{
			Code: transport.CodeIMAPFlagsMismatch, Evidence: ev,
			Message: fmt.Sprintf("IMAP UID %d flags %q do not match requested additions %q and removals %q; concurrent changes may have occurred; inspect the observed state before retrying", ev.UID, ev.ActualFlags, addFlags, removeFlags),
		}
	}
	ev.Outcome = transport.MutationOutcomeCompleted
	return ev, nil
}

func unknownFlagResult(ev transport.MutationEvidence, cause error) (transport.MutationEvidence, error) {
	ev.Outcome = transport.MutationOutcomeUnknown
	ev.FlagsState = transport.FlagObservationUnverified
	ev.ActualFlags = nil
	return ev, &transport.MutationOutcomeError{
		Code: transport.CodeIMAPFlagsOutcomeUnknown, Evidence: ev, Err: cause,
		Message: fmt.Sprintf("IMAP flag outcome for UID %d is unknown after STORE; observe mailbox %q before retrying", ev.UID, ev.Mailbox),
	}
}

func missingFlagResult(ev transport.MutationEvidence) (transport.MutationEvidence, error) {
	ev.Outcome = transport.MutationOutcomeUnknown
	ev.FlagsState = transport.FlagObservationMissing
	ev.ActualFlags = nil
	return ev, &transport.MutationOutcomeError{
		Code: transport.CodeIMAPMessageNotFound, Evidence: ev,
		Message: fmt.Sprintf("IMAP UID %d is missing after STORE in mailbox %q; no resulting flags can be confirmed", ev.UID, ev.Mailbox),
	}
}

// UID command responses contain sequence numbers, not UIDs, in their prefix.
// Accept proof only after parsing the UID attribute, and consume subsequent
// updates through tagged completion so an expunged or superseded observation
// cannot be reported as current. Verification never sends another STORE.
func (c *Client) readFlagResult(ctx context.Context, sess *session, tag string, uid, uidvalidity uint32, permissions *flagPermissions) (flagCommandResult, error) {
	var result flagCommandResult
	remaining := int64(maxFlagResponseBytes)
	for range maxFlagResponseCount {
		if err := ctx.Err(); err != nil {
			sess.dirty = true
			return result, err
		}
		line, literals, err := c.readLogicalLineWithLiterals(sess, maxIMAPResponseLineBytes, remaining, maxFetchLiteralCount)
		if err != nil {
			return result, wrapIOError(ctx, err, transport.CodeIMAPResponseMalformed, "IMAP flag response read")
		}
		remaining -= int64(len(line) + 2)
		for _, literal := range literals {
			remaining -= int64(len(literal))
		}
		if remaining < 0 {
			break
		}
		if err := flagResponseValidity(line, tag, uidvalidity, permissions); err != nil {
			sess.dirty = true
			return result, err
		}
		if strings.HasPrefix(line, tag+" ") {
			result.status = strings.ToUpper(parseStatus(line, tag))
			result.response = strings.TrimPrefix(line, tag+" ")
			if result.status == "OK" {
				return result, nil
			}
			if result.status != "NO" && result.status != "BAD" {
				sess.dirty = true
			}
			return result, &transport.TransportError{Code: transport.CodeIMAPMutationFailed, Message: "IMAP flag command failed: " + result.response}
		}
		if err := result.observation.consume(line, literals, uid); err != nil {
			sess.dirty = true
			return result, &transport.TransportError{Code: transport.CodeIMAPResponseMalformed, Message: "IMAP flag observation malformed", Err: err}
		}
	}
	sess.dirty = true
	return result, &transport.TransportError{Code: transport.CodeIMAPResponseMalformed, Message: "IMAP flag verification exceeded its response budget"}
}

func flagResponseValidity(line, tag string, expected uint32, permissions *flagPermissions) error {
	code, err := flagResponseCode(line, tag)
	if err != nil {
		return err
	}
	if err := permissions.observeCode(code); err != nil {
		return err
	}
	values := strings.Fields(code)
	if len(values) == 0 || !strings.EqualFold(values[0], "UIDVALIDITY") {
		return nil
	}
	if len(values) != 2 {
		return errors.New("IMAP flag response has malformed UIDVALIDITY")
	}
	observed, err := parsePositiveUIDValue(values[1])
	if err != nil {
		return err
	}
	return checkUIDValidity(expected, observed)
}

func (observation *flagObservation) consume(line string, literals [][]byte, uid uint32) error {
	if isFetchResponseCandidate(line) {
		response, err := parseFetchResponse(line, literals)
		if err != nil {
			return err
		}
		if response.uidPresent && response.uid == uid {
			if observation.missing {
				return errors.New("target UID reappeared after EXPUNGE")
			}
			if observation.seenUID && response.sequence != observation.sequence {
				return errors.New("target UID changed sequence without matching EXPUNGE evidence")
			}
			observation.sequence, observation.seenUID = response.sequence, true
			if response.flagsPresent {
				observation.flags, observation.observed = response.flags, true
			}
			return nil
		}
		if response.uidPresent && response.sequence == observation.sequence && !observation.missing {
			return errors.New("target sequence was reassigned to an unrelated UID")
		}
		if response.sequence == observation.sequence && response.flagsPresent {
			// An uncorrelated later update invalidates the snapshot. A single
			// targeted FETCH may restore proof; never infer a UID from a prefix.
			observation.observed = false
		}
		return nil
	}
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "*" || !strings.EqualFold(fields[2], "EXPUNGE") {
		return nil
	}
	sequence, err := parsePositiveUIDValue(fields[1])
	if err != nil || len(fields) != 3 {
		return errors.New("invalid EXPUNGE response during flag verification")
	}
	if sequence == observation.sequence {
		observation.missing, observation.observed = true, false
	} else if sequence < observation.sequence {
		observation.sequence--
	}
	return nil
}
