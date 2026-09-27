package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func draftMutationFixture(t testing.TB, count int) (string, string) {
	t.Helper()
	root := t.TempDir()
	draft, err := prepareDraft(CreateDraftRequest{Input: DraftInput{
		To: []Recipient{{Address: "ada@example.com"}}, Subject: "Mutation", Body: "complete draft bytes",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for index := range count {
		draft.Ref = fmt.Sprintf("draft_%024d", index)
		if err := refreshDraftRevision(&draft); err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(draft)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, draft.Ref+".json"), payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, "draft_000000000000000000000000"
}

func TestDraftMutationWithoutDirectoryEnumeration(t *testing.T) {
	for _, count := range []int{10, 10000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			root, ref := draftMutationFixture(t, count)
			lease, err := acquireDraftLease(context.Background(), root, ref)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Chmod(root, 0o700); err != nil {
					t.Error(err)
				}
				if err := lease.release(); err != nil {
					t.Error(err)
				}
			})
			// Keep filename lookup available while forbidding root enumeration;
			// the empty inventory proves cleanup does not reopen the directory.
			if err := os.Chmod(root, 0o100); err != nil {
				t.Fatal(err)
			}
			if removed, err := removeDraftTemporaryFiles(lease.storage, ref, nil); err != nil || removed != 0 {
				t.Fatalf("empty inventory cleanup = %d, %v; want no enumeration", removed, err)
			}
			draft, err := readDraftForMutation(lease, root, ref)
			if err != nil || draft.Ref != ref || draft.Body != "complete draft bytes" {
				t.Fatalf("single-ref mutation read = %+v, %v", draft, err)
			}
		})
	}
}

func BenchmarkDraftMutationDirectorySize(b *testing.B) {
	for _, count := range []int{10, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			root, ref := draftMutationFixture(b, count)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				lease, err := acquireDraftLease(context.Background(), root, ref)
				if err != nil {
					b.Fatal(err)
				}
				draft, readErr := readDraftForMutation(lease, root, ref)
				if err := errors.Join(readErr, lease.release()); err != nil || draft.Ref != ref {
					b.Fatalf("mutation read = %s, %v", draft.Ref, err)
				}
			}
		})
	}
}

// The real cancellation boundary observes the completed private temporary.
// Only this test context pauses; production has no injected test callback.
type draftTemporaryBoundaryContext struct {
	context.Context
	root    string
	ref     string
	entered chan string
	resume  chan struct{}
	once    sync.Once
}

func (c *draftTemporaryBoundaryContext) Err() error {
	entries, err := os.ReadDir(c.root)
	if err == nil {
		for _, entry := range entries {
			if ref, known := draftTemporaryRef(entry.Name()); known && ref == c.ref {
				c.once.Do(func() { c.entered <- entry.Name(); <-c.resume })
				break
			}
		}
	}
	return c.Context.Err()
}

func TestDraftTemporaryWriterLeaseRefusalPreservesStoredDraft(t *testing.T) {
	root := t.TempDir()
	draft, err := prepareDraft(CreateDraftRequest{Input: DraftInput{To: []Recipient{{Address: "ada@example.com"}}, Subject: "original", Body: "complete bytes"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDraftFile(root, draft); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, draft.Ref+".json"))
	if err != nil {
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	draft.Subject = "must not publish"
	if err := writeDraftFileContext(ctx, root, draft); errorCode(err) != "draft_busy" {
		t.Fatalf("competing writer = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(root, draft.Ref+".json"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("refused writer changed draft: %q, %v", after, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if _, known := draftTemporaryRef(entry.Name()); known {
			t.Fatalf("refused writer exposed %s", entry.Name())
		}
	}
}

func TestDraftTemporaryPublicationLeaseProtectsActualWriter(t *testing.T) {
	for _, update := range []bool{false, true} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("update_%t/canceled_%t", update, canceled), func(t *testing.T) {
				root := t.TempDir()
				service := NewServiceWithDraftRoot(nil, root)
				draft, err := prepareDraft(CreateDraftRequest{Input: DraftInput{To: []Recipient{{Address: "ada@example.com"}}, Subject: "original", Body: "complete draft bytes"}})
				if err != nil {
					t.Fatal(err)
				}
				var storage []*draftStorage
				if update {
					if err := writeDraftFile(root, draft); err != nil {
						t.Fatal(err)
					}
					lease, err := acquireDraftLease(context.Background(), root, draft.Ref)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := lease.release(); err != nil {
							t.Error(err)
						}
					})
					storage = append(storage, lease.storage)
				}
				draft.Subject = "published"
				base, cancel := context.WithCancel(context.Background())
				defer cancel()
				ctx := &draftTemporaryBoundaryContext{Context: base, root: root, ref: draft.Ref, entered: make(chan string, 1), resume: make(chan struct{})}
				var resumeOnce sync.Once
				resume := func() { resumeOnce.Do(func() { close(ctx.resume) }) }
				finished := make(chan error, 1)
				go func() { finished <- writeDraftFileContext(ctx, root, draft, storage...) }()
				joined := false
				t.Cleanup(func() {
					resume()
					if !joined {
						select {
						case <-finished:
						case <-time.After(5 * time.Second):
							t.Error("writer did not terminate")
						}
					}
				})
				var name string
				select {
				case name = <-ctx.entered:
				case err := <-finished:
					joined = true
					t.Fatalf("writer did not expose its temporary: %v", err)
				case <-time.After(5 * time.Second):
					t.Fatal("writer boundary timed out")
				}
				path := filepath.Join(root, name)
				ageDraftTemporary(t, path)
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				result, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: 24 * time.Hour, Confirm: true})
				if err != nil || len(result.Failed) != 0 || len(result.SweptArtifacts) != 0 || len(result.TemporaryArtifacts) != 0 {
					t.Fatalf("prune touched active writer: %+v, %v", result, err)
				}
				if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
					t.Fatalf("active temporary changed: %q, %v", after, err)
				}
				if canceled {
					cancel()
				}
				resume()
				select {
				case err = <-finished:
					joined = true
				case <-time.After(5 * time.Second):
					t.Fatal("writer did not finish")
				}
				if (err != nil) != canceled {
					t.Fatalf("writer error=%v canceled=%t", err, canceled)
				}
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("publication temporary remains: %v", err)
				}
				loaded, err := service.GetDraft(draft.Ref)
				if canceled && !update {
					if errorCode(err) != "not_found" {
						t.Fatalf("canceled create published: %+v, %v", loaded, err)
					}
					return
				}
				want := "published"
				if canceled {
					want = "original"
				}
				if err != nil || loaded.Subject != want || loaded.Body != draft.Body {
					t.Fatalf("published draft=%+v, %v", loaded, err)
				}
			})
		}
	}
}

type draftRootSwapObserver struct {
	once        sync.Once
	root        string
	moved       string
	replacement string
	ref         string
	err         error
}

func (observer *draftRootSwapObserver) ContentRendered() {
	observer.once.Do(func() {
		if err := os.Rename(observer.root, observer.moved); err != nil {
			observer.err = err
			return
		}
		if err := os.Mkdir(observer.replacement, 0o700); err != nil {
			observer.err = err
			return
		}
		if err := os.Symlink(observer.replacement, observer.root); err != nil {
			observer.err = err
			return
		}
		path, err := draftPath(observer.replacement, observer.ref)
		if err != nil {
			observer.err = err
			return
		}
		observer.err = os.WriteFile(path, []byte("replacement"), 0o600)
	})
}

func TestDraftLeasePinsReadsWritesClaimsAndCleanupAfterRootReplacement(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "drafts")
	service := NewServiceWithDraftRoot(nil, root)
	draft, err := service.CreateDraft(CreateDraftRequest{
		Kind:  DraftKindNew,
		Input: DraftInput{To: []Recipient{{Address: "recipient@example.com"}}, Subject: "original", Body: "body"},
	})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	lease, err := acquireDraftLease(context.Background(), root, draft.Ref)
	if err != nil {
		t.Fatalf("acquireDraftLease() error = %v", err)
	}
	defer func() {
		if err := lease.release(); err != nil {
			t.Errorf("release() error = %v", err)
		}
	}()

	moved := filepath.Join(base, "moved-drafts")
	if err := os.Rename(root, moved); err != nil {
		t.Fatalf("rename original draft root: %v", err)
	}
	replacement := filepath.Join(base, "replacement-drafts")
	if err := os.Mkdir(replacement, 0o700); err != nil {
		t.Fatalf("create replacement draft root: %v", err)
	}
	if err := os.Symlink(replacement, root); err != nil {
		t.Fatalf("symlink replacement draft root: %v", err)
	}
	replacementDraft, err := draftPath(replacement, draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacementDraft, []byte("replacement"), 0o600); err != nil {
		t.Fatalf("write replacement draft: %v", err)
	}

	loaded, err := readDraftForMutation(lease, root, draft.Ref)
	if err != nil {
		t.Fatalf("readDraftForMutation() error = %v", err)
	}
	if loaded.Subject != "original" {
		t.Fatalf("loaded subject = %q, want original root subject", loaded.Subject)
	}

	updated := loaded
	updated.Subject = "pinned update"
	if err := writeDraftFile(root, updated, lease.storage); err != nil {
		t.Fatalf("writeDraftFile() error = %v", err)
	}
	movedDraft, err := draftPath(moved, draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	movedPayload, err := os.ReadFile(movedDraft)
	if err != nil {
		t.Fatalf("read moved draft: %v", err)
	}
	if string(movedPayload) == "replacement" {
		t.Fatal("pinned write used replacement root")
	}
	replacementPayload, err := os.ReadFile(replacementDraft)
	if err != nil {
		t.Fatalf("read replacement draft: %v", err)
	}
	if string(replacementPayload) != "replacement" {
		t.Fatalf("replacement draft changed to %q", replacementPayload)
	}

	if _, err := beginSendAttempt(sendAttemptOptions{
		Root: root, Ref: draft.Ref, Storage: lease.storage,
		MessageID:           "<message@example.com>",
		EnvelopeFingerprint: envelopeFingerprint(updated, "<message@example.com>"),
	}); err != nil {
		t.Fatalf("beginSendAttempt() error = %v", err)
	}
	claimPath, err := sendClaimPath(moved, draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(claimPath); err != nil {
		t.Fatalf("pinned send claim error = %v", err)
	}
	replacementClaim, err := sendClaimPath(replacement, draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(replacementClaim); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement send claim error = %v, want absent", err)
	}

	if err := discardDraftFiles(lease, root, draft.Ref); err != nil {
		t.Fatalf("discardDraftFiles() error = %v", err)
	}
	if _, err := os.Stat(movedDraft); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("moved draft error = %v, want absent", err)
	}
	replacementPayload, err = os.ReadFile(replacementDraft)
	if err != nil {
		t.Fatalf("read replacement draft after cleanup: %v", err)
	}
	if string(replacementPayload) != "replacement" {
		t.Fatalf("replacement draft changed during cleanup to %q", replacementPayload)
	}
	if err := lease.release(); err != nil {
		t.Fatalf("second release() error = %v", err)
	}
}

func TestUpdateDraftUsesLeasedRootWhenPathSwapsDuringPreparation(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "drafts")
	service := NewServiceWithDraftRoot(nil, root)
	draft, err := service.CreateDraft(CreateDraftRequest{
		Kind:  DraftKindNew,
		Input: DraftInput{To: []Recipient{{Address: "recipient@example.com"}}, Subject: "original", Body: "body"},
	})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	observer := &draftRootSwapObserver{
		root: root, moved: filepath.Join(base, "moved-drafts"),
		replacement: filepath.Join(base, "replacement-drafts"), ref: draft.Ref,
	}
	service.contentObserver = observer
	updated, err := service.UpdateDraft(UpdateDraftRequest{
		Ref:              draft.Ref,
		ExpectedRevision: draft.Revision,
		Input:            DraftInput{To: []Recipient{{Address: "recipient@example.com"}}, Subject: "updated", Body: "body 2"},
	})
	if err != nil {
		t.Fatalf("UpdateDraft() error = %v", err)
	}
	if observer.err != nil {
		t.Fatalf("root swap error = %v", observer.err)
	}
	if updated.Subject != "updated" {
		t.Fatalf("UpdateDraft() subject = %q, want updated", updated.Subject)
	}
	movedDraft, err := draftPath(observer.moved, draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := readDraftFile(observer.moved, draft.Ref)
	if err != nil {
		t.Fatalf("read moved draft error = %v", err)
	}
	if loaded.Subject != "updated" {
		t.Fatalf("moved draft subject = %q, want updated", loaded.Subject)
	}
	if _, err := os.Stat(movedDraft); err != nil {
		t.Fatalf("moved draft path error = %v", err)
	}
	replacementDraft, err := draftPath(observer.replacement, draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	replacementPayload, err := os.ReadFile(replacementDraft)
	if err != nil {
		t.Fatalf("read replacement draft: %v", err)
	}
	if string(replacementPayload) != "replacement" {
		t.Fatalf("replacement draft changed to %q", replacementPayload)
	}
}
