package imapclient

import (
	"bufio"
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestTransferVerifiesCapabilityIdentityAndEffects(t *testing.T) {
	for _, test := range []struct {
		name                      string
		config                    fakeServerConfig
		withoutID                 bool
		outcome                   string
		code                      string
		moves                     int
		copies                    int
		stores                    int
		copyUID                   bool
		authenticatedCapabilities bool
		earlyAcquireFailure       bool
	}{
		{name: "native generation-bound UID without Message-ID", config: fakeServerConfig{moveSupported: true}, withoutID: true, outcome: transport.MutationOutcomeCompleted, moves: 1, copyUID: true},
		{name: "native exact Message-ID without COPYUID", config: fakeServerConfig{moveSupported: true, omitCopyUID: true}, outcome: transport.MutationOutcomeCompleted, moves: 1},
		{name: "native missing COPYUID and Message-ID is unknown", config: fakeServerConfig{moveSupported: true, omitCopyUID: true}, withoutID: true, outcome: transport.MutationOutcomeUnknown, code: transport.CodeIMAPMoveOutcomeUnknown, moves: 1},
		{name: "native wrong COPYUID source", config: fakeServerConfig{moveSupported: true, moveResponse: "<tag> OK [COPYUID 12345 43 100] moved"}, outcome: transport.MutationOutcomeUnknown, code: transport.CodeIMAPMoveOutcomeUnknown, moves: 1},
		{name: "native wrong COPYUID generation", config: fakeServerConfig{moveSupported: true, moveResponse: "<tag> OK [COPYUID 54321 42 100] moved"}, outcome: transport.MutationOutcomeUnknown, code: transport.CodeIMAPMoveOutcomeUnknown, moves: 1, copyUID: true},
		{name: "native duplicate COPYUID", config: fakeServerConfig{moveSupported: true, moveResponse: "* OK [COPYUID 12345 42 100] copied\r\n<tag> OK [COPYUID 12345 42 100] moved"}, outcome: transport.MutationOutcomeUnknown, code: transport.CodeIMAPMoveOutcomeUnknown, moves: 1},
		{name: "native source still present", config: fakeServerConfig{moveSupported: true, leaveMovedSource: true}, outcome: transport.MutationOutcomePartial, code: transport.CodeIMAPMoveOutcomeUnknown, moves: 1, copyUID: true},
		{name: "native COPYUID destination vanished", config: fakeServerConfig{moveSupported: true, missingUIDs: []uint32{100}}, outcome: transport.MutationOutcomeUnknown, code: transport.CodeIMAPMoveOutcomeUnknown, moves: 1, copyUID: true},
		{name: "native without COPYUID rejects wrong destination header", config: fakeServerConfig{moveSupported: true, omitCopyUID: true, fetchPayloadByUID: map[uint32][]byte{100: []byte("Message-ID: <other@example.com>\r\n\r\nDifferent destination")}}, outcome: transport.MutationOutcomeUnknown, code: transport.CodeIMAPMoveOutcomeUnknown, moves: 1},
		{name: "native source absent before dispatch", config: fakeServerConfig{moveSupported: true, missingUIDs: []uint32{42}}, outcome: transport.MutationOutcomeNotStarted, code: transport.CodeIMAPMessageNotFound},
		{name: "native UID removed before MOVE OK no-op", config: fakeServerConfig{moveSupported: true, removeSourceOnTransfer: true}, outcome: transport.MutationOutcomeUnknown, code: transport.CodeIMAPMoveOutcomeUnknown, moves: 1},
		{name: "native advertised MOVE rejected BAD never copies", config: fakeServerConfig{capabilities: []string{"IMAP4rev1", "MOVE"}}, outcome: transport.MutationOutcomeUnknown, code: transport.CodeIMAPMoveOutcomeUnknown, moves: 1},
		{name: "native NO retains observed effects without fallback", config: fakeServerConfig{moveSupported: true, moveResponse: "<tag> NO transfer failed"}, outcome: transport.MutationOutcomePartial, code: transport.CodeIMAPMoveOutcomeUnknown, moves: 1},
		{name: "native lost response retains COPYUID", config: fakeServerConfig{moveSupported: true, dropMoveResponse: true}, outcome: transport.MutationOutcomeUnknown, code: transport.CodeIMAPMoveOutcomeUnknown, moves: 1, copyUID: true},
		{name: "native generation changes after dispatch", config: fakeServerConfig{moveSupported: true, changedUIDValidityAfter: 2, changedUIDValidityValue: 54321}, outcome: transport.MutationOutcomeUnknown, code: transport.CodeIMAPMoveOutcomeUnknown, moves: 1, copyUID: true},
		{name: "source generation mismatch before dispatch", config: fakeServerConfig{moveSupported: true, changedUIDValidityAfter: 1, changedUIDValidityValue: 54321}, outcome: transport.MutationOutcomeNotStarted, code: "mailbox_uidvalidity_changed"},
		{name: "fallback requires independent Message-ID", withoutID: true, outcome: transport.MutationOutcomeNotStarted, code: transport.CodeIMAPMessageUIDUnknown},
		{name: "fallback without COPYUID observes destination", config: fakeServerConfig{omitCopyUID: true}, outcome: transport.MutationOutcomeCompleted, copies: 1, stores: 1},
		{name: "fallback no-op COPY cannot delete source", config: fakeServerConfig{omitCopyUID: true, removeSourceOnTransfer: true}, outcome: transport.MutationOutcomeUnknown, code: transport.CodeIMAPMoveOutcomeUnknown, copies: 1},
		{name: "fallback without COPYUID rejects wrong destination header", config: fakeServerConfig{omitCopyUID: true, fetchPayloadByUID: map[uint32][]byte{100: []byte("Message-ID: <other@example.com>\r\n\r\nDifferent destination")}}, outcome: transport.MutationOutcomeUnknown, code: transport.CodeIMAPMoveOutcomeUnknown, copies: 1},
		{name: "fallback malformed COPYUID cannot delete source", config: fakeServerConfig{omitCopyUID: true, copyResponse: "<tag> OK [COPYUID 12345 43 100] copied"}, outcome: transport.MutationOutcomeUnknown, code: transport.CodeIMAPMoveOutcomeUnknown, copies: 1},
		{name: "fallback source rollover retains COPY without STORE", config: fakeServerConfig{changedUIDValidityAfter: 2, changedUIDValidityValue: 54321}, outcome: transport.MutationOutcomePartial, code: transport.CodeIMAPMoveOutcomeUnknown, copies: 1, copyUID: true},
		{name: "existing destination refuses both transfer paths", config: fakeServerConfig{searchMatchID: "<transfer@example.com>"}, outcome: transport.MutationOutcomeNotStarted, code: transport.CodeIMAPMoveOutcomeUnknown},
		{name: "ambiguous destination refuses both transfer paths", config: fakeServerConfig{searchMatchID: "<transfer@example.com>", searchUIDs: []uint32{41, 77}}, outcome: transport.MutationOutcomeNotStarted, code: transport.CodeIMAPMoveOutcomeUnknown},
		{name: "missing capability response never writes", config: fakeServerConfig{capabilityResponse: "<tag> OK capabilities"}, outcome: transport.MutationOutcomeNotStarted, code: transport.CodeIMAPResponseMalformed, earlyAcquireFailure: true},
		{name: "rejected capability response never writes", config: fakeServerConfig{capabilityResponse: "<tag> NO denied"}, outcome: transport.MutationOutcomeNotStarted, code: transport.CodeIMAPCommandRejected, earlyAcquireFailure: true},
		{name: "malformed capability response never writes", config: fakeServerConfig{capabilityResponse: "* CAPABILITY MOVE\r\n<tag> OK capabilities"}, outcome: transport.MutationOutcomeNotStarted, code: transport.CodeIMAPResponseMalformed, earlyAcquireFailure: true},
		{name: "bracketed data cannot spoof capability absence", config: fakeServerConfig{capabilityResponse: "* CAPABILITY [CAPABILITY IMAP4rev1]\r\n<tag> OK capabilities"}, outcome: transport.MutationOutcomeNotStarted, code: transport.CodeIMAPResponseMalformed, earlyAcquireFailure: true},
		{name: "duplicate capability responses never write", config: fakeServerConfig{capabilityResponse: "* CAPABILITY IMAP4rev1\r\n* CAPABILITY IMAP4rev1\r\n<tag> OK capabilities"}, outcome: transport.MutationOutcomeNotStarted, code: transport.CodeIMAPResponseMalformed, earlyAcquireFailure: true},
		{name: "capability work remains bounded", config: fakeServerConfig{capabilityResponse: strings.Repeat("* OK still working\r\n", maxFlagResponseCount) + "* CAPABILITY IMAP4rev1\r\n<tag> OK capabilities"}, outcome: transport.MutationOutcomeNotStarted, code: transport.CodeIMAPResponseMalformed, earlyAcquireFailure: true},
		{name: "greeting MOVE does not override authenticated absence", config: fakeServerConfig{moveSupported: true, capabilityResponse: "* CAPABILITY IMAP4rev1\r\n<tag> OK capabilities"}, outcome: transport.MutationOutcomeCompleted, copies: 1, stores: 1},
		{name: "authenticated LOGIN capabilities avoid another query", config: fakeServerConfig{moveSupported: true, loginResponse: "<tag> OK [CAPABILITY IMAP4rev1 MOVE] authenticated"}, withoutID: true, outcome: transport.MutationOutcomeCompleted, moves: 1, copyUID: true, authenticatedCapabilities: true},
		{name: "IMAP4rev2 includes MOVE", config: fakeServerConfig{moveSupported: true, capabilityResponse: "* CAPABILITY IMAP4rev2\r\n<tag> OK capabilities"}, withoutID: true, outcome: transport.MutationOutcomeCompleted, moves: 1, copyUID: true},
	} {
		for _, operation := range []string{"move", "delete"} {
			t.Run(test.name+"/"+operation, func(t *testing.T) {
				config := test.config
				config.authOK, config.trashMboxes, config.otherMboxes = true, []string{"Trash"}, []string{"INBOX", "Archive"}
				if len(config.fetchPayload) == 0 && !test.withoutID {
					config.fetchPayload = []byte("Message-ID: <transfer@example.com>\r\n\r\nOriginal source")
				}
				server := newFakeServer(t, config)
				client, cfg := newFakeClient(t, server)
				t.Cleanup(func() {
					if err := client.Close(); err != nil {
						t.Error(err)
					}
				})
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				messageID := "<transfer@example.com>"
				if test.withoutID {
					messageID = ""
				}
				var ev transport.MutationEvidence
				var err error
				if operation == "move" {
					ev, err = client.MoveMessage(ctx, cfg, "INBOX", 42, 12345, "Archive", messageID)
				} else {
					ev, err = client.DeleteMessage(ctx, cfg, "INBOX", 42, 12345, messageID)
				}
				wantOutcome := test.outcome
				// DELETE has no destination or transfer evidence until LIST resolves Trash.
				if operation == "delete" && test.earlyAcquireFailure {
					wantOutcome = ""
					if !reflect.DeepEqual(ev, transport.MutationEvidence{}) {
						t.Fatalf("failed connection invented DELETE evidence: %+v", ev)
					}
				}
				if transport.ErrorCode(err) != test.code || ev.Outcome != wantOutcome || (err == nil) != (test.code == "") {
					t.Fatalf("transfer result=%+v error=%v; want outcome=%s code=%s", ev, err, test.outcome, test.code)
				}
				if test.copyUID && (ev.CopyUIDResponse == "" || ev.CopySourceUID != 42 || ev.CopyDestinationUID != 100 || ev.DestinationUID != 100) {
					t.Errorf("COPYUID identity was discarded: %+v", ev)
				}
				if err != nil && test.outcome != transport.MutationOutcomeNotStarted {
					var outcome *transport.MutationOutcomeError
					if !errors.As(err, &outcome) || outcome.Evidence.OperationID != ev.OperationID || outcome.Evidence.Outcome != ev.Outcome {
						t.Errorf("typed outcome lost transfer evidence: %+v, %v", ev, err)
					}
				}
				counts := map[string]int{}
				for _, command := range server.Commands() {
					counts[command]++
				}
				if counts["UID MOVE"] != test.moves || counts["UID COPY"] != test.copies || counts["UID STORE"] != test.stores || counts["EXPUNGE"] != 0 || (test.code != "" && counts["UID EXPUNGE"] != 0) {
					t.Errorf("unexpected dispatch or replay: %v", server.Commands())
				}
				server.mu.Lock()
				capabilities := server.capabilityCalls
				server.mu.Unlock()
				wantedCapabilities := 1
				if test.authenticatedCapabilities {
					wantedCapabilities = 0
				}
				if capabilities != wantedCapabilities {
					t.Errorf("CAPABILITY calls=%d, want %d", capabilities, wantedCapabilities)
				}
				if test.outcome == transport.MutationOutcomeCompleted && test.moves > 0 && !strings.Contains(ev.ServerResponse, "OK") {
					t.Errorf("successful MOVE lost its completion response: %+v", ev)
				}
			})
		}
	}
}
