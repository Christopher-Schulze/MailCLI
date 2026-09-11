package imapclient

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestJunkTransitionsUsePermittedExclusiveKeywords(t *testing.T) {
	for _, initial := range []struct {
		name  string
		flags []string
	}{
		{"neither", nil}, {"junk", []string{"$Junk"}},
		{"not junk", []string{"$NotJunk"}}, {"both", []string{"$Junk", "$NotJunk"}},
	} {
		for _, desired := range []struct{ name, add, remove string }{
			{"junk", "$Junk", "$NotJunk"}, {"not junk", "$NotJunk", "$Junk"},
		} {
			for _, support := range []struct {
				name  string
				flags []string
			}{
				{"omitted default", nil}, {"explicit", []string{"$Junk", "$NotJunk"}}, {"wildcard", []string{"\\*"}},
			} {
				t.Run(initial.name+"/"+desired.name+"/"+support.name, func(t *testing.T) {
					preserved := []string{"\\Seen", "\\Answered", "\\Flagged", "custom", "Junk", "NotJunk"}
					flags := append(append([]string(nil), preserved...), initial.flags...)
					server := newFakeServer(t, fakeServerConfig{authOK: true, initialFlags: map[uint32][]string{42: flags}, permanentFlags: support.flags})
					client, cfg := flagTestClient(t, server)
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					evidence, err := client.SetFlags(ctx, cfg, "INBOX", 42, 12345, []string{desired.add}, []string{desired.remove})
					wanted := append(preserved, desired.add)
					if err != nil || evidence.Outcome != transport.MutationOutcomeCompleted || evidence.FlagsState != transport.FlagObservationObserved || !reflect.DeepEqual(evidence.ActualFlags, wanted) {
						t.Fatalf("transition = %+v, %v; wanted %v", evidence, err, wanted)
					}
					stores := 0
					if !containsFlag(initial.flags, desired.add) {
						stores++
					}
					if containsFlag(initial.flags, desired.remove) {
						stores++
					}
					assertFlagCommandCounts(t, server, stores, 1)
					server.mu.Lock()
					actual := append([]string(nil), server.messageFlags[42]...)
					lastStore := server.storeFlags
					server.mu.Unlock()
					if !reflect.DeepEqual(actual, wanted) || (stores == 2 && lastStore != "+FLAGS ("+desired.add+")") {
						t.Fatalf("server flags %v, last STORE %q; wanted %v with removal first", actual, lastStore, wanted)
					}
				})
			}
		}
	}
}

func TestJunkPermissionAndPartialResults(t *testing.T) {
	for _, test := range []struct {
		name                            string
		config                          fakeServerConfig
		initial, add, remove, wantFlags []string
		code, outcome                   string
		state                           transport.FlagObservationState
		stores, fetches                 int
	}{
		{name: "creation prohibited before opposite removal", config: fakeServerConfig{permanentFlags: []string{"$NotJunk"}}, initial: []string{"$NotJunk"}, add: []string{"$Junk"}, remove: []string{"$NotJunk"}, wantFlags: []string{"$NotJunk"}, code: transport.CodeIMAPFlagsUnsupported, outcome: transport.MutationOutcomeNotStarted, state: transport.FlagObservationObserved, fetches: 1},
		{name: "opposite removal prohibited before creation", config: fakeServerConfig{permanentFlags: []string{"$Junk"}}, initial: []string{"$NotJunk"}, add: []string{"$Junk"}, remove: []string{"$NotJunk"}, wantFlags: []string{"$NotJunk"}, code: transport.CodeIMAPFlagsUnsupported, outcome: transport.MutationOutcomeNotStarted, state: transport.FlagObservationObserved, fetches: 1},
		{name: "empty permissions allow verified noop", config: fakeServerConfig{permanentFlags: []string{}}, initial: []string{"$Junk"}, add: []string{"$Junk"}, remove: []string{"$NotJunk"}, wantFlags: []string{"$Junk"}, outcome: transport.MutationOutcomeCompleted, state: transport.FlagObservationObserved, fetches: 1},
		{name: "only needed keyword requires permission", config: fakeServerConfig{permanentFlags: []string{"$Junk"}}, add: []string{"$Junk"}, remove: []string{"$NotJunk"}, wantFlags: []string{"$Junk"}, outcome: transport.MutationOutcomeCompleted, state: transport.FlagObservationObserved, stores: 1, fetches: 1},
		{name: "keyword wildcard excludes system flags", config: fakeServerConfig{permanentFlags: []string{"\\*"}}, add: []string{"$Junk", "\\Seen"}, remove: []string{"$NotJunk"}, wantFlags: []string{}, code: transport.CodeIMAPFlagsUnsupported, outcome: transport.MutationOutcomeNotStarted, state: transport.FlagObservationObserved, fetches: 1},
		{name: "first phase rejected retains original", config: fakeServerConfig{rejectStoreCall: 1}, initial: []string{"\\Seen", "$NotJunk"}, add: []string{"$Junk"}, remove: []string{"$NotJunk"}, wantFlags: []string{"\\Seen", "$NotJunk"}, code: transport.CodeIMAPMutationFailed, outcome: transport.MutationOutcomeRejected, state: transport.FlagObservationObserved, stores: 1, fetches: 2},
		{name: "second phase rejected retains partial", config: fakeServerConfig{rejectStoreCall: 2}, initial: []string{"\\Seen", "$NotJunk"}, add: []string{"$Junk"}, remove: []string{"$NotJunk"}, wantFlags: []string{"\\Seen"}, code: transport.CodeIMAPFlagsPartial, outcome: transport.MutationOutcomePartial, state: transport.FlagObservationObserved, stores: 2, fetches: 2},
		{name: "each silent phase verified once", config: fakeServerConfig{storeResponses: [][]byte{[]byte("<tag> OK removed\r\n"), []byte("<tag> OK added\r\n")}}, initial: []string{"$NotJunk"}, add: []string{"$Junk"}, remove: []string{"$NotJunk"}, wantFlags: []string{"$Junk"}, outcome: transport.MutationOutcomeCompleted, state: transport.FlagObservationObserved, stores: 2, fetches: 3},
		{name: "permission withdrawal between phases", config: fakeServerConfig{storeResponses: [][]byte{[]byte("* 1 FETCH (UID 42 FLAGS ())\r\n<tag> OK [PERMANENTFLAGS ($NotJunk)] removed\r\n")}}, initial: []string{"$NotJunk"}, add: []string{"$Junk"}, remove: []string{"$NotJunk"}, wantFlags: []string{}, code: transport.CodeIMAPFlagsUnsupported, outcome: transport.MutationOutcomePartial, state: transport.FlagObservationObserved, stores: 1, fetches: 1},
		{name: "persistence revoked during STORE", config: fakeServerConfig{storeResponses: [][]byte{[]byte("* 1 FETCH (UID 42 FLAGS ($Junk))\r\n<tag> OK [PERMANENTFLAGS ()] session flags only\r\n")}}, add: []string{"$Junk"}, remove: []string{"$NotJunk"}, wantFlags: []string{"$Junk"}, code: transport.CodeIMAPFlagsUnsupported, outcome: transport.MutationOutcomeUnknown, state: transport.FlagObservationObserved, stores: 1, fetches: 1},
		{name: "contradictory first phase stops before add", config: fakeServerConfig{storeResponses: [][]byte{[]byte("* 1 FETCH (UID 42 FLAGS ($NotJunk))\r\n<tag> OK ignored removal\r\n")}}, initial: []string{"$NotJunk"}, add: []string{"$Junk"}, remove: []string{"$NotJunk"}, wantFlags: []string{"$NotJunk"}, code: transport.CodeIMAPFlagsMismatch, outcome: transport.MutationOutcomeObserved, state: transport.FlagObservationObserved, stores: 1, fetches: 1},
		{name: "final concurrent contradiction fails", config: fakeServerConfig{storeResponses: [][]byte{nil, []byte("* 1 FETCH (UID 42 FLAGS ($Junk $NotJunk))\r\n<tag> OK concurrent change\r\n")}}, initial: []string{"$NotJunk"}, add: []string{"$Junk"}, remove: []string{"$NotJunk"}, wantFlags: []string{"$Junk", "$NotJunk"}, code: transport.CodeIMAPFlagsMismatch, outcome: transport.MutationOutcomeObserved, state: transport.FlagObservationObserved, stores: 2, fetches: 1},
		{name: "missing target before writes", config: fakeServerConfig{fetchResponse: []byte("<tag> OK missing\r\n")}, add: []string{"$Junk"}, remove: []string{"$NotJunk"}, code: transport.CodeIMAPMessageNotFound, outcome: transport.MutationOutcomeNotStarted, state: transport.FlagObservationMissing, fetches: 1},
		{name: "unproven preflight", config: fakeServerConfig{fetchResponse: []byte("* 1 FETCH (UID 42)\r\n<tag> OK incomplete\r\n")}, add: []string{"$Junk"}, remove: []string{"$NotJunk"}, code: transport.CodeIMAPResponseMalformed, outcome: transport.MutationOutcomeNotStarted, state: transport.FlagObservationUnverified, fetches: 1},
		{name: "disconnect during second phase never replays", config: fakeServerConfig{storeResponses: [][]byte{nil, []byte("* BYE disconnected\r\n")}}, initial: []string{"$NotJunk"}, add: []string{"$Junk"}, remove: []string{"$NotJunk"}, code: transport.CodeIMAPFlagsOutcomeUnknown, outcome: transport.MutationOutcomeUnknown, state: transport.FlagObservationUnverified, stores: 2, fetches: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.config.authOK = true
			test.config.initialFlags = map[uint32][]string{42: test.initial}
			server := newFakeServer(t, test.config)
			client, cfg := flagTestClient(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			evidence, err := client.SetFlags(ctx, cfg, "INBOX", 42, 12345, test.add, test.remove)
			if transport.ErrorCode(err) != test.code || evidence.Outcome != test.outcome || evidence.FlagsState != test.state || !reflect.DeepEqual(evidence.ActualFlags, test.wantFlags) {
				t.Fatalf("result %+v, %v; want %s/%s/%s flags %#v", evidence, err, test.code, test.outcome, test.state, test.wantFlags)
			}
			if err != nil {
				var outcome *transport.MutationOutcomeError
				if !errors.As(err, &outcome) || !reflect.DeepEqual(outcome.Evidence, evidence) {
					t.Fatalf("error lost evidence: %v", err)
				}
			}
			assertFlagCommandCounts(t, server, test.stores, test.fetches)
		})
	}
}

func assertFlagCommandCounts(t *testing.T, server *fakeServer, wantStores, wantFetches int) {
	t.Helper()
	stores, fetches := 0, 0
	for _, command := range server.Commands() {
		if command == "UID STORE" {
			stores++
		}
		if command == "UID FETCH" {
			fetches++
		}
	}
	if stores != wantStores || fetches != wantFetches {
		t.Fatalf("commands %v, want %d STORE and %d FETCH", server.Commands(), wantStores, wantFetches)
	}
}
