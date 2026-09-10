package compose

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHandoffCancelledContextReturnsBeforeNativeCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Handoff(ctx, Request{Recipients: []string{"a@example.com"}}); err != context.Canceled {
		t.Errorf("Handoff() error = %v, want %v", err, context.Canceled)
	}
}

func TestHandoffWithDispatchSuppressesLateSuccessAfterCancellation(t *testing.T) {
	original := invokeNativeCompose
	t.Cleanup(func() { invokeNativeCompose = original })
	started := make(chan struct{})
	invokeNativeCompose = func(_ string, cancelRequested <-chan struct{}) (string, error) {
		close(started)
		<-cancelRequested
		return `{"ok":true,"opened":true,"mail_application":"com.apple.mail","dispatched":true}`, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observerCalls := 0
	done := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		result, err := HandoffWithDispatch(ctx, Request{Recipients: []string{"a@example.com"}}, func() error {
			observerCalls++
			return nil
		})
		done <- struct {
			result Result
			err    error
		}{result, err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("native invoker did not start")
	}
	cancel()
	select {
	case outcome := <-done:
		var composeErr *Error
		if !errors.As(outcome.err, &composeErr) || !composeErr.OutcomeUnknown() || !composeErr.DispatchedToNative() {
			t.Fatalf("HandoffWithDispatch() error = %v, want dispatched uncertainty", outcome.err)
		}
		if outcome.result.Opened || outcome.result.State != StateOutcomeUnknown {
			t.Fatalf("HandoffWithDispatch() result = %+v, want unknown without success", outcome.result)
		}
		if observerCalls != 1 {
			t.Fatalf("dispatch observer calls = %d, want 1", observerCalls)
		}
	case <-time.After(time.Second):
		t.Fatal("HandoffWithDispatch() did not return after cancellation")
	}
}

func TestHandoffWithDispatchDoesNotInvokeCanceledRequest(t *testing.T) {
	original := invokeNativeCompose
	t.Cleanup(func() { invokeNativeCompose = original })
	invoked := false
	observerCalls := 0
	invokeNativeCompose = func(string, <-chan struct{}) (string, error) {
		invoked = true
		return `{"ok":true}`, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := HandoffWithDispatch(ctx, Request{}, func() error {
		observerCalls++
		return nil
	})
	if err != context.Canceled || invoked || observerCalls != 0 {
		t.Fatalf("HandoffWithDispatch() = %v, invoked=%t observer_calls=%d", err, invoked, observerCalls)
	}
}

func TestHandoffWithDispatchStopsWhenDispatchObserverFails(t *testing.T) {
	original := invokeNativeCompose
	t.Cleanup(func() { invokeNativeCompose = original })
	invoked := false
	invokeNativeCompose = func(string, <-chan struct{}) (string, error) {
		invoked = true
		return `{"ok":true}`, nil
	}
	want := errors.New("persist dispatch marker")
	_, err := HandoffWithDispatch(context.Background(), Request{}, func() error { return want })
	if !errors.Is(err, want) || invoked {
		t.Fatalf("HandoffWithDispatch() error = %v, invoked=%t, want observer error before native call", err, invoked)
	}
}

func TestHandoffWithDispatchCancelsAfterObserverBeforeNativeCall(t *testing.T) {
	original := invokeNativeCompose
	t.Cleanup(func() { invokeNativeCompose = original })
	invoked := false
	invokeNativeCompose = func(string, <-chan struct{}) (string, error) {
		invoked = true
		return `{"ok":true}`, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := HandoffWithDispatch(ctx, Request{}, func() error {
		cancel()
		return nil
	})
	var composeErr *Error
	if !errors.As(err, &composeErr) || composeErr.State != StateCanceledBeforeDispatch || composeErr.DispatchedToNative() ||
		!errors.Is(err, context.Canceled) || invoked {
		t.Fatalf("HandoffWithDispatch() = %v, invoked=%t, compose error=%+v", err, invoked, composeErr)
	}
}

func TestHandoffWithDispatchPreservesNativePreDispatchFailure(t *testing.T) {
	original := invokeNativeCompose
	t.Cleanup(func() { invokeNativeCompose = original })
	invokeNativeCompose = func(string, <-chan struct{}) (string, error) {
		return `{"ok":false,"code":"handoff_canceled_before_dispatch","message":"not dispatched","dispatched":false}`, nil
	}
	_, err := HandoffWithDispatch(context.Background(), Request{}, nil)
	var composeErr *Error
	if !errors.As(err, &composeErr) || composeErr.State != StateCanceledBeforeDispatch || composeErr.DispatchedToNative() {
		t.Fatalf("HandoffWithDispatch() = %v, compose error=%+v", err, composeErr)
	}
}

func TestHandoffWithDispatchKeepsConfirmedResultWithCleanupError(t *testing.T) {
	original := invokeNativeCompose
	t.Cleanup(func() { invokeNativeCompose = original })
	want := errors.New("remove cancellation directory")
	invokeNativeCompose = func(string, <-chan struct{}) (string, error) {
		return `{"ok":true,"opened":true,"mail_application":"com.apple.mail","dispatched":true}`, want
	}
	result, err := HandoffWithDispatch(context.Background(), Request{}, nil)
	if !errors.Is(err, want) || result.State != StateConfirmedCompletion || !result.Opened {
		t.Fatalf("HandoffWithDispatch() = result=%+v err=%v, want confirmed result with cleanup error", result, err)
	}
}

func TestHandoffWithDispatchTreatsMalformedResultAsUnknownAfterDispatch(t *testing.T) {
	original := invokeNativeCompose
	t.Cleanup(func() { invokeNativeCompose = original })
	invokeNativeCompose = func(string, <-chan struct{}) (string, error) {
		return "not-json", nil
	}
	result, err := HandoffWithDispatch(context.Background(), Request{}, func() error { return nil })
	var composeErr *Error
	if !errors.As(err, &composeErr) || !composeErr.OutcomeUnknown() || !composeErr.DispatchedToNative() || result.State != StateOutcomeUnknown {
		t.Fatalf("HandoffWithDispatch() = result=%+v err=%v compose error=%+v, want retained uncertainty", result, err, composeErr)
	}
}
