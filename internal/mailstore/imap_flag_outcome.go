package mailstore

import (
	"mailcli/internal/transport"
)

// Only a definitive pre-mutation rebuild permits a retry. An outer uncertain
// outcome retains precedence over any nested UIDVALIDITY diagnostic.
func isUIDValidityChangedError(err error) bool {
	return transport.ErrorCode(err) == "mailbox_uidvalidity_changed"
}

// shouldRetryFlagMutation reports whether a UIDVALIDITY change on a mutation
// that provably never started permits exactly one target re-resolution. A
// mutation that already attempted or whose outcome is uncertain is never
// retried.
func shouldRetryFlagMutation(err error, ev transport.MutationEvidence) bool {
	return isUIDValidityChangedError(err) &&
		(ev.Outcome == "" || ev.Outcome == transport.MutationOutcomeNotStarted)
}

// flagOutcomeDecision names the fail-closed ruling for a STORE evidence/error
// pair after the (possibly retried) call returns.
type flagOutcomeDecision int

const (
	// flagOutcomePropagate propagates the raw error: it arrived without any
	// mutation evidence to classify.
	flagOutcomePropagate flagOutcomeDecision = iota
	// flagOutcomeAccept trusts the evidence as returned.
	flagOutcomeAccept
	// flagOutcomeForceUnknown discards the observed state and forces an
	// outcome-unknown error: either the observation is bound to a different
	// message identity, or the call succeeded without a complete observation.
	flagOutcomeForceUnknown
)

// classifyFlagOutcome collapses the nested post-STORE evidence checks into one
// auditable decision. An error without mutation evidence propagates unchanged;
// an observed flags state bound to a different message identity or a
// successful call lacking a complete observation is forced to
// outcome-unknown instead of being trusted.
func classifyFlagOutcome(ev transport.MutationEvidence, err error, target imapTarget) flagOutcomeDecision {
	if err != nil && ev.Command == "" {
		return flagOutcomePropagate
	}
	identityMismatch := ev.UID != target.uid || ev.Mailbox != target.imapMailbox || ev.UIDValidity != target.uidvalidity
	if (ev.FlagsState == transport.FlagObservationObserved && identityMismatch) ||
		(err == nil && (ev.FlagsState != transport.FlagObservationObserved || ev.Outcome != transport.MutationOutcomeCompleted)) {
		return flagOutcomeForceUnknown
	}
	return flagOutcomeAccept
}
