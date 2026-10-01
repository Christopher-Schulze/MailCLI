package mailstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

type selectedAccountCredentials struct {
	stubCredentials
	loads int
}

func (c *selectedAccountCredentials) Load(account string) (string, error) {
	c.loads++
	return c.stubCredentials.Load(account)
}

func TestNewAndSyncSelectDecodedAccountIdentity(t *testing.T) {
	canonical, err := mailref.EncodeAccount(testAccountID)
	if err != nil {
		t.Fatal(err)
	}
	refs := []string{canonical}
	for _, accountID := range []string{testAccountID, strings.ToLower(testAccountID)} {
		legacy, err := json.Marshal(mailref.Account{Version: mailref.LegacyFormatVersion, AccountID: accountID})
		if err != nil {
			t.Fatal(err)
		}
		ref, err := mailref.EncodeToken("acct_", legacy)
		if err != nil {
			t.Fatal(err)
		}
		compact, err := mailref.EncodeCompactTokenPayload("acct_", &mailref.CompactPayload{AccountID: accountID}, byte(mailref.FormatVersion))
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref, compact)
	}
	for _, operation := range []string{"new", "sync"} {
		for _, ref := range refs {
			t.Run(operation+"/"+ref, func(t *testing.T) {
				client, operator := newMessagesFixture(t, "selection@gmail.com", map[string]transport.RecentMailbox{"INBOX": {UIDValidity: 900}})
				credentials := &selectedAccountCredentials{stubCredentials: stubCredentials{}}
				client.send.Credentials = credentials
				if operation == "new" {
					result, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{AccountRef: ref, Limit: 5})
					if err != nil || !result.Complete || len(result.Mailboxes) != 1 || result.Mailboxes[0].AccountRef != canonical {
						t.Fatalf("decoded selection result=%+v error=%v", result, err)
					}
				} else {
					result, err := client.SyncCheck(context.Background(), ref)
					if err != nil || len(result.Mailboxes) == 0 {
						t.Fatalf("decoded selection result=%+v error=%v", result, err)
					}
					for _, mailbox := range result.Mailboxes {
						if mailbox.AccountRef != canonical {
							t.Fatalf("catalog identity changed: %+v", mailbox)
						}
					}
				}
				if credentials.loads == 0 || operator.listCalls != 1 {
					t.Fatalf("selection never reached the selected account: loads=%d LIST=%d", credentials.loads, operator.listCalls)
				}
			})
		}
	}
}

func TestNewAndSyncRejectInvalidAccountBeforeCredentialIO(t *testing.T) {
	unsupported, err := mailref.EncodeToken("acct_", []byte(`{"version":99,"account_id":"`+testAccountID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	missing, err := mailref.EncodeAccount("FFFFFFFF-BBBB-4CCC-8DDD-EEEEEEEEEEEE")
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"new", "sync"} {
		for _, test := range []struct{ ref, code string }{
			{"acct_!!!", "account_reference_corrupt"}, {"wrong_prefix", "account_reference_corrupt"},
			{unsupported, "account_reference_version_unsupported"}, {missing, "account_not_found"},
		} {
			t.Run(operation+"/"+test.ref, func(t *testing.T) {
				client, operator := newMessagesFixture(t, "selection@gmail.com", nil)
				credentials := &selectedAccountCredentials{stubCredentials: stubCredentials{}}
				client.send.Credentials = credentials
				var err error
				if operation == "new" {
					_, err = client.NewMessages(context.Background(), mail.NewMessagesRequest{AccountRef: test.ref, Limit: 5})
				} else {
					_, err = client.SyncCheck(context.Background(), test.ref)
				}
				var typed interface{ ErrorCode() string }
				if !errors.As(err, &typed) || typed.ErrorCode() != test.code || credentials.loads != 0 || operator.listCalls != 0 || len(operator.calls) != 0 {
					t.Fatalf("error=%v want=%q loads=%d LIST=%d recent=%v", err, test.code, credentials.loads, operator.listCalls, operator.calls)
				}
			})
		}
	}
}
