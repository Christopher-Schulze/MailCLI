package cli

import (
	"context"
	"errors"
	"os"
	"testing"
	"testing/synctest"
	"time"
)

func TestUpdateLockVirtualTimePolicy(t *testing.T) {
	for _, test := range []struct {
		name       string
		deadline   time.Duration
		cancelAt   time.Duration
		releaseAt  time.Duration
		completeAt time.Duration
		wantError  error
	}{
		{name: "deadline", deadline: 225 * time.Millisecond, completeAt: 225 * time.Millisecond, wantError: context.DeadlineExceeded},
		{name: "cancellation", cancelAt: 125 * time.Millisecond, completeAt: 125 * time.Millisecond, wantError: context.Canceled},
		{name: "release waits for retry tick", releaseAt: 25 * time.Millisecond, completeAt: 50 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			synctest.Test(t, func(t *testing.T) {
				owner, err := acquireUpdateLock(context.Background(), home)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if owner != nil {
						if err := owner.Close(); err != nil {
							t.Error(err)
						}
					}
				}()
				ctx, cancel := context.WithCancel(context.Background())
				if test.deadline != 0 {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), test.deadline)
				}
				defer cancel()
				started := time.Now()
				var acquired *os.File
				var acquireErr error
				var elapsed time.Duration
				done := make(chan struct{})
				go func() {
					acquired, acquireErr = acquireUpdateLock(ctx, home)
					elapsed = time.Since(started)
					close(done)
				}()
				synctest.Wait()
				if test.cancelAt != 0 {
					go func() { time.Sleep(test.cancelAt); cancel() }()
				}
				if test.releaseAt != 0 {
					time.Sleep(test.releaseAt)
					if err := owner.Close(); err != nil {
						t.Fatal(err)
					}
					owner = nil
				}
				time.Sleep(test.completeAt - time.Since(started) - time.Nanosecond)
				synctest.Wait()
				select {
				case <-done:
					t.Fatalf("lock acquisition completed early: elapsed=%s, error=%v", elapsed, acquireErr)
				default:
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				select {
				case <-done:
				default:
					t.Fatal("lock acquisition missed its exact virtual completion time")
				}
				if elapsed != test.completeAt || !errors.Is(acquireErr, test.wantError) {
					t.Fatalf("acquisition elapsed=%s, error=%v; want %s, %v", elapsed, acquireErr, test.completeAt, test.wantError)
				}
				if test.wantError != nil {
					if acquired != nil || updateErrorCodeForTest(acquireErr) != "update_busy" {
						t.Fatalf("failed acquisition lock=%v, error=%v", acquired, acquireErr)
					}
					return
				}
				if acquired == nil {
					t.Fatal("released owner did not permit acquisition")
				}
				if err := validateUpdateLock(acquired); err != nil {
					t.Error(err)
				}
				if err := acquired.Close(); err != nil {
					t.Error(err)
				}
			})
		})
	}
}
