package compose

import (
	"context"
	"testing"
)

func TestHandoffCancelledContextReturnsBeforeNativeCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Handoff(ctx, Request{Recipients: []string{"a@example.com"}}); err != context.Canceled {
		t.Errorf("Handoff() error = %v, want %v", err, context.Canceled)
	}
}
