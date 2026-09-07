package imapclient

import (
	"context"
	"crypto/tls"
	"net"
	"reflect"
	"slices"
	"testing"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

func TestOperationContracts(t *testing.T) {
	want := []OperationContract{
		{Operation: "LIST", Class: OperationClassRead, Concurrency: OperationConcurrencySharedAccount, SessionOwnership: SessionOwnershipPooled, MailboxSelection: MailboxSelectionNone, UIDValidity: UIDValidityNotUsed},
		{Operation: "STATUS", Class: OperationClassStatus, Concurrency: OperationConcurrencySharedAccount, SessionOwnership: SessionOwnershipPooled, MailboxSelection: MailboxSelectionNone, UIDValidity: UIDValidityObserved},
		{Operation: "SEARCH", Class: OperationClassRead, Concurrency: OperationConcurrencySharedAccount, SessionOwnership: SessionOwnershipPooled, MailboxSelection: MailboxSelectionReuseOrSelect, UIDValidity: UIDValidityObserved},
		{Operation: "FETCH", Class: OperationClassFetch, Concurrency: OperationConcurrencySharedAccount, SessionOwnership: SessionOwnershipPooled, MailboxSelection: MailboxSelectionFresh, UIDValidity: UIDValidityRequiredMatch},
		{Operation: "APPEND", Class: OperationClassMutation, Concurrency: OperationConcurrencyExclusiveAccount, SessionOwnership: SessionOwnershipDedicated, MailboxSelection: MailboxSelectionFresh, UIDValidity: UIDValidityNotUsed},
		{Operation: "STORE", Class: OperationClassMutation, Concurrency: OperationConcurrencyExclusiveAccount, SessionOwnership: SessionOwnershipPooled, MailboxSelection: MailboxSelectionFresh, UIDValidity: UIDValidityRequiredMatch},
		{Operation: "COPY", Class: OperationClassMutation, Concurrency: OperationConcurrencyExclusiveAccount, SessionOwnership: SessionOwnershipPooled, MailboxSelection: MailboxSelectionFresh, UIDValidity: UIDValidityRequiredMatch},
		{Operation: "MOVE", Class: OperationClassMutation, Concurrency: OperationConcurrencyExclusiveAccount, SessionOwnership: SessionOwnershipPooled, MailboxSelection: MailboxSelectionFresh, UIDValidity: UIDValidityRequiredMatch},
		{Operation: "DELETE", Class: OperationClassMutation, Concurrency: OperationConcurrencyExclusiveAccount, SessionOwnership: SessionOwnershipPooled, MailboxSelection: MailboxSelectionFresh, UIDValidity: UIDValidityRequiredMatch},
		{Operation: "CLOSE", Class: OperationClassClose, Concurrency: OperationConcurrencyExclusiveClient, SessionOwnership: SessionOwnershipAllPooled, MailboxSelection: MailboxSelectionNone, UIDValidity: UIDValidityNotUsed},
	}
	got := OperationContracts()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OperationContracts() = %+v, want %+v", got, want)
	}
	got[0].Operation = "changed"
	if OperationContracts()[0].Operation != "LIST" {
		t.Fatal("OperationContracts() exposed mutable package state")
	}
}

func TestMutationProtocolOrderAfterSelectedRead(t *testing.T) {
	searchStarted := make(chan struct{}, 1)
	searchContinue := make(chan struct{})
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX"}, searchMatchID: "<found@example.com>",
		searchStartedEvents: searchStarted, searchContinue: searchContinue,
	})
	client, cfg := newFakeClient(t, srv)
	t.Cleanup(func() { _ = client.Close() })
	readResult := make(chan error, 1)
	go func() {
		_, _, _, err := client.SearchUID(context.Background(), cfg, "INBOX", "<found@example.com>")
		readResult <- err
	}()
	select {
	case <-searchStarted:
	case <-time.After(time.Second):
		t.Fatal("SEARCH did not reach the server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.SetFlags(ctx, cfg, "INBOX", 42, 12345, []string{"\\Seen"}, nil); transport.ErrorCode(err) != transport.CodeIMAPTimeout {
		t.Fatalf("contended STORE error = %v, want %s", err, transport.CodeIMAPTimeout)
	}
	close(searchContinue)
	if err := <-readResult; err != nil {
		t.Fatalf("SearchUID() error = %v", err)
	}
	if _, err := client.SetFlags(context.Background(), cfg, "INBOX", 42, 12345, []string{"\\Seen"}, nil); err != nil {
		t.Fatalf("SetFlags() error = %v", err)
	}
	want := []string{"SELECT", "UID SEARCH", "SELECT", "UID STORE"}
	if got := protocolCommands(srv.Commands()); !slices.Equal(got, want) {
		t.Fatalf("protocol commands = %q, want %q", got, want)
	}
}

func TestCloseProtocolOrderAfterStatus(t *testing.T) {
	statusStarted := make(chan struct{}, 1)
	statusContinue := make(chan struct{})
	srv := newFakeServer(t, fakeServerConfig{
		authOK: true, otherMboxes: []string{"INBOX"},
		statusStartedEvents: statusStarted, statusContinue: statusContinue,
	})
	client, cfg := newFakeClient(t, srv)
	statusResult := make(chan error, 1)
	go func() {
		_, err := client.CheckStatus(context.Background(), cfg, "INBOX")
		statusResult <- err
	}()
	select {
	case <-statusStarted:
	case <-time.After(time.Second):
		t.Fatal("STATUS did not reach the server")
	}
	closeResult := make(chan error, 1)
	go func() { closeResult <- client.Close() }()
	select {
	case err := <-closeResult:
		t.Fatalf("Close() returned before STATUS completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(statusContinue)
	if err := <-statusResult; err != nil {
		t.Fatalf("CheckStatus() error = %v", err)
	}
	if err := <-closeResult; err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	commands := protocolCommands(srv.Commands())
	if len(commands) == 0 || commands[0] != "STATUS" {
		t.Fatalf("protocol commands = %q, want STATUS first", commands)
	}
	if stats := client.PoolStats(); stats.AccountPools != 0 || stats.PooledSessions != 0 {
		t.Fatalf("pool stats after Close = %+v", stats)
	}
}

func protocolCommands(commands []string) []string {
	var filtered []string
	for _, command := range commands {
		if command != "LOGIN" {
			filtered = append(filtered, command)
		}
	}
	return filtered
}

func BenchmarkFetchConcurrency(b *testing.B) {
	for _, test := range []struct {
		name  string
		limit int
	}{{name: "serialized", limit: 1}, {name: "two sessions", limit: 2}} {
		b.Run(test.name, func(b *testing.B) {
			srv := newFakeServer(b, fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX"}, fetchDelay: 2 * time.Millisecond})
			host, portValue, err := net.SplitHostPort(srv.Addr())
			if err != nil {
				b.Fatalf("split host port: %v", err)
			}
			port, err := atoiPositive(portValue)
			if err != nil {
				b.Fatalf("parse port: %v", err)
			}
			client, err := NewWithOptions(ClientOptions{MaxConnectionsPerAccount: test.limit})
			if err != nil {
				b.Fatalf("NewWithOptions() error = %v", err)
			}
			client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
			cfg := sessionTestConfig(host, port)
			b.Cleanup(func() { _ = client.Close() })
			b.ResetTimer()
			for b.Loop() {
				results := make(chan error, 2)
				for range 2 {
					go func() {
						_, err := client.FetchMessage(context.Background(), cfg, "INBOX", 42, 12345, mail.MaximumRawSourceBytes)
						results <- err
					}()
				}
				for range 2 {
					if err := <-results; err != nil {
						b.Fatalf("FetchMessage() error = %v", err)
					}
				}
			}
			b.StopTimer()
			if got := srv.MaxActiveConnections(); got > test.limit {
				b.Fatalf("maximum active connections = %d, limit = %d", got, test.limit)
			}
		})
	}
}
