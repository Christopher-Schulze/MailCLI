package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"mailcli/internal/mail"
)

func TestDraftWorkflowReusesReviewedResultsThroughRecovery(t *testing.T) {
	for _, format := range []string{"plain", "html", "markdown"} {
		t.Run(format, func(t *testing.T) {
			root := t.TempDir()
			service := mail.NewServiceWithDraftRoot(nil, root)
			invoke := func(args []string, wantCode int) envelope {
				t.Helper()
				var stdout, stderr bytes.Buffer
				code := Run(context.Background(), service, append(args, "--json"), &stdout, &stderr)
				var result envelope
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || code != wantCode || stderr.Len() != 0 || result.OK != (wantCode == 0) {
					t.Fatalf("code=%d decode=%v stdout=%s stderr=%s", code, err, &stdout, &stderr)
				}
				return result
			}
			created := invoke([]string{"drafts", "create", "--to", "recipient@example.com", "--cc", "cc@example.com",
				"--subject", "Before", "--body", "<p>Body</p>\n\n**Source**", "--format", format, "--view", "full"}, 0).Data.Draft
			if created == nil || created.Ref == "" || created.Revision == "" || created.Body == "" {
				t.Fatalf("incomplete creation review: %+v", created)
			}
			updated := invoke([]string{"drafts", "update", "--ref", created.Ref, "--expected-revision", created.Revision,
				"--subject", "After", "--view", "full"}, 0).Data.Draft
			if updated == nil || updated.Ref != created.Ref || updated.Revision == created.Revision || updated.Subject != "After" {
				t.Fatalf("partial update review: %+v", updated)
			}
			repeated := invoke([]string{"drafts", "update", "--ref", updated.Ref, "--expected-revision", updated.Revision,
				"--format", format, "--cc", "", "--view", "full"}, 0).Data.Draft
			if repeated == nil || len(repeated.CC) != 0 || repeated.Revision == updated.Revision {
				t.Fatalf("same-format explicit clear: %+v", repeated)
			}
			beforeConflict, err := service.GetDraft(created.Ref)
			if err != nil {
				t.Fatal(err)
			}
			conflict := invoke([]string{"drafts", "update", "--ref", created.Ref, "--expected-revision", created.Revision,
				"--subject", "Wrong"}, 1)
			afterConflict, err := service.GetDraft(created.Ref)
			if err != nil || conflict.Error == nil || conflict.Error.Code != "draft_revision_conflict" || !reflect.DeepEqual(beforeConflict, afterConflict) {
				t.Fatalf("conflict mutated reviewed state: %v %+v", err, conflict.Error)
			}
			inspected := invoke([]string{"drafts", "inspect", "--ref", repeated.Ref, "--view", "full"}, 0).Data.Draft
			if inspected == nil || inspected.Revision != repeated.Revision {
				t.Fatalf("conflict inspection lost current revision: %+v", inspected)
			}
			limited := invoke([]string{"drafts", "update", "--ref", inspected.Ref, "--expected-revision", inspected.Revision,
				"--subject", "Recovered", "--max-bytes", "1"}, 1)
			if limited.Error == nil || limited.Error.Code != "output_too_large" || limited.Error.Guidance == nil || limited.Data.Draft == nil {
				t.Fatalf("missing completed-output evidence: %+v", limited)
			}
			guidance := limited.Error.Guidance
			if guidance.EffectCertainty != mail.EffectComplete || guidance.ReplayAllowed || guidance.Recovery.Command != "drafts.inspect" {
				t.Fatalf("unsafe recovery: %+v", guidance)
			}
			recovered := invoke(append([]string{"drafts", "inspect"}, guidance.Recovery.Args...), 0).Data.Draft
			if recovered == nil || recovered.Ref != created.Ref || recovered.Revision != limited.Data.Draft.Revision ||
				recovered.Revision == inspected.Revision || recovered.Subject != "Recovered" || len(recovered.CC) != 0 {
				t.Fatalf("recovery did not inspect the completed update: %+v", recovered)
			}
			for _, draft := range []*mail.Draft{updated, repeated, inspected, recovered} {
				if draft.Body != created.Body || draft.BodySource != created.BodySource || draft.BodyHTML != created.BodyHTML ||
					draft.BodyFormat != created.BodyFormat || !reflect.DeepEqual(draft.To, created.To) {
					t.Fatalf("omitted content changed: %+v", draft)
				}
			}
			paths, err := filepath.Glob(filepath.Join(root, "draft_*.json"))
			if err != nil || len(paths) != 1 {
				t.Fatalf("recovery duplicated the draft: %v %v", paths, err)
			}
		})
	}
}
