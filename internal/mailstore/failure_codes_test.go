package mailstore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestReadCoverageFailureDistinguishesCancellation(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, finish := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer finish()
	for _, classifier := range []struct {
		name    string
		code    func(context.Context, error) string
		timeout string
		failed  string
	}{
		{"new", newMessagesFailureCode, "operation_timeout", "operation_failed"},
		{"sync", failureCode, "sync_check_timeout", "sync_check_failed"},
	} {
		for _, test := range []struct {
			name string
			ctx  context.Context
			err  error
			code string
		}{
			{"canceled caller", canceled, errors.New("closed"), "operation_canceled"},
			{"canceled wins over stale timeout", canceled, context.DeadlineExceeded, "operation_canceled"},
			{"expired caller", expired, errors.New("closed"), classifier.timeout},
			{"deadline wins over inner cancellation", expired, context.Canceled, classifier.timeout},
			{"wrapped cancellation", context.Background(), fmt.Errorf("inner: %w", context.Canceled), "operation_canceled"},
			{"wrapped deadline", context.Background(), fmt.Errorf("inner: %w", context.DeadlineExceeded), classifier.timeout},
			{"typed unrelated failure", context.Background(), &transport.TransportError{Code: transport.CodeIMAPAuthFailed}, transport.CodeIMAPAuthFailed},
			{"uncoded failure", context.Background(), errors.New("failed"), classifier.failed},
		} {
			t.Run(classifier.name+"/"+test.name, func(t *testing.T) {
				if code := classifier.code(test.ctx, test.err); code != test.code {
					t.Fatalf("code=%s, want %s", code, test.code)
				}
			})
		}
	}
}
