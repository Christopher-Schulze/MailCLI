package mail

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

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

	if _, err := beginSendAttempt(root, draft.Ref, "<message@example.com>", envelopeFingerprint(updated, "<message@example.com>"), lease.storage); err != nil {
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
		Ref:   draft.Ref,
		Input: DraftInput{To: []Recipient{{Address: "recipient@example.com"}}, Subject: "updated", Body: "body 2"},
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
