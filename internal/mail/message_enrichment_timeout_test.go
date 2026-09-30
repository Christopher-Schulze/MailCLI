package mail

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type partialEnrichmentGateway struct {
	gatewayStub
	metadata []MessageSummary
	err      error
}

func (g *partialEnrichmentGateway) EnrichMessages(context.Context, []string, MessageEnrichmentRequest) ([]MessageSummary, error) {
	return g.metadata, g.err
}

func TestEnrichmentDeadlinePartialValidation(t *testing.T) {
	valid := []MessageSummary{
		{Ref: "first", InReplyTo: []string{"<sent@example.com>"}, ThreadingComplete: true, Excerpt: "completed", ExcerptComplete: true, ExcerptSource: ExcerptSourceLocal},
		{Ref: "second", ThreadingComplete: true, ExcerptSource: ExcerptSourceUnavailable, EnrichmentError: "operation_timeout"},
	}
	for _, test := range []struct {
		name     string
		metadata []MessageSummary
		err      error
		live     bool
		apply    bool
	}{
		{name: "valid partial", metadata: valid, err: context.DeadlineExceeded, apply: true},
		{name: "short partial", metadata: valid[:1], err: context.DeadlineExceeded},
		{name: "wrong mapping", metadata: []MessageSummary{valid[1], valid[0]}, err: context.DeadlineExceeded},
		{name: "unnamed partial", metadata: []MessageSummary{{}, {}}, err: context.DeadlineExceeded},
		{name: "deadline with live context", metadata: valid, err: context.DeadlineExceeded, live: true},
		{name: "cancellation", metadata: valid, err: context.Canceled},
		{name: "unrelated error", metadata: valid, err: errors.New("read failed")},
		{name: "combined failure", metadata: valid, err: errors.Join(context.DeadlineExceeded, errors.New("close failed"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			if !test.live {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			}
			rows := []MessageSummary{{Ref: "first", Subject: "keep first"}, {Ref: "second", Subject: "keep second"}}
			before := append([]MessageSummary(nil), rows...)
			service := NewService(&partialEnrichmentGateway{metadata: test.metadata, err: test.err})
			err := service.EnrichMessages(ctx, []*MessageSummary{&rows[0], &rows[1]}, MessageEnrichmentRequest{Threading: true, Excerpt: true, ExcerptLength: 240})
			if err == nil {
				t.Fatal("deadline/error must remain available to caller policy")
			}
			if !test.apply {
				if !reflect.DeepEqual(rows, before) {
					t.Fatalf("invalid partial applied: %+v", rows)
				}
				return
			}
			if err != context.DeadlineExceeded || rows[0].Ref != "first" || rows[0].Subject != "keep first" ||
				!rows[0].ThreadingComplete || rows[0].Excerpt != "completed" || !rows[0].ExcerptComplete ||
				!rows[1].ThreadingComplete || rows[1].ExcerptComplete || rows[1].EnrichmentError != "operation_timeout" ||
				!rows[1].ThreadingRequested || !rows[1].ExcerptRequested {
				t.Fatalf("deadline partial lost: rows=%+v error=%v", rows, err)
			}
		})
	}
}
