package mailstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNextObservationDelayTable(t *testing.T) {
	tests := []struct {
		name    string
		current time.Duration
		maximum time.Duration
		want    time.Duration
	}{
		{name: "double", current: 100 * time.Millisecond, maximum: 800 * time.Millisecond, want: 200 * time.Millisecond},
		{name: "cap instead of overshoot", current: 600 * time.Millisecond, maximum: 800 * time.Millisecond, want: 800 * time.Millisecond},
		{name: "stay capped", current: 800 * time.Millisecond, maximum: 800 * time.Millisecond, want: 800 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := nextObservationDelay(test.current, test.maximum); got != test.want {
				t.Fatalf("nextObservationDelay(%s, %s) = %s, want %s", test.current, test.maximum, got, test.want)
			}
		})
	}
}

func TestObserveWithBackoffChecksImmediately(t *testing.T) {
	calls := 0
	value, observed, err := observeWithBackoff(
		context.Background(), time.Hour, time.Hour,
		func() (string, bool, error) {
			calls++
			return "observed", true, nil
		},
	)
	if err != nil || !observed || value != "observed" || calls != 1 {
		t.Fatalf("observeWithBackoff() = %q, %t, %v after %d calls", value, observed, err, calls)
	}
}

func TestObserveWithBackoffPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, observed, err := observeWithBackoff(
		ctx, time.Millisecond, time.Millisecond,
		func() (struct{}, bool, error) { return struct{}{}, false, nil },
	)
	if observed || !errors.Is(err, context.Canceled) {
		t.Fatalf("observeWithBackoff() observed = %t, error = %v", observed, err)
	}
}

func TestObserveWithBackoffTreatsDeadlineAsUnobserved(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, observed, err := observeWithBackoff(
		ctx, time.Hour, time.Hour,
		func() (struct{}, bool, error) { return struct{}{}, false, nil },
	)
	if observed || err != nil {
		t.Fatalf("observeWithBackoff() observed = %t, error = %v", observed, err)
	}
}
