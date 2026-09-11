package imapclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestSetFlagsObservesServerResult(t *testing.T) {
	tests := []struct {
		name        string
		store       string
		fetch       string
		wantFlags   []string
		wantState   transport.FlagObservationState
		wantCode    string
		wantSource  string
		wantFetches int
	}{
		{name: "empty observed set", store: "* 7 FETCH (FLAGS () UID 42)\r\n<tag> OK STORE done\r\n", wantFlags: []string{}, wantState: transport.FlagObservationObserved, wantSource: "STORE"},
		{name: "contradictory flags", store: "* 7 FETCH (UID 42 FLAGS (\\Seen \\Flagged))\r\n<tag> OK STORE done\r\n", wantFlags: []string{"\\Seen", "\\Flagged"}, wantState: transport.FlagObservationObserved, wantCode: transport.CodeIMAPFlagsMismatch, wantSource: "STORE"},
		{name: "last concurrent observation wins", store: "* 7 FETCH (UID 42 FLAGS ())\r\n* 7 FETCH (FLAGS (\\Seen) UID 42)\r\n<tag> OK STORE done\r\n", wantFlags: []string{"\\Seen"}, wantState: transport.FlagObservationObserved, wantCode: transport.CodeIMAPFlagsMismatch, wantSource: "STORE"},
		{name: "concurrent change settles as requested", store: "* 7 FETCH (UID 42 FLAGS (\\Seen))\r\n* 7 FETCH (UID 42 FLAGS (\\Flagged $Junk))\r\n<tag> OK STORE done\r\n", wantFlags: []string{"\\Flagged", "$Junk"}, wantState: transport.FlagObservationObserved, wantSource: "STORE"},
		{name: "store without flags uses one fetch", store: "<tag> OK STORE done\r\n", wantFlags: []string{}, wantState: transport.FlagObservationObserved, wantSource: "FETCH", wantFetches: 1},
		{name: "wrong UID prefix is not identity", store: "* 42 FETCH (UID 99 FLAGS (\\Seen))\r\n<tag> OK STORE done\r\n", wantFlags: []string{}, wantState: transport.FlagObservationObserved, wantSource: "FETCH", wantFetches: 1},
		{name: "vanished target", store: "<tag> OK STORE done\r\n", fetch: "<tag> OK FETCH done\r\n", wantState: transport.FlagObservationMissing, wantCode: transport.CodeIMAPMessageNotFound, wantSource: "FETCH", wantFetches: 1},
		{name: "only unrelated UID in fetch", store: "<tag> OK STORE done\r\n", fetch: "* 42 FETCH (UID 99 FLAGS ())\r\n<tag> OK FETCH done\r\n", wantState: transport.FlagObservationMissing, wantCode: transport.CodeIMAPMessageNotFound, wantSource: "FETCH", wantFetches: 1},
		{name: "target returned without flags", store: "<tag> OK STORE done\r\n", fetch: "* 7 FETCH (UID 42)\r\n<tag> OK FETCH done\r\n", wantState: transport.FlagObservationUnverified, wantCode: transport.CodeIMAPFlagsOutcomeUnknown, wantSource: "FETCH", wantFetches: 1},
		{name: "uncorrelated late flags require fetch", store: "* 7 FETCH (UID 42 FLAGS ())\r\n* 7 FETCH (FLAGS (\\Seen))\r\n<tag> OK STORE done\r\n", wantFlags: []string{}, wantState: transport.FlagObservationObserved, wantSource: "FETCH", wantFetches: 1},
		{name: "uncorrelated late fetch never loops", store: "<tag> OK STORE done\r\n", fetch: "* 7 FETCH (UID 42 FLAGS ())\r\n* 7 FETCH (FLAGS (\\Seen))\r\n<tag> OK FETCH done\r\n", wantState: transport.FlagObservationUnverified, wantCode: transport.CodeIMAPFlagsOutcomeUnknown, wantSource: "FETCH", wantFetches: 1},
		{name: "target expunged after lower sequence", store: "* 7 FETCH (UID 42 FLAGS ())\r\n* 2 EXPUNGE\r\n* 6 EXPUNGE\r\n<tag> OK STORE done\r\n", wantState: transport.FlagObservationMissing, wantCode: transport.CodeIMAPMessageNotFound, wantSource: "STORE"},
		{name: "unrelated expunge preserves flags", store: "* 7 FETCH (UID 42 FLAGS ())\r\n* 2 EXPUNGE\r\n* 8 EXPUNGE\r\n<tag> OK STORE done\r\n", wantFlags: []string{}, wantState: transport.FlagObservationObserved, wantSource: "STORE"},
		{name: "untagged UIDVALIDITY rollover", store: "* 7 FETCH (UID 42 FLAGS ())\r\n* OK [UIDVALIDITY 54321] changed\r\n<tag> OK STORE done\r\n", wantState: transport.FlagObservationUnverified, wantCode: transport.CodeIMAPFlagsOutcomeUnknown, wantSource: "STORE"},
		{name: "tagged UIDVALIDITY rollover", store: "* 7 FETCH (UID 42 FLAGS ())\r\n<tag> OK [UIDVALIDITY 54321] changed\r\n", wantState: transport.FlagObservationUnverified, wantCode: transport.CodeIMAPFlagsOutcomeUnknown, wantSource: "STORE"},
		{name: "verification UIDVALIDITY rollover", store: "<tag> OK STORE done\r\n", fetch: "* OK [uidvalidity 54321] changed\r\n* 7 FETCH (UID 42 FLAGS ())\r\n<tag> OK FETCH done\r\n", wantState: transport.FlagObservationUnverified, wantCode: transport.CodeIMAPFlagsOutcomeUnknown, wantSource: "FETCH", wantFetches: 1},
		{name: "duplicate FLAGS cannot hide conflict", store: "* 7 FETCH (UID 42 FLAGS () FLAGS (\\Seen))\r\n<tag> OK STORE done\r\n", wantState: transport.FlagObservationUnverified, wantCode: transport.CodeIMAPFlagsOutcomeUnknown, wantSource: "STORE"},
		{name: "invalid sequence", store: "* 0 FETCH (UID 42 FLAGS ())\r\n<tag> OK STORE done\r\n", wantState: transport.FlagObservationUnverified, wantCode: transport.CodeIMAPFlagsOutcomeUnknown, wantSource: "STORE"},
		{name: "unexplained sequence change", store: "* 7 FETCH (UID 42 FLAGS ())\r\n* 8 FETCH (UID 42 FLAGS ())\r\n<tag> OK STORE done\r\n", wantState: transport.FlagObservationUnverified, wantCode: transport.CodeIMAPFlagsOutcomeUnknown, wantSource: "STORE"},
		{name: "unrelated UID reassigns sequence", store: "* 7 FETCH (UID 42 FLAGS ())\r\n* 7 FETCH (UID 99)\r\n<tag> OK STORE done\r\n", wantState: transport.FlagObservationUnverified, wantCode: transport.CodeIMAPFlagsOutcomeUnknown, wantSource: "STORE"},
		{name: "updated target after lower expunge", store: "* 7 FETCH (UID 42 FLAGS (\\Seen))\r\n* 2 EXPUNGE\r\n* 6 FETCH (UID 42 FLAGS ())\r\n<tag> OK STORE done\r\n", wantFlags: []string{}, wantState: transport.FlagObservationObserved, wantSource: "STORE"},
		{name: "BYE after flags is unknown", store: "* 7 FETCH (UID 42 FLAGS ())\r\n* BYE lost mailbox\r\n", wantState: transport.FlagObservationUnverified, wantCode: transport.CodeIMAPFlagsOutcomeUnknown, wantSource: "STORE"},
		{name: "unsolicited literal does not spoof UIDVALIDITY", store: "* 9 FETCH (UID 99 BODY[] {31}\r\n* OK [UIDVALIDITY 54321] fake\r\n FLAGS (\\Seen))\r\n* 7 FETCH (UID 42 FLAGS ())\r\n<tag> OK STORE done\r\n", wantFlags: []string{}, wantState: transport.FlagObservationObserved, wantSource: "STORE"},
		{name: "response count bounded", store: strings.Repeat("* OK unrelated\r\n", maxFlagResponseCount) + "<tag> OK STORE done\r\n", wantState: transport.FlagObservationUnverified, wantCode: transport.CodeIMAPFlagsOutcomeUnknown, wantSource: "STORE"},
		{name: "response bytes bounded", store: strings.Repeat("* OK "+strings.Repeat("x", 8192)+"\r\n", 520) + "<tag> OK STORE done\r\n", wantState: transport.FlagObservationUnverified, wantCode: transport.CodeIMAPFlagsOutcomeUnknown, wantSource: "STORE"},
		{name: "first STORE rejected", store: "<tag> NO STORE denied\r\n", wantState: transport.FlagObservationUnverified, wantCode: transport.CodeIMAPMutationFailed, wantSource: "STORE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newFakeServer(t, fakeServerConfig{authOK: true, storeResponses: [][]byte{[]byte(test.store)}, fetchResponse: []byte(test.fetch)})
			client, cfg := flagTestClient(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			evidence, err := client.SetFlags(ctx, cfg, "INBOX", 42, 12345, nil, []string{"\\Seen"})
			if got := transport.ErrorCode(err); got != test.wantCode {
				t.Fatalf("error = %v (code %q), want %q", err, got, test.wantCode)
			}
			if evidence.FlagsState != test.wantState || evidence.FlagsSource != test.wantSource || !reflect.DeepEqual(evidence.ActualFlags, test.wantFlags) {
				t.Fatalf("evidence = %+v, want state %s, source %s, flags %#v", evidence, test.wantState, test.wantSource, test.wantFlags)
			}
			if evidence.UID != 42 || evidence.UIDValidity != 12345 || evidence.ExpectedUIDValidity != 12345 || evidence.Mailbox != "INBOX" || evidence.Command != "STORE" || evidence.OperationID == "" {
				t.Fatalf("incorrect identity evidence: %+v", evidence)
			}
			if (err == nil) != (evidence.Outcome == transport.MutationOutcomeCompleted) {
				t.Fatalf("outcome %q contradicts error %v", evidence.Outcome, err)
			}
			stores, fetches := 0, 0
			for _, command := range server.Commands() {
				if command == "UID STORE" {
					stores++
				}
				if command == "UID FETCH" {
					fetches++
				}
			}
			if stores != 1 || fetches != test.wantFetches {
				t.Fatalf("commands = %v, want one STORE and %d FETCH", server.Commands(), test.wantFetches)
			}
			if test.wantCode == transport.CodeIMAPFlagsMismatch || test.wantCode == transport.CodeIMAPFlagsOutcomeUnknown || test.wantCode == transport.CodeIMAPMessageNotFound {
				var outcome *transport.MutationOutcomeError
				if !errors.As(err, &outcome) || !reflect.DeepEqual(outcome.Evidence, evidence) {
					t.Fatalf("error lost returned evidence: %v / %+v", err, evidence)
				}
			}
		})
	}
}

func flagTestClient(t *testing.T, server *fakeServer) (*Client, transport.ImapConfig) {
	t.Helper()
	host, portValue, err := net.SplitHostPort(server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	port, err := atoiPositive(portValue)
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{TLSConfig: &tls.Config{InsecureSkipVerify: true}}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	return client, sessionTestConfig(host, port)
}

func TestSetFlagsCompoundAndInterruptedVerification(t *testing.T) {
	tests := []struct {
		name        string
		config      fakeServerConfig
		add, remove []string
		wantCode    string
		wantFlags   []string
		deadline    time.Duration
		wantStores  int
	}{
		{name: "both commands reflected", config: fakeServerConfig{authOK: true}, add: []string{"\\Seen", "\\Flagged"}, remove: []string{"\\Draft"}, wantFlags: []string{"\\Seen", "\\Flagged"}, wantStores: 2},
		{name: "second command rejected", config: fakeServerConfig{authOK: true, storeResponses: [][]byte{nil, []byte("<tag> NO remove rejected\r\n")}}, add: []string{"\\Seen"}, remove: []string{"\\Draft"}, wantCode: transport.CodeIMAPFlagsOutcomeUnknown, wantStores: 2},
		{name: "verification deadline", config: fakeServerConfig{authOK: true, storeResponses: [][]byte{[]byte("<tag> OK STORE done\r\n")}, fetchDelay: 200 * time.Millisecond}, add: []string{"\\Seen"}, wantCode: transport.CodeIMAPFlagsOutcomeUnknown, deadline: 100 * time.Millisecond, wantStores: 1},
		{name: "verification connection lost", config: fakeServerConfig{authOK: true, storeResponses: [][]byte{[]byte("<tag> OK STORE done\r\n")}, dropAfterCommands: 4}, add: []string{"\\Seen"}, wantCode: transport.CodeIMAPFlagsOutcomeUnknown, wantStores: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newFakeServer(t, test.config)
			client, cfg := flagTestClient(t, server)
			deadline := test.deadline
			if deadline == 0 {
				deadline = 3 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			evidence, err := client.SetFlags(ctx, cfg, "INBOX", 42, 12345, test.add, test.remove)
			if transport.ErrorCode(err) != test.wantCode || !reflect.DeepEqual(evidence.ActualFlags, test.wantFlags) {
				t.Fatalf("result = %+v, %v; want flags %v, code %s", evidence, err, test.wantFlags, test.wantCode)
			}
			stores := 0
			for _, command := range server.Commands() {
				if command == "UID STORE" {
					stores++
				}
			}
			if stores != test.wantStores {
				t.Fatalf("STORE count %d, want %d", stores, test.wantStores)
			}
		})
	}
}

func TestSetFlagsCancellationDuringVerification(t *testing.T) {
	started := make(chan struct{}, 1)
	server := newFakeServer(t, fakeServerConfig{authOK: true, storeResponses: [][]byte{[]byte("<tag> OK STORE done\r\n")}, fetchStartedEvents: started, fetchDelay: 250 * time.Millisecond})
	client, cfg := flagTestClient(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		evidence transport.MutationEvidence
		err      error
	}
	finished := make(chan result, 1)
	go func() {
		evidence, err := client.SetFlags(ctx, cfg, "INBOX", 42, 12345, []string{"\\Seen"}, nil)
		finished <- result{evidence, err}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		cancel()
		<-finished
		t.Fatal("targeted flag FETCH did not start")
	}
	cancel()
	select {
	case got := <-finished:
		if transport.ErrorCode(got.err) != transport.CodeIMAPFlagsOutcomeUnknown || got.evidence.FlagsState != transport.FlagObservationUnverified || got.evidence.FlagsSource != "FETCH" {
			t.Fatalf("canceled verification claimed an outcome: %+v, %v", got.evidence, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled flag verification did not finish")
	}
	stores := 0
	for _, command := range server.Commands() {
		if command == "UID STORE" {
			stores++
		}
	}
	if stores != 1 {
		t.Fatalf("cancellation replayed STORE: %v", server.Commands())
	}
}

func TestFlagValueValidation(t *testing.T) {
	for _, test := range []struct {
		name, value string
		valid       bool
	}{
		{"empty", "()", true}, {"system and keyword", "(\\Seen $Custom)", true},
		{"recent observation", "(\\Recent)", true}, {"non-list", "NIL", false},
		{"quoted flag", "(\"\\Seen\")", false}, {"nested flag", "((\\Seen))", false},
		{"control in flag", "(\\Seen\x1b)", false}, {"flag count", "(" + strings.Repeat("keyword ", maxFlagCount+1) + ")", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseFetchResponse("* 1 FETCH (UID 42 FLAGS "+test.value+")", nil)
			if (err == nil) != test.valid {
				t.Fatalf("FLAGS %q: %v, want valid %t", test.value, err, test.valid)
			}
		})
	}
}

func TestFlagChangesRejectInvalidIntentBeforeConnecting(t *testing.T) {
	for _, test := range []struct {
		name        string
		add, remove []string
	}{
		{"no changes", nil, nil}, {"empty atom", []string{""}, nil},
		{"conflicting intent", []string{"\\Seen"}, []string{"\\sEeN"}},
		{"recent is read only", []string{"\\Recent"}, nil},
		{"line injection", []string{"\\Seen)\r\nA LOGOUT"}, nil},
		{"attribute injection", []string{"\\Seen) UID 99 ("}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &Client{}
			evidence, err := client.SetFlags(context.Background(), transport.ImapConfig{}, "INBOX", 42, 12345, test.add, test.remove)
			if transport.ErrorCode(err) != transport.CodeIMAPInvalidValue || evidence.Outcome != transport.MutationOutcomeNotStarted {
				t.Fatalf("invalid intent reached connection: %+v, %v", evidence, err)
			}
		})
	}
}

func TestLogicalFlagResponseBudgetCountsTextAndLiteralsTogether(t *testing.T) {
	for _, test := range []struct {
		name      string
		limit     int64
		wantError bool
	}{
		{"combined bytes fit", 17, false}, {"combined bytes exceed", 16, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			sess := &session{br: bufio.NewReader(strings.NewReader("12345678 {6}\r\nabcdef!\r\n"))}
			line, literals, err := (&Client{}).readLogicalLineWithLiterals(sess, 6, test.limit, 1)
			if (err != nil) != test.wantError {
				t.Fatalf("response %q / %v, error %v, limit %d", line, literals, err, test.limit)
			}
			if test.wantError && !sess.dirty {
				t.Fatal("over-budget response left reusable session")
			}
		})
	}
}
