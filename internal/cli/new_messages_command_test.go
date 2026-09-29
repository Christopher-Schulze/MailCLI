package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

type newMessagesTestGateway struct {
	testGateway
	request mail.NewMessagesRequest
	result  mail.NewMessagesResult
	err     error
}

func (g *newMessagesTestGateway) NewMessages(_ context.Context, request mail.NewMessagesRequest) (mail.NewMessagesResult, error) {
	g.request = request
	return g.result, g.err
}

func runNewMessages(t *testing.T, gateway *newMessagesTestGateway, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), mail.NewService(gateway), append([]string{"messages", "new"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func sampleNewMessagesResult(complete bool, newCount int) mail.NewMessagesResult {
	result := mail.NewMessagesResult{
		Mailboxes: []mail.NewMailbox{{
			AccountRef: "acct_a", MailboxRef: "mbx_a", Name: "INBOX", State: mail.NewMailboxStateChecked,
			ServerMessages: 9, ScannedMessages: 9, NewCount: newCount, Messages: []mail.NewMessage{},
		}},
		Failures: []mail.NewMessagesFailure{}, Skipped: []mail.SyncCheckSkip{}, Complete: complete, NewCount: newCount,
	}
	for index := range newCount {
		result.Mailboxes[0].Messages = append(result.Mailboxes[0].Messages, mail.NewMessage{
			ServerRef: "srv_" + string(rune('a'+index)), Subject: "Subject", Sender: "Sender <s@example.com>", Unseen: true,
		})
	}
	if !complete {
		result.Failures = append(result.Failures, mail.NewMessagesFailure{Account: "acct_b", Code: "imap_credentials_missing", Message: "no stored password"})
	}
	return result
}

func TestMessagesNewPassesFlagsAndReturnsTheEnvelope(t *testing.T) {
	gateway := &newMessagesTestGateway{result: sampleNewMessagesResult(true, 2)}
	code, stdout, stderr := runNewMessages(t, gateway, "--account", "acct_a", "--mailbox", "sent", "--limit", "7", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if gateway.request != (mail.NewMessagesRequest{AccountRef: "acct_a", MailboxRef: "sent", Limit: 7}) {
		t.Fatalf("request = %+v", gateway.request)
	}
	var response envelope
	if err := json.Unmarshal([]byte(stdout), &response); err != nil {
		t.Fatalf("decode: %v, output = %q", err, stdout)
	}
	if !response.OK || response.Command != "messages.new" || response.Data.NewMessages == nil ||
		response.Data.NewMessages.NewCount != 2 || len(response.Data.NewMessages.Mailboxes[0].Messages) != 2 {
		t.Fatalf("response = %+v", response)
	}
	if response.Next == nil || response.Next.Do != "check_state" || response.Next.Command != "" {
		t.Fatalf("next = %+v, want check_state without a command", response.Next)
	}
}

func TestMessagesNewNextActionFollowsTheOutcome(t *testing.T) {
	tests := []struct {
		name     string
		result   mail.NewMessagesResult
		wantNext string
	}{
		{"complete without new messages has no next action", sampleNewMessagesResult(true, 0), ""},
		{"incomplete without new messages asks the user", sampleNewMessagesResult(false, 0), "ask_user"},
		{"partial failure remains visible beside new messages", sampleNewMessagesResult(false, 1), "ask_user"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, _ := runNewMessages(t, &newMessagesTestGateway{result: test.result}, "--json")
			var response envelope
			if err := json.Unmarshal([]byte(stdout), &response); code != 0 || err != nil {
				t.Fatalf("exit code = %d, err = %v, output = %q", code, err, stdout)
			}
			got := ""
			if response.Next != nil {
				got = response.Next.Do
			}
			if got != test.wantNext {
				t.Fatalf("next = %q, want %q", got, test.wantNext)
			}
		})
	}
}

func TestMessagesNewRejectsALimitOutsideTheRange(t *testing.T) {
	for _, limit := range []string{"0", "-1", "51"} {
		code, stdout, _ := runNewMessages(t, &newMessagesTestGateway{}, "--limit", limit, "--require-complete", "--json")
		if code != 2 || !strings.Contains(stdout, `"ok":false`) {
			t.Errorf("--limit %s: exit code = %d, output = %q", limit, code, stdout)
		}
	}
}

func TestMessagesNewHumanOutputListsRowsFailuresAndSkips(t *testing.T) {
	result := sampleNewMessagesResult(false, 1)
	result.Skipped = []mail.SyncCheckSkip{{Account: "acct_local", Reason: "local_account"}}
	code, stdout, _ := runNewMessages(t, &newMessagesTestGateway{result: result})
	if code != 0 {
		t.Fatalf("exit code = %d, output = %q", code, stdout)
	}
	for _, want := range []string{"complete\tfalse\tnew_count\t1", "scanned_messages=9", "window_limited=false", "srv_a", "unseen", "Sender <s@example.com>", "skipped", "local_account", "failures", "imap_credentials_missing"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("human output lacks %q: %q", want, stdout)
		}
	}
}

func TestMessagesNewStrictCoveragePreservesEvidenceAndExitPrecedence(t *testing.T) {
	for _, test := range []struct {
		name       string
		complete   bool
		args       []string
		wantExit   int
		json       bool
		limited    bool
		unresolved bool
		runtimeErr bool
	}{
		{name: "complete default JSON", complete: true, args: []string{"--json"}, json: true},
		{name: "complete strict JSON", complete: true, args: []string{"--require-complete", "--json"}, json: true},
		{name: "partial default JSON", args: []string{"--json"}, json: true},
		{name: "partial strict JSON", args: []string{"--require-complete", "--json"}, wantExit: 3, json: true},
		{name: "partial explicitly non-strict JSON", args: []string{"--require-complete=false", "--json"}, json: true},
		{name: "complete strict human", complete: true, args: []string{"--require-complete"}},
		{name: "partial default human"},
		{name: "partial strict human", args: []string{"--require-complete"}, wantExit: 3},
		{name: "bounded window alone is complete", complete: true, args: []string{"--require-complete", "--json"}, json: true, limited: true},
		{name: "unresolved local comparison is incomplete", args: []string{"--require-complete", "--json"}, json: true, wantExit: 3, unresolved: true},
		{name: "runtime failure wins over strictness", args: []string{"--require-complete", "--json"}, json: true, wantExit: 1, runtimeErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := sampleNewMessagesResult(test.complete, 1)
			if test.limited {
				result.Mailboxes[0].ServerMessages, result.Mailboxes[0].ScannedMessages = 900, 100
				result.Mailboxes[0].WindowLimited = true
			}
			if test.unresolved {
				result.Failures, result.NewCount = []mail.NewMessagesFailure{}, 0
				result.Mailboxes[0].State, result.Mailboxes[0].Reason = mail.NewMailboxStateUnresolved, "local UIDVALIDITY unavailable"
				result.Mailboxes[0].NewCount, result.Mailboxes[0].Messages = 0, []mail.NewMessage{}
			}
			gateway := &newMessagesTestGateway{result: result}
			if test.runtimeErr {
				gateway.err = errors.New("discovery gateway unavailable")
			}
			code, stdout, stderr := runNewMessages(t, gateway, test.args...)
			if code != test.wantExit || stderr != "" {
				t.Fatalf("exit=%d stderr=%q stdout=%q; want exit %d", code, stderr, stdout, test.wantExit)
			}
			if !test.json {
				for _, want := range []string{"scanned_messages=9", "window_limited=false", "srv_a"} {
					if !strings.Contains(stdout, want) {
						t.Fatalf("strict human output lost %q: %s", want, stdout)
					}
				}
				return
			}
			var response envelope
			if err := json.Unmarshal([]byte(stdout), &response); err != nil {
				t.Fatal(err)
			}
			if test.runtimeErr {
				if response.OK || response.Error == nil || response.Error.Code != "operation_failed" {
					t.Fatalf("runtime failure=%+v", response)
				}
				return
			}
			got := response.Data.NewMessages
			if !response.OK || response.Error != nil || got == nil || got.Complete != test.complete || got.NewCount != result.NewCount || len(got.Failures) != len(result.Failures) || len(got.Mailboxes) != 1 {
				t.Fatalf("strict coverage changed success evidence: %+v", response)
			}
			mailbox := got.Mailboxes[0]
			if mailbox.ScannedMessages != result.Mailboxes[0].ScannedMessages || mailbox.WindowLimited != test.limited || len(mailbox.Messages) != len(result.Mailboxes[0].Messages) {
				t.Fatalf("scope/rows lost: %+v", mailbox)
			}
			wantNext := "check_state"
			if !test.complete {
				wantNext = "ask_user"
			}
			if response.Next == nil || response.Next.Do != wantNext || response.Next.Command != "" || len(response.Next.Args) != 0 {
				t.Fatalf("next=%+v; want %s without an invented command", response.Next, wantNext)
			}
			if !test.complete && (!strings.Contains(response.Next.Why, "failures") || !strings.Contains(response.Next.Why, "mailbox reasons") || strings.Contains(response.Next.Why, "sync")) {
				t.Fatalf("incomplete next hides failure reasons: %+v", response.Next)
			}
		})
	}
}

func TestMessagesNewStrictCoverageCannotHideOutputFailure(t *testing.T) {
	for _, mode := range []string{"json", "human"} {
		t.Run(mode, func(t *testing.T) {
			gateway := &newMessagesTestGateway{result: sampleNewMessagesResult(false, 1)}
			var stderr bytes.Buffer
			args := []string{"messages", "new", "--require-complete"}
			if mode == "json" {
				args = append(args, "--json")
			}
			code := Run(context.Background(), mail.NewService(gateway), args, failingWriter{}, &stderr)
			if code != 1 {
				t.Fatalf("output failure returned %d; want runtime exit 1", code)
			}
		})
	}
}

func TestMessagesNewPublishesStrictFlagAndWindowEvidence(t *testing.T) {
	code, help, stderr := runNewMessages(t, &newMessagesTestGateway{}, "--help")
	if code != 0 || stderr != "" || !strings.Contains(help, "--require-complete") || !strings.Contains(help, "exit 3") {
		t.Fatalf("help=%q stderr=%q exit=%d", help, stderr, code)
	}
	code, _, response := captureCapabilitiesJSON(t, "--for", "messages.new", "--schemas", "--outputs", "--json")
	manifest := response.Data.Capabilities
	if code != 0 || manifest == nil || len(manifest.Commands) != 1 {
		t.Fatalf("scoped discovery=%+v exit=%d", response, code)
	}
	var schema struct {
		Flags []outputParameter `json:"flags"`
	}
	if err := json.Unmarshal(manifest.Commands[0].Schema, &schema); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, flag := range schema.Flags {
		if flag.Name == "--require-complete" {
			found = flag.ValueType == "boolean" && !flag.TakesValue && strings.Contains(flag.Description, "exit 3")
		}
	}
	if !found {
		t.Fatal("canonical strict flag is absent or wrong")
	}
	for name, kind := range map[string]string{"scanned_messages": "integer", "window_limited": "boolean"} {
		found := false
		for _, field := range manifest.OutputDefinitions["new_mailbox"].Fields {
			if field.Name == name {
				found = field.Type == kind && field.AlwaysPresent && !field.Nullable
			}
		}
		if !found {
			t.Fatalf("missing always-present typed output field %s", name)
		}
	}
}
