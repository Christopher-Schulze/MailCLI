package imapclient

import (
	"bufio"
	"context"
	"errors"
	"net"
	"testing"

	"mailcli/internal/transport"
)

func TestMoveCommandPreservesDispatchEvidence(t *testing.T) {
	command := `A001 UID MOVE 42 "Archive"`
	for _, test := range []struct {
		name              string
		command           string
		failDeadline      bool
		cancel            bool
		failWriteAfter    int
		closeAfterCommand bool
		wantBytes         string
		wantDispatched    bool
		wantOutcome       string
		wantCode          string
	}{
		{name: "deadline", command: command, failDeadline: true, wantOutcome: transport.MutationOutcomeNotStarted, wantCode: transport.CodeIMAPTimeout},
		{name: "canceled before write", command: command, cancel: true, wantOutcome: transport.MutationOutcomeNotStarted, wantCode: transport.CodeIMAPTimeout},
		{name: "validation", command: "A001 UID MOVE 42\r\n\"Archive\"", wantOutcome: transport.MutationOutcomeNotStarted, wantCode: transport.CodeIMAPInvalidValue},
		{name: "partial write", command: command, failWriteAfter: 3, wantBytes: "A00", wantDispatched: true, wantOutcome: transport.MutationOutcomeUnknown, wantCode: transport.CodeIMAPMoveOutcomeUnknown},
		{name: "lost reply", command: command, closeAfterCommand: true, wantBytes: command + "\r\n", wantDispatched: true, wantOutcome: transport.MutationOutcomeUnknown, wantCode: transport.CodeIMAPMoveOutcomeUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			peerConn, clientConn := net.Pipe()
			wire := &copyOutcomeConn{Conn: clientConn, failDeadline: test.failDeadline, failWriteAfter: test.failWriteAfter}
			sess := &session{conn: wire, br: bufio.NewReader(wire), bw: bufio.NewWriter(wire)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.cancel {
				cancel()
			}
			peerResult := startCopyOutcomePeer(peerConn, test.closeAfterCommand)
			status, text, _, dispatched, commandErr := (&Client{}).doTransferCommandResponse(ctx, sess, test.command, "MOVE")
			if commandErr == nil {
				t.Fatal("MOVE accepted injected protocol failure")
			}
			evidence := copyEvidenceForTest()
			evidence.Command = "MOVE"
			evidence.OperationID = "move-test-operation"
			evidence, err := transferCommandError(evidence, status, text, dispatched, commandErr)
			if closeErr := wire.Close(); closeErr != nil {
				t.Error(closeErr)
			}
			peer := <-peerResult
			if peer.err != nil || string(peer.data) != test.wantBytes {
				t.Fatalf("peer bytes=%q, error=%v; want %q", peer.data, peer.err, test.wantBytes)
			}
			if dispatched != test.wantDispatched || evidence.Outcome != test.wantOutcome || transport.ErrorCode(err) != test.wantCode {
				t.Fatalf("dispatch=%t, evidence=%+v, error=%v", dispatched, evidence, err)
			}
			var outcome *transport.MutationOutcomeError
			if !errors.As(err, &outcome) || outcome.Evidence.OperationID != "move-test-operation" ||
				outcome.Evidence.Command != "MOVE" || outcome.Evidence.Outcome != test.wantOutcome ||
				outcome.Evidence.UIDValidity != 12345 || outcome.Evidence.ExpectedUIDValidity != 12345 {
				t.Fatalf("MOVE lost typed identity evidence: %+v, %v", outcome, err)
			}
			if test.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("MOVE lost cancellation cause: %v", err)
			}
		})
	}
}
