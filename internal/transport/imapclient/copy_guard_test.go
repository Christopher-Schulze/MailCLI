package imapclient

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func guardedCopyClient(t *testing.T, srv *fakeServer, lockDir string) (*Client, transport.ImapConfig) {
	t.Helper()
	host, rawPort, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewWithOptions(ClientOptions{MutationLockDir: lockDir})
	if err != nil {
		t.Fatal(err)
	}
	client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	return client, transport.ImapConfig{Host: host, Port: port, Username: "user", Password: "pass"}
}

func TestCopyMessageIfAbsentSkipsTheCopyWhenTheDestinationHoldsTheMessage(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX", "Archive"}, searchMatchID: "<found@example.com>", searchUID: 77,
	})
	client, cfg := guardedCopyClient(t, srv, t.TempDir())
	ev, match, err := client.CopyMessageIfAbsent(context.Background(), cfg, "INBOX", 42, 12345, "Archive", "<found@example.com>")
	if err != nil {
		t.Fatalf("CopyMessageIfAbsent() error = %v", err)
	}
	if match.Count != 1 || match.UID != 77 || match.UIDValidity != 12345 || ev.Outcome != transport.MutationOutcomeNotStarted {
		t.Fatalf("match = %+v, evidence = %+v", match, ev)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.copyCalled {
		t.Fatal("COPY was sent although the destination already holds the message")
	}
}

func TestCopyMessageIfAbsentCopiesWhenTheDestinationLacksTheMessage(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX", "Archive"}, searchMatchID: "<other@example.com>", searchUID: 77,
	})
	client, cfg := guardedCopyClient(t, srv, t.TempDir())
	ev, match, err := client.CopyMessageIfAbsent(context.Background(), cfg, "INBOX", 42, 12345, "Archive", "<absent@example.com>")
	if err != nil || match.Count != 0 {
		t.Fatalf("match = %+v, error = %v", match, err)
	}
	if ev.Command != "COPY" || ev.Outcome != transport.MutationOutcomeCompleted || ev.CopyDestinationUID != 100 {
		t.Fatalf("evidence = %+v", ev)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.copyCalled || srv.copyUID != 42 || srv.copyDst != "Archive" {
		t.Fatalf("copy not recorded: called=%v uid=%d dst=%s", srv.copyCalled, srv.copyUID, srv.copyDst)
	}
}

func TestCopyMessageIfAbsentWrapsAnObservationFailureAndSendsNoCopy(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX", "Archive"}})
	client, cfg := guardedCopyClient(t, srv, t.TempDir())
	_, _, err := client.CopyMessageIfAbsent(context.Background(), cfg, "INBOX", 42, 12345, "Archive", "not a message id")
	var guardErr *transport.CopyGuardError
	if !errors.As(err, &guardErr) {
		t.Fatalf("error = %v, want a CopyGuardError", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.copyCalled {
		t.Fatal("COPY was sent although the destination check failed")
	}
}

// Two processes copy the same message: the destination search of the second
// one may only start after the first one released the account lock.
func TestCopyMessageIfAbsentSearchesUnderTheCrossProcessLock(t *testing.T) {
	searchStarts := make(chan struct{}, 4)
	searchContinue := make(chan struct{})
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX", "Archive"},
		searchStartedEvents: searchStarts, searchContinue: searchContinue,
	})
	lockDir := t.TempDir()
	first, cfg := guardedCopyClient(t, srv, lockDir)
	second, _ := guardedCopyClient(t, srv, lockDir)

	var group sync.WaitGroup
	run := func(client *Client) {
		defer group.Done()
		if _, _, err := client.CopyMessageIfAbsent(context.Background(), cfg, "INBOX", 42, 12345, "Archive", "<absent@example.com>"); err != nil {
			t.Errorf("CopyMessageIfAbsent() error = %v", err)
		}
	}
	group.Add(1)
	go run(first)
	select {
	case <-searchStarts:
	case <-time.After(5 * time.Second):
		t.Fatal("the first destination search never started")
	}
	group.Add(1)
	go run(second)
	select {
	case <-searchStarts:
		t.Fatal("the second destination search started while the first process held the lock")
	case <-time.After(400 * time.Millisecond):
	}
	close(searchContinue)
	group.Wait()
	select {
	case <-searchStarts:
	default:
		t.Fatal("the second destination search never ran after the first process released the lock")
	}
}

func TestMoveAndDeleteGuardDestinationUnderTheCrossProcessLock(t *testing.T) {
	for _, operation := range []string{"move", "delete"} {
		t.Run(operation, func(t *testing.T) {
			started := make(chan struct{}, 4)
			proceed := make(chan struct{})
			server := newFakeServer(t, fakeServerConfig{
				authOK: true, trashMboxes: []string{"Trash"}, otherMboxes: []string{"INBOX", "Archive"},
				searchStartedEvents: started, searchContinue: proceed,
				fetchPayload: []byte("Message-ID: <transfer@example.com>\r\n\r\nOriginal source"),
			})
			lockDir := t.TempDir()
			first, cfg := guardedCopyClient(t, server, lockDir)
			second, _ := guardedCopyClient(t, server, lockDir)
			t.Cleanup(func() {
				if err := first.Close(); err != nil {
					t.Error(err)
				}
				if err := second.Close(); err != nil {
					t.Error(err)
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			type result struct {
				evidence transport.MutationEvidence
				err      error
			}
			invoke := func(client *Client, done chan<- result) {
				var ev transport.MutationEvidence
				var err error
				if operation == "delete" {
					ev, err = client.DeleteMessage(ctx, cfg, "INBOX", 42, 12345, "<transfer@example.com>")
				} else {
					ev, err = client.MoveMessage(ctx, cfg, "INBOX", 42, 12345, "Archive", "<transfer@example.com>")
				}
				done <- result{ev, err}
			}
			firstResult, secondResult := make(chan result, 1), make(chan result, 1)
			go invoke(first, firstResult)
			select {
			case <-started:
			case <-ctx.Done():
				close(proceed)
				<-firstResult
				t.Fatal("first locked destination guard did not start")
			}
			go invoke(second, secondResult)
			select {
			case <-started:
				t.Error("second destination guard ran before the first transfer released its account lock")
			case <-time.After(100 * time.Millisecond):
			}
			close(proceed)
			one, two := <-firstResult, <-secondResult
			if one.err != nil || one.evidence.Outcome != transport.MutationOutcomeCompleted || transport.ErrorCode(two.err) != transport.CodeIMAPMoveOutcomeUnknown || two.evidence.Outcome != transport.MutationOutcomeNotStarted || two.evidence.DestinationUID != 100 {
				t.Fatalf("first=%+v/%v second=%+v/%v", one.evidence, one.err, two.evidence, two.err)
			}
			copies := 0
			for _, command := range server.Commands() {
				if command == "UID COPY" {
					copies++
				}
				if command == "UID MOVE" || command == "EXPUNGE" {
					t.Errorf("unexpected mutation: %v", server.Commands())
				}
			}
			if copies != 1 {
				t.Fatalf("concurrent transfers dispatched %d COPY commands: %v", copies, server.Commands())
			}
		})
	}
}
