package mailstore

import (
	"errors"
	"fmt"
	"testing"

	"mailcli/internal/transport"
)

func decisionTarget() imapTarget {
	return imapTarget{uid: 101, imapMailbox: "INBOX", uidvalidity: 12345}
}

func TestShouldRetryFlagMutation(t *testing.T) {
	uidErr := func() error {
		return &transport.TransportError{Code: "mailbox_uidvalidity_changed", Message: "changed"}
	}
	tests := []struct {
		name    string
		err     error
		outcome string
		want    bool
	}{
		{"rebuild before start", uidErr(), "", true},
		{"rebuild with not_started evidence", uidErr(), transport.MutationOutcomeNotStarted, true},
		{"rebuild after attempt", uidErr(), transport.MutationOutcomeAttempted, false},
		{"rebuild with unknown outcome", uidErr(), transport.MutationOutcomeUnknown, false},
		{"rebuild with completed outcome", uidErr(), transport.MutationOutcomeCompleted, false},
		{"other error before start", errors.New("io"), "", false},
		{"no error", nil, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := transport.MutationEvidence{Command: "STORE", Outcome: tt.outcome}
			if got := shouldRetryFlagMutation(tt.err, ev); got != tt.want {
				t.Fatalf("shouldRetryFlagMutation(%v, outcome=%q) = %v, want %v", tt.err, tt.outcome, got, tt.want)
			}
		})
	}
	if !shouldRetryFlagMutation(fmt.Errorf("wrapped: %w", uidErr()), transport.MutationEvidence{}) {
		t.Fatal("wrapped uidvalidity change must still permit the single retry")
	}
}

func TestClassifyFlagOutcomeDecisionTable(t *testing.T) {
	storeErr := errors.New("store failed")
	tests := []struct {
		name string
		ev   transport.MutationEvidence
		err  error
		want flagOutcomeDecision
	}{
		{
			name: "error without evidence propagates",
			ev:   transport.MutationEvidence{},
			err:  storeErr,
			want: flagOutcomePropagate,
		},
		{
			name: "error with evidence accepted for summary",
			ev:   transport.MutationEvidence{Command: "STORE", Outcome: transport.MutationOutcomeUnknown, UID: 101, Mailbox: "INBOX", UIDValidity: 12345},
			err:  storeErr,
			want: flagOutcomeAccept,
		},
		{
			name: "complete observation on target identity accepted",
			ev:   transport.MutationEvidence{Command: "STORE", Outcome: transport.MutationOutcomeCompleted, FlagsState: transport.FlagObservationObserved, UID: 101, Mailbox: "INBOX", UIDValidity: 12345},
			err:  nil,
			want: flagOutcomeAccept,
		},
		{
			name: "observed state on foreign uid forced unknown",
			ev:   transport.MutationEvidence{Command: "STORE", Outcome: transport.MutationOutcomeCompleted, FlagsState: transport.FlagObservationObserved, UID: 202, Mailbox: "INBOX", UIDValidity: 12345},
			err:  nil,
			want: flagOutcomeForceUnknown,
		},
		{
			name: "observed state on foreign mailbox forced unknown",
			ev:   transport.MutationEvidence{Command: "STORE", Outcome: transport.MutationOutcomeCompleted, FlagsState: transport.FlagObservationObserved, UID: 101, Mailbox: "Archive", UIDValidity: 12345},
			err:  nil,
			want: flagOutcomeForceUnknown,
		},
		{
			name: "observed state on foreign uidvalidity forced unknown",
			ev:   transport.MutationEvidence{Command: "STORE", Outcome: transport.MutationOutcomeCompleted, FlagsState: transport.FlagObservationObserved, UID: 101, Mailbox: "INBOX", UIDValidity: 999},
			err:  nil,
			want: flagOutcomeForceUnknown,
		},
		{
			name: "observed state on foreign identity forced unknown even with error",
			ev:   transport.MutationEvidence{Command: "STORE", Outcome: transport.MutationOutcomeCompleted, FlagsState: transport.FlagObservationObserved, UID: 202, Mailbox: "INBOX", UIDValidity: 12345},
			err:  storeErr,
			want: flagOutcomeForceUnknown,
		},
		{
			name: "success without observation forced unknown",
			ev:   transport.MutationEvidence{Command: "STORE", Outcome: transport.MutationOutcomeCompleted, FlagsState: transport.FlagObservationUnverified, UID: 101, Mailbox: "INBOX", UIDValidity: 12345},
			err:  nil,
			want: flagOutcomeForceUnknown,
		},
		{
			name: "success with incomplete outcome forced unknown",
			ev:   transport.MutationEvidence{Command: "STORE", Outcome: transport.MutationOutcomeAttempted, FlagsState: transport.FlagObservationObserved, UID: 101, Mailbox: "INBOX", UIDValidity: 12345},
			err:  nil,
			want: flagOutcomeForceUnknown,
		},
		{
			name: "missing observation on success forced unknown",
			ev:   transport.MutationEvidence{Command: "STORE", Outcome: transport.MutationOutcomeCompleted, FlagsState: transport.FlagObservationMissing, UID: 101, Mailbox: "INBOX", UIDValidity: 12345},
			err:  nil,
			want: flagOutcomeForceUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyFlagOutcome(tt.ev, tt.err, decisionTarget()); got != tt.want {
				t.Fatalf("classifyFlagOutcome = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClassifyFlagOutcomeDoesNotMutateEvidence(t *testing.T) {
	ev := transport.MutationEvidence{
		Command: "STORE", Outcome: transport.MutationOutcomeCompleted,
		FlagsState: transport.FlagObservationObserved, ActualFlags: []string{"\\Seen"},
		UID: 101, Mailbox: "INBOX", UIDValidity: 12345,
		ServerResponse: "OK STORE completed", OperationID: "store_abc",
	}
	before := ev
	if got := classifyFlagOutcome(ev, nil, decisionTarget()); got != flagOutcomeAccept {
		t.Fatalf("classifyFlagOutcome = %v, want accept", got)
	}
	if ev.Command != before.Command || ev.Outcome != before.Outcome || ev.FlagsState != before.FlagsState ||
		len(ev.ActualFlags) != len(before.ActualFlags) || ev.UID != before.UID || ev.Mailbox != before.Mailbox ||
		ev.UIDValidity != before.UIDValidity || ev.ServerResponse != before.ServerResponse || ev.OperationID != before.OperationID {
		t.Fatalf("classifier mutated evidence: %+v -> %+v", before, ev)
	}
}
