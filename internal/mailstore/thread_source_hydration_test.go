package mailstore

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

type threadSourceHeaderFetcher struct {
	*readIntentHeaderFetcher
	headerErr error
	uid       uint32
	validity  uint32
}

func (o *threadSourceHeaderFetcher) FetchMessageHeaders(ctx context.Context, cfg transport.ImapConfig, mailbox string, uid, validity uint32, maximum int64) ([]byte, error) {
	o.uid, o.validity = uid, validity
	if o.headerErr != nil {
		o.headerFetchCalls++
		return nil, o.headerErr
	}
	return o.readIntentHeaderFetcher.FetchMessageHeaders(ctx, cfg, mailbox, uid, validity, maximum)
}

func TestThreadSourceHeaderHydration(t *testing.T) {
	for _, test := range []struct {
		name        string
		missing     bool
		legacy      bool
		stale       bool
		unsafe      bool
		mismatched  bool
		headerErr   error
		wantCode    string
		wantFetches int
	}{
		{name: "local headers"},
		{name: "missing local headers", missing: true, wantFetches: 1},
		{name: "legacy operator must not fetch body", missing: true, legacy: true, wantCode: "message_source_missing"},
		{name: "stale ref", missing: true, stale: true, wantCode: "stale_reference"},
		{name: "unsafe local source", unsafe: true, wantCode: "unsafe_message_source"},
		{name: "mismatched Message-ID", missing: true, mismatched: true, wantCode: transport.CodeIMAPMessageUIDMismatch, wantFetches: 1},
		{name: "mismatched UID generation", missing: true, headerErr: &transport.TransportError{Code: transport.CodeIMAPMessageUIDMismatch, Message: "generation changed"}, wantCode: transport.CodeIMAPMessageUIDMismatch, wantFetches: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, inbox := newSearchFixture(t)
			closeTestResource(t, store, "thread source fixture")
			installImapIdentityFixture(t, store, "thread-headers@gmail.com")
			page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inbox, Limit: 3})
			if err != nil {
				t.Fatal(err)
			}
			ref := messageRefWithExpectedID(t, messageRefWithSubject(t, page.Messages, "Status Update"), "102@example.com")
			client := &Client{store: store}
			local, err := client.MessageThreadSource(context.Background(), ref)
			if err != nil {
				t.Fatal(err)
			}
			message, err := store.GetMessageWithIntent(context.Background(), ref, mail.MessageReadIntentHeaders)
			if err != nil {
				t.Fatal(err)
			}
			operator := &threadSourceHeaderFetcher{readIntentHeaderFetcher: &readIntentHeaderFetcher{
				countingFetchOperator: &countingFetchOperator{stubImapOperator: stubImapOperator{uid: 202, boxes: []transport.MailboxInfo{{Name: "INBOX"}}}},
				headers:               []byte(message.Headers),
			}, headerErr: test.headerErr}
			client.send = mail.SendTransport{Imap: operator, Credentials: stubCredentials{"thread-headers@gmail.com": "secret"}}
			if test.legacy {
				client.send.Imap = operator.countingFetchOperator
			}
			base, err := store.messageBasePath(mustMailboxLocation(t, "imap://"+testAccountID+"/INBOX"), 102)
			if err != nil {
				t.Fatal(err)
			}
			if test.missing || test.unsafe {
				if err := os.Remove(base + ".emlx"); err != nil {
					t.Fatal(err)
				}
			}
			if test.unsafe {
				if err := os.Symlink(base+".absent", base+".emlx"); err != nil {
					t.Fatal(err)
				}
			}
			if test.stale {
				updateFixtureMessage(t, store, `UPDATE messages SET subject = 1 WHERE ROWID = 102`)
			}
			if test.mismatched {
				operator.headers = []byte(strings.ReplaceAll(message.Headers, "102@example.com", "other@example.com"))
			}
			got, err := client.MessageThreadSource(context.Background(), ref)
			if nestedErrorCode(err) != test.wantCode || operator.headerFetchCalls != test.wantFetches || operator.fetchCalls != 0 {
				t.Fatalf("thread source=%+v error=%v headers=%d body=%d", got, err, operator.headerFetchCalls, operator.fetchCalls)
			}
			if test.wantCode == "" && !reflect.DeepEqual(got, local) {
				t.Fatalf("hydrated source=%+v; local=%+v", got, local)
			}
			if test.wantFetches > 0 && (operator.uid != 202 || operator.validity != 12345) {
				t.Fatalf("unverified header target uid=%d validity=%d", operator.uid, operator.validity)
			}
		})
	}
}
