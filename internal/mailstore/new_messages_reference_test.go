package mailstore

import (
	"context"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

func TestNewMessagesMailboxReferences(t *testing.T) {
	accountRef, err := mailref.EncodeAccount(testAccountID)
	if err != nil {
		t.Fatal(err)
	}
	otherAccount, err := mailref.EncodeAccount("FFFFFFFF-BBBB-4CCC-8DDD-EEEEEEEEEEEE")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		path       []string
		account    string
		malformed  bool
		wantCode   string
		wantListed bool
	}{
		{name: "implicit account", path: []string{"INBOX"}, wantListed: true},
		{name: "explicit account", path: []string{"INBOX"}, account: accountRef, wantListed: true},
		{name: "account conflict", path: []string{"INBOX"}, account: otherAccount, wantCode: "invalid_argument"},
		{name: "malformed", malformed: true, wantCode: "invalid_reference"},
		{name: "missing mailbox", path: []string{"Missing"}, wantCode: "not_found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, operator := newMessagesFixture(t, "new-refs@gmail.com", map[string]transport.RecentMailbox{"INBOX": {UIDValidity: 900}})
			other, err := parseAccountRoot("imap://FFFFFFFF-BBBB-4CCC-8DDD-EEEEEEEEEEEE/")
			if err != nil {
				t.Fatal(err)
			}
			client.store.activeAccounts = append(client.store.activeAccounts, other)
			client.store.activeAccountKeys[other.rootKey()] = struct{}{}
			ref := "mbx_invalid"
			if !test.malformed {
				ref, err = mailref.EncodeMailbox(testAccountID, test.path)
				if err != nil {
					t.Fatal(err)
				}
			}
			if !test.wantListed {
				client.send.Credentials = strictCredentials{}
			}
			result, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{MailboxRef: ref, AccountRef: test.account, Limit: 5})
			code := errorCodeForTest(err)
			if code == "" && len(result.Failures) == 1 {
				code = result.Failures[0].Code
			}
			if code != test.wantCode {
				t.Fatalf("result=%+v error=%v code=%q; want %q", result, err, code, test.wantCode)
			}
			if test.wantListed {
				if err != nil || !result.Complete || len(result.Mailboxes) != 1 || result.Mailboxes[0].AccountRef != accountRef || result.Mailboxes[0].MailboxRef != ref || len(operator.calls) != 1 || operator.calls[0] != "INBOX" {
					t.Fatalf("reference selection result=%+v error=%v calls=%v", result, err, operator.calls)
				}
			} else if len(operator.calls) != 0 {
				t.Fatalf("invalid selection reached IMAP: %v", operator.calls)
			}
		})
	}
}
