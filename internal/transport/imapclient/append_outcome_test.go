package imapclient

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestDoAppendDistinguishesPreTerminatorFailures(t *testing.T) {
	partial := bytes.Repeat([]byte{'x'}, 5000)
	oversized := bytes.Repeat([]byte{'y'}, 4096)
	literal := []byte("literal")

	tests := []struct {
		name             string
		reader           io.Reader
		size             int64
		failDeadlineCall int
		failWriteCall    int
		failWriteAfter   int
		peerReadBytes    int
		wantPeerBytes    []byte
		wantPeerPrefix   []byte
		wantCode         string
	}{
		{
			name:             "transfer deadline before literal",
			reader:           bytes.NewReader(literal),
			size:             int64(len(literal)),
			failDeadlineCall: 2,
			peerReadBytes:    -1,
			wantCode:         transport.CodeIMAPAppendIncomplete,
		},
		{
			name:           "source read after partial literal",
			reader:         &appendFailureReader{data: partial, err: errors.New("injected source read")},
			size:           int64(len(partial) + 1000),
			peerReadBytes:  -1,
			wantPeerPrefix: partial,
			wantCode:       transport.CodeIMAPAppendIncomplete,
		},
		{
			name:           "short source",
			reader:         bytes.NewReader(partial),
			size:           int64(len(partial) + 1000),
			peerReadBytes:  -1,
			wantPeerPrefix: partial,
			wantCode:       transport.CodeIMAPAppendIncomplete,
		},
		{
			name:           "literal flush after partial write",
			reader:         bytes.NewReader(literal),
			size:           int64(len(literal)),
			failWriteCall:  2,
			failWriteAfter: 3,
			peerReadBytes:  -1,
			wantPeerBytes:  literal[:3],
			wantCode:       transport.CodeIMAPAppendIncomplete,
		},
		{
			name:          "oversized source cannot supply the terminator",
			reader:        bytes.NewReader(append(append([]byte(nil), oversized...), '\r', '\n')),
			size:          int64(len(oversized)),
			peerReadBytes: -1,
			wantPeerBytes: oversized,
			wantCode:      transport.CodeIMAPAppendIncomplete,
		},
		{
			name:           "partial terminator write",
			reader:         bytes.NewReader(literal),
			size:           int64(len(literal)),
			failWriteCall:  3,
			failWriteAfter: 1,
			peerReadBytes:  -1,
			wantPeerBytes:  append(append([]byte(nil), literal...), '\r'),
			wantCode:       transport.CodeIMAPAppendOutcomeUnknown,
		},
		{
			name:          "final reply lost after complete terminator",
			reader:        bytes.NewReader(literal),
			size:          int64(len(literal)),
			peerReadBytes: len(literal) + 2,
			wantPeerBytes: append(append([]byte(nil), literal...), '\r', '\n'),
			wantCode:      transport.CodeIMAPAppendOutcomeUnknown,
		},
		{
			name:             "final reply deadline after complete terminator",
			reader:           bytes.NewReader(literal),
			size:             int64(len(literal)),
			failDeadlineCall: 3,
			peerReadBytes:    len(literal) + 2,
			wantPeerBytes:    append(append([]byte(nil), literal...), '\r', '\n'),
			wantCode:         transport.CodeIMAPAppendOutcomeUnknown,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serverConn, clientConn := net.Pipe()
			clientWire := &appendOutcomeConn{
				Conn:             clientConn,
				failDeadlineCall: test.failDeadlineCall,
				failWriteCall:    test.failWriteCall,
				failWriteAfter:   test.failWriteAfter,
			}
			sess := &session{
				conn: clientWire,
				br:   bufio.NewReader(clientWire),
				bw:   bufio.NewWriter(clientWire),
				nextTag: func() string {
					return "A001"
				},
			}
			peerResult := startAppendOutcomePeer(serverConn, test.peerReadBytes)
			gotErr := (&Client{}).doAppend(context.Background(), sess, "A001", "Sent", test.reader, test.size)
			_ = clientWire.Close()
			peer := <-peerResult
			if peer.err != nil {
				t.Fatalf("fake IMAP peer read error = %v", peer.err)
			}
			if test.wantPeerPrefix != nil {
				if len(peer.data) == 0 || len(peer.data) > len(test.wantPeerPrefix) || !bytes.Equal(peer.data, test.wantPeerPrefix[:len(peer.data)]) {
					t.Fatalf("fake IMAP peer received bytes that are not a source prefix: %x", peer.data)
				}
			} else if !bytes.Equal(peer.data, test.wantPeerBytes) {
				t.Fatalf("fake IMAP peer received %d bytes, want %d", len(peer.data), len(test.wantPeerBytes))
			}
			if gotErr == nil || transport.ErrorCode(gotErr) != test.wantCode {
				t.Fatalf("doAppend() error = %v, want code %q", gotErr, test.wantCode)
			}
			if !sess.dirty {
				t.Fatal("APPEND failure left the interrupted session clean")
			}
		})
	}
}

type appendOutcomeConn struct {
	net.Conn
	deadlineCalls    int
	failDeadlineCall int
	writeCalls       int
	failWriteCall    int
	failWriteAfter   int
}

func (c *appendOutcomeConn) SetDeadline(deadline time.Time) error {
	c.deadlineCalls++
	if c.failDeadlineCall != 0 && c.deadlineCalls == c.failDeadlineCall {
		return errors.New("injected IMAP deadline failure")
	}
	return c.Conn.SetDeadline(deadline)
}

func (c *appendOutcomeConn) Write(data []byte) (int, error) {
	c.writeCalls++
	if c.failWriteCall != 0 && c.writeCalls == c.failWriteCall {
		written, err := c.Conn.Write(data[:c.failWriteAfter])
		if err != nil {
			return written, err
		}
		return written, errors.New("injected IMAP write failure")
	}
	return c.Conn.Write(data)
}

type appendFailureReader struct {
	data []byte
	err  error
}

func (r *appendFailureReader) Read(buffer []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	written := copy(buffer, r.data)
	r.data = r.data[written:]
	return written, nil
}

type appendPeerResult struct {
	data []byte
	err  error
}

func startAppendOutcomePeer(conn net.Conn, readBytes int) <-chan appendPeerResult {
	result := make(chan appendPeerResult, 1)
	go func() {
		peer := appendPeerResult{}
		defer func() {
			peer.err = errors.Join(peer.err, conn.Close())
			result <- peer
		}()
		reader := bufio.NewReader(conn)
		command, err := reader.ReadString('\n')
		if err != nil {
			peer.err = err
			return
		}
		fields := strings.Fields(command)
		if len(fields) < 3 || fields[1] != "APPEND" {
			peer.err = errors.New("fake IMAP peer expected APPEND")
			return
		}
		if _, err := io.WriteString(conn, "+ continue\r\n"); err != nil {
			peer.err = err
			return
		}
		if readBytes < 0 {
			peer.data, peer.err = io.ReadAll(reader)
		} else {
			peer.data = make([]byte, readBytes)
			_, peer.err = io.ReadFull(reader, peer.data)
		}
	}()
	return result
}
