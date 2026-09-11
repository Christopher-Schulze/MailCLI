package imapclient

import (
	"context"
	"fmt"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestSelectedFlagPermissionsAreAuthoritative(t *testing.T) {
	literal := "* OK [PERMANENTFLAGS (\\*)] fabricated permission\r\n"
	for _, test := range []struct {
		name, response, code string
		stores, fetches      int
	}{
		{"lowercase tagged permissions", "* OK [UIDVALIDITY 12345] valid\r\n<tag> OK [permanentflags ($junk)] selected\r\n", "", 1, 1},
		{"last permission code wins", "* OK [UIDVALIDITY 12345] valid\r\n* OK [PERMANENTFLAGS (\\*)] supported\r\n<tag> OK [PERMANENTFLAGS ()] revoked\r\n", transport.CodeIMAPFlagsUnsupported, 0, 1},
		{"prose cannot grant permissions", "* OK [UIDVALIDITY 12345] valid\r\n* OK [PERMANENTFLAGS ()] none\r\n<tag> OK text [PERMANENTFLAGS (\\*)] is not authority\r\n", transport.CodeIMAPFlagsUnsupported, 0, 1},
		{"literal cannot grant permissions", fmt.Sprintf("* OK [UIDVALIDITY 12345] valid\r\n* OK [PERMANENTFLAGS ()] none\r\n* 9 FETCH (UID 99 BODY[] {%d}\r\n%s)\r\n<tag> OK selected\r\n", len(literal), literal), transport.CodeIMAPFlagsUnsupported, 0, 1},
		{"malformed permission list", "* OK [UIDVALIDITY 12345] valid\r\n<tag> OK [PERMANENTFLAGS NIL] selected\r\n", transport.CodeIMAPResponseMalformed, 0, 0},
		{"trailing permission token", "* OK [UIDVALIDITY 12345] valid\r\n<tag> OK [PERMANENTFLAGS ($Junk) trailing] selected\r\n", transport.CodeIMAPResponseMalformed, 0, 0},
		{"unterminated code", "* OK [UIDVALIDITY 12345] valid\r\n<tag> OK [PERMANENTFLAGS ($Junk)\r\n", transport.CodeIMAPResponseMalformed, 0, 0},
		{"tab cannot hide permission restriction", "* OK [UIDVALIDITY 12345] valid\r\n<tag> OK [PERMANENTFLAGS\t()] selected\r\n", transport.CodeIMAPResponseMalformed, 0, 0},
		{"conflicting selected identity", "* OK [UIDVALIDITY 12345] valid\r\n<tag> OK [UIDVALIDITY 54321] changed\r\n", transport.CodeIMAPResponseMalformed, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newFakeServer(t, fakeServerConfig{authOK: true, selectResponse: test.response})
			client, cfg := flagTestClient(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			evidence, err := client.SetFlags(ctx, cfg, "INBOX", 42, 12345, []string{"$Junk"}, []string{"$NotJunk"})
			if transport.ErrorCode(err) != test.code {
				t.Fatalf("result %+v, %v; want %s", evidence, err, test.code)
			}
			if test.stores == 0 && evidence.Outcome != transport.MutationOutcomeNotStarted {
				t.Fatalf("claimed pre-write effect: %+v", evidence)
			}
			assertFlagCommandCounts(t, server, test.stores, test.fetches)
		})
	}
}

func TestJunkCancellationBeforeStore(t *testing.T) {
	started := make(chan struct{}, 1)
	server := newFakeServer(t, fakeServerConfig{authOK: true, fetchStartedEvents: started, fetchDelay: 250 * time.Millisecond})
	client, cfg := flagTestClient(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		evidence transport.MutationEvidence
		err      error
	}
	finished := make(chan result, 1)
	go func() {
		evidence, err := client.SetFlags(ctx, cfg, "INBOX", 42, 12345, []string{"$Junk"}, []string{"$NotJunk"})
		finished <- result{evidence, err}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		cancel()
		<-finished
		t.Fatal("preflight did not start")
	}
	cancel()
	select {
	case got := <-finished:
		if got.err == nil || got.evidence.Outcome != transport.MutationOutcomeNotStarted || got.evidence.FlagsState == transport.FlagObservationObserved {
			t.Fatalf("preflight cancellation: %+v, %v", got.evidence, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not finish")
	}
	assertFlagCommandCounts(t, server, 0, 1)
}
