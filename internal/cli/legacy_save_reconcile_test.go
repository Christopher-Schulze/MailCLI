package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"mailcli/internal/mail"
)

type legacySaveObservationGateway struct {
	testGateway
	observed     bool
	observations int
	saves        int
}

func (g *legacySaveObservationGateway) SaveDraft(context.Context, mail.Draft) (mail.MessageSummary, error) {
	g.saves++
	return mail.MessageSummary{Ref: "unexpected_new_save"}, nil
}

func (g *legacySaveObservationGateway) ReconcileDraftSave(context.Context, mail.Draft, mail.DraftSaveAttempt) (mail.DraftSaveEvidence, error) {
	g.observations++
	if g.observed {
		return mail.DraftSaveEvidence{ObservedMessage: mail.MessageSummary{Ref: "msg_observed", Subject: "Saved once"}}, nil
	}
	return mail.DraftSaveEvidence{}, nil
}

func TestDraftReconcileHistoricalSaveClaims(t *testing.T) {
	for _, test := range []struct {
		name             string
		observed         bool
		invalid          bool
		handoffConflict  bool
		wantCode         string
		wantObservations int
	}{
		{name: "observed", observed: true, wantObservations: 1},
		{name: "unknown", wantCode: "draft_save_outcome_unknown", wantObservations: 1},
		{name: "invalid", invalid: true, wantCode: "draft_state_error"},
		{name: "conflicting handoff", handoffConflict: true, wantCode: "draft_state_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "drafts")
			gateway := &legacySaveObservationGateway{observed: test.observed}
			service := mail.NewServiceWithDraftRoot(gateway, root)
			draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{To: []mail.Recipient{{Address: "recipient@example.com"}}, Body: "reviewed retained body"}})
			if err != nil {
				t.Fatal(err)
			}
			if test.handoffConflict {
				session, err := service.BeginDraftHandoff(draft.Ref)
				if err != nil {
					t.Fatal(err)
				}
				if err := session.MarkDispatched(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := session.Finish(mail.HandoffOutcomeUnknown); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC()
			claim := struct {
				Version  int                   `json:"version"`
				DraftRef string                `json:"draft_ref"`
				Attempt  mail.DraftSaveAttempt `json:"attempt"`
			}{Version: 1, DraftRef: draft.Ref, Attempt: mail.DraftSaveAttempt{ID: "save_legacy", StartedAt: now, UpdatedAt: now, ObservationBaseline: &mail.SendObservationBaseline{StoreUUID: "store", CapturedUnix: 1, SentMailboxIDs: []int64{1}}}}
			if test.invalid {
				claim.Attempt.ObservationBaseline = nil
			}
			payload, err := json.Marshal(claim)
			if err != nil {
				t.Fatal(err)
			}
			claimPath := filepath.Join(root, draft.Ref+".save-claim")
			if err := os.WriteFile(claimPath, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			paths := []string{filepath.Join(root, draft.Ref+".json"), claimPath}
			if test.handoffConflict {
				paths = append(paths, filepath.Join(root, draft.Ref+".handoff-claim"))
			}
			before := make([][]byte, len(paths))
			for i, path := range paths {
				before[i], err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), service, []string{"drafts", "reconcile", "--ref", draft.Ref, "--json"}, &stdout, &stderr)
			var response envelope
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatalf("decode %q: %v", stdout.String(), err)
			}
			if stderr.Len() != 0 || gateway.saves != 0 || gateway.observations != test.wantObservations || response.Command != "drafts.reconcile" {
				t.Fatalf("code=%d response=%+v stderr=%q saves=%d observations=%d", code, response, stderr.String(), gateway.saves, gateway.observations)
			}
			if test.observed {
				if code != 0 || !response.OK || response.Data.SavedDraft == nil || response.Data.SavedDraft.LocalDraftRef != draft.Ref || response.Data.SavedDraft.Message.Ref != "msg_observed" || response.Data.SendResult != nil {
					t.Fatalf("response=%+v", response)
				}
				for _, path := range paths {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("observed cleanup %s: %v", path, err)
					}
				}
				return
			}
			if code != 1 || response.OK || response.Error == nil || response.Error.Code != test.wantCode {
				t.Fatalf("code=%d response=%+v", code, response)
			}
			for i, path := range paths {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before[i], after) {
					t.Fatalf("retained state changed %s: %v", path, err)
				}
			}
			if test.wantCode == "draft_save_outcome_unknown" {
				guidance := response.Error.Guidance
				if guidance == nil || guidance.ReplayAllowed || guidance.Recovery.Command != "drafts.reconcile" || !slices.Equal(guidance.Recovery.Args, []string{"--ref", draft.Ref, "--json"}) {
					t.Fatalf("guidance=%+v", guidance)
				}
			}
		})
	}
}

func TestRemovedDraftSaveIsNormalUnknownCommand(t *testing.T) {
	for _, args := range [][]string{{"drafts", "save"}, {"drafts", "save", "--help"}, {"drafts", "save", "--ref", "draft_missing", "--json"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), nil, args, &stdout, &stderr); code != 2 {
			t.Fatalf("args=%q code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
		if slices.Contains(args, "--json") {
			var response envelope
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Error == nil || response.Error.Code != "unknown_command" || response.Command != "drafts" || slices.Contains(response.Error.ValidSubcommands, "save") {
				t.Fatalf("response=%+v", response)
			}
		} else if !bytes.Contains(stderr.Bytes(), []byte(`unknown drafts command "save"`)) {
			t.Fatalf("stderr=%q", stderr.String())
		}
	}
}

func TestDraftReconcileConsumedSendReceiptWithoutTransport(t *testing.T) {
	submitter := &cliSubmitter{}
	mirror := &cliMirror{}
	service := mail.NewServiceWithTransport(nil, filepath.Join(t.TempDir(), "drafts"), mail.SendTransport{
		Submitter: submitter, Mirror: mirror, Credentials: cliCredentials{},
	})
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		From: "sender@icloud.com", To: []mail.Recipient{{Address: "recipient@example.com"}}, Body: "reviewed body",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SendDraft(context.Background(), mail.SendDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), service, []string{"drafts", "reconcile", "--ref", draft.Ref, "--json"}, &stdout, &stderr)
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode %q: %v", stdout.String(), err)
	}
	result := response.Data.SendResult
	if code != 0 || stderr.Len() != 0 || !response.OK || response.Data.SavedDraft != nil ||
		result == nil || result.DraftRef != draft.Ref || !result.Reconciled || !result.Replayed ||
		result.Outcome != mail.SendOutcomeSent || result.DraftRetained || submitter.calls != 1 || mirror.calls != 1 {
		t.Fatalf("code=%d response=%+v stderr=%q transport=%d/%d", code, response, stderr.String(), submitter.calls, mirror.calls)
	}
}
