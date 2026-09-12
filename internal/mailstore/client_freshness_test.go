package mailstore

import (
	"context"
	"testing"

	"mailcli/internal/mail"
)

func TestClientCompleteLocalReadsWithoutMailAppOrTransport(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	// No Mail.app fallback, credentials or IMAP adapter exists in this client.
	client := &Client{store: store}
	closeTestResource(t, client, "local-only client")
	query, err := mail.PrepareQuery(mail.Query{MailboxRef: inboxRef, Subject: "Status", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.SearchMessages(context.Background(), query)
	if err != nil || len(page.Messages) != 1 || !page.Coverage.Complete {
		t.Fatalf("local search = %#v, error = %v", page, err)
	}
	message, err := client.GetMessage(context.Background(), page.Messages[0].Summary.Ref)
	if err != nil || !message.ContentComplete || message.ContentSource != "emlx_full" ||
		message.Content != "needle beta" || message.Summary.MessageID != "102@example.com" {
		t.Fatalf("complete local read = %#v, error = %v", message, err)
	}
}

func TestClientFreshSearchObservesExternalIndexUpdate(t *testing.T) {
	t.Parallel()
	store, inboxRef := newSearchFixture(t)
	client := &Client{store: store}
	closeTestResource(t, client, "local-only client")
	query, err := mail.PrepareQuery(mail.Query{MailboxRef: inboxRef, Subject: "Status", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	previousRevision := ""
	for _, test := range []struct {
		name string
		rows int
	}{
		{name: "before local synchronization", rows: 1},
		{name: "after external index update", rows: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.rows == 2 {
				mutateSearchIndex(t, store, `INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color)
					VALUES (104,1004,2004,1,2,2,400,400,1,0,0,0,0,100,4,0,400,0)`)
			}
			page, err := client.SearchMessages(context.Background(), query)
			if err != nil || len(page.Messages) != test.rows || !page.Coverage.Complete ||
				!page.Coverage.CandidateMessagesExact || page.Coverage.CandidateMessages != test.rows {
				t.Fatalf("fresh search = %#v, error = %v", page, err)
			}
			if page.Coverage.IndexRevision == "" || page.Coverage.IndexRevision == previousRevision {
				t.Fatalf("index revision did not advance: %q", page.Coverage.IndexRevision)
			}
			previousRevision = page.Coverage.IndexRevision
		})
	}
}
