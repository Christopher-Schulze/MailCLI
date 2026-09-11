package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

type flagResultGateway struct {
	testGateway
	state mail.MessageSummary
	err   error
}

func (gateway flagResultGateway) MarkMessage(context.Context, mail.MarkMessageRequest) (mail.MessageSummary, error) {
	return gateway.state, gateway.err
}

func TestMarkFailureJSONRetainsActualStateAndNoReplayGuidance(t *testing.T) {
	for _, test := range []struct {
		name, code string
		state      transport.FlagObservationState
		read       bool
	}{
		{"conflicting", transport.CodeIMAPFlagsMismatch, transport.FlagObservationObserved, true},
		{"missing", transport.CodeIMAPMessageNotFound, transport.FlagObservationMissing, false},
		{"unverified", transport.CodeIMAPFlagsOutcomeUnknown, transport.FlagObservationUnverified, false},
		{"partial", transport.CodeIMAPFlagsPartial, transport.FlagObservationObserved, false},
		{"unsupported", transport.CodeIMAPFlagsUnsupported, transport.FlagObservationObserved, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			evidence := transport.MutationEvidence{OperationID: "store_retained", Command: "STORE", FlagsState: test.state, Outcome: transport.MutationOutcomeUnknown}
			state := mail.MessageSummary{Ref: "msg_ref", Read: test.read, ServerTruth: &mail.ServerMutationEvidence{OperationID: evidence.OperationID, Command: evidence.Command, FlagsState: string(test.state), FlagsSource: "FETCH", UID: 42, UIDValidity: 12345}}
			if test.read {
				state.ServerTruth.ActualFlags = []string{"\\Seen"}
			}
			gateway := flagResultGateway{state: state, err: &transport.MutationOutcomeError{Code: test.code, Message: "mark result requires observation", Evidence: evidence}}
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), mail.NewService(gateway), []string{"messages", "mark", "--ref", "msg_ref", "--read", "false", "--json"}, &stdout, &stderr)
			var result envelope
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if code != 1 || result.OK || stderr.Len() != 0 || result.Data.MessageState == nil || result.Data.MessageState.Read != test.read || result.Data.MessageState.ServerTruth.FlagsState != string(test.state) {
				t.Fatalf("JSON lost actual mark evidence: code %d, stdout %s, stderr %s", code, stdout.String(), stderr.String())
			}
			if result.Error == nil || result.Error.Code != test.code || result.Error.Guidance == nil || result.Error.Guidance.ReplayAllowed || result.Error.Guidance.Retryability != mail.RetryObserveRequired || result.Error.Guidance.Recovery.OperationID != evidence.OperationID {
				t.Fatalf("unsafe or incomplete mark guidance: %+v", result.Error)
			}
		})
	}
}
