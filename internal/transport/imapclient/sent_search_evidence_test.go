package imapclient

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestSentAppendRequiresValidSearchEvidence(t *testing.T) {
	messageID := "<search-proof@example.com>"
	message := []byte("Message-ID: " + messageID + "\r\n\r\nBody\r\n")
	for _, response := range []struct {
		name  string
		lines []string
	}{
		{name: "zero", lines: []string{"* SEARCH 0"}},
		{name: "negative", lines: []string{"* SEARCH -1"}},
		{name: "overflow", lines: []string{"* SEARCH 4294967296"}},
		{name: "text", lines: []string{"* SEARCH garbage"}},
		{name: "duplicate identities", lines: []string{"* SEARCH 1 1"}},
		{name: "repeated empty", lines: []string{"* SEARCH", "* SEARCH"}},
		{name: "contradictory", lines: []string{"* SEARCH", "* SEARCH 1"}},
		{name: "invalid prefix", lines: []string{"* SEARCHING"}},
	} {
		for _, afterAppend := range []bool{false, true} {
			phase := "before APPEND"
			if afterAppend {
				phase = "after APPEND"
			}
			t.Run(response.name+" "+phase, func(t *testing.T) {
				lines := append(append([]string(nil), response.lines...), "<tag> OK SEARCH completed")
				responses := [][]string{lines}
				if afterAppend {
					responses = [][]string{nil, lines}
				}
				srv := newFakeServer(t, fakeServerConfig{authOK: true, sentMboxes: []string{"Sent"}, appendOK: true, searchResponses: responses})
				client, cfg := newFakeClient(t, srv)
				t.Cleanup(func() {
					if err := client.Close(); err != nil {
						t.Error(err)
					}
				})
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				source := &appendContinuationReader{reader: bytes.NewReader(message)}
				evidence, err := client.AppendToSentReader(ctx, cfg, source, int64(len(message)), messageID)
				wantCode := transport.CodeIMAPResponseMalformed
				if afterAppend {
					wantCode = transport.CodeIMAPAppendOutcomeUnknown
				}
				if err == nil || transport.ErrorCode(err) != wantCode || evidence != (transport.AppendEvidence{}) {
					t.Fatalf("APPEND = %+v, %v; want %s", evidence, err, wantCode)
				}
				if afterAppend && transport.ErrorCode(errors.Unwrap(err)) != transport.CodeIMAPResponseMalformed {
					t.Fatalf("uncertain APPEND lost malformed SEARCH cause: %v", err)
				}
				called, _, _, data := srv.AppendRecord()
				appendCommands := 0
				for _, command := range srv.Commands() {
					if command == "APPEND" {
						appendCommands++
					}
				}
				if afterAppend {
					if !called || appendCommands != 1 || !bytes.Equal(data, message) {
						t.Fatalf("uncertain APPEND dispatched %d times with data %q", appendCommands, data)
					}
				} else if called || appendCommands != 0 || source.reads != 0 {
					t.Fatalf("invalid absence started APPEND: committed=%t, commands=%d, reads=%d", called, appendCommands, source.reads)
				}
			})
		}
	}
}

func TestSentAppendValidSearchEvidence(t *testing.T) {
	messageID := "<search-proof@example.com>"
	message := []byte("Message-ID: " + messageID + "\r\n\r\nBody\r\n")
	for _, test := range []struct {
		name     string
		lines    []string
		existing bool
		appended bool
		code     string
	}{
		{name: "empty", lines: []string{"* SEARCH"}, appended: true},
		{name: "omitted empty", appended: true},
		{name: "one verified", lines: []string{"* SEARCH 1"}, existing: true},
		{name: "multiple", lines: []string{"* SEARCH 1 2"}, existing: true, code: transport.CodeIMAPAmbiguousMessageID},
	} {
		t.Run(test.name, func(t *testing.T) {
			lines := append(append([]string(nil), test.lines...), "<tag> OK SEARCH completed")
			cfg := fakeServerConfig{authOK: true, sentMboxes: []string{"Sent"}, appendOK: true,
				searchResponses: [][]string{lines}}
			if test.existing {
				cfg.searchMatchID = messageID
			}
			srv := newFakeServer(t, cfg)
			client, transportConfig := newFakeClient(t, srv)
			t.Cleanup(func() {
				if err := client.Close(); err != nil {
					t.Error(err)
				}
			})
			evidence, err := client.AppendToSent(context.Background(), transportConfig, message, messageID)
			if (err != nil) != (test.code != "") || transport.ErrorCode(err) != test.code {
				t.Fatalf("APPEND = %+v, %v", evidence, err)
			}
			called, _, _, data := srv.AppendRecord()
			if called != test.appended || evidence.Appended != test.appended {
				t.Fatalf("APPEND committed=%t, evidence=%+v", called, evidence)
			}
			if err == nil && (evidence.UID != 42 || evidence.MatchCount != 1 || evidence.UIDValidity != 12345 || (called && !bytes.Equal(data, message))) {
				t.Fatalf("Sent evidence = %+v, data=%q", evidence, data)
			}
		})
	}
}

func TestSentAppendOmittedSearchAfterAppendStaysUnknown(t *testing.T) {
	messageID := "<search-proof@example.com>"
	message := []byte("Message-ID: " + messageID + "\r\n\r\nBody\r\n")
	srv := newFakeServer(t, fakeServerConfig{authOK: true, sentMboxes: []string{"Sent"}, appendOK: true,
		searchResponses: [][]string{nil, {"<tag> OK SEARCH completed"}}})
	client, cfg := newFakeClient(t, srv)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	evidence, err := client.AppendToSent(context.Background(), cfg, message, messageID)
	if transport.ErrorCode(err) != transport.CodeIMAPAppendOutcomeUnknown || evidence != (transport.AppendEvidence{}) {
		t.Fatalf("APPEND = %+v, %v; want %s", evidence, err, transport.CodeIMAPAppendOutcomeUnknown)
	}
	appendCommands := 0
	for _, command := range srv.Commands() {
		if command == "APPEND" {
			appendCommands++
		}
	}
	if called, _, _, data := srv.AppendRecord(); !called || appendCommands != 1 || !bytes.Equal(data, message) {
		t.Fatalf("APPEND committed=%t dispatched %d times", called, appendCommands)
	}
}
