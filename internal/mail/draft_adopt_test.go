package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type adoptGateway struct {
	gatewayStub
	message     Message
	openErr     error
	openCalls   int
	attachments map[string][]byte
	saveErr     error
	saveCalls   int
	savedPaths  []string
	afterSave   func(string) error
}

func (g *adoptGateway) OpenDraft(_ context.Context, ref string) (Message, error) {
	g.openCalls++
	if g.openErr != nil {
		return Message{}, g.openErr
	}
	return g.message, nil
}

func (g *adoptGateway) SaveAttachmentTo(_ context.Context, messageRef string, attachmentID string, outputPath string) error {
	g.saveCalls++
	if g.saveErr != nil {
		return g.saveErr
	}
	content, ok := g.attachments[attachmentID]
	if !ok {
		return &OperationError{Code: "not_found", Message: "attachment is not present on this message"}
	}
	g.savedPaths = append(g.savedPaths, outputPath)
	if err := os.WriteFile(outputPath, content, 0o600); err != nil {
		return err
	}
	if g.afterSave != nil {
		return g.afterSave(outputPath)
	}
	return nil
}

func adoptService(gateway *adoptGateway) (*Service, string) {
	root, err := os.MkdirTemp("", "mailcli-adopt-*")
	if err != nil {
		panic(err)
	}
	return NewServiceWithDraftRoot(gateway, root), root
}

func storeDraftMessage() Message {
	return Message{
		Summary: MessageSummary{
			Ref:     "msg_store_draft",
			Sender:  "Author <author@example.com>",
			Subject: "Store draft subject",
		},
		To:              []Recipient{{Name: "Recipient", Address: "to@example.com"}},
		CC:              []Recipient{{Address: "cc@example.com"}},
		BCC:             []Recipient{{Address: "bcc@example.com"}},
		Content:         "store draft body\n",
		ContentComplete: true,
	}
}

func TestAdoptStoreDraftTextOnly(t *testing.T) {
	gateway := &adoptGateway{message: storeDraftMessage()}
	service, root := adoptService(gateway)
	defer func() { _ = os.RemoveAll(root) }()

	draft, err := service.AdoptStoreDraft(context.Background(), "msg_store_draft")
	if err != nil {
		t.Fatalf("AdoptStoreDraft() error = %v", err)
	}
	if !validDraftReference(draft.Ref) || draft.Kind != DraftKindNew {
		t.Fatalf("adopted draft ref/kind = %q/%q", draft.Ref, draft.Kind)
	}
	if draft.From != "Author <author@example.com>" || draft.Subject != "Store draft subject" ||
		draft.Body != "store draft body\n" || draft.BodyFormat != DraftBodyPlain {
		t.Fatalf("adopted draft fields = %+v", draft)
	}
	if len(draft.To) != 1 || draft.To[0].Address != "to@example.com" ||
		len(draft.CC) != 1 || len(draft.BCC) != 1 {
		t.Fatalf("adopted draft recipients = %+v", draft)
	}
	if len(draft.Attachments) != 0 {
		t.Fatalf("text-only adoption carried attachments: %+v", draft.Attachments)
	}
	if _, err := os.Stat(filepath.Join(root, draft.Ref+".json")); err != nil {
		t.Fatalf("adopted draft file missing: %v", err)
	}
	// The adopted draft participates in the normal local lifecycle.
	inspected, err := service.GetDraft(draft.Ref)
	if err != nil || inspected.Revision != draft.Revision || inspected.Body != draft.Body {
		t.Fatalf("GetDraft() = %+v, error = %v", inspected, err)
	}
	if gateway.openCalls != 1 || gateway.saveCalls != 0 {
		t.Fatalf("gateway calls = %d open, %d save", gateway.openCalls, gateway.saveCalls)
	}
}

func TestAdoptStoreDraftEmptyRecipients(t *testing.T) {
	message := storeDraftMessage()
	message.To, message.CC, message.BCC = nil, nil, nil
	gateway := &adoptGateway{message: message}
	service, root := adoptService(gateway)
	defer func() { _ = os.RemoveAll(root) }()

	draft, err := service.AdoptStoreDraft(context.Background(), "msg_store_draft")
	if err != nil {
		t.Fatalf("AdoptStoreDraft() with no recipients error = %v", err)
	}
	if len(draft.To)+len(draft.CC)+len(draft.BCC) != 0 {
		t.Fatalf("adopted draft recipients = %+v", draft)
	}
}

func TestAdoptStoreDraftWithAttachments(t *testing.T) {
	message := storeDraftMessage()
	message.Attachments = []Attachment{
		{ID: "att1", Name: "report.pdf", Size: 11, SizeKnown: true, Downloaded: true},
		{ID: "att2", Name: "../evil/../notes.txt", Size: 5, SizeKnown: true, Downloaded: true},
	}
	gateway := &adoptGateway{
		message: message,
		attachments: map[string][]byte{
			"att1": []byte("pdf-content"),
			"att2": []byte("notes"),
		},
	}
	service, root := adoptService(gateway)
	defer func() { _ = os.RemoveAll(root) }()

	draft, err := service.AdoptStoreDraft(context.Background(), "msg_store_draft")
	if err != nil {
		t.Fatalf("AdoptStoreDraft() error = %v", err)
	}
	if len(draft.Attachments) != 2 || gateway.saveCalls != 2 {
		t.Fatalf("adopted attachments = %+v, save calls = %d", draft.Attachments, gateway.saveCalls)
	}
	directory := filepath.Join(root, draft.Ref+".attachments")
	for index, attachment := range draft.Attachments {
		if !strings.HasPrefix(attachment.Path, directory+string(filepath.Separator)) {
			t.Fatalf("attachment path outside adopted directory: %q", attachment.Path)
		}
		content, err := os.ReadFile(attachment.Path)
		if err != nil {
			t.Fatalf("read adopted attachment: %v", err)
		}
		want := [][]byte{[]byte("pdf-content"), []byte("notes")}[index]
		if string(content) != string(want) {
			t.Fatalf("adopted attachment %d bytes = %q", index, content)
		}
		if attachment.Size != int64(len(content)) {
			t.Fatalf("adopted attachment %d size = %d", index, attachment.Size)
		}
	}
	// Traversal names are rewritten inside the draft-owned directory.
	if base := filepath.Base(draft.Attachments[1].Path); base != "1-notes.txt" {
		t.Fatalf("sanitized attachment name = %q", base)
	}
	// Discarding removes the draft-owned attachment bytes too.
	if err := service.DiscardDraftContext(context.Background(), draft.Ref); err != nil {
		t.Fatalf("DiscardDraftContext() error = %v", err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("adopted attachment directory survived discard: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, draft.Ref+".json")); !os.IsNotExist(err) {
		t.Fatalf("adopted draft file survived discard: %v", err)
	}
}

func TestAdoptStoreDraftTooManyAttachments(t *testing.T) {
	message := storeDraftMessage()
	message.Attachments = make([]Attachment, MaximumDraftAttachments+1)
	for index := range message.Attachments {
		message.Attachments[index] = Attachment{ID: "a", Name: "f"}
	}
	gateway := &adoptGateway{message: message}
	service, root := adoptService(gateway)
	defer func() { _ = os.RemoveAll(root) }()

	if _, err := service.AdoptStoreDraft(context.Background(), "msg_store_draft"); err == nil {
		t.Fatal("AdoptStoreDraft() over the attachment limit error = nil")
	}
	if gateway.saveCalls != 0 {
		t.Fatalf("attachment bytes written despite limit: %d saves", gateway.saveCalls)
	}
}

func TestAdoptStoreDraftOversizedAttachments(t *testing.T) {
	message := storeDraftMessage()
	message.Attachments = []Attachment{
		{ID: "big", Name: "big.bin", Size: MaximumDraftAttachmentBytes + 1, SizeKnown: true},
	}
	gateway := &adoptGateway{message: message}
	service, root := adoptService(gateway)
	defer func() { _ = os.RemoveAll(root) }()

	if _, err := service.AdoptStoreDraft(context.Background(), "msg_store_draft"); err == nil {
		t.Fatal("AdoptStoreDraft() oversized attachment error = nil")
	}
	if gateway.saveCalls != 0 {
		t.Fatalf("oversized attachment written: %d saves", gateway.saveCalls)
	}
}

func TestAdoptStoreDraftOversizedBody(t *testing.T) {
	message := storeDraftMessage()
	message.Content = strings.Repeat("x", MaximumDraftBodyBytes+1)
	gateway := &adoptGateway{message: message}
	service, root := adoptService(gateway)
	defer func() { _ = os.RemoveAll(root) }()

	if _, err := service.AdoptStoreDraft(context.Background(), "msg_store_draft"); err == nil {
		t.Fatal("AdoptStoreDraft() oversized body error = nil")
	}
}

func TestAdoptStoreDraftMissing(t *testing.T) {
	gateway := &adoptGateway{openErr: &OperationError{Code: "not_found", Message: "message not found"}}
	service, root := adoptService(gateway)
	defer func() { _ = os.RemoveAll(root) }()

	_, err := service.AdoptStoreDraft(context.Background(), "msg_missing")
	if err == nil {
		t.Fatal("AdoptStoreDraft() missing draft error = nil")
	}
	var operation *OperationError
	if !errors.As(err, &operation) || operation.Code != "not_found" {
		t.Fatalf("AdoptStoreDraft() missing draft error = %v", err)
	}
}

func TestAdoptStoreDraftIncompleteSource(t *testing.T) {
	message := storeDraftMessage()
	message.ContentComplete = false
	gateway := &adoptGateway{message: message}
	service, root := adoptService(gateway)
	defer func() { _ = os.RemoveAll(root) }()

	_, err := service.AdoptStoreDraft(context.Background(), "msg_store_draft")
	var operation *OperationError
	if !errors.As(err, &operation) || operation.Code != "adopt_source_incomplete" {
		t.Fatalf("AdoptStoreDraft() incomplete source error = %v", err)
	}
}

func TestAdoptStoreDraftSaveFailureCleansDirectory(t *testing.T) {
	message := storeDraftMessage()
	message.Attachments = []Attachment{{ID: "att1", Name: "a.bin", SizeKnown: true, Size: 1}}
	gateway := &adoptGateway{
		message:     message,
		attachments: map[string][]byte{"att1": []byte("x")},
		saveErr:     &OperationError{Code: "attachment_not_downloaded", Message: "not downloaded"},
	}
	service, root := adoptService(gateway)
	defer func() { _ = os.RemoveAll(root) }()

	if _, err := service.AdoptStoreDraft(context.Background(), "msg_store_draft"); err == nil {
		t.Fatal("AdoptStoreDraft() save failure error = nil")
	}
	matches, err := filepath.Glob(filepath.Join(root, "*.attachments"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("adopted attachment directory leaked after failure: %v %v", matches, err)
	}
}

func TestAdoptStoreDraftEmptyRef(t *testing.T) {
	gateway := &adoptGateway{message: storeDraftMessage()}
	service, root := adoptService(gateway)
	defer func() { _ = os.RemoveAll(root) }()
	if _, err := service.AdoptStoreDraft(context.Background(), "  "); err == nil {
		t.Fatal("AdoptStoreDraft() empty ref error = nil")
	}
	if gateway.openCalls != 0 {
		t.Fatalf("empty ref reached the gateway: %d calls", gateway.openCalls)
	}
}

func TestSweepOrphanAdoptedAttachments(t *testing.T) {
	message := storeDraftMessage()
	message.Attachments = []Attachment{{ID: "att1", Name: "a.bin", SizeKnown: true, Size: 3}}
	gateway := &adoptGateway{
		message:     message,
		attachments: map[string][]byte{"att1": []byte("xyz")},
	}
	service, root := adoptService(gateway)
	defer func() { _ = os.RemoveAll(root) }()

	draft, err := service.AdoptStoreDraft(context.Background(), "msg_store_draft")
	if err != nil {
		t.Fatalf("AdoptStoreDraft() error = %v", err)
	}
	directory := filepath.Join(root, draft.Ref+".attachments")
	if err := os.Remove(filepath.Join(root, draft.Ref+".json")); err != nil {
		t.Fatalf("remove draft file: %v", err)
	}
	if _, err := os.Stat(directory); err != nil {
		t.Fatalf("adopted attachment directory missing before sweep: %v", err)
	}
	swept, failures, err := sweepOrphanDraftArtifacts(context.Background(), root)
	if err != nil || len(failures) != 0 {
		t.Fatalf("sweepOrphanDraftArtifacts() = %v %v %v", swept, failures, err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("orphan adopted attachment directory survived sweep: %v", err)
	}
}

func TestAdoptStoreDraftDownloadOutsidePruneRootWithoutLease(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	message := storeDraftMessage()
	message.Attachments = []Attachment{{ID: "a", Name: "a.bin", SizeKnown: true, Size: 3}}
	ready, resume := make(chan string, 1), make(chan struct{})
	gateway := &adoptGateway{message: message, attachments: map[string][]byte{"a": []byte("xyz")}, afterSave: func(path string) error {
		ready <- path
		<-resume
		return nil
	}}
	service := NewServiceWithDraftRoot(gateway, root)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type outcome struct {
		draft Draft
		err   error
	}
	finished := make(chan outcome, 1)
	go func() { draft, err := service.AdoptStoreDraft(ctx, "msg_store_draft"); finished <- outcome{draft, err} }()
	joined := false
	defer func() {
		close(resume)
		if !joined {
			select {
			case <-finished:
			case <-ctx.Done():
				t.Error("adoption did not finish")
			}
		}
	}()
	var path string
	select {
	case path = <-ready:
	case result := <-finished:
		joined = true
		t.Fatalf("download failed: %v", result.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if filepath.Dir(filepath.Dir(path)) != filepath.Dir(root) || strings.HasPrefix(path, root+string(filepath.Separator)) {
		t.Fatalf("download is exposed to prune: %s", path)
	}
	ref := strings.TrimPrefix(filepath.Base(filepath.Dir(path)), ".mailcli-adopt-")
	probeCtx, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	lease, err := acquireDraftLease(probeCtx, root, ref)
	stop()
	if err != nil {
		t.Fatalf("download held ref lease: %v", err)
	}
	if err := lease.release(); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: 24 * time.Hour, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(identity, current) {
		t.Fatalf("prune changed download: %v", err)
	}
	resume <- struct{}{}
	select {
	case result := <-finished:
		joined = true
		if result.err != nil {
			t.Fatal(result.err)
		}
		assertAdoptionPublishedAttachments(t, service, result.draft)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

type adoptionPublicationFixture struct {
	stage    *adoptionStaging
	root     string
	draft    Draft
	finished bool
}

func newAdoptionPublicationFixture(t *testing.T) *adoptionPublicationFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "drafts")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := newDraftReference()
	if err != nil {
		t.Fatal(err)
	}
	stage, err := newAdoptionStaging(root, ref, identity)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &adoptionPublicationFixture{stage: stage, root: root}
	t.Cleanup(func() {
		if !fixture.finished {
			if err := stage.close(); err != nil {
				t.Error(err)
			}
		}
	})
	path := filepath.Join(stage.path, "0-a.bin")
	if err := os.WriteFile(path, []byte("verified bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stage.files = append(stage.files, adoptionStagedFile{name: "0-a.bin", identity: file})
	fixture.draft, err = prepareDraftWithAttachmentsObserverContext(context.Background(), CreateDraftRequest{
		Kind: DraftKindNew, preassignedRef: ref, allowEmptyRecipients: true,
		Input: DraftInput{From: "author@example.com", Body: "body", BodyFormat: DraftBodyPlain, Attachments: []string{path}},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *adoptionPublicationFixture) acquire(t *testing.T, attachments bool) *draftLease {
	t.Helper()
	lease, err := acquireDraftLease(context.Background(), f.root, f.draft.Ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.release(); err != nil {
			t.Error(err)
		}
	})
	if attachments {
		if err := lease.storage.root.Mkdir(adoptedAttachmentDirName(f.draft.Ref), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return lease
}

func (f *adoptionPublicationFixture) finish(err error, started bool) error {
	f.finished = true
	return finishDraftAdoption(f.stage, f.draft.Ref, started, err)
}

func assertAdoptionPublishedAttachments(t *testing.T, service *Service, expected Draft) {
	t.Helper()
	loaded, err := service.GetDraft(expected.Ref)
	if err != nil || loaded.Revision != expected.Revision || len(loaded.Attachments) != len(expected.Attachments) {
		t.Fatalf("persisted draft=%+v, %v", loaded, err)
	}
	for _, attachment := range loaded.Attachments {
		payload, err := os.ReadFile(attachment.Path)
		digest := sha256.Sum256(payload)
		if err != nil || int64(len(payload)) != attachment.Size || hex.EncodeToString(digest[:]) != attachment.SHA256 {
			t.Fatalf("published attachment=%+v, %v", attachment, err)
		}
		if !strings.HasPrefix(attachment.Path, filepath.Join(service.draftRoot, adoptedAttachmentDirName(expected.Ref))+string(filepath.Separator)) {
			t.Fatalf("non-final path: %s", attachment.Path)
		}
	}
}

func TestAdoptStoreDraftFingerprintedStageSurvivesPrune(t *testing.T) {
	f := newAdoptionPublicationFixture(t)
	service := NewServiceWithDraftRoot(nil, f.root)
	if _, err := service.PruneDrafts(PruneDraftsRequest{OlderThan: 24 * time.Hour, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	lease := f.acquire(t, true)
	if err := publishAdoptionAttachments(context.Background(), f.stage, lease.storage, &f.draft); err != nil {
		t.Fatal(err)
	}
	if err := refreshDraftRevision(&f.draft); err != nil {
		t.Fatal(err)
	}
	if err := writeDraftFileContext(context.Background(), f.root, f.draft, lease.storage); err != nil {
		t.Fatal(err)
	}
	if err := f.finish(nil, true); err != nil {
		t.Fatal(err)
	}
	assertAdoptionPublishedAttachments(t, service, f.draft)
	if _, err := os.Lstat(f.stage.path); !os.IsNotExist(err) {
		t.Fatalf("owned staging survived success: %v", err)
	}
}

type adoptionIOBoundaryContext struct {
	context.Context
	action func()
	called bool
}

func (c *adoptionIOBoundaryContext) Err() error {
	if !c.called {
		c.called = true
		c.action()
	}
	return c.Context.Err()
}

func TestAdoptStoreDraftPublicationRefusesReplacedObjects(t *testing.T) {
	for _, name := range []string{"staging directory", "staging file", "final directory", "final file"} {
		t.Run(name, func(t *testing.T) {
			f := newAdoptionPublicationFixture(t)
			lease := f.acquire(t, true)
			path, directory := f.stage.path, true
			started := strings.HasPrefix(name, "final")
			if started {
				path = filepath.Join(f.root, adoptedAttachmentDirName(f.draft.Ref))
			}
			if strings.HasSuffix(name, "file") {
				path = filepath.Join(path, "0-a.bin")
				directory = false
			}
			var foreignIdentity os.FileInfo
			foreignPath := path
			replace := func() {
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if directory {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
					foreignPath = filepath.Join(path, "foreign")
				}
				if err := os.WriteFile(foreignPath, []byte("foreign bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
				var err error
				foreignIdentity, err = os.Lstat(foreignPath)
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			if started {
				ctx = &adoptionIOBoundaryContext{Context: ctx, action: replace}
			} else {
				replace()
			}
			err := publishAdoptionAttachments(ctx, f.stage, lease.storage, &f.draft)
			if err == nil {
				t.Fatal("replaced publication succeeded")
			}
			err = f.finish(err, started)
			var failure *DraftAdoptionError
			if !errors.As(err, &failure) || !failure.StagingRetained || failure.PublicationStarted != started || failure.Ref != f.draft.Ref {
				t.Fatalf("replacement diagnosis: %v", err)
			}
			payload, readErr := os.ReadFile(foreignPath)
			current, statErr := os.Lstat(foreignPath)
			if readErr != nil || statErr != nil || string(payload) != "foreign bytes" || !os.SameFile(foreignIdentity, current) {
				t.Fatalf("foreign object changed: %q, %v, %v", payload, readErr, statErr)
			}
			ownedPath := path + ".saved"
			if directory {
				ownedPath = filepath.Join(ownedPath, "0-a.bin")
			}
			if payload, err := os.ReadFile(ownedPath); err != nil || string(payload) != "verified bytes" {
				t.Fatalf("recoverable bytes lost: %q, %v", payload, err)
			}
		})
	}
}

func TestAdoptStoreDraftFailuresPreservePublicationBoundaries(t *testing.T) {
	for _, name := range []string{"before attachments", "after attachments", "before JSON", "after JSON rename"} {
		t.Run(name, func(t *testing.T) {
			f := newAdoptionPublicationFixture(t)
			started := name != "before attachments"
			lease := f.acquire(t, started)
			if started {
				if err := publishAdoptionAttachments(context.Background(), f.stage, lease.storage, &f.draft); err != nil {
					t.Fatal(err)
				}
				if err := refreshDraftRevision(&f.draft); err != nil {
					t.Fatal(err)
				}
			}
			failure := errors.New("interrupted publication")
			switch name {
			case "before JSON":
				if err := os.Mkdir(filepath.Join(f.root, f.draft.Ref+".json"), 0o700); err != nil {
					t.Fatal(err)
				}
				failure = writeDraftFileContext(context.Background(), f.root, f.draft, lease.storage)
			case "after JSON rename":
				if err := writeDraftFileContext(context.Background(), f.root, f.draft, lease.storage); err != nil {
					t.Fatal(err)
				}
				// The JSON exists before a real failing directory sync. This tests
				// recovery after publication without an injected production hook.
				directory, err := os.Open(f.root)
				if err != nil {
					t.Fatal(err)
				}
				if err := directory.Close(); err != nil {
					t.Fatal(err)
				}
				state := &draftStorage{root: lease.storage.root, rootName: f.root, directory: directory}
				failure = state.apply(draftStorageSync, "", "", 0)
				if !errors.Is(failure, os.ErrClosed) {
					t.Fatalf("directory sync did not fail: %v", failure)
				}
			}
			if failure == nil {
				t.Fatal("failure boundary succeeded")
			}
			err := f.finish(failure, started)
			var outcome *DraftAdoptionError
			if !errors.As(err, &outcome) || outcome.Ref != f.draft.Ref || outcome.PublicationStarted != started || outcome.StagingRetained != started {
				t.Fatalf("boundary diagnosis: %v", err)
			}
			if started {
				if payload, err := os.ReadFile(f.draft.Attachments[0].Path); err != nil || string(payload) != "verified bytes" {
					t.Fatalf("published attachment rolled back: %q, %v", payload, err)
				}
				if _, err := os.Stat(f.stage.path); err != nil {
					t.Fatalf("recoverable staging lost: %v", err)
				}
			} else if _, err := os.Lstat(f.stage.path); !os.IsNotExist(err) {
				t.Fatalf("verified pre-publication cleanup failed: %v", err)
			}
			if name == "after JSON rename" {
				assertAdoptionPublishedAttachments(t, NewServiceWithDraftRoot(nil, f.root), f.draft)
			}
		})
	}
}

func TestAdoptStoreDraftCleanupPreservesUnverifiedEntries(t *testing.T) {
	f := newAdoptionPublicationFixture(t)
	path := filepath.Join(f.stage.path, "foreign")
	if err := os.WriteFile(path, []byte("foreign bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	cause := &OperationError{Code: "attachment_not_downloaded", Message: "download interrupted"}
	err = f.finish(cause, false)
	var outcome *DraftAdoptionError
	if !errors.As(err, &outcome) || outcome.PublicationStarted || !outcome.StagingRetained || !errors.Is(err, cause) || outcome.ErrorCode() != cause.Code {
		t.Fatalf("cleanup diagnosis: %v", err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("foreign inode removed: %v", err)
	}
	for _, name := range []string{"foreign", "0-a.bin"} {
		if _, err := os.ReadFile(filepath.Join(f.stage.path, name)); err != nil {
			t.Fatalf("recovery file %s lost: %v", name, err)
		}
	}
}

func TestAdoptStoreDraftExistingFinalDirectoryIsPreserved(t *testing.T) {
	root := filepath.Join(t.TempDir(), "drafts")
	message := storeDraftMessage()
	message.Attachments = []Attachment{{ID: "a", Name: "a.bin", SizeKnown: true, Size: 3}}
	var foreignPath string
	var foreignIdentity os.FileInfo
	gateway := &adoptGateway{message: message, attachments: map[string][]byte{"a": []byte("xyz")}, afterSave: func(path string) error {
		ref := strings.TrimPrefix(filepath.Base(filepath.Dir(path)), ".mailcli-adopt-")
		directory := filepath.Join(root, adoptedAttachmentDirName(ref))
		if err := os.Mkdir(directory, 0o700); err != nil {
			return err
		}
		foreignPath = filepath.Join(directory, "foreign")
		if err := os.WriteFile(foreignPath, []byte("foreign bytes"), 0o600); err != nil {
			return err
		}
		var err error
		foreignIdentity, err = os.Lstat(foreignPath)
		return err
	}}
	service := NewServiceWithDraftRoot(gateway, root)
	_, err := service.AdoptStoreDraft(context.Background(), "msg_store_draft")
	var outcome *DraftAdoptionError
	if !errors.As(err, &outcome) || outcome.PublicationStarted || outcome.StagingRetained || outcome.Ref == "" {
		t.Fatalf("exclusive destination diagnosis: %v", err)
	}
	current, statErr := os.Lstat(foreignPath)
	payload, readErr := os.ReadFile(foreignPath)
	if statErr != nil || readErr != nil || !os.SameFile(foreignIdentity, current) || string(payload) != "foreign bytes" {
		t.Fatalf("foreign destination changed: %q, %v, %v", payload, statErr, readErr)
	}
	if _, err := os.Lstat(filepath.Join(root, outcome.Ref+".json")); !os.IsNotExist(err) {
		t.Fatalf("failed adoption published JSON: %v", err)
	}
	if _, err := os.Lstat(outcome.StagingPath); !os.IsNotExist(err) {
		t.Fatalf("verified staging leaked: %v", err)
	}
}
