package mailstore

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/cli"
	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

func TestUnavailableStoreReadRecovery(t *testing.T) {
	accountRef, err := mailref.EncodeAccount(testAccountID)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		mutation   string
		permission bool
		code       string
		retry      mail.Retryability
		next       string
	}{
		{
			name:     "unsupported generation",
			mutation: "UPDATE properties SET value='99999' WHERE key='minor_version'",
			code:     "unsupported_mail_store_schema", retry: mail.RetryTerminal, next: "stop",
		},
		{
			name: "missing capability", mutation: "ALTER TABLE server_messages DROP COLUMN forwarded",
			code: "unsupported_mail_store_schema", retry: mail.RetryTerminal, next: "stop",
		},
		{
			name: "permission denied", permission: true,
			retry: mail.RetryUserInputRequired, next: "ask_user",
		},
		{
			name: "not initialized", code: "mail_store_not_initialized",
			retry: mail.RetryUserInputRequired, next: "ask_user",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			fallback := &inboxFallbackSpy{}
			config := Config{MailRoot: t.TempDir()}
			if test.mutation != "" {
				fixture := createSearchFixtureData(t, 0, false)
				config = Config{MailRoot: fixture.mailRoot, ActiveAccountURLs: fixture.activeAccountURLs}
				writer := openTestWriter(t, filepath.Join(fixture.mailRoot, "V10", "MailData", envelopeIndexName))
				if _, err := writer.ExecContext(ctx, test.mutation); err != nil {
					closeTestResourceNow(t, writer, "schema writer")
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			}
			client := NewClient(ctx, fallback, config, mail.SendTransport{})
			closeTestResource(t, client, "unavailable store client")
			if test.permission {
				// Inject the OS access-denied boundary without depending on the
				// test runner's privileges or touching a real Mail store.
				client.storeErr = operationErrorWithCause("mail_store_unavailable", "access denied", fs.ErrPermission)
			}
			if client.store != nil || client.storeErr == nil {
				t.Fatalf("expected a refused store, got %+v", client)
			}
			service := mail.NewService(client)
			for _, command := range []struct {
				args        []string
				unavailable string
			}{
				{[]string{"doctor"}, "mail_store_unavailable"},
				{[]string{"mailboxes", "list", "--limit", "1"}, "safe_mailbox_listing_unavailable"},
				{[]string{"mailboxes", "resolve", "--account", accountRef, "--path", "INBOX"}, "safe_mailbox_listing_unavailable"},
				{[]string{"messages", "list", "--limit", "1"}, "safe_message_listing_unavailable"},
				{[]string{"messages", "list", "--mailbox", "inbox", "--limit", "1"}, "safe_message_listing_unavailable"},
				{[]string{"messages", "search", "--query", "recovery-probe", "--limit", "1"}, "safe_search_unavailable"},
				{[]string{"messages", "filter", "--read", "false", "--limit", "1"}, "safe_search_unavailable"},
			} {
				t.Run(strings.Join(command.args, " "), func(t *testing.T) {
					var stdout, stderr bytes.Buffer
					args := append(append([]string(nil), command.args...), "--json")
					status := cli.Run(ctx, service, args, &stdout, &stderr)
					var response struct {
						OK    bool `json:"ok"`
						Error struct {
							Code     string                 `json:"code"`
							Guidance mail.OperationGuidance `json:"guidance"`
						} `json:"error"`
						Next struct {
							Do  string `json:"do"`
							Why string `json:"why"`
						} `json:"next"`
					}
					if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
						t.Fatalf("decode response: %v; stdout=%s stderr=%s", err, &stdout, &stderr)
					}
					wantCode := test.code
					if wantCode == "" {
						wantCode = command.unavailable
					}
					guidance := response.Error.Guidance
					if status != 1 || response.OK || response.Error.Code != wantCode ||
						guidance.Retryability != test.retry || guidance.EffectCertainty != mail.EffectNone ||
						guidance.ReplayAllowed || response.Next.Do != test.next || stderr.Len() != 0 {
						t.Fatalf("status=%d response=%s stderr=%s", status, &stdout, &stderr)
					}
					if test.next == "stop" {
						for _, forbidden := range []string{"Full Disk Access", "open Mail", "create its local store"} {
							if strings.Contains(guidance.Recovery.Instruction+response.Next.Why, forbidden) {
								t.Fatalf("unsupported schema requests unrelated repair: %s", &stdout)
							}
						}
					}
				})
			}
			if fallback.listCalls != 0 || fallback.mailboxCalls != 0 || fallback.accountCalls != 0 {
				t.Fatalf("refused read called fallback: %+v", fallback)
			}
		})
	}
}
