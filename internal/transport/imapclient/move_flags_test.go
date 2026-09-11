package imapclient

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestMoveFallbackFlagProtocol(t *testing.T) {
	for _, test := range []struct {
		name      string
		store     string
		fetch     string
		state     transport.FlagObservationState
		flags     []string
		source    string
		complete  bool
		deferred  bool
		reject    bool
		drop      bool
		permanent []string
		initial   []string
		stores    int
		fetches   int
	}{
		{name: "observed STORE", complete: true, state: transport.FlagObservationObserved, flags: []string{"\\Deleted"}, source: "STORE", stores: 1},
		{name: "bounded FETCH after silent STORE", store: "<tag> OK STORE done\r\n", complete: true, state: transport.FlagObservationObserved, flags: []string{"\\Deleted"}, source: "FETCH", stores: 1, fetches: 1},
		{name: "last target update wins", store: "* 7 FETCH (UID 42 FLAGS (\\Deleted))\r\n* 7 FETCH (UID 42 FLAGS (\\Seen))\r\n<tag> OK STORE done\r\n", state: transport.FlagObservationObserved, flags: []string{"\\Seen"}, source: "STORE", stores: 1},
		{name: "unrelated UID cannot prove target", store: "* 42 FETCH (UID 99 FLAGS (\\Deleted))\r\n<tag> OK STORE done\r\n", fetch: "* 42 FETCH (UID 99 FLAGS (\\Deleted))\r\n<tag> OK FETCH done\r\n", state: transport.FlagObservationMissing, source: "FETCH", stores: 1, fetches: 1},
		{name: "plain OK cannot prove target", store: "<tag> OK STORE done\r\n", fetch: "<tag> OK FETCH done\r\n", state: transport.FlagObservationMissing, source: "FETCH", stores: 1, fetches: 1},
		{name: "target lacks FLAGS", store: "<tag> OK STORE done\r\n", fetch: "* 7 FETCH (UID 42)\r\n<tag> OK FETCH done\r\n", state: transport.FlagObservationUnverified, source: "FETCH", stores: 1, fetches: 1},
		{name: "target expunged during STORE", store: "* 7 FETCH (UID 42 FLAGS (\\Deleted))\r\n* 7 EXPUNGE\r\n<tag> OK STORE done\r\n", state: transport.FlagObservationMissing, source: "STORE", stores: 1},
		{name: "foreign expunge updates sequence", store: "* 7 FETCH (UID 42 FLAGS (\\Deleted))\r\n* 3 EXPUNGE\r\n* 6 FETCH (UID 42 FLAGS (\\Deleted))\r\n<tag> OK STORE done\r\n", complete: true, state: transport.FlagObservationObserved, flags: []string{"\\Deleted"}, source: "STORE", stores: 1},
		{name: "untagged STORE rollover", store: "* 7 FETCH (UID 42 FLAGS (\\Deleted))\r\n* OK [UIDVALIDITY 54321] changed\r\n<tag> OK STORE done\r\n", state: transport.FlagObservationUnverified, source: "STORE", stores: 1},
		{name: "tagged STORE rollover", store: "* 7 FETCH (UID 42 FLAGS (\\Deleted))\r\n<tag> OK [UIDVALIDITY 54321] changed\r\n", state: transport.FlagObservationUnverified, source: "STORE", stores: 1},
		{name: "FETCH rollover", store: "<tag> OK STORE done\r\n", fetch: "* 7 FETCH (UID 42 FLAGS (\\Deleted))\r\n<tag> OK [UIDVALIDITY 54321] changed\r\n", state: transport.FlagObservationUnverified, source: "FETCH", stores: 1, fetches: 1},
		{name: "rejected STORE preserves actual flags", reject: true, initial: []string{"\\Seen"}, state: transport.FlagObservationObserved, flags: []string{"\\Seen"}, source: "FETCH", stores: 1, fetches: 1},
		{name: "unsupported permanent Deleted", permanent: []string{"\\Seen"}, initial: []string{"\\Seen"}, state: transport.FlagObservationObserved, flags: []string{"\\Seen"}, source: "FETCH", fetches: 1},
		{name: "already Deleted needs no unsupported STORE", permanent: []string{"\\Seen"}, initial: []string{"\\Deleted"}, complete: true, state: transport.FlagObservationObserved, flags: []string{"\\Deleted"}, source: "FETCH", fetches: 1},
		{name: "connection lost after COPY", store: "<tag> OK STORE done\r\n", drop: true, state: transport.FlagObservationUnverified, source: "FETCH", stores: 1},
		{name: "response count bounded", store: strings.Repeat("* OK still working\r\n", maxFlagResponseCount) + "<tag> OK STORE done\r\n", state: transport.FlagObservationUnverified, source: "STORE", stores: 1},
		{name: "unsupported UID EXPUNGE", complete: true, deferred: true, state: transport.FlagObservationObserved, flags: []string{"\\Deleted"}, source: "STORE", stores: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			configuration := fakeServerConfig{authOK: true, uidExpungeSupported: !test.deferred, initialDeletedUIDs: []uint32{99}, storeResponses: [][]byte{[]byte(test.store)}, fetchResponse: []byte(test.fetch), rejectStore: test.reject, permanentFlags: test.permanent, initialFlags: map[uint32][]string{42: test.initial}}
			if test.drop {
				configuration.dropAfterCommands = 6
			}
			server := newFakeServer(t, configuration)
			client, cfg := flagTestClient(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			evidence, err := client.MoveMessage(ctx, cfg, "INBOX", 42, 12345, "Archive")
			effects := []string{"copy"}
			if test.complete {
				effects = append(effects, "source_flag")
				if test.deferred {
					effects = append(effects, "cleanup_deferred")
				} else {
					effects = append(effects, "uid_expunge")
				}
			}
			if (err == nil) != test.complete || evidence.FlagsState != test.state || evidence.FlagsSource != test.source || !reflect.DeepEqual(evidence.ActualFlags, test.flags) || !reflect.DeepEqual(evidence.CompletedEffects, effects) {
				t.Errorf("flag phase = %+v, %v", evidence, err)
			}
			if evidence.OperationID != transport.MutationOperationID("MOVE", cfg.Username, "INBOX", 42, 12345, "Archive") || evidence.Mailbox != "INBOX" || evidence.UID != 42 || evidence.UIDValidity != 12345 || evidence.CopyDestinationUID != 100 || !strings.Contains(evidence.ServerResponse, "COPY completed") {
				t.Errorf("lost COPY/source identity: %+v", evidence)
			}
			if !test.complete {
				var outcome *transport.MutationOutcomeError
				if transport.ErrorCode(err) != transport.CodeIMAPMoveOutcomeUnknown || evidence.Outcome != transport.MutationOutcomePartial || !errors.As(err, &outcome) || !reflect.DeepEqual(outcome.Evidence, evidence) {
					t.Errorf("lost partial outcome: %+v, %v", evidence, err)
				}
			} else if evidence.Outcome != transport.MutationOutcomeCompleted {
				t.Errorf("verified operation did not complete: %+v", evidence)
			}
			counts := map[string]int{}
			for _, command := range server.Commands() {
				counts[command]++
			}
			if counts["UID COPY"] != 1 || counts["UID STORE"] != test.stores || counts["UID FETCH"] != test.fetches || counts["EXPUNGE"] != 0 || (counts["UID EXPUNGE"] == 1) != test.complete {
				t.Errorf("unsafe or repeated commands: %v", server.Commands())
			}
			if !slices.Contains(server.DeletedUIDs(), uint32(99)) || (test.deferred && (evidence.ExpungeBranch != "deferred" || evidence.ForeignDeletedCount != 1)) {
				t.Errorf("foreign deleted message lost or cleanup misreported: %v, %+v", server.DeletedUIDs(), evidence)
			}
		})
	}
}

func TestMoveFallbackCancellationRetainsCopy(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "cancel"
		if deadline {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{}, 1)
			server := newFakeServer(t, fakeServerConfig{authOK: true, uidExpungeSupported: true, initialDeletedUIDs: []uint32{99}, storeResponses: [][]byte{[]byte("<tag> OK STORE done\r\n")}, fetchStartedEvents: started, fetchDelay: 500 * time.Millisecond})
			client, cfg := flagTestClient(t, server)
			timeout := 3 * time.Second
			if deadline {
				timeout = 250 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			type result struct {
				evidence transport.MutationEvidence
				err      error
			}
			finished := make(chan result, 1)
			go func() {
				evidence, err := client.MoveMessage(ctx, cfg, "INBOX", 42, 12345, "Archive")
				finished <- result{evidence, err}
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				cancel()
				<-finished
				t.Fatal("source flag verification did not start")
			}
			if !deadline {
				cancel()
			}
			got := <-finished
			if transport.ErrorCode(got.err) != transport.CodeIMAPMoveOutcomeUnknown || got.evidence.Outcome != transport.MutationOutcomePartial || got.evidence.FlagsState != transport.FlagObservationUnverified || got.evidence.CopyDestinationUID != 100 || !reflect.DeepEqual(got.evidence.CompletedEffects, []string{"copy"}) {
				t.Errorf("aborted verification lost COPY or claimed completion: %+v, %v", got.evidence, got.err)
			}
			counts := map[string]int{}
			for _, command := range server.Commands() {
				counts[command]++
			}
			if counts["UID COPY"] != 1 || counts["UID STORE"] != 1 || counts["UID FETCH"] != 1 || counts["UID EXPUNGE"] != 0 || counts["EXPUNGE"] != 0 || !reflect.DeepEqual(server.DeletedUIDs(), []uint32{42, 99}) {
				t.Errorf("aborted verification replayed or expunged: %v, deleted %v", server.Commands(), server.DeletedUIDs())
			}
		})
	}
}

func TestMoveFallbackRequiresObservedDeleted(t *testing.T) {
	for _, deleteMessage := range []bool{false, true} {
		name := "move"
		if deleteMessage {
			name = "delete"
		}
		t.Run(name, func(t *testing.T) {
			server := newFakeServer(t, fakeServerConfig{
				authOK: true, trashMboxes: []string{"Trash"}, otherMboxes: []string{"INBOX", "Archive"},
				uidExpungeSupported: true, initialDeletedUIDs: []uint32{99},
				storeResponses: [][]byte{[]byte("* 7 FETCH (UID 42 FLAGS (\\Seen))\r\n<tag> OK STORE done\r\n")},
			})
			client, cfg := flagTestClient(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var evidence transport.MutationEvidence
			var err error
			destination := "Archive"
			if deleteMessage {
				destination = "Trash"
				evidence, err = client.DeleteMessage(ctx, cfg, "INBOX", 42, 12345)
			} else {
				evidence, err = client.MoveMessage(ctx, cfg, "INBOX", 42, 12345, destination)
			}
			if transport.ErrorCode(err) != transport.CodeIMAPMoveOutcomeUnknown || evidence.Outcome != transport.MutationOutcomePartial || evidence.FlagsState != transport.FlagObservationObserved || !reflect.DeepEqual(evidence.ActualFlags, []string{"\\Seen"}) || !reflect.DeepEqual(evidence.CompletedEffects, []string{"copy"}) {
				t.Errorf("contradictory Deleted result = %+v, %v", evidence, err)
			}
			if evidence.OperationID != transport.MutationOperationID("MOVE", cfg.Username, "INBOX", 42, 12345, destination) || evidence.CopyUIDValidity != 12345 || evidence.CopySourceUID != 42 || evidence.CopyDestinationUID != 100 {
				t.Errorf("lost COPY identity: %+v", evidence)
			}
			var outcome *transport.MutationOutcomeError
			if !errors.As(err, &outcome) || !reflect.DeepEqual(outcome.Evidence, evidence) {
				t.Errorf("returned/error evidence differ: %+v / %v", evidence, err)
			}
			stores, copies := 0, 0
			for _, command := range server.Commands() {
				switch command {
				case "UID STORE":
					stores++
				case "UID COPY":
					copies++
				case "UID EXPUNGE", "EXPUNGE":
					t.Errorf("unverified Deleted reached expunge: %v", server.Commands())
				}
			}
			if stores != 1 || copies != 1 || !reflect.DeepEqual(server.DeletedUIDs(), []uint32{42, 99}) {
				t.Errorf("unexpected command effects: %v, deleted %v", server.Commands(), server.DeletedUIDs())
			}
		})
	}
}
