package imapclient

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"mailcli/internal/transport"
)

type wireCommandFailureCase struct {
	name          string
	rejectionCode string
	call          func(context.Context, *Client, *session) error
}

func wireCommandFailureCases() []wireCommandFailureCase {
	return []wireCommandFailureCase{
		{name: "LIST", rejectionCode: transport.CodeIMAPSentMailboxNotFound, call: func(ctx context.Context, client *Client, sess *session) error {
			_, err := client.doList(ctx, sess, "A001")
			return err
		}},
		{name: "STATUS", rejectionCode: transport.CodeIMAPMailboxNotFound, call: func(ctx context.Context, client *Client, sess *session) error {
			_, err := client.doStatus(ctx, sess, "A001", "INBOX")
			return err
		}},
		{name: "SELECT", rejectionCode: transport.CodeIMAPMailboxNotFound, call: func(ctx context.Context, client *Client, sess *session) error {
			_, err := client.doSelectInfo(ctx, sess, "A001", "INBOX")
			return err
		}},
		{name: "SEARCH", rejectionCode: transport.CodeIMAPAppendFailed, call: func(ctx context.Context, client *Client, sess *session) error {
			_, err := client.doSearch(ctx, sess, "A001", "<wire@example.com>")
			return err
		}},
		{name: "UID SEARCH", rejectionCode: transport.CodeIMAPMutationFailed, call: func(ctx context.Context, client *Client, sess *session) error {
			_, err := client.doUIDSearchCriteria(ctx, sess, "A001", "ALL")
			return err
		}},
	}
}

func newWireCommandFailureSession(t *testing.T) (*session, net.Conn, func() error) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	var closeOnce sync.Once
	var closeErr error
	closeServer := func() error {
		closeOnce.Do(func() { closeErr = serverConn.Close() })
		return closeErr
	}
	t.Cleanup(func() {
		if err := closeServer(); err != nil {
			t.Errorf("close fake IMAP peer: %v", err)
		}
	})
	t.Cleanup(func() {
		if err := clientConn.Close(); err != nil {
			t.Errorf("close command session: %v", err)
		}
	})
	return &session{conn: clientConn, br: bufio.NewReader(clientConn), bw: bufio.NewWriter(clientConn)}, serverConn, closeServer
}

func serveOneWireCommand(peer net.Conn, closePeer func() error, response string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, readErr := bufio.NewReader(peer).ReadString('\n')
		if readErr != nil {
			done <- errors.Join(readErr, closePeer())
			return
		}
		if response != "" {
			if _, err := io.WriteString(peer, response); err != nil {
				done <- errors.Join(err, closePeer())
				return
			}
		}
		done <- closePeer()
	}()
	return done
}

func requireFakePeerComplete(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("fake IMAP peer: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake IMAP peer did not complete")
	}
}

func TestWireCommandWriteErrorsUseDisconnectedCode(t *testing.T) {
	for _, command := range wireCommandFailureCases() {
		t.Run(command.name, func(t *testing.T) {
			sess, _, closePeer := newWireCommandFailureSession(t)
			if err := closePeer(); err != nil {
				t.Fatalf("close fake IMAP peer before write: %v", err)
			}
			err := command.call(context.Background(), New(), sess)
			if transport.ErrorCode(err) != transport.CodeIMAPDisconnected || !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("%s write error = %v, want typed disconnect preserving pipe error", command.name, err)
			}
		})
	}
}

func TestWireCommandReadErrorsUseDisconnectedCode(t *testing.T) {
	for _, command := range wireCommandFailureCases() {
		t.Run(command.name, func(t *testing.T) {
			sess, peer, closePeer := newWireCommandFailureSession(t)
			peerDone := serveOneWireCommand(peer, closePeer, "")
			err := command.call(context.Background(), New(), sess)
			requireFakePeerComplete(t, peerDone)
			if transport.ErrorCode(err) != transport.CodeIMAPDisconnected || !errors.Is(err, io.EOF) {
				t.Fatalf("%s read error = %v, want typed disconnect preserving EOF", command.name, err)
			}
		})
	}
}

func TestTaggedServerRejectionsRequireNOorBAD(t *testing.T) {
	for _, command := range wireCommandFailureCases() {
		for _, status := range []string{"NO", "BAD"} {
			t.Run(command.name+"/"+status, func(t *testing.T) {
				sess, peer, closePeer := newWireCommandFailureSession(t)
				peerDone := serveOneWireCommand(peer, closePeer, "A001 "+status+" rejected\r\n")
				err := command.call(context.Background(), New(), sess)
				requireFakePeerComplete(t, peerDone)
				if got := transport.ErrorCode(err); got != command.rejectionCode {
					t.Fatalf("%s %s error code = %q, want %q: %v", command.name, status, got, command.rejectionCode, err)
				}
				if sess.dirty {
					t.Fatalf("%s %s rejection dirtied the session", command.name, status)
				}
			})
		}
	}
}

func TestUnknownTaggedStatusIsMalformed(t *testing.T) {
	for _, command := range wireCommandFailureCases() {
		t.Run(command.name, func(t *testing.T) {
			sess, peer, closePeer := newWireCommandFailureSession(t)
			peerDone := serveOneWireCommand(peer, closePeer, "A001 BYE closing\r\n")
			err := command.call(context.Background(), New(), sess)
			requireFakePeerComplete(t, peerDone)
			if transport.ErrorCode(err) != transport.CodeIMAPResponseMalformed || !sess.dirty {
				t.Fatalf("%s unknown tagged status = %v, want malformed response and discarded session", command.name, err)
			}
		})
	}
}

func TestWireCommandCancellationUsesCanceledCode(t *testing.T) {
	for _, command := range wireCommandFailureCases() {
		t.Run(command.name, func(t *testing.T) {
			sess, _, _ := newWireCommandFailureSession(t)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := command.call(ctx, New(), sess)
			if transport.ErrorCode(err) != transport.CodeIMAPCanceled || !errors.Is(err, context.Canceled) {
				t.Fatalf("%s canceled command error = %v, want typed cancellation preserving cause", command.name, err)
			}
		})
	}
}

func TestPublicReadCommandsClassifyServerDisconnect(t *testing.T) {
	tests := []struct {
		name      string
		dropAfter int
		call      func(*Client, transport.ImapConfig) error
	}{
		{name: "LIST", dropAfter: 2, call: func(client *Client, cfg transport.ImapConfig) error {
			_, err := client.ListMailboxes(context.Background(), cfg)
			return err
		}},
		{name: "STATUS", dropAfter: 2, call: func(client *Client, cfg transport.ImapConfig) error {
			_, err := client.CheckStatus(context.Background(), cfg, "INBOX")
			return err
		}},
		{name: "UID SEARCH", dropAfter: 3, call: func(client *Client, cfg transport.ImapConfig) error {
			_, _, _, err := client.SearchUID(context.Background(), cfg, "INBOX", "<wire@example.com>")
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := newFakeServer(t, fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX"}, dropAfterCommands: test.dropAfter})
			client, cfg := newFakeClient(t, srv)
			err := test.call(client, cfg)
			if transport.ErrorCode(err) != transport.CodeIMAPDisconnected {
				t.Fatalf("%s disconnect error = %v, want %s", test.name, err, transport.CodeIMAPDisconnected)
			}
		})
	}
}

func TestCommandIOClassificationPreservesTimeoutAndMalformedResponse(t *testing.T) {
	timeoutErr := wrapCommandIOError(context.Background(), context.DeadlineExceeded, "IMAP test read")
	if transport.ErrorCode(timeoutErr) != transport.CodeIMAPTimeout || !errors.Is(timeoutErr, context.DeadlineExceeded) {
		t.Fatalf("timeout classification = %v", timeoutErr)
	}
	malformedCause := &malformedResponseError{err: io.ErrUnexpectedEOF}
	malformedErr := wrapCommandIOError(context.Background(), malformedCause, "IMAP test read")
	if transport.ErrorCode(malformedErr) != transport.CodeIMAPResponseMalformed || !errors.Is(malformedErr, io.ErrUnexpectedEOF) {
		t.Fatalf("malformed response classification = %v", malformedErr)
	}
}
