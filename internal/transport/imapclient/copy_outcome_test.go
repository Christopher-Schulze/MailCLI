package imapclient

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestDoCopyCommandResponsePreservesPreDispatchAndUncertainEvidence(t *testing.T) {
	command := `A001 UID COPY 42 "Archive"`
	tests := []struct {
		name              string
		command           string
		failDeadline      bool
		failWriteAfter    int
		closeAfterCommand bool
		wantBytes         []byte
		wantDispatched    bool
		wantOutcome       string
		wantErrorCode     string
	}{
		{
			name:           "deadline before write",
			command:        command,
			failDeadline:   true,
			wantDispatched: false,
			wantOutcome:    transport.MutationOutcomeNotStarted,
			wantErrorCode:  transport.CodeIMAPTimeout,
		},
		{
			name:           "line validation before write",
			command:        "A001 UID COPY 42\r\n\"Archive\"",
			wantDispatched: false,
			wantOutcome:    transport.MutationOutcomeNotStarted,
			wantErrorCode:  transport.CodeIMAPInvalidValue,
		},
		{
			name:           "partial command write",
			command:        command,
			failWriteAfter: 3,
			wantBytes:      []byte("A00"),
			wantDispatched: true,
			wantOutcome:    transport.MutationOutcomeUnknown,
			wantErrorCode:  transport.CodeIMAPCopyOutcomeUnknown,
		},
		{
			name:              "final reply lost",
			command:           command,
			closeAfterCommand: true,
			wantBytes:         []byte(command + "\r\n"),
			wantDispatched:    true,
			wantOutcome:       transport.MutationOutcomeUnknown,
			wantErrorCode:     transport.CodeIMAPCopyOutcomeUnknown,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			peerConn, clientConn := net.Pipe()
			wire := &copyOutcomeConn{
				Conn:           clientConn,
				failDeadline:   test.failDeadline,
				failWriteAfter: test.failWriteAfter,
			}
			sess := &session{
				conn: wire,
				br:   bufio.NewReader(wire),
				bw:   bufio.NewWriter(wire),
				nextTag: func() string {
					return "A001"
				},
			}
			peerResult := startCopyOutcomePeer(peerConn, test.closeAfterCommand)
			status, text, _, dispatched, commandErr := (&Client{}).doCopyCommandResponse(
				context.Background(), sess, test.command,
			)
			if commandErr == nil {
				t.Fatal("doCopyCommandResponse() error = nil, want an injected failure")
			}
			evidence, classifiedErr := copyCommandError(copyEvidenceForTest(), status, text, dispatched, commandErr)
			_ = wire.Close()
			peer := <-peerResult
			if peer.err != nil {
				t.Fatalf("fake IMAP peer read error = %v", peer.err)
			}
			if string(peer.data) != string(test.wantBytes) {
				t.Fatalf("fake IMAP peer received %q, want %q", peer.data, test.wantBytes)
			}
			if dispatched != test.wantDispatched {
				t.Fatalf("command dispatched = %v, want %v", dispatched, test.wantDispatched)
			}
			if evidence.Outcome != test.wantOutcome || evidence.UIDValidity != 12345 || evidence.ExpectedUIDValidity != 12345 {
				t.Fatalf("mutation evidence = %+v, want outcome %q with UIDVALIDITY evidence", evidence, test.wantOutcome)
			}
			if evidence.ServerResponse != "" {
				t.Fatalf("server response = %q, want no response before completion", evidence.ServerResponse)
			}
			if got := transport.ErrorCode(classifiedErr); got != test.wantErrorCode {
				t.Fatalf("classified error code = %q, want %q: %v", got, test.wantErrorCode, classifiedErr)
			}
			var outcomeErr *transport.MutationOutcomeError
			if !errors.As(classifiedErr, &outcomeErr) || outcomeErr.Evidence.Outcome != test.wantOutcome ||
				outcomeErr.Evidence.OperationID != evidence.OperationID || outcomeErr.Evidence.UIDValidity != 12345 {
				t.Fatalf("mutation error evidence = %+v, want preserved operation and UIDVALIDITY: %v", outcomeErr, classifiedErr)
			}
			if !sess.dirty {
				t.Fatal("COPY failure left the interrupted session clean")
			}
		})
	}
}

func copyEvidenceForTest() transport.MutationEvidence {
	return transport.MutationEvidence{
		OperationID:         "copy-test-operation",
		Outcome:             transport.MutationOutcomeNotStarted,
		SourceAccount:       "user",
		Command:             "COPY",
		Mailbox:             "INBOX",
		TargetMailbox:       "Archive",
		UID:                 42,
		UIDValidity:         12345,
		ExpectedUIDValidity: 12345,
	}
}

type copyOutcomeConn struct {
	net.Conn
	failDeadline   bool
	failWriteAfter int
}

func (c *copyOutcomeConn) SetDeadline(deadline time.Time) error {
	if c.failDeadline {
		return errors.New("injected IMAP deadline failure")
	}
	return c.Conn.SetDeadline(deadline)
}

func (c *copyOutcomeConn) Write(data []byte) (int, error) {
	if c.failWriteAfter > 0 {
		written, err := c.Conn.Write(data[:c.failWriteAfter])
		if err != nil {
			return written, err
		}
		return written, errors.New("injected IMAP write failure")
	}
	return c.Conn.Write(data)
}

type copyOutcomePeerResult struct {
	data []byte
	err  error
}

func startCopyOutcomePeer(conn net.Conn, closeAfterCommand bool) <-chan copyOutcomePeerResult {
	result := make(chan copyOutcomePeerResult, 1)
	go func() {
		peer := copyOutcomePeerResult{}
		defer func() {
			peer.err = errors.Join(peer.err, conn.Close())
			result <- peer
		}()
		if closeAfterCommand {
			line, err := bufio.NewReader(conn).ReadString('\n')
			peer.data, peer.err = []byte(line), err
		} else {
			peer.data, peer.err = io.ReadAll(conn)
		}
	}()
	return result
}
