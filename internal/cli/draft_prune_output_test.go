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
	"time"

	"mailcli/internal/mail"
)

type pruneOutputBoundaryContext struct {
	context.Context
	checks int
	action func()
}

func (c *pruneOutputBoundaryContext) Err() error {
	c.checks++
	if c.checks == 4 {
		c.action()
	}
	return c.Context.Err()
}

func TestDraftPruneJSONIncludesDirectoryStability(t *testing.T) {
	for _, change := range []bool{false, true} {
		t.Run(fmt.Sprint(change), func(t *testing.T) {
			root := t.TempDir()
			service := mail.NewServiceWithDraftRoot(nil, root)
			request := mail.PruneDraftsRequest{OlderThan: 24 * time.Hour}
			before, err := service.PruneDraftsContext(context.Background(), request)
			if err != nil || before.Revision == "" || before.Stable == nil || !*before.Stable {
				t.Fatalf("initial dry run = %+v, %v", before, err)
			}
			ctx := &pruneOutputBoundaryContext{Context: context.Background(), action: func() {
				if !change {
					return
				}
				if err := os.WriteFile(filepath.Join(root, "unrelated-marker"), []byte("retain"), 0o600); err != nil {
					t.Fatal(err)
				}
				future := time.Now().Add(2 * time.Second)
				if err := os.Chtimes(root, future, future); err != nil {
					t.Fatal(err)
				}
			}}
			result, err := service.PruneDraftsContext(ctx, request)
			if err != nil || ctx.checks < 4 || result.Revision != before.Revision || result.Stable == nil || *result.Stable == change {
				t.Fatalf("reported dry run = %+v, %v after %d checks", result, err, ctx.checks)
			}
			var stdout bytes.Buffer
			if code := writeSuccess(&stdout, "drafts.prune", responseData{PruneResult: &result}); code != 0 {
				t.Fatalf("write prune envelope = %d: %s", code, &stdout)
			}
			var response envelope
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if !response.OK || response.Command != "drafts.prune" || response.Data.PruneResult == nil ||
				response.Data.PruneResult.Revision != before.Revision || response.Data.PruneResult.Stable == nil ||
				*response.Data.PruneResult.Stable == change ||
				!strings.Contains(stdout.String(), fmt.Sprintf(`"stable":%t`, !change)) {
				t.Fatalf("missing original revision or explicit stability: %s", &stdout)
			}
		})
	}
}

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
						if confirm {
							if result.Revision != "" || result.Stable != nil || strings.Contains(stdout.String(), `"revision"`) ||
								strings.Contains(stdout.String(), `"stable"`) {
								t.Fatalf("confirmed prune changed its response shape: %s", &stdout)
							}
						} else if result.Revision == "" || result.Stable == nil {
							t.Fatalf("dry-run omitted revision or stability: %s", &stdout)
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
		{"bounded unknown", mail.PruneDraftsResult{PreservedTemporaries: []string{"unknown"}, PreservedTemporaryCount: 21}, true, "preserved unknown temporary\tunknown\npreserved unknown temporaries omitted\t20\n"},
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

func TestDraftPruneTemporaryOutputMatchesEligibility(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		for _, live := range []bool{false, true} {
			t.Run(fmt.Sprintf("json_%t/live_%t", jsonOutput, live), func(t *testing.T) {
				root := t.TempDir()
				service := mail.NewServiceWithDraftRoot(nil, root)
				ref := "draft_123456789012345678901234"
				if live {
					draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: mail.DraftInput{To: []mail.Recipient{{Address: "ada@example.com"}}, Body: "retained body"}})
					if err != nil {
						t.Fatal(err)
					}
					ref = draft.Ref
				}
				name := "." + ref + ".send-spool.mailcli-0123456789abcdef01234567"
				unknown := "." + ref + ".future.mailcli-0123456789abcdef01234567"
				for _, name := range []string{name, unknown} {
					path := filepath.Join(root, name)
					if err := os.WriteFile(path, []byte("temporary bytes"), 0o600); err != nil {
						t.Fatal(err)
					}
					old := time.Now().Add(-time.Hour)
					if err := os.Chtimes(path, old, old); err != nil {
						t.Fatal(err)
					}
				}
				for _, confirm := range []bool{false, true} {
					args := []string{"drafts", "prune"}
					if confirm {
						args = append(args, "--confirm")
					}
					if jsonOutput {
						args = append(args, "--json")
					}
					var stdout, stderr bytes.Buffer
					if code := Run(context.Background(), service, args, &stdout, &stderr); code != 0 {
						t.Fatalf("code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
					}
					if jsonOutput {
						var response envelope
						if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
							t.Fatal(err)
						}
						result := response.Data.PruneResult
						if !response.OK || result == nil || len(result.TemporaryArtifacts) != 1 || result.TemporaryArtifacts[0].Name != name || result.TemporaryArtifacts[0].Size != 15 || result.PreservedTemporaryCount != 1 || len(result.PreservedTemporaries) != 1 || result.PreservedTemporaries[0] != unknown {
							t.Fatalf("temporary response=%s", &stdout)
						}
					} else if (!confirm && !strings.Contains(stdout.String(), "would remove temporary\t"+name+"\t15 bytes\n")) || !strings.Contains(stdout.String(), "preserved unknown temporary\t"+unknown+"\n") || strings.Contains(stdout.String(), "no stale") {
						t.Fatalf("temporary stdout=%s", &stdout)
					}
					if confirm {
						if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
							t.Fatalf("confirmed temporary remains: %v", err)
						}
					} else if _, err := os.Lstat(filepath.Join(root, name)); err != nil {
						t.Fatal(err)
					}
					if got, err := os.ReadFile(filepath.Join(root, unknown)); err != nil || string(got) != "temporary bytes" {
						t.Fatalf("unknown temporary changed: %q, %v", got, err)
					}
				}
			})
		}
	}
}
