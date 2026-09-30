package imapclient

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestSpecializedResponseMetadataLimits(t *testing.T) {
	for _, mode := range []string{"source", "excerpt", "search", "status", "enable"} {
		for _, test := range []struct {
			name     string
			bytes    bool
			overflow bool
		}{
			{name: "line boundary"}, {name: "line overflow", overflow: true},
			{name: "byte boundary", bytes: true}, {name: "byte overflow", bytes: true, overflow: true},
		} {
			t.Run(mode+"/"+test.name, func(t *testing.T) {
				valid := specializedValidResponse(mode)
				prefix := strings.Repeat("* OK ignored\r\n", maxFlagResponseCount-2)
				if test.overflow {
					prefix = strings.Repeat("* OK ignored\r\n", maxFlagResponseCount)
				}
				if test.bytes {
					metadata := len(valid)
					if mode == "excerpt" {
						metadata -= len(excerptTestHeader) + len("Body")
					}
					prefix = specializedMetadataPadding(maxFlagResponseBytes - metadata)
					if test.overflow {
						prefix += "* OK extra\r\n"
					}
				}
				reader := strings.NewReader(prefix + valid)
				peer, conn := net.Pipe()
				t.Cleanup(func() {
					if err := errors.Join(conn.Close(), peer.Close()); err != nil {
						t.Error(err)
					}
				})
				sess := &session{conn: conn, br: bufio.NewReader(reader), bw: bufio.NewWriter(io.Discard), utf8Accept: true,
					nextTag: func() string { return "T1" }}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				err := consumeSpecializedResponse(ctx, sess, mode)
				if !test.overflow {
					if err != nil || sess.dirty {
						t.Fatalf("boundary rejected: dirty=%t, err=%v", sess.dirty, err)
					}
					return
				}
				var limit *transport.TransportError
				if !errors.As(err, &limit) || limit.Code != transport.CodeIMAPResourceLimitExceeded || limit.Limit == nil ||
					!sess.dirty || ctx.Err() != nil {
					t.Fatalf("flood did not stop with bounded evidence: dirty=%t, err=%+v", sess.dirty, err)
				}
			})
		}
	}
}

func specializedValidResponse(mode string) string {
	switch mode {
	case "source":
		return "* 1 FETCH (UID 42 BODY[] \"Body\")\r\nT1 OK done\r\n"
	case "excerpt":
		return excerptTestResponse(42, "BODY[TEXT]<0>", "Body") + "T1 OK done\r\n"
	case "search":
		return "* SEARCH 42\r\nT1 OK done\r\n"
	case "status":
		return "* STATUS INBOX (MESSAGES 1 UNSEEN 0 UIDNEXT 43 UIDVALIDITY 12345)\r\nT1 OK done\r\n"
	default:
		return "* ENABLED UTF8=ACCEPT\r\nT1 OK done\r\n"
	}
}

func specializedMetadataPadding(size int) string {
	var result strings.Builder
	for size > 0 {
		n := min(size, maxIMAPResponseLineBytes/2)
		result.WriteString("* OK " + strings.Repeat("x", n-7) + "\r\n")
		size -= n
	}
	return result.String()
}

func consumeSpecializedResponse(ctx context.Context, sess *session, mode string) error {
	client := New()
	switch mode {
	case "source":
		source, err := client.readFetchSource(ctx, sess, "T1", 42, 12345, 64, true, "BODY[]")
		if source != nil {
			err = errors.Join(err, source.Close())
		}
		return err
	case "excerpt":
		_, err := client.readExcerptResponses(ctx, sess, "T1", 12345, map[uint32]bool{42: true}, 64, make(map[uint32]transport.MessageExcerptSource))
		return err
	case "search":
		_, err := client.readSearchResults(ctx, sess, "T1", "UID SEARCH")
		return err
	case "status":
		_, err := client.doStatus(ctx, sess, "T1", "INBOX")
		return err
	default:
		return client.enableUTF8(ctx, sess)
	}
}

func TestSpecializedPayloadBudgets(t *testing.T) {
	for _, test := range []struct {
		name     string
		response string
		maxBytes int64
		overflow bool
	}{
		{name: "large source", response: fmt.Sprintf("* 1 FETCH (UID 42 BODY[] {%d}\r\n%s)\r\nT1 OK done\r\n", 5<<20, strings.Repeat("x", 5<<20)), maxBytes: 5 << 20},
		{name: "foreign literals exhaust source budget", response: "* 1 FETCH (UID 99 BODY[] {4}\r\nabcd)\r\n* 2 FETCH (UID 42 BODY[] {4}\r\nefgh)\r\nT1 OK done\r\n", maxBytes: 4, overflow: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			sess := &session{br: bufio.NewReader(strings.NewReader(test.response))}
			source, err := New().readFetchSource(context.Background(), sess, "T1", 42, 12345, test.maxBytes, true, "BODY[]")
			if test.overflow {
				if transport.ErrorCode(err) != transport.CodeIMAPResourceLimitExceeded || !sess.dirty || source != nil {
					t.Fatalf("literal flood accepted: %+v, %v", source, err)
				}
				return
			}
			if err != nil || source == nil {
				t.Fatalf("valid source rejected: %v", err)
			}
			n, readErr := io.Copy(io.Discard, source)
			if err := errors.Join(readErr, source.Close()); err != nil || n != test.maxBytes {
				t.Fatalf("source size=%d, err=%v", n, err)
			}
		})
	}
	t.Run("batched excerpts exceed metadata cap", func(t *testing.T) {
		const size = 3 << 20
		response := excerptTestResponse(42, "BODY[TEXT]<0>", strings.Repeat("x", size)) + excerptTestResponse(43, "BODY[TEXT]<0>", strings.Repeat("y", size)) + "T1 OK done\r\n"
		sess := &session{br: bufio.NewReader(strings.NewReader(response))}
		results, err := New().readExcerptResponses(context.Background(), sess, "T1", 12345, map[uint32]bool{42: true, 43: true}, size, make(map[uint32]transport.MessageExcerptSource))
		if err != nil || len(results) != 2 || len(results[42].Source) != size+len(excerptTestHeader) || len(results[43].Source) != size+len(excerptTestHeader) {
			t.Fatalf("valid batched payload rejected: %v", err)
		}
	})
}

func TestSentAppendResponseFloodPreservesUnknownOutcome(t *testing.T) {
	messageID := "<bounded-search@example.com>"
	message := []byte("Message-ID: " + messageID + "\r\n\r\nBody\r\n")
	for _, afterAppend := range []bool{false, true} {
		t.Run(fmt.Sprintf("after APPEND %t", afterAppend), func(t *testing.T) {
			lines := make([]string, maxFlagResponseCount)
			for i := range lines {
				lines[i] = "* OK ignored"
			}
			lines = append(lines, "* SEARCH", "<tag> OK SEARCH done")
			responses := [][]string{lines}
			if afterAppend {
				responses = [][]string{nil, lines}
			}
			server := newFakeServer(t, fakeServerConfig{authOK: true, sentMboxes: []string{"Sent"}, appendOK: true, searchResponses: responses})
			client, cfg := newFakeClient(t, server)
			t.Cleanup(func() {
				if err := client.Close(); err != nil {
					t.Error(err)
				}
			})
			evidence, err := client.AppendToSent(context.Background(), cfg, message, messageID)
			wantCode := transport.CodeIMAPResourceLimitExceeded
			if afterAppend {
				wantCode = transport.CodeIMAPAppendOutcomeUnknown
			}
			if transport.ErrorCode(err) != wantCode || evidence != (transport.AppendEvidence{}) {
				t.Fatalf("flood lost outcome: %+v, %v", evidence, err)
			}
			if afterAppend && transport.ErrorCode(errors.Unwrap(err)) != transport.CodeIMAPResourceLimitExceeded {
				t.Fatalf("flood cause lost: %v", err)
			}
			appends := 0
			for _, command := range server.Commands() {
				if command == "APPEND" {
					appends++
				}
			}
			called, _, _, data := server.AppendRecord()
			if afterAppend {
				if !called || appends != 1 || !bytes.Equal(data, message) {
					t.Fatalf("APPEND replayed or lost: count=%d", appends)
				}
			} else if called || appends != 0 {
				t.Fatal("flood started APPEND")
			}
		})
	}
}

func TestSpecializedLiteralFramingAndQuotedBounds(t *testing.T) {
	for _, test := range []struct {
		name, response, mode, code string
	}{
		{name: "source quoted cap", mode: "source", response: "* 1 FETCH (UID 42 BODY[] \"" + strings.Repeat("x", 65) + "\")\r\nT1 OK done\r\n", code: transport.CodeIMAPRawSourceTooLarge},
		{name: "excerpt quoted cap", mode: "excerpt", response: strings.Replace(excerptTestResponse(42, "BODY[TEXT]<0>", "Body"), "{4}\r\nBody", "\""+strings.Repeat("x", 65)+"\"", 1) + "T1 OK done\r\n", code: transport.CodeIMAPResponseMalformed},
		{name: "status literal metadata", mode: "status", response: strings.Repeat("* OK {1048576}\r\n"+strings.Repeat("x", 1<<20)+"\r\n", 4) + specializedValidResponse("status"), code: transport.CodeIMAPResourceLimitExceeded},
		{name: "single logical metadata flood", mode: "source", response: strings.Repeat("* OK "+strings.Repeat("x", (1<<20)-20)+" {0}\r\n", 5) + "end\r\nT1 OK done\r\n", code: transport.CodeIMAPResourceLimitExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			peer, conn := net.Pipe()
			t.Cleanup(func() {
				if err := errors.Join(conn.Close(), peer.Close()); err != nil {
					t.Error(err)
				}
			})
			sess := &session{conn: conn, br: bufio.NewReader(strings.NewReader(test.response)), bw: bufio.NewWriter(io.Discard)}
			var err error
			if test.name == "single logical metadata flood" {
				source, readErr := New().readFetchSource(context.Background(), sess, "T1", 42, 12345, 1<<30, true, "BODY[]")
				err = readErr
				if source != nil {
					err = errors.Join(err, source.Close())
				}
			} else {
				err = consumeSpecializedResponse(context.Background(), sess, test.mode)
			}
			if transport.ErrorCode(err) != test.code || !sess.dirty {
				t.Fatalf("bound not enforced: dirty=%t, err=%v", sess.dirty, err)
			}
		})
	}
}
