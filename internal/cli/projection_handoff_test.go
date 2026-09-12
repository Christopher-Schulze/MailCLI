package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

func TestDraftProjectionPreservesRealHandoffEvidence(t *testing.T) {
	for _, outcome := range []mail.HandoffOutcome{
		"", mail.HandoffOutcomePrepared, mail.HandoffOutcomeDispatched,
		mail.HandoffOutcomeUnknown, mail.HandoffOutcomeConfirmedOpened,
		mail.HandoffOutcomeConfirmedFailed, mail.HandoffOutcomeCanceled,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			service, draft := createProjectionHandoffState(t, outcome)
			for _, view := range []string{"metadata", "plain", "full", "narrow", "explicit", "fallback", "export"} {
				t.Run(view, func(t *testing.T) {
					args := []string{"drafts", "inspect", "--ref", draft.Ref, "--json"}
					wantCode := 0
					exportPath := ""
					switch view {
					case "narrow":
						args = append(args, "--fields", "ref")
					case "explicit":
						args = append(args, "--fields", "handoff_attempt")
					case "fallback":
						args = append(args, "--view", "full", "--max-bytes", "1")
						wantCode = 1
					case "export":
						exportPath = filepath.Join(t.TempDir(), "body.txt")
						args = append(args, "--view", "full", "--export", exportPath)
					default:
						args = append(args, "--view", view)
					}
					var stdout, stderr bytes.Buffer
					code := Run(context.Background(), service, args, &stdout, &stderr)
					if code != wantCode || stderr.Len() != 0 {
						t.Fatalf("code=%d stderr=%s output=%s", code, &stderr, &stdout)
					}
					assertProjectedHandoff(t, stdout.Bytes(), draft)
					if view == "fallback" && (!strings.Contains(stdout.String(), `"code":"output_too_large"`) || strings.Contains(stdout.String(), `"body":`)) {
						t.Fatalf("invalid fallback: %s", &stdout)
					}
					if exportPath != "" {
						assertExportFile(t, exportPath, []byte(draft.Body), stdout.String())
						if strings.Contains(stdout.String(), `"body":`) {
							t.Fatalf("export leaked body: %s", &stdout)
						}
					}
				})
			}
			if outcome == mail.HandoffOutcomeUnknown {
				session, err := service.BeginDraftHandoff(draft.Ref)
				if session != nil {
					if closeErr := session.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
				}
				if err == nil || !strings.Contains(err.Error(), draft.HandoffAttempt.ID) {
					t.Fatalf("retained attempt did not block replay with usable identity: %v", err)
				}
			}
		})
	}
	if !slices.Contains(capabilities().Limits.OutputProjection.DraftFields, "handoff_attempt") {
		t.Fatal("capabilities omit the selectable handoff_attempt field")
	}
}

func createProjectionHandoffState(t *testing.T, outcome mail.HandoffOutcome) (*mail.Service, mail.Draft) {
	t.Helper()
	service := mail.NewServiceWithDraftRoot(nil, filepath.Join(t.TempDir(), "drafts"))
	attachment := filepath.Join(t.TempDir(), "private-attachment.txt")
	if err := os.WriteFile(attachment, []byte("retained bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		To: []mail.Recipient{{Address: "recipient@example.com"}}, Body: "canonical body",
		Attachments: []string{attachment},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if outcome == "" {
		return service, draft
	}
	session, err := service.BeginDraftHandoff(draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	if outcome == mail.HandoffOutcomeCanceled {
		err = session.CancelBeforeDispatch()
	} else if outcome != mail.HandoffOutcomePrepared {
		err = session.MarkDispatched(context.Background())
		if err == nil && outcome != mail.HandoffOutcomeDispatched {
			err = session.Finish(outcome)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	draft, err = service.GetDraft(draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	retained := outcome == mail.HandoffOutcomePrepared || outcome == mail.HandoffOutcomeDispatched || outcome == mail.HandoffOutcomeUnknown
	if retained != (draft.HandoffAttempt != nil) {
		t.Fatalf("unexpected retained state for %s: %+v", outcome, draft.HandoffAttempt)
	}
	if retained && (draft.HandoffAttempt.ID != session.AttemptID() || draft.HandoffAttempt.Outcome != outcome) {
		t.Fatalf("incorrect transition: %+v", draft.HandoffAttempt)
	}
	return service, draft
}

func assertProjectedHandoff(t *testing.T, payload []byte, draft mail.Draft) {
	t.Helper()
	var response struct {
		Data struct {
			Draft struct {
				Ref     string                           `json:"ref"`
				Attempt *mail.DraftHandoffAttemptSummary `json:"handoff_attempt"`
			} `json:"draft"`
			Projection projectionInfo `json:"projection"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatal(err)
	}
	got, want := response.Data.Draft.Attempt, draft.HandoffAttempt
	if response.Data.Draft.Ref != draft.Ref || !slices.Contains(response.Data.Projection.Fields, "handoff_attempt") {
		t.Fatalf("identity or field schema missing: %s", payload)
	}
	if want == nil {
		if got != nil || bytes.Contains(payload, []byte(`"handoff_attempt":`)) {
			t.Fatalf("invented attempt: %s", payload)
		}
		return
	}
	if got == nil || got.ID != want.ID || got.Outcome != want.Outcome ||
		!got.StartedAt.Equal(want.StartedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) ||
		got.DispatchStarted != want.DispatchStarted || got.SnapshotsRetained != want.SnapshotsRetained ||
		got.SnapshotCount != 1 || got.SnapshotBytes != int64(len("retained bytes")) {
		t.Fatalf("lost recovery evidence: got=%+v want=%+v output=%s", got, want, payload)
	}
	var raw struct {
		Data struct {
			Draft struct {
				Attempt map[string]json.RawMessage `json:"handoff_attempt"`
			} `json:"draft"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Data.Draft.Attempt) != 8 || raw.Data.Draft.Attempt["snapshots"] != nil || raw.Data.Draft.Attempt["draft_ref"] != nil {
		t.Fatalf("handoff projection exposed persistence fields: %s", payload)
	}
}
