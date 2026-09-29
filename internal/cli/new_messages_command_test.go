package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

type newMessagesTestGateway struct {
	testGateway
	request mail.NewMessagesRequest
	result  mail.NewMessagesResult
}

func (g *newMessagesTestGateway) NewMessages(_ context.Context, request mail.NewMessagesRequest) (mail.NewMessagesResult, error) {
	g.request = request
	return g.result, nil
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
			ServerMessages: 9, NewCount: newCount, Messages: []mail.NewMessage{},
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
		{"new messages win over a partial failure", sampleNewMessagesResult(false, 1), "check_state"},
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
		code, stdout, _ := runNewMessages(t, &newMessagesTestGateway{}, "--limit", limit, "--json")
		if code == 0 || !strings.Contains(stdout, `"ok":false`) {
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
	for _, want := range []string{"complete\tfalse\tnew_count\t1", "srv_a", "unseen", "Sender <s@example.com>", "skipped", "local_account", "failures", "imap_credentials_missing"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("human output lacks %q: %q", want, stdout)
		}
	}
}
