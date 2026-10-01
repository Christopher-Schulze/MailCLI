package mail

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"testing"

	"mailcli/internal/transport"
)

func TestBatchMutationAliasesMatchCommandGuidance(t *testing.T) {
	for _, operation := range []string{BatchOperationMark, BatchOperationMove, BatchOperationCopy, BatchOperationDelete} {
		for _, cause := range []error{context.Canceled, context.DeadlineExceeded, fs.ErrPermission, errors.New("unclassified failure")} {
			t.Run(operation+"/"+cause.Error(), func(t *testing.T) {
				got := GuidanceForError(operation, cause)
				want := GuidanceForError("messages."+operation, cause)
				if !reflect.DeepEqual(got, want) || got.EffectCertainty != EffectUnknown || got.ReplayAllowed {
					t.Fatalf("batch alias guidance differs: got=%+v, want=%+v", got, want)
				}
			})
		}
	}
}

func TestBatchMutationCancellationUsesOutcomeEvidence(t *testing.T) {
	for _, operation := range []string{BatchOperationMark, BatchOperationMove, BatchOperationCopy, BatchOperationDelete} {
		for _, test := range []struct {
			name, outcome string
			effects       []string
			state         BatchItemState
			certainty     EffectCertainty
		}{
			{"no evidence", "", nil, BatchItemUncertain, EffectUnknown},
			{"not started", transport.MutationOutcomeNotStarted, nil, BatchItemFailed, EffectNone},
			{"rejected", transport.MutationOutcomeRejected, nil, BatchItemFailed, EffectNone},
			{"unknown", transport.MutationOutcomeUnknown, nil, BatchItemUncertain, EffectUnknown},
			{"partial", transport.MutationOutcomePartial, []string{"COPY"}, BatchItemUncertain, EffectPartial},
			{"contradictory no-start with effects", transport.MutationOutcomeNotStarted, []string{"COPY"}, BatchItemUncertain, EffectPartial},
		} {
			for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
				t.Run(operation+"/"+test.name+"/"+cause.Error(), func(t *testing.T) {
					command := "MOVE"
					switch operation {
					case BatchOperationMark:
						command = "STORE"
					case BatchOperationCopy:
						command = "COPY"
					}
					failure := cause
					if test.outcome != "" {
						failure = &transport.MutationOutcomeError{
							Code: transport.CodeIMAPMoveOutcomeUnknown, Message: "mutation interrupted", Err: cause,
							Evidence: transport.MutationEvidence{Command: command, Outcome: test.outcome, CompletedEffects: test.effects},
						}
					}
					ref := batchTestMessageRef(t, "source")
					gateway := &batchGateway{
						gatewayStub: &gatewayStub{}, markErr: failure,
						transferErrs: map[string]error{ref: failure}, deleteErrs: map[string]error{ref: failure},
					}
					item := BatchItem{ID: "item", Ref: ref}
					switch operation {
					case BatchOperationMark:
						item.Read = boolPointer(true)
					case BatchOperationMove, BatchOperationCopy:
						item.Mailbox = batchTestMailboxRef(t, "Archive")
					}
					result, err := NewService(gateway).ExecuteBatch(context.Background(), BatchRequest{Operation: operation, Items: []BatchItem{item}})
					if err != nil || len(result.Items) != 1 {
						t.Fatalf("execute: result=%+v, error=%v", result, err)
					}
					got := result.Items[0]
					if got.State != test.state || got.Error == nil || got.Error.Guidance == nil || got.Error.Guidance.EffectCertainty != test.certainty || got.Error.Retryable {
						t.Fatalf("outcome and cancellation disagree: item=%+v, guidance=%+v", got, got.Error)
					}
					if result.Completed != 0 || result.Skipped != 0 || result.Failed+result.Uncertain != 1 ||
						(result.Uncertain == 1) != (test.state == BatchItemUncertain) {
						t.Fatalf("counts disagree with item outcome: %+v", result)
					}
					if test.certainty != EffectNone && got.Error.Guidance.ReplayAllowed {
						t.Fatalf("unproven mutation permits replay: %+v", got.Error.Guidance)
					}
				})
			}
		}
	}
}
