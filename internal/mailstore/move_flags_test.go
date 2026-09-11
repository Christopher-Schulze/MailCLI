package mailstore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

type moveFlagResultOperator struct {
	*stubImapOperator
	evidence transport.MutationEvidence
	cause    error
	calls    int
}

func (operator *moveFlagResultOperator) MoveMessage(_ context.Context, cfg transport.ImapConfig, mailbox string, uid, validity uint32, destination string) (transport.MutationEvidence, error) {
	operator.calls++
	evidence := operator.evidence
	evidence.OperationID = transport.MutationOperationID("MOVE", cfg.Username, mailbox, uid, validity, destination)
	evidence.Command, evidence.SourceAccount = "MOVE", cfg.Username
	evidence.Mailbox, evidence.TargetMailbox = mailbox, destination
	evidence.UID, evidence.UIDValidity, evidence.ExpectedUIDValidity = uid, validity, validity
	evidence.CopyUIDResponse, evidence.CopyUIDValidity, evidence.CopySourceUID, evidence.CopyDestinationUID = "COPYUID 23456 101 500", 23456, uid, 500
	evidence.DestinationUIDValidity, evidence.DestinationUID = 23456, 500
	if evidence.Outcome == transport.MutationOutcomeCompleted {
		return evidence, nil
	}
	return evidence, &transport.MutationOutcomeError{Code: transport.CodeIMAPMoveOutcomeUnknown, Message: "COPY completed; source flag verification failed", Evidence: evidence, Err: operator.cause}
}

func (operator *moveFlagResultOperator) DeleteMessage(ctx context.Context, cfg transport.ImapConfig, mailbox string, uid, validity uint32) (transport.MutationEvidence, error) {
	return operator.MoveMessage(ctx, cfg, mailbox, uid, validity, "Trash")
}

func TestMoveFlagEvidenceSurvivesStoreResults(t *testing.T) {
	for _, test := range []struct {
		name     string
		state    transport.FlagObservationState
		flags    []string
		complete bool
		cause    error
	}{
		{name: "contradictory empty flags", state: transport.FlagObservationObserved, flags: []string{}},
		{name: "contradictory observed flags", state: transport.FlagObservationObserved, flags: []string{"\\Seen", "custom"}},
		{name: "target missing", state: transport.FlagObservationMissing},
		{name: "verification unavailable", state: transport.FlagObservationUnverified},
		{name: "nested rollover cannot replay COPY", state: transport.FlagObservationUnverified, cause: uidValidityChangedErrorForTest()},
		{name: "completed source flag", state: transport.FlagObservationObserved, flags: []string{"\\Seen", "\\Deleted"}, complete: true},
	} {
		for _, operation := range []string{"move", "delete"} {
			t.Run(test.name+"/"+operation, func(t *testing.T) {
				evidence := transport.MutationEvidence{Outcome: transport.MutationOutcomePartial, CompletedEffects: []string{"copy"}, FlagsState: test.state, ActualFlags: test.flags, FlagsSource: "FETCH", ServerResponse: "OK COPY completed; source flag response: OK STORE done"}
				if test.complete {
					evidence.Outcome = transport.MutationOutcomeCompleted
					evidence.CompletedEffects = []string{"copy", "source_flag", "uid_expunge"}
					evidence.ExpungeBranch = "uid_expunge"
				}
				operator := &moveFlagResultOperator{stubImapOperator: &stubImapOperator{searchMatchesByMailbox: map[string][]int{"Archive": {0}}}, evidence: evidence, cause: test.cause}
				client, original := flagResultFixture(t, operator)
				destination, err := mailref.EncodeMailbox(testAccountID, []string{"Archive"})
				if err != nil {
					t.Fatal(err)
				}
				var proof *mail.ServerMutationEvidence
				if operation == "move" {
					var state mail.MessageSummary
					state, err = client.TransferMessage(context.Background(), mail.TransferMessageRequest{Ref: original.Ref, DestinationMailbox: destination, AllowDraftMutation: true})
					proof = state.ServerTruth
					wantMailbox := original.MailboxRef
					if test.complete {
						wantMailbox = destination
					}
					if state.MailboxRef != wantMailbox || state.Subject != original.Subject || state.Ref == "" {
						t.Errorf("source metadata or mailbox identity lost: %+v", state)
					}
					if !test.complete && !strings.Contains(state.StalenessNote, "incomplete") {
						t.Errorf("partial result looks complete: %+v", state)
					}
				} else {
					var result mail.DeleteResult
					result, err = client.DeleteMessage(context.Background(), mail.DeleteMessageRequest{Ref: original.Ref, AllowDraftMutation: true})
					proof = result.ServerTruth
					if result.MessageRef != original.Ref || result.Deleted != test.complete {
						t.Errorf("delete completion was invented or identity lost: %+v", result)
					}
				}
				if (err == nil) != test.complete || (!test.complete && transport.ErrorCode(err) != transport.CodeIMAPMoveOutcomeUnknown) {
					t.Errorf("result error = %v, complete=%t", err, test.complete)
				}
				if operator.calls != 1 {
					t.Errorf("compound mutation replayed: %d calls", operator.calls)
				}
				if proof == nil {
					t.Fatal("store discarded the mutation evidence")
				}
				if proof.OperationID == "" || proof.Outcome != evidence.Outcome || proof.Mailbox != "INBOX" || proof.UID != 101 || proof.UIDValidity != 12345 || proof.ExpectedUIDValidity != 12345 || proof.CopyUIDValidity != 23456 || proof.CopySourceUID != 101 || proof.CopyDestinationUID != 500 || proof.DestinationUIDValidity != 23456 || proof.DestinationUID != 500 || proof.CopyUIDResponse != "COPYUID 23456 101 500" || !reflect.DeepEqual(proof.CompletedEffects, evidence.CompletedEffects) {
					t.Errorf("COPY/source evidence changed: %+v", proof)
				}
				if proof.FlagsState != string(test.state) || proof.FlagsSource != "FETCH" || !reflect.DeepEqual(proof.ActualFlags, append([]string(nil), test.flags...)) {
					t.Errorf("actual flag evidence changed: %+v", proof)
				}
				if !test.complete {
					var outcome *transport.MutationOutcomeError
					guidance := mail.GuidanceForError("messages."+operation, err)
					if !errors.As(err, &outcome) || outcome.Evidence.OperationID != proof.OperationID || guidance.ReplayAllowed || guidance.EffectCertainty != mail.EffectPartial || guidance.Recovery.OperationID != proof.OperationID {
						t.Errorf("lost error identity or unsafe guidance: %v, %+v", err, guidance)
					}
				}
			})
		}
	}
}
