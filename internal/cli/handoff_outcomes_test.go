package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/compose"
	"mailcli/internal/mail"
)

func TestDraftHandoffOutcomesPreserveLifecycleEvidence(t *testing.T) {
	tests := []struct {
		name       string
		dispatch   bool
		result     compose.Result
		err        error
		outcome    draftHandoffOutcome
		wantCode   int
		wantRetain bool
	}{
		{name: "delegate acceptance", dispatch: true, result: compose.Result{State: compose.StateConfirmedCompletion, Opened: true}, outcome: draftHandoffHandedOff},
		{name: "acceptance is not window evidence", dispatch: true, result: compose.Result{State: compose.StateConfirmedCompletion}, outcome: draftHandoffHandedOff},
		{name: "acceptance with teardown error", dispatch: true, result: compose.Result{State: compose.StateConfirmedCompletion, Opened: true}, err: errors.New("native cleanup failed"), outcome: draftHandoffHandedOff, wantCode: 1},
		{name: "rejected before native dispatch", err: &compose.Error{Code: "compose_service_unavailable", State: compose.StateConfirmedFailure}, outcome: draftHandoffNotHandedOff, wantCode: 1},
		{name: "canceled before native dispatch", err: &compose.Error{Code: "handoff_canceled_before_dispatch", State: compose.StateCanceledBeforeDispatch}, outcome: draftHandoffNotHandedOff, wantCode: 1},
		{name: "delegate rejection after dispatch", dispatch: true, err: &compose.Error{Code: "handoff_failed", State: compose.StateConfirmedFailure, Dispatched: true}, outcome: draftHandoffNotHandedOff, wantCode: 1},
		{name: "typed unknown", dispatch: true, err: &compose.Error{Code: "handoff_outcome_unknown", State: compose.StateOutcomeUnknown, Dispatched: true}, outcome: draftHandoffUnknown, wantCode: 1, wantRetain: true},
		{name: "untyped failure after dispatch", dispatch: true, err: errors.New("empty native result"), outcome: draftHandoffUnknown, wantCode: 1, wantRetain: true},
		{name: "caller cancellation after dispatch", dispatch: true, err: context.Canceled, outcome: draftHandoffUnknown, wantCode: 1, wantRetain: true},
		{name: "deadline after dispatch", dispatch: true, err: context.DeadlineExceeded, outcome: draftHandoffUnknown, wantCode: 1, wantRetain: true},
		{name: "untyped failure before dispatch", err: errors.New("native preflight failed"), outcome: draftHandoffNotHandedOff, wantCode: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := mail.NewServiceWithDraftRoot(nil, t.TempDir())
			attachment := filepath.Join(t.TempDir(), "attachment.txt")
			if err := os.WriteFile(attachment, []byte("retained evidence"), 0o600); err != nil {
				t.Fatal(err)
			}
			draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
				To: []mail.Recipient{{Address: "review@example.com"}}, Body: "review", Attachments: []string{attachment},
			}})
			if err != nil {
				t.Fatal(err)
			}
			var snapshot string
			handoff := func(_ context.Context, request compose.Request, observer compose.DispatchObserver) (compose.Result, error) {
				snapshot = request.Attachments[0]
				if test.dispatch {
					if err := observer(); err != nil {
						t.Fatal(err)
					}
				}
				return test.result, test.err
			}
			var stdout, stderr bytes.Buffer
			code := runDraftHandoffWithDispatch(context.Background(), service, []string{"--ref", draft.Ref, "--json"}, &stdout, &stderr, handoff)
			var response envelope
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			result := response.Data.DraftHandoff
			if code != test.wantCode || stderr.Len() != 0 || result == nil || result.Outcome != test.outcome ||
				result.DraftRef != draft.Ref || result.AttemptID == "" || !result.DraftRetained || result.SnapshotsRetained != test.wantRetain ||
				result.DispatchStarted != test.dispatch || result.Opened != test.result.Opened {
				t.Fatalf("code=%d envelope=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			loaded, err := service.GetDraft(draft.Ref)
			if err != nil {
				t.Fatal(err)
			}
			if nativeError, ok := test.err.(*compose.Error); ok {
				if response.Error == nil || response.Error.Code != nativeError.Code {
					t.Fatalf("native error code lost: envelope=%s", stdout.String())
				}
				if nativeError.State == compose.StateCanceledBeforeDispatch {
					for _, path := range []string{"README.md", "docs/documentation.md"} {
						content := strings.ToLower(readRepositoryFile(t, path))
						if !strings.Contains(content, "returns error code `"+response.Error.Code+"` with public outcome `"+string(result.Outcome)+"`") {
							t.Errorf("%s differs from executed cancellation error/outcome", path)
						}
					}
				}
			}
			if test.wantRetain {
				if loaded.HandoffAttempt == nil || loaded.HandoffAttempt.ID != result.AttemptID || loaded.HandoffAttempt.Outcome != mail.HandoffOutcomeUnknown ||
					!loaded.HandoffAttempt.DispatchStarted || response.Error == nil || response.Error.Guidance == nil ||
					response.Error.Guidance.ReplayAllowed || response.Error.Guidance.Recovery.Command != "drafts.handoff-reconcile" {
					t.Fatalf("unknown result lost claim or recovery: draft=%+v envelope=%s", loaded, stdout.String())
				}
				if content, err := os.ReadFile(snapshot); err != nil || string(content) != "retained evidence" {
					t.Fatalf("snapshot lost: %q, %v", content, err)
				}
				if _, err := service.BeginDraftHandoff(draft.Ref); errorCode(err) != "handoff_retry_blocked" {
					t.Fatalf("uncertain handoff became replayable: %v", err)
				}
				if _, err := service.ReconcileDraftHandoff(draft.Ref, result.AttemptID, mail.HandoffResolutionFailed); err != nil {
					t.Fatal(err)
				}
			} else if loaded.HandoffAttempt != nil {
				t.Fatalf("terminal outcome retained claim: %+v", loaded.HandoffAttempt)
			}
			if _, err := os.Lstat(snapshot); !os.IsNotExist(err) {
				t.Fatalf("terminal/reconciled snapshot remains: %v", err)
			}
		})
	}
}
