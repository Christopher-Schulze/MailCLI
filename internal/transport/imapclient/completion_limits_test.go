package imapclient

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestGenericCompletionResponseLimits(t *testing.T) {
	t.Parallel()
	const ignored = "* OK [ALERT notice] background\r\n"
	large := "* OK [" + strings.Repeat("x", maxIMAPResponseLineBytes-9) + "]\r\n"
	for _, test := range []struct {
		name, response, limit string
	}{
		{name: "normal", response: "T1 OK complete\r\n"},
		{name: "line boundary", response: strings.Repeat(ignored, maxFlagResponseCount-1) + "T1 OK complete\r\n"},
		{name: "line overflow", response: strings.Repeat(ignored, maxFlagResponseCount) + "T1 OK complete\r\n", limit: "command completion response lines"},
		{name: "byte overflow", response: strings.Repeat(large, maxFlagResponseBytes/len(large)) + "T1 OK complete\r\n", limit: "command completion response bytes"},
	} {
		for _, retainCodes := range []bool{false, true} {
			name := test.name + "/plain"
			if retainCodes {
				name = test.name + "/with-codes"
			}
			t.Run(name, func(t *testing.T) {
				client := &Client{}
				sess := &session{br: bufio.NewReader(strings.NewReader(test.response))}
				var status string
				var err error
				if retainCodes {
					status, _, _, err = client.readFinalWithCodes(context.Background(), sess, "T1", transport.CodeIMAPMutationFailed, "read final")
				} else {
					status, _, err = client.readFinal(context.Background(), sess, "T1")
				}
				if test.limit == "" {
					if err != nil || status != "OK" || sess.dirty {
						t.Fatalf("valid completion status=%q, dirty=%t, err=%v", status, sess.dirty, err)
					}
					return
				}
				var limit *transport.TransportError
				if !errors.As(err, &limit) || limit.Code != transport.CodeIMAPResourceLimitExceeded || limit.Limit == nil || limit.Limit.Name != test.limit || !sess.dirty || status != "" {
					t.Fatalf("overflow status=%q, dirty=%t, err=%+v", status, sess.dirty, err)
				}
				if retainCodes {
					for _, command := range []string{"COPY", "MOVE"} {
						evidence, failure := transferCommandError(transport.MutationEvidence{Command: command}, status, "", true, err)
						if evidence.Outcome != transport.MutationOutcomeUnknown || !transport.IsMutationOutcomeUnknown(failure) {
							t.Fatalf("overflow lost %s dispatch uncertainty: %+v, %v", command, evidence, failure)
						}
					}
				}
			})
		}
	}
}

func TestGenericCompletionPreservesUntaggedCopyUID(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, response string
		wantCodes      int
	}{
		{name: "untagged code", response: "* OK [COPYUID 9 1 2] copied\r\nT1 OK complete\r\n", wantCodes: 1},
		{name: "tagged code", response: "T1 OK [COPYUID 9 1 2] complete\r\n", wantCodes: 1},
		{name: "prose is not evidence", response: "* OK copied [COPYUID 9 1 2]\r\nT1 OK complete\r\n"},
		{name: "FETCH text is not evidence", response: "* 1 FETCH ([COPYUID 9 1 2])\r\nT1 OK complete\r\n"},
		{name: "another tag is not evidence", response: "T2 OK [COPYUID 9 1 2] complete\r\nT1 OK complete\r\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			sess := &session{br: bufio.NewReader(strings.NewReader(test.response))}
			status, text, codes, err := (&Client{}).readFinalWithCodes(context.Background(), sess, "T1", transport.CodeIMAPMutationFailed, "read final")
			if err != nil || status != "OK" || !strings.HasSuffix(text, "complete") || len(codes) != test.wantCodes || (len(codes) != 0 && codes[0] != "COPYUID 9 1 2") {
				t.Fatalf("completion evidence: %q, %q, %v, %v", status, text, codes, err)
			}
		})
	}
}

func TestMalformedAppendCompletionKeepsUnknownOutcome(t *testing.T) {
	t.Parallel()
	peer, conn := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	defer func() { _ = conn.Close() }()
	done := make(chan error, 1)
	go func() {
		var err error
		defer func() { done <- errors.Join(err, peer.Close()) }()
		reader := bufio.NewReader(peer)
		if _, err = reader.ReadString('\n'); err != nil {
			return
		}
		if _, err = io.WriteString(peer, "+ continue\r\n"); err != nil {
			return
		}
		literal := make([]byte, 3)
		if _, err = io.ReadFull(reader, literal); err != nil {
			return
		}
		if string(literal) != "x\r\n" {
			err = errors.New("peer did not receive complete APPEND terminator")
			return
		}
		_, err = io.WriteString(peer, "T1 WHAT invalid completion\r\n")
	}()
	sess := &session{conn: conn, br: bufio.NewReader(conn), bw: bufio.NewWriter(conn)}
	err := (&Client{}).doAppend(ctx, sess, "T1", "Sent", strings.NewReader("x"), 1)
	if peerErr := <-done; peerErr != nil {
		t.Fatal(peerErr)
	}
	if transport.ErrorCode(err) != transport.CodeIMAPAppendOutcomeUnknown || !sess.dirty {
		t.Fatalf("malformed post-terminator reply lost uncertainty: dirty=%t, err=%v", sess.dirty, err)
	}
}
