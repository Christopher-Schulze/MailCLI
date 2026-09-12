package imapclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func BenchmarkFetchPayload(b *testing.B) {
	for _, size := range []int{4096, 4 << 20} {
		for _, mode := range []string{"bytes", "reader"} {
			b.Run(fmt.Sprintf("%s-%d", mode, size), func(b *testing.B) {
				payload := []byte(strings.Repeat("x", size))
				server := newFakeServer(b, fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX"}, fetchPayload: payload})
				host, portText, err := net.SplitHostPort(server.Addr())
				if err != nil {
					b.Fatal(err)
				}
				port, err := atoiPositive(portText)
				if err != nil {
					b.Fatal(err)
				}
				client := New()
				client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
				b.Cleanup(func() {
					if err := client.Close(); err != nil {
						b.Error(err)
					}
				})
				cfg := transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}
				if _, err := client.FetchMessage(context.Background(), cfg, "INBOX", 42, 12345, int64(size)); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.SetBytes(int64(size))
				b.ResetTimer()
				for range b.N {
					if mode == "bytes" {
						got, err := client.FetchMessage(context.Background(), cfg, "INBOX", 42, 12345, int64(size))
						if err != nil || len(got) != size {
							b.Fatalf("FETCH length=%d: %v", len(got), err)
						}
					} else {
						source, gotSize, err := client.FetchMessageReader(context.Background(), cfg, "INBOX", 42, 12345, int64(size))
						if err != nil {
							b.Fatal(err)
						}
						count, copyErr := io.Copy(io.Discard, source)
						err = errors.Join(copyErr, source.Close())
						if err != nil || count != int64(size) || gotSize != count {
							b.Fatalf("reader length=%d: %v", count, err)
						}
					}
				}
			})
		}
	}
}

func TestFetchSourceResponses(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("TMPDIR", directory)
	body := strings.Repeat("x", fetchMemoryThreshold)
	literal := fmt.Sprintf("{%d}\r\n%s", len(body), body)
	good := "* 1 FETCH (BODY[] " + literal + " UID 42)\r\n"
	for _, test := range []struct{ name, response, expected, code string }{
		{"body before UID", good + "A001 OK done\r\n", body, ""},
		{"unknown literal", "* 1 FETCH (X {3}\r\naux BODY[] " + literal + " UID 42)\r\nA001 OK done\r\n", body, ""},
		{"unsolicited body", "* 2 FETCH (UID 99 BODY[] " + literal + ")\r\n" + good + "A001 OK done\r\n", body, ""},
		{"quoted", "* 1 FETCH (UID 42 BODY[] \"small\")\r\nA001 OK done\r\n", "small", ""},
		{"wrong UID", "* 2 FETCH (UID 99 BODY[] " + literal + ")\r\nA001 OK done\r\n", "", transport.CodeIMAPMessageUIDMismatch},
		{"duplicate", good + good + "A001 OK done\r\n", "", transport.CodeIMAPResponseMalformed},
		{"duplicate attributes", "* 1 FETCH (UID 42 BODY[] " + literal + " BODY[] " + literal + ")\r\nA001 OK done\r\n", "", transport.CodeIMAPResponseMalformed},
		{"final failure", good + "A001 NO failed\r\n", "", transport.CodeIMAPFetchFailed},
		{"changed validity", good + "* OK [UIDVALIDITY 54321] rebuilt\r\nA001 OK done\r\n", "", "mailbox_uidvalidity_changed"},
		{"malformed validity", good + "* OK [UIDVALIDITY invalid] rebuilt\r\nA001 OK done\r\n", "", transport.CodeIMAPResponseMalformed},
		{"truncated", fmt.Sprintf("* 1 FETCH (UID 42 BODY[] {%d}\r\nshort", len(body)), "", transport.CodeIMAPFetchFailed},
		{"oversized", "* 1 FETCH (UID 42 BODY[] {4194305}\r\n", "", transport.CodeIMAPRawSourceTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			sess := &session{br: bufio.NewReader(strings.NewReader(test.response))}
			source, err := New().readFetchSource(context.Background(), sess, "A001", 42, 12345, 4<<20, true)
			if test.code != "" {
				if source != nil || transport.ErrorCode(err) != test.code {
					t.Fatalf("source=%v error=%v, want %s", source, err, test.code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if source.size >= fetchMemoryThreshold && (source.file == nil || len(source.data) != 0) {
				t.Fatal("large literal retained in memory")
			}
			got, readErr := io.ReadAll(source)
			if readErr != nil || string(got) != test.expected {
				t.Fatalf("read bytes differ: %v", readErr)
			}
			if _, err := source.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			repeated, readErr := io.ReadAll(source)
			if err := errors.Join(readErr, source.Close()); err != nil {
				t.Fatal(err)
			}
			if string(repeated) != test.expected {
				t.Fatal("replay changed bytes")
			}
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			if source.file != nil {
				if _, err := source.file.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("spool not closed: %v", err)
				}
			}
		})
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("spool artifacts remain: %v, %v", entries, err)
	}
}

func TestFetchSourceSurvivesPoolClose(t *testing.T) {
	body := strings.Repeat("body", fetchMemoryThreshold/2)
	server := newFakeServer(t, fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX"}, fetchPayload: []byte(body)})
	client, cfg := newFakeClient(t, server)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	source, size, err := client.FetchMessageReader(context.Background(), cfg, "INBOX", 42, 12345, int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(source)
	if err := errors.Join(readErr, source.Close()); err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != size || string(got) != body {
		t.Fatal("pool close invalidated the returned source")
	}
	if stats := client.PoolStats(); stats.AccountPools != 0 || stats.PooledSessions != 0 {
		t.Fatalf("pool remains: %+v", stats)
	}
}

func TestFetchReaderCancellationReleasesPool(t *testing.T) {
	started := make(chan struct{}, 1)
	server := newFakeServer(t, fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX"}, fetchStartedEvents: started, fetchDelay: 100 * time.Millisecond})
	client, cfg := newFakeClient(t, server)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		source, _, err := client.FetchMessageReader(ctx, cfg, "INBOX", 42, 12345, 4<<20)
		if source != nil {
			err = errors.Join(err, source.Close(), errors.New("canceled FETCH returned source"))
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("FETCH did not start")
	}
	cancel()
	if err := <-result; transport.ErrorCode(err) != transport.CodeIMAPTimeout || ctx.Err() != context.Canceled {
		t.Fatalf("cancellation lost: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if stats := client.PoolStats(); stats.AccountPools != 0 || stats.PooledSessions != 0 {
		t.Fatalf("pool remains: %+v", stats)
	}
}

type fetchTestWriter func([]byte) (int, error)

func (write fetchTestWriter) Write(buffer []byte) (int, error) { return write(buffer) }

func TestFetchSpoolCopyFailures(t *testing.T) {
	failure := errors.New("write failure")
	for _, test := range []struct {
		name     string
		writer   io.Writer
		expected error
	}{
		{"short", fetchTestWriter(func([]byte) (int, error) { return 0, nil }), io.ErrShortWrite},
		{"failure", fetchTestWriter(func([]byte) (int, error) { return 0, failure }), failure},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := copyFetchLiteral(context.Background(), test.writer, strings.NewReader("body"), 4); !errors.Is(err, test.expected) {
				t.Fatalf("copy error=%v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	written := 0
	writer := fetchTestWriter(func(buffer []byte) (int, error) { written += len(buffer); cancel(); return len(buffer), nil })
	if err := copyFetchLiteral(ctx, writer, strings.NewReader(strings.Repeat("x", 1<<20)), 1<<20); !errors.Is(err, context.Canceled) || written >= 1<<20 || written == 0 {
		t.Fatalf("cancel error=%v written=%d", err, written)
	}
	file, err := os.CreateTemp(t.TempDir(), "closed-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	source := &fetchSource{ReadSeeker: file, file: file}
	if err := closeFetchSources([]*fetchSource{source}); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("close error lost: %v", err)
	}
}

func TestFetchSourceSpoolsAggregateLiteralMemory(t *testing.T) {
	body := strings.Repeat("x", 3*fetchMemoryThreshold/4)
	literal := fmt.Sprintf("{%d}\r\n%s", len(body), body)
	response := "* 1 FETCH (X " + literal + " BODY[] " + literal + " UID 42)\r\nA001 OK done\r\n"
	sess := &session{br: bufio.NewReader(strings.NewReader(response))}
	source, err := New().readFetchSource(context.Background(), sess, "A001", 42, 12345, 4<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	}()
	if source.file == nil || len(source.data) != 0 || source.size != int64(len(body)) {
		t.Fatal("multiple small literals bypassed the response memory threshold")
	}
	got, err := io.ReadAll(source)
	if err != nil || string(got) != body {
		t.Fatalf("selected literal differs: %v", err)
	}
}

func TestFetchReaderRejectsInvalidInputsBeforeConnection(t *testing.T) {
	for _, test := range []struct {
		uid   uint32
		limit int64
	}{{0, 1024}, {42, 0}, {42, -1}} {
		expected := transport.CodeIMAPInvalidValue
		if test.uid == 0 {
			expected = transport.CodeIMAPMessageUIDUnknown
		}
		source, _, err := New().FetchMessageReader(context.Background(), transport.ImapConfig{}, "INBOX", test.uid, 12345, test.limit)
		if source != nil || transport.ErrorCode(err) != expected {
			t.Fatalf("input validation failed: %v", err)
		}
	}
}

func TestConcurrentFetchReadersKeepIndependentSources(t *testing.T) {
	body := strings.Repeat("mail", fetchMemoryThreshold/2)
	server := newFakeServer(t, fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX"}, fetchPayload: []byte(body), fetchDelay: 20 * time.Millisecond})
	client, cfg := newFakeClient(t, server)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	type outcome struct {
		source io.ReadSeekCloser
		size   int64
		err    error
	}
	results := make(chan outcome, 2)
	for _, uid := range []uint32{42, 43} {
		go func() {
			source, size, err := client.FetchMessageReader(context.Background(), cfg, "INBOX", uid, 12345, 4<<20)
			results <- outcome{source, size, err}
		}()
	}
	var sources []outcome
	for range 2 {
		sources = append(sources, <-results)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	for _, result := range sources {
		if result.err != nil {
			t.Error(result.err)
			continue
		}
		got, err := io.ReadAll(result.source)
		err = errors.Join(err, result.source.Close())
		if err != nil || string(got) != body || result.size != int64(len(body)) {
			t.Errorf("independent source mismatch: %v", err)
		}
	}
	if server.MaxActiveConnections() > DefaultMaxConnectionsPerAccount {
		t.Fatal("FETCH exceeded the configured connection limit")
	}
}
