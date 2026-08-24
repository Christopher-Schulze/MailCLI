package mailstore

import (
	"context"
	"errors"
	"time"
)

func observeWithBackoff[T any](
	ctx context.Context,
	initialDelay time.Duration,
	maximumDelay time.Duration,
	check func() (T, bool, error),
) (T, bool, error) {
	delay := initialDelay
	if delay <= 0 {
		delay = time.Millisecond
	}
	if maximumDelay < delay {
		maximumDelay = delay
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		value, observed, err := check()
		if err != nil || observed {
			return value, observed, err
		}
		select {
		case <-ctx.Done():
			var zero T
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return zero, false, nil
			}
			return zero, false, ctx.Err()
		case <-timer.C:
		}
		delay = nextObservationDelay(delay, maximumDelay)
		timer.Reset(delay)
	}
}

func nextObservationDelay(current time.Duration, maximum time.Duration) time.Duration {
	if current >= maximum || current > maximum/2 {
		return maximum
	}
	return current * 2
}
