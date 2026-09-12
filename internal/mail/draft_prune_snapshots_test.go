package mail

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrunePreservesSymlinkedSnapshotObjects(t *testing.T) {
	for _, level := range []string{"parent", "attempt", "index", "leaf"} {
		t.Run(level, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "drafts")
			service := NewServiceWithDraftRoot(&draftGateway{}, root)
			draft := createSendTestDraft(t, service)
			if err := os.Remove(filepath.Join(root, draft.Ref+".json")); err != nil {
				t.Fatal(err)
			}
			claim := filepath.Join(root, draft.Ref+".save-claim")
			if err := os.WriteFile(claim, []byte("retained claim"), 0o600); err != nil {
				t.Fatal(err)
			}
			parent := filepath.Join(root, draft.Ref+handoffSnapshotSuffix)
			attempt := "handoff_123456789012345678901234"
			parts := map[string][]string{
				"parent":  {parent},
				"attempt": {parent, attempt},
				"index":   {parent, attempt, "0"},
				"leaf":    {parent, attempt, "0", "keep.txt"},
			}
			link := filepath.Join(parts[level]...)
			if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			unrelated := filepath.Join(root, "unrelated")
			remaining := []string{unrelated}
			switch level {
			case "parent":
				remaining = append(remaining, attempt, "0")
			case "attempt":
				remaining = append(remaining, "0")
			}
			remaining = append(remaining, "keep.txt")
			sentinel := filepath.Join(remaining...)
			if err := os.MkdirAll(filepath.Dir(sentinel), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sentinel, []byte("unrelated bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			identity, err := os.Lstat(sentinel)
			if err != nil {
				t.Fatal(err)
			}
			target := unrelated
			if level == "leaf" {
				target = sentinel
			}
			relativeTarget, err := filepath.Rel(filepath.Dir(link), target)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(relativeTarget, link); err != nil {
				t.Fatal(err)
			}
			result, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: 24 * time.Hour, Confirm: true})
			if err == nil || len(result.Failed) != 1 || len(result.SweptArtifacts) != 0 {
				t.Errorf("unsafe snapshot cleanup = %+v, %v", result, err)
			}
			payload, readErr := os.ReadFile(sentinel)
			current, statErr := os.Lstat(sentinel)
			if readErr != nil || statErr != nil || string(payload) != "unrelated bytes" || !os.SameFile(identity, current) {
				t.Fatalf("unrelated sentinel changed: %q, %v, %v", payload, readErr, statErr)
			}
			if payload, err := os.ReadFile(claim); err != nil || string(payload) != "retained claim" {
				t.Fatalf("unsafe cleanup removed recovery claim: %q, %v", payload, err)
			}
		})
	}
}

func TestOrphanSnapshotDirectoryRejectsReplacement(t *testing.T) {
	for _, level := range []int{0, 1, 2} {
		for _, openedFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("level_%d/opened_%t", level, openedFirst), func(t *testing.T) {
				base := t.TempDir()
				parts := []string{"snapshots", "handoff_123456789012345678901234", "0"}
				target := filepath.Join(append([]string{base}, parts[:level+1]...)...)
				if err := os.MkdirAll(target, 0o700); err != nil {
					t.Fatal(err)
				}
				parent, err := os.OpenRoot(filepath.Dir(target))
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := parent.Close(); err != nil {
						t.Error(err)
					}
				}()
				identity, err := parent.Lstat(filepath.Base(target))
				if err != nil {
					t.Fatal(err)
				}
				check := func() error { return nil }
				var pinned *orphanSnapshotDirectory
				if openedFirst {
					pinned, err = openOrphanSnapshotDirectory(parent, filepath.Base(target), identity, check)
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						if err := pinned.root.Close(); err != nil {
							t.Error(err)
						}
					}()
				}
				if err := os.Rename(target, target+".retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
				sentinel := filepath.Join(target, "keep.txt")
				if err := os.WriteFile(sentinel, []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
				if openedFirst {
					err = removeOrphanSnapshotDirectory(pinned, level)
				} else {
					pinned, err = openOrphanSnapshotDirectory(parent, filepath.Base(target), identity, check)
					if pinned != nil {
						if closeErr := pinned.root.Close(); closeErr != nil {
							t.Error(closeErr)
						}
					}
				}
				if err == nil {
					t.Fatal("directory replacement was accepted")
				}
				if payload, err := os.ReadFile(sentinel); err != nil || string(payload) != "replacement" {
					t.Fatalf("replacement changed: %q, %v", payload, err)
				}
			})
		}
	}
}

func TestOrphanSnapshotFileRejectsReplacement(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprintf("symlink_%t", symlink), func(t *testing.T) {
			base := t.TempDir()
			path := filepath.Join(base, "index")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			parent, err := os.OpenRoot(base)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := parent.Close(); err != nil {
					t.Error(err)
				}
			}()
			info, err := parent.Lstat("index")
			if err != nil {
				t.Fatal(err)
			}
			directory, err := openOrphanSnapshotDirectory(parent, "index", info, func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := directory.root.Close(); err != nil {
					t.Error(err)
				}
			}()
			leaf := filepath.Join(path, "keep.txt")
			if err := os.WriteFile(leaf, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			expected, err := directory.root.Lstat("keep.txt")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(leaf, leaf+".retained"); err != nil {
				t.Fatal(err)
			}
			if symlink {
				err = os.Symlink("keep.txt.retained", leaf)
			} else {
				err = os.WriteFile(leaf, []byte("replacement"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := removeOrphanSnapshotFile(directory, "keep.txt", expected); err == nil {
				t.Fatal("replaced file was removed")
			}
			if _, err := os.Lstat(leaf); err != nil {
				t.Fatalf("replacement disappeared: %v", err)
			}
			if payload, err := os.ReadFile(leaf + ".retained"); err != nil || string(payload) != "original" {
				t.Fatalf("original changed: %q, %v", payload, err)
			}
		})
	}
}

func TestOrphanSnapshotCleanupStopsOnCanceledOrReappearingDraft(t *testing.T) {
	for _, reappeared := range []bool{false, true} {
		t.Run(fmt.Sprintf("reappeared_%t", reappeared), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "drafts")
			service := NewServiceWithDraftRoot(&draftGateway{}, root)
			draft := createSendTestDraft(t, service)
			draftFile := filepath.Join(root, draft.Ref+".json")
			if err := os.Remove(draftFile); err != nil {
				t.Fatal(err)
			}
			lease, err := acquireDraftLease(context.Background(), root, draft.Ref)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := lease.release(); err != nil {
					t.Error(err)
				}
			}()
			parent := draft.Ref + handoffSnapshotSuffix
			path := filepath.Join(root, parent)
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			info, err := lease.storage.lstat(parent)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			check := func() error { return orphanDraftCleanupCheck(ctx, lease.storage, draft.Ref) }
			directory, err := openOrphanSnapshotDirectory(lease.storage.root, parent, info, check)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := directory.root.Close(); err != nil {
					t.Error(err)
				}
			}()
			if reappeared {
				if err := os.WriteFile(draftFile, []byte("reappeared"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			if err := removeOrphanSnapshotDirectory(directory, 0); err == nil {
				t.Fatal("cleanup ignored cancellation or reappearing draft")
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("snapshot directory removed: %v", err)
			}
		})
	}
}

func TestPruneReportsUnsafeLayoutAlongsideCompletedCleanup(t *testing.T) {
	for _, layout := range []string{"invalid_attempt", "invalid_index", "non_regular_leaf", "multiple_files", "regular_parent"} {
		t.Run(layout, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "drafts")
			service := NewServiceWithDraftRoot(&draftGateway{}, root)
			bad := createSendTestDraft(t, service)
			good := createSendTestDraft(t, service)
			for _, draft := range []Draft{bad, good} {
				if err := os.Remove(filepath.Join(root, draft.Ref+".json")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, draft.Ref+".save-claim"), []byte("claim"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			parent := filepath.Join(root, bad.Ref+handoffSnapshotSuffix)
			attempt, index := "handoff_123456789012345678901234", "0"
			if layout == "invalid_attempt" {
				attempt = "unrelated"
			}
			if layout == "invalid_index" {
				index = "100"
			}
			sentinel := filepath.Join(parent, attempt, index, "keep.txt")
			if layout == "regular_parent" {
				sentinel = parent
			}
			if err := os.MkdirAll(filepath.Dir(sentinel), 0o700); err != nil {
				t.Fatal(err)
			}
			if layout == "non_regular_leaf" {
				if err := os.Mkdir(sentinel, 0o700); err != nil {
					t.Fatal(err)
				}
				sentinel = filepath.Join(sentinel, "nested.txt")
			}
			if err := os.WriteFile(sentinel, []byte("preserved"), 0o600); err != nil {
				t.Fatal(err)
			}
			if layout == "multiple_files" {
				if err := os.WriteFile(sentinel+".other", []byte("also preserved"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: 24 * time.Hour, Confirm: true})
			if err == nil || len(result.Failed) != 1 || result.Failed[0].Ref != bad.Ref ||
				len(result.SweptArtifacts) != 1 || result.SweptArtifacts[0] != good.Ref {
				t.Fatalf("partial cleanup = %+v, %v", result, err)
			}
			if payload, err := os.ReadFile(sentinel); err != nil || string(payload) != "preserved" {
				t.Fatalf("unsafe layout changed: %q, %v", payload, err)
			}
			if _, err := os.Lstat(filepath.Join(root, bad.Ref+".save-claim")); err != nil {
				t.Fatalf("failed cleanup discarded its claim: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(root, good.Ref+".save-claim")); !os.IsNotExist(err) {
				t.Fatalf("successful cleanup retained its claim: %v", err)
			}
		})
	}
}

func TestPruneRejectsUnclaimedSymlinkSnapshotRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	service := NewServiceWithDraftRoot(&draftGateway{}, root)
	draft := createSendTestDraft(t, service)
	if err := os.Remove(filepath.Join(root, draft.Ref+".json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "unrelated"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, draft.Ref+handoffSnapshotSuffix)
	if err := os.Symlink("unrelated", link); err != nil {
		t.Fatal(err)
	}
	for _, confirm := range []bool{false, true} {
		result, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: 24 * time.Hour, Confirm: confirm})
		if confirm {
			if err == nil || len(result.Failed) != 1 || len(result.SweptArtifacts) != 0 {
				t.Fatalf("confirmed unsafe root = %+v, %v", result, err)
			}
		} else if err != nil || len(result.OrphanArtifacts) != 1 || result.OrphanArtifacts[0] != draft.Ref {
			t.Fatalf("unsafe root discovery = %+v, %v", result, err)
		}
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("unsafe snapshot root changed: %v, %v", info, err)
		}
	}
}
