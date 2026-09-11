package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

type moveFlagResultGateway struct {
	testGateway
	state   mail.MessageSummary
	deleted mail.DeleteResult
	err     error
	calls   int
}

func (gateway *moveFlagResultGateway) TransferMessage(context.Context, mail.TransferMessageRequest) (mail.MessageSummary, error) {
	gateway.calls++
	return gateway.state, gateway.err
}

func (gateway *moveFlagResultGateway) DeleteMessage(context.Context, mail.DeleteMessageRequest) (mail.DeleteResult, error) {
	gateway.calls++
	return gateway.deleted, gateway.err
}

func TestMoveFlagFailureJSONRetainsCompoundEvidence(t *testing.T) {
	for _, state := range []transport.FlagObservationState{transport.FlagObservationObserved, transport.FlagObservationMissing, transport.FlagObservationUnverified} {
		for _, operation := range []string{"move", "delete"} {
			t.Run(string(state)+"/"+operation, func(t *testing.T) {
				proof := &mail.ServerMutationEvidence{OperationID: "move_retained", Outcome: transport.MutationOutcomePartial, Command: "MOVE", Mailbox: "INBOX", TargetMailbox: "Archive", UID: 42, UIDValidity: 12345, ExpectedUIDValidity: 12345, CopyUIDValidity: 23456, CopySourceUID: 42, CopyDestinationUID: 100, CompletedEffects: []string{"copy"}, FlagsState: string(state), FlagsSource: "FETCH"}
				if state == transport.FlagObservationObserved {
					proof.ActualFlags = []string{"\\Seen"}
				}
				evidence := transport.MutationEvidence{OperationID: proof.OperationID, Outcome: proof.Outcome, Command: proof.Command, CompletedEffects: proof.CompletedEffects}
				gateway := &moveFlagResultGateway{
					state: mail.MessageSummary{Ref: "msg_ref", MailboxRef: "source_ref", ServerTruth: proof}, deleted: mail.DeleteResult{MessageRef: "msg_ref", ServerTruth: proof},
					err: &transport.MutationOutcomeError{Code: transport.CodeIMAPMoveOutcomeUnknown, Message: "COPY completed; inspect source flags before another operation", Evidence: evidence},
				}
				args := []string{"messages", operation, "--ref", "msg_ref", "--json"}
				if operation == "move" {
					args = append(args, "--mailbox", "destination_ref")
				} else {
					args = append(args, "--confirm")
				}
				var stdout, stderr bytes.Buffer
				code := Run(context.Background(), mail.NewService(gateway), args, &stdout, &stderr)
				var response envelope
				if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if code != 1 || response.OK || stderr.Len() != 0 || gateway.calls != 1 || response.Error == nil || response.Error.Code != transport.CodeIMAPMoveOutcomeUnknown {
					t.Fatalf("failure envelope = %s, stderr=%s, exit=%d, calls=%d", stdout.String(), stderr.String(), code, gateway.calls)
				}
				var actual *mail.ServerMutationEvidence
				if operation == "move" && response.Data.MessageState != nil {
					actual = response.Data.MessageState.ServerTruth
					if response.Data.MessageState.MailboxRef != "source_ref" {
						t.Error("failed move changed the source mailbox")
					}
				}
				if operation == "delete" && response.Data.DeleteResult != nil {
					actual = response.Data.DeleteResult.ServerTruth
					if response.Data.DeleteResult.Deleted {
						t.Error("failed delete claimed completion")
					}
				}
				if !reflect.DeepEqual(actual, proof) {
					t.Errorf("JSON lost source/COPY proof: %+v, want %+v", actual, proof)
				}
				guidance := response.Error.Guidance
				if guidance == nil || guidance.ReplayAllowed || guidance.Retryability != mail.RetryObserveRequired || guidance.EffectCertainty != mail.EffectPartial || guidance.Recovery.OperationID != proof.OperationID {
					t.Errorf("incomplete recovery guidance: %+v", guidance)
				}
			})
		}
	}
}
