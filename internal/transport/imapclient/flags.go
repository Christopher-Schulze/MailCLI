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

func parseFetchFlagsValue(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != '(' || value[len(value)-1] != ')' {
		return nil, errors.New("FETCH FLAGS value is not a flag list")
	}
	flags := strings.Fields(value[1 : len(value)-1])
	if len(flags) > maxFlagCount {
		return nil, errors.New("FETCH FLAGS exceeds the flag count limit")
	}
	for _, flag := range flags {
		if !validFlagAtom(flag) {
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

func (c *Client) setFlagsAndVerify(ctx context.Context, sess *session, ev transport.MutationEvidence, addFlags, removeFlags []string) (transport.MutationEvidence, error) {
	var result flagCommandResult
	for index, flags := range [][]string{addFlags, removeFlags} {
		if len(flags) == 0 {
			continue
		}
		if err := c.setDeadline(ctx, sess); err != nil {
			if ev.Outcome == transport.MutationOutcomeNotStarted {
				return ev, err
			}
			return unknownFlagResult(ev, err)
		}
		operation := "+FLAGS"
		if index == 1 {
			operation = "-FLAGS"
		}
		tag := sess.nextTag()
		previouslyAttempted := ev.Outcome != transport.MutationOutcomeNotStarted
		ev.Outcome = transport.MutationOutcomeAttempted
		ev.FlagsSource = "STORE"
		command := fmt.Sprintf("%s UID STORE %d %s (%s)", tag, ev.UID, operation, strings.Join(flags, " "))
		if err := c.writeLine(sess, command); err != nil {
			return unknownFlagResult(ev, err)
		}
		var err error
		result, err = c.readFlagResult(ctx, sess, tag, ev.UID, ev.UIDValidity)
		ev.ServerResponse = result.response
		if err != nil {
			if !previouslyAttempted && (result.status == "NO" || result.status == "BAD") {
				ev.Outcome = transport.MutationOutcomeRejected
				return ev, err
			}
			return unknownFlagResult(ev, err)
		}
		if result.observation.missing {
			ev.FlagsSource = "STORE"
			return missingFlagResult(ev)
		}
	}
	return c.verifyFlagResult(ctx, sess, ev, result.observation, addFlags, removeFlags)
}

func (c *Client) verifyFlagResult(ctx context.Context, sess *session, ev transport.MutationEvidence, observation flagObservation, addFlags, removeFlags []string) (transport.MutationEvidence, error) {
	ev.FlagsSource = "STORE"
	if !observation.observed {
		ev.FlagsSource = "FETCH"
		if err := c.setDeadline(ctx, sess); err != nil {
			return unknownFlagResult(ev, err)
		}
		tag := sess.nextTag()
		if err := c.writeLine(sess, fmt.Sprintf("%s UID FETCH %d (UID FLAGS)", tag, ev.UID)); err != nil {
			return unknownFlagResult(ev, err)
		}
		result, err := c.readFlagResult(ctx, sess, tag, ev.UID, ev.UIDValidity)
		if err != nil {
			return unknownFlagResult(ev, err)
		}
		observation = result.observation
		if observation.missing || !observation.seenUID {
			return missingFlagResult(ev)
		}
	}
	if !observation.observed {
		return unknownFlagResult(ev, errors.New("target UID was returned without complete FLAGS proof"))
	}
	ev.ActualFlags = append([]string{}, observation.flags...)
	ev.FlagsState = transport.FlagObservationObserved
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
func (c *Client) readFlagResult(ctx context.Context, sess *session, tag string, uid, uidvalidity uint32) (flagCommandResult, error) {
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
		if err := flagResponseValidity(line, tag, uidvalidity); err != nil {
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

func flagResponseValidity(line, tag string, expected uint32) error {
	fields := strings.Fields(line)
	if len(fields) < 2 || (fields[0] != "*" && fields[0] != tag) {
		return nil
	}
	status := strings.ToUpper(fields[1])
	if status == "BYE" {
		return errors.New("IMAP server closed the selected mailbox session")
	}
	if status != "OK" && status != "NO" && status != "BAD" {
		return nil
	}
	code, present := bracketedResponseCode(line)
	values := strings.Fields(code)
	if !present || len(values) == 0 || !strings.EqualFold(values[0], "UIDVALIDITY") {
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
