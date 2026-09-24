package imapclient

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

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
	if mailboxes != nil {
		t.Fatalf("resource-limit error returned %d partial mailboxes", len(mailboxes))
	}
	if !dirty {
		t.Fatal("resource-limit error retained a reusable session")
	}
}
