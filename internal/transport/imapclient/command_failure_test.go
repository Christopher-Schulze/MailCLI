package imapclient

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"mailcli/internal/transport"
)

type wireCommandFailureCase struct {
	name          string
	command       string
	rejectionCode string
	call          func(context.Context, *Client, *session) error
}

func wireCommandFailureCases() []wireCommandFailureCase {
	return []wireCommandFailureCase{
		{name: "LIST", command: "LIST", rejectionCode: transport.CodeIMAPCommandRejected, call: func(ctx context.Context, client *Client, sess *session) error {
			_, err := client.doList(ctx, sess, "A001")
			return err
		}},
		{name: "STATUS", command: "STATUS", rejectionCode: transport.CodeIMAPCommandRejected, call: func(ctx context.Context, client *Client, sess *session) error {
			_, err := client.doStatus(ctx, sess, "A001", "INBOX")
			return err
		}},
		{name: "SELECT", command: "SELECT", rejectionCode: transport.CodeIMAPCommandRejected, call: func(ctx context.Context, client *Client, sess *session) error {
			_, err := client.doSelectInfo(ctx, sess, "A001", "INBOX")
			return err
		}},
		{name: "SEARCH", command: "SEARCH", rejectionCode: transport.CodeIMAPCommandRejected, call: func(ctx context.Context, client *Client, sess *session) error {
			_, err := client.doSearch(ctx, sess, "A001", "<wire@example.com>")
			return err
		}},
		{name: "UID SEARCH", command: "UID SEARCH", rejectionCode: transport.CodeIMAPCommandRejected, call: func(ctx context.Context, client *Client, sess *session) error {
			_, err := client.doUIDSearchCriteria(ctx, sess, "A001", "ALL")
			return err
		}},
	}
}

func taggedRejectionCommandCases() []wireCommandFailureCase {
	commands := wireCommandFailureCases()
	return append(commands,
		wireCommandFailureCase{name: "LOGIN", command: "LOGIN", rejectionCode: transport.CodeIMAPAuthFailed, call: func(ctx context.Context, client *Client, sess *session) error {
			return client.doLogin(ctx, sess, "A001", transport.ImapConfig{Username: "user", Password: "pass"})
		}},
		wireCommandFailureCase{name: "APPEND continuation", command: "APPEND", rejectionCode: transport.CodeIMAPCommandRejected, call: func(ctx context.Context, client *Client, sess *session) error {
			return client.doAppend(ctx, sess, "A001", "Sent", strings.NewReader(""), 0)
		}},
	)
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
	for _, command := range taggedRejectionCommandCases() {
		for _, status := range []string{"NO", "BAD"} {
			t.Run(command.name+"/"+status, func(t *testing.T) {
				sess, peer, closePeer := newWireCommandFailureSession(t)
				peerDone := serveOneWireCommand(peer, closePeer, "A001 "+status+" [UNAVAILABLE] server temporarily unavailable\r\n")
				err := command.call(context.Background(), New(), sess)
				requireFakePeerComplete(t, peerDone)
				wantCode := command.rejectionCode
				if status == "BAD" {
					wantCode = transport.CodeIMAPCommandRejected
				}
				if got := transport.ErrorCode(err); got != wantCode {
					t.Fatalf("%s %s error code = %q, want %q: %v", command.name, status, got, wantCode, err)
				}
				rejection, ok := transport.TaggedIMAPRejection(err)
				if !ok || rejection.Command != command.command || rejection.Status != status || rejection.ResponseCode == nil || *rejection.ResponseCode != "UNAVAILABLE" || rejection.Text != "[UNAVAILABLE] server temporarily unavailable" {
					t.Fatalf("%s %s rejection evidence = %+v, present=%t", command.name, status, rejection, ok)
				}
				if sess.dirty {
					t.Fatalf("%s %s rejection dirtied the session", command.name, status)
				}
			})
		}
	}
}

func TestTaggedRejectionMappingsRequireCommandContext(t *testing.T) {
	tests := []struct {
		command string
		status  string
		code    string
		want    string
	}{
		{command: "STATUS", code: "NONEXISTENT", want: transport.CodeIMAPMailboxNotFound},
		{command: "SELECT", code: "NONEXISTENT", want: transport.CodeIMAPMailboxNotFound},
		{command: "APPEND", code: "NONEXISTENT", want: transport.CodeIMAPMailboxNotFound},
		{command: "APPEND", code: "OVERQUOTA", want: transport.CodeIMAPQuotaExceeded},
		{command: "LIST", code: "NONEXISTENT", want: transport.CodeIMAPCommandRejected},
		{command: "SEARCH", code: "OVERQUOTA", want: transport.CodeIMAPCommandRejected},
		{command: "SEARCH", code: "FUTURECODE", want: transport.CodeIMAPCommandRejected},
		{command: "STATUS", status: "BAD", code: "NONEXISTENT", want: transport.CodeIMAPCommandRejected},
		{command: "APPEND", status: "BAD", code: "OVERQUOTA", want: transport.CodeIMAPCommandRejected},
		{command: "SEARCH", status: "BAD", code: "UNAVAILABLE", want: transport.CodeIMAPCommandRejected},
		{command: "SELECT", code: "UIDVALIDITY", want: transport.CodeIMAPCommandRejected},
	}
	commands := taggedRejectionCommandCases()
	for _, test := range tests {
		t.Run(test.command+"/"+test.code, func(t *testing.T) {
			var selected *wireCommandFailureCase
			for i := range commands {
				if commands[i].command == test.command {
					selected = &commands[i]
					break
				}
			}
			if selected == nil {
				t.Fatalf("missing fake command case for %s", test.command)
			}
			status := test.status
			if status == "" {
				status = "NO"
			}
			sess, peer, closePeer := newWireCommandFailureSession(t)
			peerDone := serveOneWireCommand(peer, closePeer, "A001 "+status+" ["+test.code+"] rejected\r\n")
			err := selected.call(context.Background(), New(), sess)
			requireFakePeerComplete(t, peerDone)
			if got := transport.ErrorCode(err); got != test.want {
				t.Fatalf("%s %s [%s] error code = %q, want %q: %v", test.command, status, test.code, got, test.want, err)
			}
			rejection, ok := transport.TaggedIMAPRejection(err)
			if !ok || rejection.Status != status || rejection.ResponseCode == nil || *rejection.ResponseCode != test.code {
				t.Fatalf("%s %s [%s] response-code evidence = %+v, present=%t", test.command, status, test.code, rejection, ok)
			}
		})
	}
}

func TestTaggedIMAPRejectionFindsEvidenceThroughTransportWrappers(t *testing.T) {
	rejection := &transport.IMAPCommandRejection{Command: "LIST", Status: "NO", Text: "temporarily unavailable"}
	inner := &transport.TransportError{Code: transport.CodeIMAPCommandRejected, IMAPRejection: rejection}
	outer := &transport.TransportError{Code: transport.CodeIMAPTimeout, Err: inner}
	tests := []struct {
		name string
		err  error
	}{
		{name: "typed wrapper", err: outer},
		{name: "joined wrapper", err: errors.Join(&transport.TransportError{Code: transport.CodeIMAPDisconnected}, inner)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := transport.TaggedIMAPRejection(test.err)
			if !ok || got != rejection {
				t.Fatalf("TaggedIMAPRejection() = (%+v, %t), want retained evidence %+v", got, ok, rejection)
			}
		})
	}
}

func TestSelectTaggedRejectionPrecedesMetadataParsing(t *testing.T) {
	sess, peer, closePeer := newWireCommandFailureSession(t)
	response := "A001 NO [UIDVALIDITY not-a-number] mailbox state changed\r\n"
	peerDone := serveOneWireCommand(peer, closePeer, response)
	_, err := New().doSelectInfo(context.Background(), sess, "A001", "INBOX")
	requireFakePeerComplete(t, peerDone)
	if got := transport.ErrorCode(err); got != transport.CodeIMAPCommandRejected {
		t.Fatalf("SELECT tagged rejection error code = %q, want %q: %v", got, transport.CodeIMAPCommandRejected, err)
	}
	rejection, ok := transport.TaggedIMAPRejection(err)
	if !ok || rejection.Command != "SELECT" || rejection.Status != "NO" || rejection.ResponseCode == nil || *rejection.ResponseCode != "UIDVALIDITY" || rejection.Text != "[UIDVALIDITY not-a-number] mailbox state changed" {
		t.Fatalf("SELECT tagged rejection evidence = %+v, present=%t", rejection, ok)
	}
	if sess.dirty {
		t.Fatal("definite SELECT rejection dirtied the session")
	}
}

func TestTaggedRejectionParserUsesOnlyLeadingResponseCode(t *testing.T) {
	tests := []struct {
		text string
		want string
	}{
		{text: "[OVERQUOTA] over limit", want: "OVERQUOTA"},
		{text: "[future-code argument] detail", want: "FUTURE-CODE"},
		{text: "server says [OVERQUOTA] later", want: ""},
		{text: "[OVERQUOTA]suffix", want: ""},
		{text: "[] empty code", want: ""},
		{text: "[OVERQUOTA]", want: "OVERQUOTA"},
	}
	for _, test := range tests {
		t.Run(test.text, func(t *testing.T) {
			got := leadingTaggedResponseCode(test.text)
			if test.want == "" {
				if got != nil {
					t.Fatalf("leadingTaggedResponseCode(%q) = %q, want no code", test.text, *got)
				}
				return
			}
			if got == nil || *got != test.want {
				t.Fatalf("leadingTaggedResponseCode(%q) = %v, want %q", test.text, got, test.want)
			}
		})
	}
}

func TestTaggedRejectionTextStripsControlsAndTruncatesOnUTF8Boundary(t *testing.T) {
	if got := boundedTaggedResponseText("a\x00b\tc\r\n\u0085d"); got != "abcd" {
		t.Fatalf("boundedTaggedResponseText() = %q, want control-free text %q", got, "abcd")
	}
	got := boundedTaggedResponseText(strings.Repeat("é", maxTaggedIMAPRejectionTextBytes/2) + "x")
	if len(got) != maxTaggedIMAPRejectionTextBytes || !strings.HasSuffix(got, "é") || !utf8.ValidString(got) {
		t.Fatalf("boundedTaggedResponseText() length=%d valid=%t suffix=%q", len(got), utf8.ValidString(got), got[len(got)-3:])
	}
}

func TestUnknownTaggedStatusIsMalformed(t *testing.T) {
	for _, command := range taggedRejectionCommandCases() {
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
