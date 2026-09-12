package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

func TestDraftPruneOrphanOutputMatchesEffects(t *testing.T) {
	for _, confirm := range []bool{false, true} {
		for _, jsonOutput := range []bool{false, true} {
			for _, unsafe := range []bool{false, true} {
				t.Run(fmt.Sprintf("confirm_%t/json_%t/unsafe_%t", confirm, jsonOutput, unsafe), func(t *testing.T) {
					root := t.TempDir()
					ref := "draft_123456789012345678901234"
					badRef := "draft_223456789012345678901234"
					claim := filepath.Join(root, ref+".save-claim")
					if err := os.WriteFile(claim, []byte("retained"), 0o600); err != nil {
						t.Fatal(err)
					}
					var sentinel, badClaim string
					if unsafe {
						badClaim = filepath.Join(root, badRef+".save-claim")
						if err := os.WriteFile(badClaim, []byte("retain unsafe claim"), 0o600); err != nil {
							t.Fatal(err)
						}
						target := t.TempDir()
						sentinel = filepath.Join(target, "untouched")
						if err := os.WriteFile(sentinel, []byte("sentinel"), 0o600); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(target, filepath.Join(root, badRef+".handoff-snapshots")); err != nil {
							t.Fatal(err)
						}
					}
					args := []string{"drafts", "prune"}
					if confirm {
						args = append(args, "--confirm")
					}
					if jsonOutput {
						args = append(args, "--json")
					}
					var stdout, stderr bytes.Buffer
					code := Run(context.Background(), mail.NewServiceWithDraftRoot(nil, root), args, &stdout, &stderr)
					wantCode := 0
					if confirm && unsafe {
						wantCode = 1
					}
					if code != wantCode || strings.Contains(stdout.String(), "no stale") {
						t.Fatalf("code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
					}
					if jsonOutput {
						var response envelope
						if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
							t.Fatal(err)
						}
						result := response.Data.PruneResult
						if result == nil || response.OK != (wantCode == 0) || result.DryRun == confirm || stderr.Len() != 0 {
							t.Fatalf("unexpected envelope: %s stderr=%s", &stdout, &stderr)
						}
						refs := result.OrphanArtifacts
						if confirm {
							refs = result.SweptArtifacts
						}
						wantRefs := 1
						if unsafe && !confirm {
							wantRefs = 2
						}
						if len(refs) != wantRefs || refs[0] != ref || len(result.Failed) != wantCode ||
							wantRefs == 2 && refs[1] != badRef || wantCode == 1 && result.Failed[0].Ref != badRef {
							t.Fatalf("result=%+v", result)
						}
					} else {
						label := "would sweep artifacts\t"
						if confirm {
							label = "swept_artifacts\t"
						}
						if strings.Count(stdout.String(), label+ref+"\n") != 1 || strings.Count(stderr.String(), "\n") != wantCode {
							t.Fatalf("stdout=%s stderr=%s", &stdout, &stderr)
						}
						if confirm && unsafe && (!strings.Contains(stdout.String(), "failed\t"+badRef+"\t") || strings.Contains(stdout.String(), "swept_artifacts\t"+badRef)) {
							t.Fatalf("partial result hidden: %s", &stdout)
						}
						if unsafe && !confirm && strings.Count(stdout.String(), label+badRef+"\n") != 1 {
							t.Fatalf("missing second candidate: %s", &stdout)
						}
					}
					payload, err := os.ReadFile(claim)
					if confirm && !errors.Is(err, os.ErrNotExist) || !confirm && (err != nil || string(payload) != "retained") {
						t.Fatalf("claim=%q err=%v", payload, err)
					}
					if unsafe {
						for path, want := range map[string]string{sentinel: "sentinel", badClaim: "retain unsafe claim"} {
							if payload, err := os.ReadFile(path); err != nil || string(payload) != want {
								t.Fatalf("preserved path %s = %q, %v", path, payload, err)
							}
						}
					}
				})
			}
		}
	}
}

func TestDraftPruneHumanResultCategories(t *testing.T) {
	for _, test := range []struct {
		name     string
		result   mail.PruneDraftsResult
		complete bool
		want     string
	}{
		{"empty", mail.PruneDraftsResult{}, true, "no stale never-sent drafts\n"},
		{"early failure", mail.PruneDraftsResult{}, false, ""},
		{"dry", mail.PruneDraftsResult{DryRun: true, Candidates: []mail.PruneCandidate{{Ref: "draft", AgeDays: 31, Subject: "subject"}}, ExpiredReceipts: []string{"receipt"}, OrphanArtifacts: []string{"orphan"}}, true, "would remove\tdraft\t31 days\tsubject\nwould remove receipt\treceipt\nwould sweep artifacts\torphan\n"},
		{"partial", mail.PruneDraftsResult{Candidates: []mail.PruneCandidate{{Ref: "candidate"}}, Removed: []string{"draft"}, SweptLocks: []string{"lock"}, ExpiredReceipts: []string{"receipt"}, SweptArtifacts: []string{"orphan"}, Failed: []mail.PruneFailure{{Ref: "failed", Error: "reason"}}}, false, "removed\tdraft\nswept_lock\tlock\nremoved receipt\treceipt\nswept_artifacts\torphan\nfailed\tfailed\treason\n"},
		{"uncompleted candidate", mail.PruneDraftsResult{Candidates: []mail.PruneCandidate{{Ref: "candidate"}}}, false, ""},
		{"dry failure", mail.PruneDraftsResult{DryRun: true, Failed: []mail.PruneFailure{{Ref: "ref", Error: "reason"}}}, false, "failed\tref\treason\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			writeDraftPruneResult(&out, test.result, test.complete)
			if out.String() != test.want {
				t.Fatalf("output=%q want=%q", out.String(), test.want)
			}
		})
	}
}
