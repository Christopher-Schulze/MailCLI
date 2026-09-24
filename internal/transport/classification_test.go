package transport

import (
	"errors"
	"fmt"
	"testing"
)

func codedErr(code string) error {
	return &TransportError{Code: code, Message: "test " + code}
}

func TestFailureClassificationPredicates(t *testing.T) {
	tests := []struct {
		name      string
		predicate func(error) bool
		positive  []string
		negative  []string
	}{
		{
			name:      "IsTimeout",
			predicate: IsTimeout,
			positive:  []string{CodeIMAPTimeout},
			negative:  []string{CodeIMAPConnectFailed, CodeIMAPCanceled, CodeSMTPTimeout, CodeIMAPFlagsOutcomeUnknown},
		},
		{
			name:      "IsTransientReadFailure",
			predicate: IsTransientReadFailure,
			positive:  []string{CodeIMAPConnectFailed, CodeIMAPCanceled, CodeIMAPDisconnected, CodeIMAPTimeout, CodeIMAPFetchFailed},
			negative:  []string{CodeSMTPTimeout, CodeSMTPTransferTimeout, CodeIMAPFlagsOutcomeUnknown},
		},
		{
			name:      "IsTransientTransportFailure",
			predicate: IsTransientTransportFailure,
			positive:  []string{CodeIMAPConnectFailed, CodeIMAPCanceled, CodeIMAPDisconnected, CodeIMAPTimeout, CodeIMAPFetchFailed, CodeSMTPTimeout, CodeSMTPTransferTimeout},
			negative:  []string{CodeSMTPRejected, CodeSMTPDataIncomplete, CodeIMAPMessageNotFound},
		},
		{
			name:      "IsRejectedSubmission",
			predicate: IsRejectedSubmission,
			positive:  []string{CodeSMTPRejected},
			negative:  []string{CodeSMTPSubmissionUnknown, CodeSMTPTimeout},
		},
		{
			name:      "IsSubmissionOutcomeUnknown",
			predicate: IsSubmissionOutcomeUnknown,
			positive:  []string{CodeSMTPSubmissionUnknown},
			negative:  []string{CodeSMTPRejected, CodeSMTPDataIncomplete},
		},
		{
			name:      "IsSMTPDataIncomplete",
			predicate: IsSMTPDataIncomplete,
			positive:  []string{CodeSMTPDataIncomplete},
			negative:  []string{CodeSMTPSubmissionUnknown, CodeSMTPRejected},
		},
		{
			name:      "IsMutationOutcomeUnknown",
			predicate: IsMutationOutcomeUnknown,
			positive:  []string{CodeIMAPCopyOutcomeUnknown, CodeIMAPMoveOutcomeUnknown, CodeIMAPFlagsOutcomeUnknown},
			negative:  []string{CodeIMAPFlagsMismatch, CodeIMAPAppendOutcomeUnknown, CodeIMAPMessageNotFound},
		},
		{
			name:      "IsFlagsStateMismatch",
			predicate: IsFlagsStateMismatch,
			positive:  []string{CodeIMAPFlagsMismatch},
			negative:  []string{CodeIMAPFlagsOutcomeUnknown},
		},
		{
			name:      "IsMirrorOutcomeUncertain",
			predicate: IsMirrorOutcomeUncertain,
			positive:  []string{CodeIMAPAppendOutcomeUnknown, CodeIMAPAmbiguousMessageID},
			negative:  []string{CodeIMAPAppendFailed, CodeIMAPAppendIncomplete, CodeIMAPFlagsOutcomeUnknown},
		},
		{
			name:      "IsAppendOutcomeUnknown",
			predicate: IsAppendOutcomeUnknown,
			positive:  []string{CodeIMAPAppendOutcomeUnknown},
			negative:  []string{CodeIMAPAmbiguousMessageID, CodeIMAPAppendFailed, CodeIMAPAppendIncomplete},
		},
		{
			name:      "IsAppendIncomplete",
			predicate: IsAppendIncomplete,
			positive:  []string{CodeIMAPAppendIncomplete},
			negative:  []string{CodeIMAPAppendOutcomeUnknown, CodeIMAPAppendFailed},
		},
		{
			name:      "IsAppendFailed",
			predicate: IsAppendFailed,
			positive:  []string{CodeIMAPAppendFailed},
			negative:  []string{CodeIMAPAppendOutcomeUnknown, CodeIMAPAppendIncomplete},
		},
		{
			name:      "IsMessageNotFound",
			predicate: IsMessageNotFound,
			positive:  []string{CodeIMAPMessageNotFound},
			negative:  []string{CodeIMAPSentMailboxNotFound, CodeIMAPFetchFailed},
		},
		{
			name:      "IsSentMailboxNotFound",
			predicate: IsSentMailboxNotFound,
			positive:  []string{CodeIMAPSentMailboxNotFound},
			negative:  []string{CodeIMAPMessageNotFound},
		},
		{
			name:      "IsSourceTooLarge",
			predicate: IsSourceTooLarge,
			positive:  []string{CodeIMAPRawSourceTooLarge},
			negative:  []string{CodeIMAPFetchFailed},
		},
		{
			name:      "IsConfigurationFailure",
			predicate: IsConfigurationFailure,
			positive:  []string{CodeSMTPAuthFailed, CodeSMTPCredentialsMissing, CodeIMAPAuthFailed, CodeSMTPTLSFailed, CodeUnsupportedProvider, CodeSMTPUTF8Unsupported},
			negative:  []string{CodeIMAPTimeout, CodeSMTPRejected},
		},
	}
	for _, tt := range tests {
		for _, code := range tt.positive {
			if !tt.predicate(codedErr(code)) {
				t.Errorf("%s(%s) = false, want true", tt.name, code)
			}
			if !tt.predicate(fmt.Errorf("wrapped: %w", codedErr(code))) {
				t.Errorf("%s(wrapped %s) = false, want true", tt.name, code)
			}
		}
		for _, code := range tt.negative {
			if tt.predicate(codedErr(code)) {
				t.Errorf("%s(%s) = true, want false", tt.name, code)
			}
		}
		if tt.predicate(nil) || tt.predicate(errors.New("plain")) {
			t.Errorf("%s(nil/plain) = true, want false", tt.name)
		}
	}
}

func TestClassificationPrefersOutcomeUncertainty(t *testing.T) {
	outcomeErr := &MutationOutcomeError{
		Code:     CodeIMAPFlagsOutcomeUnknown,
		Evidence: MutationEvidence{Command: "STORE", OperationID: "op_1"},
		Err:      codedErr(CodeIMAPTimeout),
	}
	if IsTimeout(outcomeErr) || IsTransientReadFailure(outcomeErr) {
		t.Fatal("wrapped timeout must not outrank the outcome-uncertain code")
	}
	if !IsMutationOutcomeUnknown(outcomeErr) {
		t.Fatal("outcome-uncertain code must classify the wrapped error")
	}
}

func TestMutationEvidenceClassification(t *testing.T) {
	store := MutationEvidence{Command: "STORE"}
	if !store.IsStore() {
		t.Fatal("STORE command must classify as store")
	}
	if (MutationEvidence{Command: "COPY"}).IsStore() {
		t.Fatal("COPY command must not classify as store")
	}
	if !(MutationEvidence{CompletedEffects: []string{"copy"}}).HasPartialEffects() {
		t.Fatal("completed effects must classify as partial")
	}
	if !(MutationEvidence{Outcome: MutationOutcomePartial}).HasPartialEffects() {
		t.Fatal("partial outcome must classify as partial")
	}
	if (MutationEvidence{}).HasPartialEffects() {
		t.Fatal("empty evidence must not classify as partial")
	}
	for _, outcome := range []string{MutationOutcomeNotStarted, MutationOutcomeRejected} {
		if !(MutationEvidence{Command: "STORE", Outcome: outcome}).StoreRejectedOrNotStarted() {
			t.Fatalf("STORE outcome %q must classify as safe to retry", outcome)
		}
	}
	if (MutationEvidence{Command: "STORE", Outcome: MutationOutcomeUnknown}).StoreRejectedOrNotStarted() ||
		(MutationEvidence{Command: "COPY", Outcome: MutationOutcomeRejected}).StoreRejectedOrNotStarted() {
		t.Fatal("unknown or non-STORE outcomes must not classify as safe to retry")
	}
}
