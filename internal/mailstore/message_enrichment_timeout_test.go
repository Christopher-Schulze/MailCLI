package mailstore

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

type deadlineExcerptOperator struct {
	*excerptBatchOperator
	fetch func(context.Context, string, []uint32) (map[uint32]transport.MessageExcerptSource, error)
}

func (o *deadlineExcerptOperator) FetchMessageExcerpts(ctx context.Context, _ transport.ImapConfig, mailbox string, _ uint32, uids []uint32, _ int64) (map[uint32]transport.MessageExcerptSource, error) {
	return o.fetch(ctx, mailbox, uids)
}

type deadlineCredentials struct {
	transport.CredentialStore
	load func(string) (string, error)
}

func (c *deadlineCredentials) Load(account string) (string, error) {
	return c.load(account)
}

func TestEnrichmentDeadlineKeepsLocalAndThreadingProof(t *testing.T) {
	operator := &excerptBatchOperator{}
	client, refs := newLocalUIDExcerptFixture(t, operator)
	var fetches atomic.Int64
	client.send.Imap = &deadlineExcerptOperator{excerptBatchOperator: operator, fetch: func(ctx context.Context, _ string, _ []uint32) (map[uint32]transport.MessageExcerptSource, error) {
		fetches.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	rows, err := client.EnrichMessages(ctx, refs, mail.MessageEnrichmentRequest{Threading: true, Excerpt: true, ExcerptLength: 240})
	if err != context.DeadlineExceeded || len(rows) != len(refs) || fetches.Load() != 1 {
		t.Fatalf("rows=%d fetches=%d error=%v", len(rows), fetches.Load(), err)
	}
	for index, row := range rows {
		if row.Ref != refs[index] || !row.ThreadingComplete {
			t.Fatalf("row %d lost identity/threading: %+v", index, row)
		}
		if index == 1 {
			if row.EnrichmentError != "" || row.Excerpt == "" || !row.ExcerptComplete || row.ExcerptSource != mail.ExcerptSourceLocal {
				t.Fatalf("completed local metadata lost: %+v", row)
			}
		} else if row.EnrichmentError != operationTimeoutCode || row.ExcerptComplete {
			t.Fatalf("unfinished remote metadata overstated: %+v", row)
		}
	}
}

func TestEnrichmentDeadlineStopsCredentialAndChunkWork(t *testing.T) {
	operator := &excerptBatchOperator{}
	client, refs := newLocalUIDExcerptFixture(t, operator)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var loads, lateLoads atomic.Int64
	client.send.Credentials = &deadlineCredentials{CredentialStore: client.send.Credentials, load: func(string) (string, error) {
		loads.Add(1)
		if ctx.Err() != nil {
			lateLoads.Add(1)
		}
		<-ctx.Done()
		return "generated-test-password", nil
	}}
	pageRefs := []string{refs[1], refs[0], refs[2], refs[1], refs[1]}
	rows, err := client.EnrichMessages(ctx, pageRefs, mail.MessageEnrichmentRequest{Threading: true, Excerpt: true, ExcerptLength: 240})
	if err != context.DeadlineExceeded || len(rows) != len(pageRefs) || loads.Load() != 2 || lateLoads.Load() != 0 ||
		operator.listCalls != 0 || operator.searchCalls != 0 || len(operator.fetchUIDs) != 0 {
		t.Fatalf("rows=%d loads=%d late=%d LIST=%d SEARCH=%d FETCH=%v error=%v", len(rows), loads.Load(), lateLoads.Load(), operator.listCalls, operator.searchCalls, operator.fetchUIDs, err)
	}
	for _, index := range []int{0, 3} {
		if rows[index].Ref != pageRefs[index] || !rows[index].ThreadingComplete || !rows[index].ExcerptComplete || rows[index].EnrichmentError != "" {
			t.Fatalf("completed row %d lost: %+v", index, rows[index])
		}
	}
	if rows[4].Ref != pageRefs[4] || rows[4].ThreadingComplete || rows[4].ExcerptComplete || rows[4].ExcerptSource != mail.ExcerptSourceUnavailable || rows[4].EnrichmentError != operationTimeoutCode {
		t.Fatalf("later chunk was started or left unqualified: %+v", rows[4])
	}
}

func TestEnrichmentExpiredDeadlineReturnsEveryUnfinishedRef(t *testing.T) {
	operator := &excerptBatchOperator{}
	client, refs := newLocalUIDExcerptFixture(t, operator)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	rows, err := client.EnrichMessages(ctx, refs, mail.MessageEnrichmentRequest{Threading: true, Excerpt: true, ExcerptLength: 240})
	if err != context.DeadlineExceeded || len(rows) != len(refs) || operator.listCalls != 0 || len(operator.fetchUIDs) != 0 {
		t.Fatalf("rows=%d LIST=%d FETCH=%v error=%v", len(rows), operator.listCalls, operator.fetchUIDs, err)
	}
	for index, row := range rows {
		if row.Ref != refs[index] || row.EnrichmentError != operationTimeoutCode || row.ThreadingComplete || row.ExcerptComplete || row.ExcerptSource != mail.ExcerptSourceUnavailable {
			t.Fatalf("expired row %d = %+v", index, row)
		}
	}
}

func TestEnrichmentDeadlineStopsCatalogCredentialProbes(t *testing.T) {
	accountRef, err := mailref.EncodeAccount(testAccountID)
	if err != nil {
		t.Fatal(err)
	}
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "unbound", true: "bound"}[bound], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			var loads atomic.Int64
			credentials := &deadlineCredentials{CredentialStore: strictCredentials{}, load: func(string) (string, error) {
				loads.Add(1)
				<-ctx.Done()
				return "", nil
			}}
			bindings := mail.AccountBindingFile{Version: mail.AccountBindingVersion}
			if bound {
				bindings.Bindings = []mail.AccountBinding{{AccountID: testAccountID, SenderAliases: []string{"first@gmail.com"}, CredentialAccount: "login@gmail.com"}}
			}
			_, _, _, err := resolveAccountIdentityFromCatalog(ctx, []mail.Account{{Ref: accountRef, State: "ok", EmailAddresses: []string{"first@gmail.com", "second@gmail.com"}}}, testAccountID, credentials, bindings)
			if err != context.DeadlineExceeded || loads.Load() != 1 {
				t.Fatalf("expired credential probe continued or changed cause: loads=%d error=%v", loads.Load(), err)
			}
		})
	}
}

func TestRemoteEnrichmentDeadlineKeepsCompletedMailboxAndSkipsCacheWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var fetches atomic.Int64
	operator := &deadlineExcerptOperator{excerptBatchOperator: &excerptBatchOperator{countingFetchOperator: &countingFetchOperator{}}, fetch: func(fetchCtx context.Context, mailbox string, uids []uint32) (map[uint32]transport.MessageExcerptSource, error) {
		fetches.Add(1)
		if mailbox == "slow" {
			<-fetchCtx.Done()
		}
		return map[uint32]transport.MessageExcerptSource{uids[0]: {Source: []byte("Content-Type: text/plain\r\n\r\nCompleted text"), Complete: true}}, nil
	}}
	client := &Client{send: mail.SendTransport{Imap: operator}, excerpts: excerptCache{dir: t.TempDir()}}
	remote := []remoteExcerpt{
		{index: 0, target: imapTarget{accountID: "account", imapMailbox: "fast", uid: 1, uidvalidity: 1}},
		{index: 1, target: imapTarget{accountID: "account", imapMailbox: "slow", uid: 2, uidvalidity: 1}},
	}
	inputs := []excerptInput{{source: mail.ExcerptSourceUnavailable, cacheKey: "fast"}, {source: mail.ExcerptSourceUnavailable, cacheKey: "slow"}}
	rows := []mail.MessageSummary{{Ref: "fast"}, {Ref: "slow"}}
	finished := make([]bool, 2)
	err := client.fetchRemoteExcerpts(ctx, remote, inputs, rows, finished, 240)
	if err != context.DeadlineExceeded || fetches.Load() != 2 || !finished[0] || !finished[1] ||
		rows[0].Excerpt != "Completed text" || !rows[0].ExcerptComplete || rows[0].EnrichmentError != "" ||
		rows[1].ExcerptComplete || rows[1].EnrichmentError != operationTimeoutCode {
		t.Fatalf("rows=%+v finished=%v fetches=%d error=%v", rows, finished, fetches.Load(), err)
	}
	if _, err := os.Stat(client.excerpts.path("fast")); err != nil {
		t.Fatalf("completed cache entry missing: %v", err)
	}
	if _, err := os.Stat(client.excerpts.path("slow")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired result published a cache entry: %v", err)
	}
	if err := client.fetchRemoteExcerpts(ctx, remote, inputs, rows, finished, 240); err != context.DeadlineExceeded || fetches.Load() != 2 {
		t.Fatalf("expired grouping dispatched remote work: fetches=%d error=%v", fetches.Load(), err)
	}
}
