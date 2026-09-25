package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
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

func TestDraftPreviewBudgetKeepsCompletePayloadOrReturnsInspectRoute(t *testing.T) {
	service := mail.NewServiceWithDraftRoot(nil, filepath.Join(t.TempDir(), "drafts"))
	small, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		To: []mail.Recipient{{Address: "recipient@example.com"}}, Subject: "Stable preview", Body: "complete preview body",
	}})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := service.GetDraft(small.Ref)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := makeDraftPreview(stored, "plain")
	if err != nil {
		t.Fatal(err)
	}
	want := &bytes.Buffer{}
	if code := writeSuccess(want, "drafts.preview", responseData{DraftPreview: &preview}); code != 0 {
		t.Fatalf("marshal baseline preview: %d", code)
	}
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), service, []string{"drafts", "preview", "--ref", small.Ref, "--json"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 || !bytes.Equal(stdout.Bytes(), want.Bytes()) {
		t.Fatalf("default preview payload changed: code=%d got=%s want=%s stderr=%s", code, stdout.String(), want.String(), stderr.String())
	}

	large, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{
		To: []mail.Recipient{{Address: "recipient@example.com"}}, Body: strings.Repeat("b", mail.MaximumDraftBodyBytes),
	}})
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := Run(context.Background(), service, []string{"drafts", "preview", "--ref", large.Ref, "--json"}, &stdout, &stderr); code != 1 {
		t.Fatalf("default limit accepted maximum body: code=%d, output=%s", code, stdout.String())
	}
	var limited envelope
	if err := json.Unmarshal(stdout.Bytes(), &limited); err != nil || limited.OK || limited.Error == nil || limited.Error.Code != "output_too_large" ||
		limited.Data.DraftPreview != nil || !strings.Contains(limited.Error.Message, large.Ref) || !strings.Contains(limited.Error.Message, "--export /absolute/new/path") || bytes.Contains(stdout.Bytes(), []byte(`"body"`)) {
		t.Fatalf("oversized preview returned partial content or lost recovery: decode=%v response=%+v output=%s", err, limited, stdout.String())
	}

	largeArgs := []string{"drafts", "preview", "--ref", large.Ref, "--max-bytes", strconv.FormatInt(maximumJSONOutputBytes, 10), "--json"}
	stdout.Reset()
	if code := Run(context.Background(), service, largeArgs, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("64 MiB preview failed: code=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var complete envelope
	if err := json.Unmarshal(stdout.Bytes(), &complete); err != nil || !complete.OK || complete.Data.DraftPreview == nil || complete.Data.DraftPreview.Body != strings.Repeat("b", mail.MaximumDraftBodyBytes) {
		t.Fatalf("maximum body preview was incomplete: decode=%v, output bytes=%d", err, stdout.Len())
	}
	boundary := append([]byte(nil), stdout.Bytes()...)
	largeArgs[len(largeArgs)-2] = strconv.Itoa(len(boundary))
	stdout.Reset()
	if code := Run(context.Background(), service, largeArgs, &stdout, &stderr); code != 0 || !bytes.Equal(stdout.Bytes(), boundary) {
		t.Fatalf("exact preview byte boundary rejected: code=%d output bytes=%d", code, stdout.Len())
	}
	largeArgs[len(largeArgs)-2] = strconv.Itoa(len(boundary) - 1)
	stdout.Reset()
	if code := Run(context.Background(), service, largeArgs, &stdout, &stderr); code != 1 || json.Unmarshal(stdout.Bytes(), &limited) != nil || limited.OK || limited.Error == nil || limited.Error.Code != "output_too_large" || limited.Data.DraftPreview != nil {
		t.Fatalf("one-byte-over preview did not fail without content: code=%d output=%s", code, stdout.String())
	}
}
