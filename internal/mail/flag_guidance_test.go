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
