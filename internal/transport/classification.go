package transport

// Failure classification at the transport boundary.
//
// The predicates in this file translate wire-level error codes into the
// operational meaning callers outside this package need. Domain packages must
// consume this classification instead of comparing Code* constants, so the
// decision of which codes are transient, uncertain, rejected, or
// caller-correctable lives in exactly one place next to the codes themselves.
// Every predicate resolves codes through ErrorCode, which prefers
// outcome-uncertainty codes over transport-level causes in wrapped errors.

// IsTimeout reports that the operation failed with an IMAP timeout.
func IsTimeout(err error) bool {
	return ErrorCode(err) == CodeIMAPTimeout
}

// IsTransientReadFailure reports an IMAP-side transient failure that is safe
// to retry unchanged: connect, cancellation, disconnect, timeout, or fetch failure.
func IsTransientReadFailure(err error) bool {
	switch ErrorCode(err) {
	case CodeIMAPConnectFailed, CodeIMAPCanceled, CodeIMAPDisconnected, CodeIMAPTimeout, CodeIMAPFetchFailed:
		return true
	}
	return false
}

// IsTransientTransportFailure reports any IMAP connect, cancellation,
// disconnect, timeout, or fetch failure and SMTP timeout.
func IsTransientTransportFailure(err error) bool {
	switch ErrorCode(err) {
	case CodeIMAPConnectFailed, CodeIMAPCanceled, CodeIMAPDisconnected, CodeIMAPTimeout, CodeIMAPFetchFailed,
		CodeSMTPTimeout, CodeSMTPTransferTimeout:
		return true
	}
	return false
}

// IsRejectedSubmission reports a definitive server rejection of the
// submission, as opposed to an outcome that could not be proven.
func IsRejectedSubmission(err error) bool {
	return ErrorCode(err) == CodeSMTPRejected
}

// IsSubmissionOutcomeUnknown reports that SMTP acceptance could not be proven.
func IsSubmissionOutcomeUnknown(err error) bool {
	return ErrorCode(err) == CodeSMTPSubmissionUnknown
}

// IsSMTPDataIncomplete reports that DATA failed before the end-of-data
// terminator was attempted, so the server could not accept the message.
func IsSMTPDataIncomplete(err error) bool {
	return ErrorCode(err) == CodeSMTPDataIncomplete
}

// IsMutationOutcomeUnknown reports that a mailbox mutation's final outcome
// cannot be proven — the copy, move, or flags outcome-unknown family. A
// provably divergent final state (imap_flags_mismatch) is intentionally not
// part of this family: it is a known-wrong result, not an unknown one.
func IsMutationOutcomeUnknown(err error) bool {
	switch ErrorCode(err) {
	case CodeIMAPCopyOutcomeUnknown, CodeIMAPMoveOutcomeUnknown, CodeIMAPFlagsOutcomeUnknown:
		return true
	}
	return false
}

// IsFlagsStateMismatch reports that the post-mutation flags state was observed
// and provably differs from the intended state.
func IsFlagsStateMismatch(err error) bool {
	return ErrorCode(err) == CodeIMAPFlagsMismatch
}

// IsMirrorOutcomeUncertain reports that Sent-copy persistence cannot be
// proven: either the APPEND outcome is unknown or the candidate identity is
// ambiguous and cannot be verified.
func IsMirrorOutcomeUncertain(err error) bool {
	switch ErrorCode(err) {
	case CodeIMAPAppendOutcomeUnknown, CodeIMAPAmbiguousMessageID:
		return true
	}
	return false
}

// IsAppendOutcomeUnknown reports an unknown outcome for the Sent APPEND
// specifically, without the ambiguous-identity widening of
// IsMirrorOutcomeUncertain.
func IsAppendOutcomeUnknown(err error) bool {
	return ErrorCode(err) == CodeIMAPAppendOutcomeUnknown
}

// IsAppendIncomplete reports that the APPEND literal failed before its
// terminating CRLF was attempted, so the server could not commit the message.
func IsAppendIncomplete(err error) bool {
	return ErrorCode(err) == CodeIMAPAppendIncomplete
}

// IsAppendFailed reports a definitive APPEND failure: the server rejected the
// Sent copy.
func IsAppendFailed(err error) bool {
	return ErrorCode(err) == CodeIMAPAppendFailed
}

// IsMessageNotFound reports that the target message could not be found.
func IsMessageNotFound(err error) bool {
	return ErrorCode(err) == CodeIMAPMessageNotFound
}

// IsSentMailboxNotFound reports that no Sent mailbox could be resolved.
func IsSentMailboxNotFound(err error) bool {
	return ErrorCode(err) == CodeIMAPSentMailboxNotFound
}

// IsSourceTooLarge reports that a raw message source exceeds the hydration
// bound.
func IsSourceTooLarge(err error) bool {
	return ErrorCode(err) == CodeIMAPRawSourceTooLarge
}

// IsConfigurationFailure reports a caller-correctable configuration problem:
// authentication, missing credentials, TLS, an unsupported provider, or
// unsupported UTF8 usage.
func IsConfigurationFailure(err error) bool {
	switch ErrorCode(err) {
	case CodeSMTPAuthFailed, CodeSMTPCredentialsMissing, CodeIMAPAuthFailed,
		CodeSMTPTLSFailed, CodeUnsupportedProvider, CodeSMTPUTF8Unsupported:
		return true
	}
	return false
}

// IsStore reports whether the mutation evidence was produced by a STORE
// command.
func (e MutationEvidence) IsStore() bool {
	return e.Command == "STORE"
}

// HasPartialEffects reports whether any phase of the compound mutation was
// independently proven to have taken effect.
func (e MutationEvidence) HasPartialEffects() bool {
	return len(e.CompletedEffects) > 0 || e.Outcome == MutationOutcomePartial
}

// StoreRejectedOrNotStarted reports whether a STORE mutation was definitively
// rejected or never started — the cases where a plain retry is safe.
func (e MutationEvidence) StoreRejectedOrNotStarted() bool {
	return e.IsStore() && (e.Outcome == MutationOutcomeNotStarted || e.Outcome == MutationOutcomeRejected)
}
