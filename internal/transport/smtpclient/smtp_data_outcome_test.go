package smtpclient

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestSendDataDistinguishesPreTerminatorFailures(t *testing.T) {
	partial := bytes.Repeat([]byte{'x'}, 8192)
	message := []byte("Subject: outcome test\r\n\r\nbody\r\n")
	terminated := append(append([]byte(nil), message...), []byte(".\r\n")...)

	tests := []struct {
		name                    string
		reader                  io.Reader
		size                    int64
		failDeadlineCall        int
		failTerminatorWrite     bool
		failPayloadWrite        bool
		peerReadBytes           int
		wantPeerBytes           []byte
		wantPeerPrefix          []byte
		wantCode                string
		wantSubmissionUncertain bool
	}{
		{
			name:             "transfer deadline before payload",
			reader:           bytes.NewReader(partial),
			size:             int64(len(partial)),
			failDeadlineCall: 2,
			peerReadBytes:    -1,
			wantCode:         transport.CodeSMTPDataIncomplete,
		},
		{
			name:           "source read after partial payload",
			reader:         &smtpFailureReader{data: partial, err: errors.New("injected source read")},
			size:           int64(len(partial) + 1),
			peerReadBytes:  -1,
			wantPeerPrefix: partial,
			wantCode:       transport.CodeSMTPSourceInvalid,
		},
		{
			name:           "short source",
			reader:         bytes.NewReader(partial),
			size:           int64(len(partial) + 1),
			peerReadBytes:  -1,
			wantPeerPrefix: partial,
			wantCode:       transport.CodeSMTPSourceInvalid,
		},
		{
			name: "socket payload write failure", reader: bytes.NewReader(partial), size: int64(len(partial)),
			failPayloadWrite: true, peerReadBytes: -1, wantPeerPrefix: partial,
			wantCode: transport.CodeSMTPDataIncomplete,
		},
		{
			name:   "socket failure takes precedence over simultaneous source error",
			reader: &smtpFailureReader{data: partial, err: io.ErrUnexpectedEOF, finalReadError: true}, size: int64(len(partial)),
			failPayloadWrite: true, peerReadBytes: -1, wantPeerPrefix: partial,
			wantCode: transport.CodeSMTPDataIncomplete,
		},
		{
			name:                    "partial terminator write",
			reader:                  bytes.NewReader(message),
			size:                    int64(len(message)),
			failTerminatorWrite:     true,
			peerReadBytes:           -1,
			wantPeerBytes:           terminated[:len(terminated)-1],
			wantCode:                transport.CodeSMTPSubmissionUnknown,
			wantSubmissionUncertain: true,
		},
		{
			name:                    "final reply lost after complete terminator",
			reader:                  bytes.NewReader(message),
			size:                    int64(len(message)),
			peerReadBytes:           len(terminated),
			wantPeerBytes:           terminated,
			wantCode:                transport.CodeSMTPSubmissionUnknown,
			wantSubmissionUncertain: true,
		},
		{
			name:                    "final reply deadline after complete terminator",
			reader:                  bytes.NewReader(message),
			size:                    int64(len(message)),
			failDeadlineCall:        3,
			peerReadBytes:           len(terminated),
			wantPeerBytes:           terminated,
			wantCode:                transport.CodeSMTPSubmissionUnknown,
			wantSubmissionUncertain: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serverConn, clientConn := net.Pipe()
			clientWire := &smtpOutcomeConn{
				Conn:                clientConn,
				failDeadlineCall:    test.failDeadlineCall,
				failTerminatorWrite: test.failTerminatorWrite,
				failPayloadWrite:    test.failPayloadWrite,
			}
			peerResult := startSMTPDataPeer(serverConn, test.peerReadBytes, false)
			client, err := smtp.NewClient(clientWire, "fake.invalid")
			if err != nil {
				t.Fatalf("smtp.NewClient() error = %v", err)
			}

			_, gotErr := sendData(clientWire, context.Background(), client, test.reader, test.size)
			_ = client.Close()
			peer := <-peerResult
			if peer.err != nil {
				t.Fatalf("fake SMTP peer read error = %v", peer.err)
			}
			if test.wantPeerPrefix != nil {
				if len(peer.data) == 0 || len(peer.data) > len(test.wantPeerPrefix) || !bytes.Equal(peer.data, test.wantPeerPrefix[:len(peer.data)]) {
					t.Fatalf("fake SMTP peer received bytes that are not a source prefix: %x", peer.data)
				}
			} else if !bytes.Equal(peer.data, test.wantPeerBytes) {
				t.Fatalf("fake SMTP peer received %d bytes, want %d", len(peer.data), len(test.wantPeerBytes))
			}
			if gotErr == nil || transport.ErrorCode(gotErr) != test.wantCode {
				t.Fatalf("sendData() error = %v, want code %q", gotErr, test.wantCode)
			}
			var submissionErr *transport.SubmissionError
			if got := errors.As(gotErr, &submissionErr); got != test.wantSubmissionUncertain {
				t.Fatalf("SubmissionError present = %t, want %t: %v", got, test.wantSubmissionUncertain, gotErr)
			}
		})
	}
}

type smtpOutcomeConn struct {
	net.Conn
	deadlineCalls       int
	failDeadlineCall    int
	failTerminatorWrite bool
	failPayloadWrite    bool
	writeCalls          int
}

func (c *smtpOutcomeConn) SetDeadline(deadline time.Time) error {
	c.deadlineCalls++
	if c.failDeadlineCall != 0 && c.deadlineCalls == c.failDeadlineCall {
		return errors.New("injected SMTP deadline failure")
	}
	return c.Conn.SetDeadline(deadline)
}

func (c *smtpOutcomeConn) Write(data []byte) (int, error) {
	c.writeCalls++
	if c.failPayloadWrite && c.writeCalls == 2 {
		c.failPayloadWrite = false
		written, err := c.Conn.Write(data[:4])
		if err != nil {
			return written, err
		}
		return written, &net.OpError{Op: "write", Net: "pipe", Err: io.ErrClosedPipe}
	}
	if c.failTerminatorWrite && bytes.HasSuffix(data, []byte(".\r\n")) {
		c.failTerminatorWrite = false
		written, err := c.Conn.Write(data[:len(data)-1])
		if err != nil {
			return written, err
		}
		return written, errors.New("injected SMTP terminator write failure")
	}
	return c.Conn.Write(data)
}

type smtpFailureReader struct {
	data           []byte
	err            error
	finalReadError bool
}

func (r *smtpFailureReader) Read(buffer []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	written := copy(buffer, r.data)
	r.data = r.data[written:]
	if r.finalReadError && len(r.data) == 0 {
		return written, r.err
	}
	return written, nil
}

type smtpDataPeerResult struct {
	data []byte
	err  error
}

func startSMTPDataPeer(conn net.Conn, readBytes int, accept bool) <-chan smtpDataPeerResult {
	result := make(chan smtpDataPeerResult, 1)
	go func() {
		peer := smtpDataPeerResult{}
		defer func() {
			peer.err = errors.Join(peer.err, conn.Close())
			result <- peer
		}()
		if _, err := io.WriteString(conn, "220 fake SMTP ready\r\n"); err != nil {
			peer.err = err
			return
		}
		reader := bufio.NewReader(conn)
		command, err := reader.ReadString('\n')
		if err != nil {
			peer.err = err
			return
		}
		if strings.TrimSpace(command) != "DATA" {
			peer.err = errors.New("fake SMTP peer expected DATA")
			return
		}
		if _, err := io.WriteString(conn, "354 continue\r\n"); err != nil {
			peer.err = err
			return
		}
		if readBytes < 0 {
			peer.data, peer.err = io.ReadAll(reader)
		} else {
			peer.data = make([]byte, readBytes)
			_, peer.err = io.ReadFull(reader, peer.data)
		}
		if accept && peer.err == nil {
			_, peer.err = io.WriteString(conn, "250 accepted\r\n")
		}
	}()
	return result
}

type smtpCountedReader struct {
	reader    io.Reader
	bytesRead int64
}

func (r *smtpCountedReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	r.bytesRead += int64(n)
	return n, err
}

func TestSendDataBoundsSourceReads(t *testing.T) {
	message := append(bytes.Repeat([]byte{'x'}, 4094), '\r', '\n')
	sourceErr := &net.OpError{Op: "read", Net: "source", Err: io.ErrUnexpectedEOF}
	tests := []struct {
		name     string
		reader   io.Reader
		size     int64
		wantRead int64
		wantErr  error
		accept   bool
	}{
		{name: "exact", reader: bytes.NewReader(message), size: 4096, wantRead: 4096, accept: true},
		{name: "short", reader: bytes.NewReader(message[:4095]), size: 4096, wantRead: 4095},
		{name: "long", reader: bytes.NewReader(append(append([]byte(nil), message...), 'y', 'z')), size: 4096, wantRead: 4097},
		{name: "read error", reader: &smtpFailureReader{data: message[:4095], err: sourceErr}, size: 4096, wantRead: 4095, wantErr: sourceErr},
		{name: "probe error", reader: &smtpFailureReader{data: message, err: sourceErr}, size: 4096, wantRead: 4096, wantErr: sourceErr},
		{name: "empty exact", reader: bytes.NewReader(nil), size: 0, accept: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serverConn, clientConn := net.Pipe()
			if err := serverConn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			wire := append(append([]byte(nil), message[:test.size]...), []byte(".\r\n")...)
			if test.size == 0 {
				wire = []byte("\r\n.\r\n")
			}
			readBytes := -1
			if test.accept {
				readBytes = len(wire)
			}
			peerResult := startSMTPDataPeer(serverConn, readBytes, test.accept)
			client, err := smtp.NewClient(clientConn, "fake.invalid")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			reader := &smtpCountedReader{reader: test.reader}
			response, gotErr := sendData(clientConn, ctx, client, reader, test.size)
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
			peer := <-peerResult
			if peer.err != nil {
				t.Fatal(peer.err)
			}
			if reader.bytesRead != test.wantRead {
				t.Fatalf("source bytes = %d, want %d", reader.bytesRead, test.wantRead)
			}
			if test.accept {
				if gotErr != nil || response != "250 accepted" || !bytes.Equal(peer.data, wire) {
					t.Fatalf("exact DATA: response=%q err=%v wire=%q", response, gotErr, peer.data)
				}
				return
			}
			if transport.ErrorCode(gotErr) != transport.CodeSMTPSourceInvalid || (test.wantErr != nil && !errors.Is(gotErr, test.wantErr)) {
				t.Fatalf("source error = %v, want integrity error preserving %v", gotErr, test.wantErr)
			}
			var submission *transport.SubmissionError
			if errors.As(gotErr, &submission) || bytes.Contains(peer.data, []byte("\r\n.\r\n")) || !bytes.HasPrefix(message, peer.data) {
				t.Fatalf("invalid source terminated or changed payload: error=%v wire=%q", gotErr, peer.data)
			}
		})
	}
}
