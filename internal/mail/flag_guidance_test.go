package mail

import (
	"errors"
	"testing"

	"mailcli/internal/transport"
)

func TestFlagUnknownCodeOutranksJoinedRejectionAndRollover(t *testing.T) {
	unknown := &transport.MutationOutcomeError{Code: transport.CodeIMAPFlagsOutcomeUnknown, Message: "verification interrupted", Evidence: transport.MutationEvidence{Command: "STORE", Outcome: transport.MutationOutcomeUnknown, OperationID: "store_unknown"}, Err: &transport.TransportError{Code: "mailbox_uidvalidity_changed", Message: "changed after STORE"}}
	for _, test := range []struct {
		name string
		err  error
	}{
		{"wrapped rollover", unknown},
		{"rejection before unknown", errors.Join(&transport.TransportError{Code: transport.CodeIMAPMutationFailed, Message: "cleanup failed"}, unknown)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := transport.ErrorCode(test.err); got != transport.CodeIMAPFlagsOutcomeUnknown {
				t.Fatalf("ErrorCode = %s", got)
			}
			guidance := GuidanceForError("messages.mark", test.err)
			if guidance.ReplayAllowed || guidance.Retryability != RetryObserveRequired || guidance.EffectCertainty != EffectUnknown || guidance.Recovery.OperationID != "store_unknown" {
				t.Fatalf("unsafe guidance: %+v", guidance)
			}
		})
	}
}

func TestFlagFailuresRetainEffectCertaintyWithoutReplay(t *testing.T) {
	for _, test := range []struct {
		name, code, outcome string
		effect              EffectCertainty
	}{
		{"unsupported before STORE", transport.CodeIMAPFlagsUnsupported, transport.MutationOutcomeNotStarted, EffectNone},
		{"rejected first STORE", transport.CodeIMAPMutationFailed, transport.MutationOutcomeRejected, EffectNone},
		{"missing before STORE", transport.CodeIMAPMessageNotFound, transport.MutationOutcomeNotStarted, EffectNone},
		{"partial rejection", transport.CodeIMAPFlagsPartial, transport.MutationOutcomePartial, EffectPartial},
		{"permissions revoked between phases", transport.CodeIMAPFlagsUnsupported, transport.MutationOutcomePartial, EffectPartial},
		{"persistence unknown after STORE", transport.CodeIMAPFlagsUnsupported, transport.MutationOutcomeUnknown, EffectUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := &transport.MutationOutcomeError{Code: test.code, Evidence: transport.MutationEvidence{Command: "STORE", Outcome: test.outcome, OperationID: "store_junk"}}
			guidance := GuidanceForError("messages.mark", err)
			if guidance.EffectCertainty != test.effect || guidance.ReplayAllowed || guidance.Retryability != RetryObserveRequired || guidance.Recovery.OperationID != "store_junk" {
				t.Fatalf("guidance %+v; want effect %s without replay", guidance, test.effect)
			}
		})
	}
}
