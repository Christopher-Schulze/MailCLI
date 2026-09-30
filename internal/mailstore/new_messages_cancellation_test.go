package mailstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"mailcli/internal/cli"
	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

type canceledRecentOperator struct {
	*recentImapOperator
	cancel context.CancelFunc
	err    error
}

func (o *canceledRecentOperator) ListRecentMessages(ctx context.Context, cfg transport.ImapConfig, mailbox string, count int) (transport.RecentMailbox, error) {
	o.calls = append(o.calls, mailbox)
	if o.cancel != nil {
		o.cancel()
	}
	return transport.RecentMailbox{}, o.err
}

func TestNewMessagesCancellationStopsDiscovery(t *testing.T) {
	for _, test := range []struct {
		name      string
		before    bool
		cancel    bool
		err       error
		wantCalls int
	}{
		{name: "before discovery", before: true},
		{name: "during discovery", cancel: true, err: errors.New("stale server error"), wantCalls: 1},
		{name: "wrapped cancellation", err: fmt.Errorf("recent read: %w", context.Canceled), wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, recent := newMessagesFixture(t, "new-cancel@gmail.com", nil)
			other, err := parseAccountRoot("imap://FFFFFFFF-BBBB-4CCC-8DDD-EEEEEEEEEEEE/")
			if err != nil {
				t.Fatal(err)
			}
			client.store.activeAccounts = append(client.store.activeAccounts, other)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			operator := &canceledRecentOperator{recentImapOperator: recent, err: test.err}
			if test.cancel {
				operator.cancel = cancel
			}
			if test.before {
				cancel()
			}
			client.send.Imap = operator
			result, err := client.NewMessages(ctx, mail.NewMessagesRequest{Limit: 5})
			if !errors.Is(err, context.Canceled) || errorCodeForTest(err) != "operation_canceled" || result.Complete || len(result.Failures) != 0 || len(operator.calls) != test.wantCalls {
				t.Fatalf("cancellation result=%+v error=%v calls=%v", result, err, operator.calls)
			}
		})
	}
}

func TestNewMessagesDeadlineRemainsPartial(t *testing.T) {
	client, operator := newMessagesFixture(t, "new-deadline@gmail.com", nil)
	operator.err = fmt.Errorf("recent FETCH: %w", context.DeadlineExceeded)
	result, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{Limit: 5})
	if err != nil || result.Complete || len(result.Failures) != 1 || result.Failures[0].Code != "operation_timeout" || len(operator.calls) != 1 {
		t.Fatalf("deadline result=%+v error=%v calls=%v", result, err, operator.calls)
	}
}

func TestNewMessagesCancellationCLIContract(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%t", jsonOutput), func(t *testing.T) {
			client, recent := newMessagesFixture(t, "new-cancel-cli@gmail.com", nil)
			client.send.Imap = &canceledRecentOperator{recentImapOperator: recent, err: fmt.Errorf("FETCH: %w", context.Canceled)}
			args := []string{"messages", "new"}
			if jsonOutput {
				args = []string{"messages", "new", "--json"}
			}
			var stdout, stderr bytes.Buffer
			code := cli.Run(context.Background(), mail.NewService(client), args, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if !jsonOutput {
				if stdout.Len() != 0 || stderr.Len() == 0 {
					t.Fatalf("human cancellation stdout=%s stderr=%s", stdout.String(), stderr.String())
				}
				return
			}
			var response struct {
				OK    bool                  `json:"ok"`
				Error struct{ Code string } `json:"error"`
				Next  struct{ Do string }   `json:"next"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.OK || response.Error.Code != "operation_canceled" || response.Next.Do != "stop" || stderr.Len() != 0 {
				t.Fatalf("cancellation envelope=%+v stderr=%s", response, stderr.String())
			}
		})
	}
}
