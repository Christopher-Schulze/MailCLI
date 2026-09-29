package imapclient

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

const (
	testListTag        = "A1"
	testListCompletion = testListTag + " OK LIST completed\r\n"
	testIgnoredPrefix  = "* OK "
	testIgnoredSuffix  = "\r\n"
)

func TestDoListEnforcesCumulativeWireByteLimit(t *testing.T) {
	for _, test := range []struct {
		name   string
		overBy int64
	}{
		{name: "exact limit"},
		{name: "one byte over", overBy: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ignoredBytes := MaxListOperationResponseBytes - int64(len(testListCompletion)) + test.overBy
			mailboxes, err, dirty := runListResponse(t, func(writer *bufio.Writer) error {
				if err := writeIgnoredResponseBytes(writer, ignoredBytes); err != nil {
					return err
				}
				_, err := writer.WriteString(testListCompletion)
				return err
			})
			if test.overBy == 0 {
				if err != nil || dirty || mailboxes != nil {
					t.Fatalf("exact byte boundary = (%d mailboxes, %v, dirty=%t)", len(mailboxes), err, dirty)
				}
				return
			}
			requireListLimitExceeded(t, mailboxes, err, dirty, "cumulative response bytes")
		})
	}
}

func TestDoListEnforcesUntaggedLogicalLineLimit(t *testing.T) {
	for _, test := range []struct {
		name  string
		count int
		limit bool
	}{
		{name: "exact limit", count: MaxListOperationResponseLines},
		{name: "one over", count: MaxListOperationResponseLines + 1, limit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			mailboxes, err, dirty := runListResponse(t, func(writer *bufio.Writer) error {
				for index := 0; index < test.count; index++ {
					if _, err := writer.WriteString(testIgnoredPrefix + "ignored" + testIgnoredSuffix); err != nil {
						return err
					}
				}
				_, err := writer.WriteString(testListCompletion)
				return err
			})
			if test.limit {
				requireListLimitExceeded(t, mailboxes, err, dirty, "untagged logical response lines")
				return
			}
			if err != nil || dirty || mailboxes != nil {
				t.Fatalf("exact line boundary = (%d mailboxes, %v, dirty=%t)", len(mailboxes), err, dirty)
			}
		})
	}
}

func TestDoListEnforcesParsedMailboxLimit(t *testing.T) {
	const mailboxResponse = `* LIST (\HasNoChildren) "/" "INBOX"` + "\r\n"
	for _, test := range []struct {
		name  string
		count int
		limit bool
	}{
		{name: "exact limit", count: MaxListOperationMailboxes},
		{name: "one over", count: MaxListOperationMailboxes + 1, limit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			mailboxes, err, dirty := runListResponse(t, func(writer *bufio.Writer) error {
				for index := 0; index < test.count; index++ {
					if _, err := writer.WriteString(mailboxResponse); err != nil {
						return err
					}
				}
				_, err := writer.WriteString(testListCompletion)
				return err
			})
			if test.limit {
				requireListLimitExceeded(t, mailboxes, err, dirty, "parsed mailboxes")
				return
			}
			if err != nil || dirty || len(mailboxes) != MaxListOperationMailboxes {
				t.Fatalf("exact mailbox boundary = (%d mailboxes, %v, dirty=%t)", len(mailboxes), err, dirty)
			}
		})
	}
}

func TestDoListCountsLiteralPayloadTowardCumulativeWireLimit(t *testing.T) {
	const literalSize = maxListLiteralBytes
	literalHeader := "* OK {" + strconv.Itoa(literalSize) + "}\r\n"
	literalWireBytes := int64(len(literalHeader) + literalSize + len(testIgnoredSuffix))
	ignoredBytes := MaxListOperationResponseBytes - int64(len(testListCompletion)) - literalWireBytes + 1
	mailboxes, err, dirty := runListResponse(t, func(writer *bufio.Writer) error {
		if err := writeIgnoredResponseBytes(writer, ignoredBytes); err != nil {
			return err
		}
		if _, err := writer.WriteString(literalHeader); err != nil {
			return err
		}
		payloadChunk := strings.Repeat("x", 32<<10)
		for remaining := literalSize; remaining > 0; {
			chunk := payloadChunk
			if remaining < len(chunk) {
				chunk = chunk[:remaining]
			}
			if _, err := writer.WriteString(chunk); err != nil {
				return err
			}
			remaining -= len(chunk)
		}
		if _, err := writer.WriteString(testIgnoredSuffix); err != nil {
			return err
		}
		_, err := writer.WriteString(testListCompletion)
		return err
	})
	requireListLimitExceeded(t, mailboxes, err, dirty, "cumulative response bytes")
}

func TestListOverflowGuidanceAtReadAndMutationResolution(t *testing.T) {
	for _, command := range []string{"mailboxes.list", "messages.delete"} {
		t.Run(command, func(t *testing.T) {
			server := newFakeServer(t, fakeServerConfig{
				authOK:       true,
				listResponse: []byte(strings.Repeat("* OK ignored\r\n", MaxListOperationResponseLines+1)),
			})
			client, config := newFakeClient(t, server)
			t.Cleanup(func() {
				if err := client.Close(); err != nil {
					t.Errorf("close IMAP client: %v", err)
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var err error
			phase := mail.OperationPhaseRead
			if command == "mailboxes.list" {
				var boxes []transport.MailboxInfo
				boxes, err = client.ListMailboxes(ctx, config)
				if boxes != nil {
					t.Fatalf("overflow returned partial mailboxes: %+v", boxes)
				}
			} else {
				phase = mail.OperationPhaseMutation
				var evidence transport.MutationEvidence
				evidence, err = client.DeleteMessage(ctx, config, "INBOX", 42, 12345, "<transfer@example.com>")
				if !reflect.DeepEqual(evidence, transport.MutationEvidence{}) {
					t.Fatalf("overflow started a mutation: %+v", evidence)
				}
			}
			if !transport.IsResourceLimitExceeded(err) {
				t.Fatalf("lookup error = %v, want resource overflow", err)
			}
			guidance := mail.GuidanceForError(command, err)
			if guidance.Phase != phase || guidance.EffectCertainty != mail.EffectNone ||
				guidance.Retryability != mail.RetryTerminal || guidance.ReplayAllowed || guidance.Recovery.Action != mail.RecoveryInspect {
				t.Fatalf("overflow guidance = %+v", guidance)
			}
			for _, dispatched := range server.Commands() {
				if dispatched == "SELECT" || strings.HasPrefix(dispatched, "UID ") || dispatched == "APPEND" {
					t.Fatalf("overflow dispatched %q", dispatched)
				}
			}
		})
	}
}

func runListResponse(
	t *testing.T,
	writeResponse func(*bufio.Writer) error,
) ([]mailbox, error, bool) {
	t.Helper()
	clientConn, peerConn := net.Pipe()
	sess := &session{
		conn:            clientConn,
		br:              bufio.NewReader(clientConn),
		bw:              bufio.NewWriter(clientConn),
		mailboxEncoding: transport.MailboxEncodingModifiedUTF7,
	}
	peerDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(peerConn)
		if _, err := reader.ReadString('\n'); err != nil {
			peerDone <- err
			return
		}
		writer := bufio.NewWriter(peerConn)
		if err := writeResponse(writer); err != nil {
			peerDone <- err
			return
		}
		peerDone <- writer.Flush()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	mailboxes, err := New().doList(ctx, sess, testListTag)
	_ = clientConn.Close()
	_ = peerConn.Close()
	select {
	case peerErr := <-peerDone:
		if err == nil && peerErr != nil {
			t.Fatalf("fake LIST peer: %v", peerErr)
		}
	case <-time.After(time.Second):
		t.Fatal("fake LIST peer did not stop after the connection closed")
	}
	return mailboxes, err, sess.dirty
}

func writeIgnoredResponseBytes(writer *bufio.Writer, wireBytes int64) error {
	const targetLineBytes = int64(32 << 10)
	const lineOverhead = int64(len(testIgnoredPrefix) + len(testIgnoredSuffix))
	if wireBytes < lineOverhead {
		return io.ErrShortWrite
	}
	lineCount := (wireBytes + targetLineBytes - 1) / targetLineBytes
	baseLineBytes := wireBytes / lineCount
	extraBytes := wireBytes % lineCount
	for line := int64(0); line < lineCount; line++ {
		lineBytes := baseLineBytes
		if line < extraBytes {
			lineBytes++
		}
		content := strings.Repeat("x", int(lineBytes-lineOverhead))
		if _, err := writer.WriteString(testIgnoredPrefix + content + testIgnoredSuffix); err != nil {
			return err
		}
	}
	return nil
}

func requireListLimitExceeded(
	t *testing.T,
	mailboxes []mailbox,
	err error,
	dirty bool,
	resource string,
) {
	t.Helper()
	if err == nil {
		t.Fatal("expected LIST resource-limit error")
	}
	if got := transport.ErrorCode(err); got != transport.CodeIMAPResourceLimitExceeded {
		t.Fatalf("error code = %q, want %q: %v", got, transport.CodeIMAPResourceLimitExceeded, err)
	}
	if !strings.Contains(err.Error(), resource+" limit exceeded") {
		t.Fatalf("error does not identify exceeded %s bound: %v", resource, err)
	}
	wantLimit := MaxListOperationResponseBytes
	switch resource {
	case "untagged logical response lines":
		wantLimit = int64(MaxListOperationResponseLines)
	case "parsed mailboxes":
		wantLimit = int64(MaxListOperationMailboxes)
	}
	var overflow *transport.TransportError
	if !errors.As(err, &overflow) || overflow.Limit == nil || overflow.Limit.Name != resource ||
		overflow.Limit.Value != wantLimit || overflow.ObservedAtLeast != wantLimit+1 {
		t.Fatalf("limit evidence = %+v, want %s bound %d and lower bound %d", overflow, resource, wantLimit, wantLimit+1)
	}
	if mailboxes != nil {
		t.Fatalf("resource-limit error returned %d partial mailboxes", len(mailboxes))
	}
	if !dirty {
		t.Fatal("resource-limit error retained a reusable session")
	}
}
