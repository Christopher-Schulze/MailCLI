package imapclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestIMAPCancellationWrappersPreserveCauses(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, finish := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer finish()
	closed := errors.New("socket closed")
	for _, wrapper := range []struct {
		name string
		wrap func(context.Context, error) error
	}{
		{"dial", wrapDialError},
		{"io", func(ctx context.Context, err error) error {
			return wrapIOError(ctx, err, transport.CodeIMAPFetchFailed, "read")
		}},
		{"command", func(ctx context.Context, err error) error {
			return wrapCommandIOError(ctx, err, "command")
		}},
	} {
		for _, test := range []struct {
			name string
			ctx  context.Context
			err  error
			code string
		}{
			{"caller canceled", canceled, closed, transport.CodeIMAPCanceled},
			{"caller deadline", expired, closed, transport.CodeIMAPTimeout},
			{"canceled wins over stale timeout", canceled, context.DeadlineExceeded, transport.CodeIMAPCanceled},
			{"deadline wins over inner cancellation", expired, context.Canceled, transport.CodeIMAPTimeout},
			{"wrapped cancellation", context.Background(), fmt.Errorf("inner: %w", context.Canceled), transport.CodeIMAPCanceled},
			{"wrapped deadline", context.Background(), fmt.Errorf("inner: %w", context.DeadlineExceeded), transport.CodeIMAPTimeout},
			{"own deadline", expired, context.DeadlineExceeded, transport.CodeIMAPTimeout},
			{"own cancellation", canceled, context.Canceled, transport.CodeIMAPCanceled},
			{"wrapped own deadline", expired, fmt.Errorf("acquire: %w", context.DeadlineExceeded), transport.CodeIMAPTimeout},
		} {
			t.Run(wrapper.name+"/"+test.name, func(t *testing.T) {
				err := wrapper.wrap(test.ctx, test.err)
				if transport.ErrorCode(err) != test.code || !errors.Is(err, test.err) {
					t.Fatalf("code=%q error=%v; want %s and original cause", transport.ErrorCode(err), err, test.code)
				}
				if cause := test.ctx.Err(); cause != nil && !errors.Is(err, cause) {
					t.Fatalf("caller cause %v lost: %v", cause, err)
				}
				if cause := test.ctx.Err(); cause != nil && strings.Count(err.Error(), cause.Error()) != 1 {
					t.Fatalf("caller cause %v repeated: %q", cause, err.Error())
				}
			})
		}
	}
}

func TestCommandCancellationKeepsUncertainOutcome(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, code := range []string{transport.CodeIMAPAppendOutcomeUnknown, transport.CodeIMAPCopyOutcomeUnknown, transport.CodeIMAPMoveOutcomeUnknown} {
		t.Run(code, func(t *testing.T) {
			cause := &transport.MutationOutcomeError{Code: code, Evidence: transport.MutationEvidence{Outcome: transport.MutationOutcomeUnknown}}
			err := wrapCommandIOError(ctx, cause, "command")
			var outcome *transport.MutationOutcomeError
			if transport.ErrorCode(err) != code || !transport.IsMutationOutcomeUnknown(err) && !transport.IsAppendOutcomeUnknown(err) ||
				!errors.As(err, &outcome) || outcome != cause || !errors.Is(err, context.Canceled) {
				t.Fatalf("unknown outcome or caller cause lost: %v", err)
			}
		})
	}
}
