package mailstore

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

type delayedSyncImapOperator struct {
	*stubImapOperator
	limit   int
	delay   time.Duration
	started chan struct{}

	mu         sync.Mutex
	active     int
	maxActive  int
	statusCall []string
}

func (s *delayedSyncImapOperator) MaxConnectionsPerAccount() int {
	return s.limit
}

func (s *delayedSyncImapOperator) CheckStatus(
	ctx context.Context,
	cfg transport.ImapConfig,
	mailbox string,
) (transport.MailboxStatus, error) {
	s.mu.Lock()
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	s.statusCall = append(s.statusCall, mailbox)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()
	if s.started != nil {
		select {
		case s.started <- struct{}{}:
		default:
		}
	}
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return s.stubImapOperator.CheckStatus(ctx, cfg, mailbox)
	case <-ctx.Done():
		return transport.MailboxStatus{}, ctx.Err()
	}
}

func (s *delayedSyncImapOperator) stats() (active, maxActive, calls int, mailboxes []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.maxActive, len(s.statusCall), append([]string(nil), s.statusCall...)
}

func newDelayedSyncCheckClient(
	t *testing.T,
	limit int,
	delay time.Duration,
	remoteCount int,
	started chan struct{},
	failedMailbox string,
) (*Client, *delayedSyncImapOperator) {
	t.Helper()
	store, _ := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	address := "sync-status@gmail.com"
	installImapIdentityFixture(t, store, address)
	boxes := []transport.MailboxInfo{
		{Name: "INBOX"},
		{Name: "All"},
		{Name: "Sent", Flags: []string{"\\Sent"}},
	}
	statusByMailbox := make(map[string]transport.MailboxStatus, remoteCount)
	for index := range remoteCount {
		name := fmt.Sprintf("Remote-%02d", index)
		boxes = append(boxes, transport.MailboxInfo{Name: name})
		statusByMailbox[name] = transport.MailboxStatus{Mailbox: name, Messages: index + 1, Unseen: index % 2}
	}
	statusErr := make(map[string]error)
	if failedMailbox != "" {
		statusErr[failedMailbox] = &transport.TransportError{
			Code:    transport.CodeIMAPTimeout,
			Message: "delayed STATUS failure",
		}
	}
	op := &delayedSyncImapOperator{
		stubImapOperator: &stubImapOperator{
			boxes:           boxes,
			status:          transport.MailboxStatus{Messages: 7, Unseen: 1},
			statusByMailbox: statusByMailbox,
			statusErr:       statusErr,
		},
		limit:   limit,
		delay:   delay,
		started: started,
	}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        op,
			Credentials: stubCredentials{address: "secret"},
		},
	}
	return client, op
}

func TestSyncCheckParallelizesStatusWithinPoolLimit(t *testing.T) {
	t.Parallel()
	const (
		remoteCount = 8
		statusDelay = 15 * time.Millisecond
	)
	serialClient, serialOp := newDelayedSyncCheckClient(t, 1, statusDelay, remoteCount, nil, "")
	parallelClient, parallelOp := newDelayedSyncCheckClient(t, 3, statusDelay, remoteCount, nil, "")

	serialResult, err := serialClient.SyncCheck(context.Background(), "")
	if err != nil {
		t.Fatalf("serial SyncCheck() error = %v", err)
	}
	parallelResult, err := parallelClient.SyncCheck(context.Background(), "")
	if err != nil {
		t.Fatalf("parallel SyncCheck() error = %v", err)
	}
	if !reflect.DeepEqual(parallelResult, serialResult) {
		t.Fatalf("parallel result differs from serial reference:\nparallel=%+v\nserial=%+v", parallelResult, serialResult)
	}
	if active, maxActive, calls, _ := parallelOp.stats(); active != 0 {
		t.Fatalf("active STATUS calls after SyncCheck() = %d, want 0", active)
	} else if maxActive != 3 {
		t.Fatalf("maximum concurrent STATUS calls = %d, want configured limit 3", maxActive)
	} else if calls < 3 {
		t.Fatalf("STATUS calls = %d, want enough work to exercise the pool limit", calls)
	}
	if _, maxActive, _, _ := serialOp.stats(); maxActive != 1 {
		t.Fatalf("serial reference maximum concurrent STATUS calls = %d, want 1", maxActive)
	}
	keys := make([]string, len(parallelResult.Mailboxes))
	for index, mailbox := range parallelResult.Mailboxes {
		keys[index] = mailbox.AccountRef + "\x00" + mailbox.ServerName + "\x00" + string(mailbox.State)
	}
	if !sort.StringsAreSorted(keys) {
		t.Fatalf("SyncCheck() mailbox results are not deterministic: %v", keys)
	}
}

func TestSyncCheckParallelStatusRetainsFailureEvidence(t *testing.T) {
	t.Parallel()
	const failedMailbox = "Remote-03"
	client, op := newDelayedSyncCheckClient(t, 3, 5*time.Millisecond, 8, nil, failedMailbox)
	result, err := client.SyncCheck(context.Background(), "")
	if err != nil {
		t.Fatalf("SyncCheck() error = %v", err)
	}
	if result.Complete {
		t.Fatal("Complete = true despite a failed parallel STATUS")
	}
	found := false
	for _, failure := range result.Failures {
		if failure.Mailbox == failedMailbox && failure.Code == transport.CodeIMAPTimeout {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Failures = %+v, want typed failure for %s", result.Failures, failedMailbox)
	}
	if active, maxActive, _, _ := op.stats(); active != 0 || maxActive > 3 {
		t.Fatalf("parallel STATUS lifecycle = active %d, max %d; want joined and capped at 3", active, maxActive)
	}
}

func TestSyncCheckCancellationJoinsStatusWorkers(t *testing.T) {
	t.Parallel()
	const limit = 3
	started := make(chan struct{}, limit)
	client, op := newDelayedSyncCheckClient(t, limit, time.Hour, 8, started, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type syncResult struct {
		result mail.SyncCheckResult
		err    error
	}
	done := make(chan syncResult, 1)
	go func() {
		result, err := client.SyncCheck(ctx, "")
		done <- syncResult{result: result, err: err}
	}()
	for range limit {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("STATUS workers did not all start")
		}
	}
	cancel()
	select {
	case outcome := <-done:
		if outcome.err != nil {
			t.Fatalf("canceled SyncCheck() error = %v", outcome.err)
		}
		if outcome.result.Complete || len(outcome.result.Failures) == 0 {
			t.Fatalf("canceled SyncCheck() = %+v, want incomplete result with failures", outcome.result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled SyncCheck() did not join STATUS workers")
	}
	if active, maxActive, calls, _ := op.stats(); active != 0 {
		t.Fatalf("active STATUS calls after cancellation = %d, want 0", active)
	} else if maxActive != limit {
		t.Fatalf("maximum active STATUS calls = %d, want %d", maxActive, limit)
	} else if calls != limit {
		t.Fatalf("STATUS calls after cancellation = %d, want only active workers", calls)
	}
}

func BenchmarkSyncStatusWorkerLimits(b *testing.B) {
	jobs := make([]syncStatusJob, 16)
	for index := range jobs {
		jobs[index] = syncStatusJob{mailbox: fmt.Sprintf("Remote-%02d", index)}
	}
	for _, limit := range []int{1, 2, 4} {
		b.Run(fmt.Sprintf("limit_%d", limit), func(b *testing.B) {
			op := &delayedSyncImapOperator{
				stubImapOperator: &stubImapOperator{},
				limit:            limit,
				delay:            time.Millisecond,
			}
			cfg := transport.ImapConfig{Host: "benchmark.invalid", Port: 993, Username: "user"}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				runSyncStatusJobs(context.Background(), op, cfg, jobs)
			}
		})
	}
}
