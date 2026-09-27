package mail

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type reconcileOnlySaveGateway struct {
	gatewayStub
	observed     bool
	saves        int
	observations int
}

func (g *reconcileOnlySaveGateway) SaveDraft(context.Context, Draft) (MessageSummary, error) {
	g.saves++
	return MessageSummary{Ref: "unexpected_new_save"}, nil
}

func (g *reconcileOnlySaveGateway) ReconcileDraftSave(context.Context, Draft, DraftSaveAttempt) (DraftSaveEvidence, error) {
	g.observations++
	if g.observed {
		return DraftSaveEvidence{ObservedMessage: MessageSummary{Ref: "msg_observed"}}, nil
	}
	return DraftSaveEvidence{}, nil
}

func TestReconcileSavedDraftNeverStartsNewSave(t *testing.T) {
	for _, test := range []struct {
		name             string
		claim            bool
		observed         bool
		conflict         bool
		invalid          bool
		wantCode         string
		wantObservations int
	}{
		{name: "observed", claim: true, observed: true, wantObservations: 1},
		{name: "unknown", claim: true, wantCode: "draft_save_outcome_unknown", wantObservations: 1},
		{name: "missing claim", wantCode: "draft_save_reconcile_unavailable"},
		{name: "conflicting send claim", claim: true, conflict: true, wantCode: "draft_state_error"},
		{name: "invalid claim", claim: true, invalid: true, wantCode: "draft_state_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "drafts")
			gateway := &reconcileOnlySaveGateway{observed: test.observed}
			service := NewServiceWithDraftRoot(gateway, root)
			draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{To: []Recipient{{Address: "recipient@example.com"}}, Body: "retained reviewed body"}})
			if err != nil {
				t.Fatal(err)
			}
			if test.claim {
				writeLegacyDraftSaveAttempt(t, root, draft.Ref, &SendObservationBaseline{StoreUUID: "store", CapturedUnix: 1, SentMailboxIDs: []int64{1}})
			}
			if test.conflict {
				now := time.Now().UTC()
				if err := replaceSendAttempt(root, draft.Ref, SendAttempt{ID: "send_conflict", StartedAt: now, UpdatedAt: now, Outcome: SendOutcomeUnknown}); err != nil {
					t.Fatal(err)
				}
			}
			if test.invalid {
				if err := os.WriteFile(filepath.Join(root, draft.Ref+".save-claim"), []byte(`{"version":1,"attempt":{}}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			paths := []string{filepath.Join(root, draft.Ref+".json")}
			if test.claim {
				paths = append(paths, filepath.Join(root, draft.Ref+".save-claim"))
			}
			if test.conflict {
				paths = append(paths, filepath.Join(root, draft.Ref+".send-claim"))
			}
			before := make([][]byte, len(paths))
			for i, path := range paths {
				before[i], err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			result, err := service.ReconcileSavedDraft(context.Background(), draft.Ref)
			if errorCode(err) != test.wantCode || gateway.saves != 0 || gateway.observations != test.wantObservations {
				t.Fatalf("result=%+v err=%v saves=%d observations=%d", result, err, gateway.saves, gateway.observations)
			}
			if test.observed {
				if result.LocalDraftRef != draft.Ref || result.Message.Ref != "msg_observed" {
					t.Fatalf("result=%+v", result)
				}
				for _, path := range paths {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("observed cleanup %s: %v", path, err)
					}
				}
				return
			}
			for i, path := range paths {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, before[i]) {
					t.Fatalf("retained state changed: %s err=%v", path, err)
				}
			}
			if test.wantCode == "draft_save_outcome_unknown" {
				guidance := GuidanceForError("drafts.reconcile", err)
				if guidance.ReplayAllowed || guidance.Recovery.Command != "drafts.reconcile" || len(guidance.Recovery.Args) != 3 || guidance.Recovery.Args[1] != draft.Ref {
					t.Fatalf("guidance=%+v", guidance)
				}
			}
		})
	}
}
