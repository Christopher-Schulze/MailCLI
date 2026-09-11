package mail

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mailcli/internal/transport"
)

func TestDraftRevisionCoversReviewedFields(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Draft)
	}{
		{"reference", func(d *Draft) { d.Ref += "changed" }},
		{"kind", func(d *Draft) { d.Kind = DraftKindForward }},
		{"account", func(d *Draft) { d.AccountRef += "changed" }},
		{"sender", func(d *Draft) { d.From = "Changed <sender@icloud.com>" }},
		{"source", func(d *Draft) { d.SourceRef += "changed" }},
		{"reply all", func(d *Draft) { d.ReplyAll = !d.ReplyAll }},
		{"message id", func(d *Draft) { d.SourceMessageID += "changed" }},
		{"references", func(d *Draft) { d.SourceReferences += "changed" }},
		{"subject", func(d *Draft) { d.Subject += "changed" }},
		{"body format", func(d *Draft) { d.BodyFormat = DraftBodyHTML }},
		{"body source", func(d *Draft) { d.BodySource += "changed" }},
		{"plain body", func(d *Draft) { d.Body += "changed" }},
		{"html body", func(d *Draft) { d.BodyHTML += "changed" }},
		{"to address", func(d *Draft) { d.To[0].Address = "changed@example.com" }},
		{"to name", func(d *Draft) { d.To[0].Name += "changed" }},
		{"to order", func(d *Draft) { d.To[0], d.To[1] = d.To[1], d.To[0] }},
		{"cc address", func(d *Draft) { d.CC[0].Address = "changed@example.com" }},
		{"cc name", func(d *Draft) { d.CC[0].Name += "changed" }},
		{"bcc address", func(d *Draft) { d.BCC[0].Address = "changed@example.com" }},
		{"bcc name", func(d *Draft) { d.BCC[0].Name += "changed" }},
		{"recipient roles", func(d *Draft) { d.CC, d.BCC = d.BCC, d.CC }},
		{"attachment path", func(d *Draft) { d.Attachments[0].Path += "changed" }},
		{"attachment size", func(d *Draft) { d.Attachments[0].Size++ }},
		{"attachment hash", func(d *Draft) { d.Attachments[0].SHA256 = strings.Repeat("b", 64) }},
		{"attachment order", func(d *Draft) { d.Attachments[0], d.Attachments[1] = d.Attachments[1], d.Attachments[0] }},
	} {
		t.Run(test.name, func(t *testing.T) {
			draft := Draft{
				Ref: "draft_review", Kind: DraftKindReply, AccountRef: "account", From: "sender@icloud.com",
				SourceRef: "source", SourceMessageID: "<source@example.com>", SourceReferences: "<ancestor@example.com>",
				Subject: "Subject", BodyFormat: DraftBodyMarkdown, BodySource: "**Body**", Body: "Body", BodyHTML: "<p><strong>Body</strong></p>",
				To: []Recipient{{Name: "Ada", Address: "ada@example.com"}, {Name: "Grace", Address: "grace@example.com"}},
				CC: []Recipient{{Name: "Copy", Address: "copy@example.com"}}, BCC: []Recipient{{Name: "Hidden", Address: "hidden@example.com"}},
				Attachments: []DraftAttachment{{Path: "/first.txt", Size: 1, SHA256: strings.Repeat("a", 64)}, {Path: "/second.txt", Size: 2, SHA256: strings.Repeat("c", 64)}},
			}
			if err := refreshDraftRevision(&draft); err != nil {
				t.Fatal(err)
			}
			before := draft.Revision
			test.change(&draft)
			if err := refreshDraftRevision(&draft); err != nil {
				t.Fatal(err)
			}
			if draft.Revision == before || !validStoredDraftRevision(draft.Revision) {
				t.Fatalf("review identity did not change for %s: %s", test.name, draft.Revision)
			}
		})
	}
}

func TestDraftRevisionReadIgnoresPersistedRevisionAndOperationalMetadata(t *testing.T) {
	for _, format := range []DraftBodyFormat{DraftBodyPlain, DraftBodyMarkdown, DraftBodyHTML} {
		t.Run(string(format), func(t *testing.T) {
			root := t.TempDir()
			service := NewServiceWithDraftRoot(nil, root)
			draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
				To: []Recipient{{Address: "recipient@example.com"}}, Body: "Body", BodyFormat: format,
			}})
			if err != nil || draft.Revision == "" {
				t.Fatalf("create revision = %q, error = %v", draft.Revision, err)
			}
			reviewed := draft.Revision
			draft.Revision = "untrusted persisted revision"
			draft.CreatedAt = draft.CreatedAt.Add(-time.Hour)
			draft.UpdatedAt = draft.UpdatedAt.Add(time.Hour)
			draft.CC, draft.BCC, draft.Attachments = nil, nil, nil
			if err := writeDraftFile(root, draft); err != nil {
				t.Fatal(err)
			}
			if _, err := beginSendAttempt(root, draft.Ref, "<attempt@example.com>", "fingerprint"); err != nil {
				t.Fatal(err)
			}
			loaded, err := service.GetDraft(draft.Ref)
			if err != nil || loaded.Revision != reviewed || loaded.SendAttempt == nil {
				t.Fatalf("read revision = %q, want %q, error = %v", loaded.Revision, reviewed, err)
			}
		})
	}
}

func TestReviewedDraftUpdateAndSendRejectStaleContent(t *testing.T) {
	root := t.TempDir()
	submitter, mirror := sendTransportStubs()
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	original := createTransportDraft(t, service)
	updated, err := service.UpdateDraft(UpdateDraftRequest{Ref: original.Ref, ExpectedRevision: original.Revision, Input: DraftInput{
		From: original.From, To: []Recipient{{Address: "changed@example.com"}}, Subject: "Reviewed B", Body: "Changed body",
	}})
	if err != nil || updated.Revision == original.Revision {
		t.Fatalf("update = %+v, error = %v", updated, err)
	}
	path := filepath.Join(root, original.Ref+".json")
	before := readRevisionTestFile(t, path)
	for _, expected := range []string{"", original.Revision} {
		_, updateErr := service.UpdateDraft(UpdateDraftRequest{Ref: original.Ref, ExpectedRevision: expected, Input: DraftInput{
			To: original.To, Body: "Must not overwrite B", Attachments: []string{"/missing-attachment"},
		}})
		_, sendErr := service.SendDraft(context.Background(), SendDraftRequest{Ref: original.Ref, ExpectedRevision: expected})
		for _, err := range []error{updateErr, sendErr} {
			if expected == "" {
				if errorCode(err) != "invalid_argument" {
					t.Fatalf("missing revision error = %v", err)
				}
				continue
			}
			var conflict *DraftRevisionConflict
			if !errors.As(err, &conflict) || conflict.CurrentRevision != updated.Revision || conflict.ExpectedRevision != original.Revision {
				t.Fatalf("stale revision error = %v, conflict = %+v", err, conflict)
			}
			guidance := GuidanceForError("drafts.send", err)
			if guidance.EffectCertainty != EffectNone || guidance.ReplayAllowed {
				t.Fatalf("conflict guidance = %+v", guidance)
			}
		}
		if !bytes.Equal(before, readRevisionTestFile(t, path)) || submitter.calls != 0 || mirror.calls != 0 {
			t.Fatal("rejected revision changed the draft or contacted transport")
		}
		assertNoSendClaim(t, root, original.Ref)
	}
	result, err := service.SendDraft(context.Background(), SendDraftRequest{Ref: updated.Ref, ExpectedRevision: updated.Revision})
	if err != nil || result.Receipt == nil || result.Receipt.DraftRevision != updated.Revision || submitter.calls != 1 || mirror.calls != 1 {
		t.Fatalf("reviewed send = %+v, error = %v", result, err)
	}
	if len(submitter.lastTo) != 1 || submitter.lastTo[0] != "changed@example.com" || !bytes.Contains(submitter.lastMessage, []byte("Changed body")) {
		t.Fatal("freshly reviewed send did not submit B")
	}
}

func TestStaleDraftSendPreservesExistingClaimsAndReceipts(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending claim", true: "consumed receipt"}[terminal], func(t *testing.T) {
			root := t.TempDir()
			submitter, mirror := sendTransportStubs()
			if !terminal {
				mirror.err = &transport.TransportError{Code: transport.CodeIMAPAppendFailed, Message: "known mirror failure"}
			}
			service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
			draft := createTransportDraft(t, service)
			result, err := service.SendDraft(context.Background(), SendDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision})
			if terminal && err != nil || !terminal && errorCode(err) != transport.CodeIMAPAppendFailed {
				t.Fatalf("initial send = %+v, error = %v", result, err)
			}
			if terminal {
				// Recreate the post-receipt/pre-claim-cleanup crash state from the
				// real terminal result, leaving the already consumed draft absent.
				attempt := SendAttempt{ID: result.AttemptID, DraftRevision: draft.Revision,
					StartedAt: result.Receipt.StartedAt, UpdatedAt: result.Receipt.CompletedAt,
					Outcome: SendOutcomeSent, InvocationStarted: true, AcceptedByMail: true, SentStoreObserved: true,
					Transport: &TransportEvidence{MessageID: result.Receipt.MessageID, ServerResponse: result.Receipt.ServerResponse},
				}
				if err := replaceSendAttempt(root, draft.Ref, attempt); err != nil {
					t.Fatal(err)
				}
			}
			claimPath := filepath.Join(root, draft.Ref+".send-claim")
			claim := readRevisionTestFile(t, claimPath)
			_, err = service.SendDraft(context.Background(), SendDraftRequest{Ref: draft.Ref, ExpectedRevision: "stale"})
			var conflict *DraftRevisionConflict
			if !errors.As(err, &conflict) || conflict.CurrentRevision != draft.Revision || submitter.calls != 1 || mirror.calls != 1 {
				t.Fatalf("stale send = %v, transport calls = %d/%d", err, submitter.calls, mirror.calls)
			}
			if !bytes.Equal(claim, readRevisionTestFile(t, claimPath)) {
				t.Fatal("stale send consumed or changed the retained claim")
			}
			if terminal {
				replayed, err := service.SendDraft(context.Background(), SendDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision})
				if err != nil || !replayed.Replayed || replayed.Receipt == nil {
					t.Fatalf("reviewed receipt replay = %+v, error = %v", replayed, err)
				}
				assertNoSendClaim(t, root, draft.Ref)
			}
		})
	}
}

func TestLegacyReceiptDoesNotPretendRevisionProtection(t *testing.T) {
	root := t.TempDir()
	service := NewServiceWithDraftRoot(nil, root)
	draft := createTransportDraft(t, service)
	now := time.Now().UTC()
	attempt := SendAttempt{ID: "send_legacy", StartedAt: now, UpdatedAt: now, Outcome: SendOutcomeObserved,
		InvocationStarted: true, SentStoreObserved: true, ObservedMessageRef: "observed"}
	if err := replaceSendAttempt(root, draft.Ref, attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureSendReceipt(root, draft.Ref, attempt); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, draft.Ref+".json")); err != nil {
		t.Fatal(err)
	}
	claimPath := filepath.Join(root, draft.Ref+".send-claim")
	before := readRevisionTestFile(t, claimPath)
	if _, err := service.SendDraft(context.Background(), SendDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision}); errorCode(err) != "draft_revision_unavailable" {
		t.Fatalf("legacy receipt send = %v", err)
	}
	if !bytes.Equal(before, readRevisionTestFile(t, claimPath)) {
		t.Fatal("legacy claim changed before explicit reconciliation")
	}
	result, err := service.ReconcileDraft(context.Background(), draft.Ref)
	if err != nil || result.Receipt == nil || result.Receipt.DraftRevision != "" || !result.Reconciled {
		t.Fatalf("legacy reconcile = %+v, error = %v", result, err)
	}
	assertNoSendClaim(t, root, draft.Ref)
}

type revisionBlockingObserver struct {
	ctx     context.Context
	entered chan struct{}
	release <-chan struct{}
}

func (o *revisionBlockingObserver) ContentRendered() {
	close(o.entered)
	select {
	case <-o.release:
	case <-o.ctx.Done():
	}
}

func TestReviewedSendAndUpdateMakeOneLockedDecision(t *testing.T) {
	for _, updateFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "update owns lock", false: "send owns lock"}[updateFirst], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			var workers sync.WaitGroup
			defer func() { cancel(); workers.Wait() }()
			root := t.TempDir()
			submitter, mirror := sendTransportStubs()
			mirror.err = &transport.TransportError{Code: transport.CodeIMAPAppendFailed, Message: "known mirror failure"}
			sender := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
			editor := NewServiceWithDraftRoot(nil, root)
			draft := createTransportDraft(t, sender)
			entered, release := make(chan struct{}), make(chan struct{})
			if updateFirst {
				editor.contentObserver = &revisionBlockingObserver{ctx: ctx, entered: entered, release: release}
			} else {
				submitter.submitHook = func(ctx context.Context) {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
			}
			updates, sends := make(chan error, 1), make(chan error, 1)
			update := func() {
				defer workers.Done()
				_, err := editor.UpdateDraftContext(ctx, UpdateDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision, Input: DraftInput{
					From: draft.From, To: draft.To, Body: "Updated under lease",
				}})
				updates <- err
			}
			send := func() {
				defer workers.Done()
				_, err := sender.SendDraft(ctx, SendDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision})
				sends <- err
			}
			workers.Add(1)
			if updateFirst {
				go update()
			} else {
				go send()
			}
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("first operation never reached its locked boundary")
			}
			workers.Add(1)
			if updateFirst {
				go send()
			} else {
				go update()
			}
			close(release)
			workers.Wait()
			updateErr, sendErr := <-updates, <-sends
			current, err := sender.GetDraft(draft.Ref)
			if err != nil {
				t.Fatal(err)
			}
			if updateFirst {
				if updateErr != nil || errorCode(sendErr) != "draft_revision_conflict" || current.Body != "Updated under lease" || submitter.calls != 0 || mirror.calls != 0 {
					t.Fatalf("update winner: update=%v send=%v body=%q calls=%d/%d", updateErr, sendErr, current.Body, submitter.calls, mirror.calls)
				}
				return
			}
			if errorCode(updateErr) != "send_retry_blocked" || errorCode(sendErr) != transport.CodeIMAPAppendFailed || current.Revision != draft.Revision || current.SendAttempt == nil || submitter.calls != 1 || mirror.calls != 1 {
				t.Fatalf("send winner: update=%v send=%v draft=%+v calls=%d/%d", updateErr, sendErr, current, submitter.calls, mirror.calls)
			}
		})
	}
}

func TestDraftRevisionIgnoresAttachmentMtimeAndFramesControlCharacters(t *testing.T) {
	draft := Draft{Ref: "draft_identity", Subject: "a\x00b", Body: "c", Attachments: []DraftAttachment{{Path: "/attachment.txt", Size: 1, SHA256: strings.Repeat("a", 64)}}}
	if err := refreshDraftRevision(&draft); err != nil {
		t.Fatal(err)
	}
	before := draft.Revision
	draft.Attachments[0].ModTimeNanos = 123456789
	draft.SendAttempt = &SendAttempt{ID: "operational"}
	draft.ContentDiagnostics = []ContentDiagnostic{{Code: ContentDiagnosticRemoteResource}}
	if err := refreshDraftRevision(&draft); err != nil || draft.Revision != before {
		t.Fatalf("operational metadata changed revision: %q, error=%v", draft.Revision, err)
	}
	draft.Subject, draft.Body = "a", "b\x00c"
	if err := refreshDraftRevision(&draft); err != nil || draft.Revision == before {
		t.Fatalf("field boundary collision: %q, error=%v", draft.Revision, err)
	}
}

func TestReviewedAttachmentUpdateInvalidatesSend(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "attachment.txt")
	if err := os.WriteFile(path, []byte("first bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	submitter, mirror := sendTransportStubs()
	service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
	input := DraftInput{From: "sender@icloud.com", To: []Recipient{{Address: "recipient@example.com"}}, Body: "Body", Attachments: []string{path}}
	draft, err := service.CreateDraft(CreateDraftRequest{Input: input})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("other bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	updated, err := service.UpdateDraft(UpdateDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision, Input: input})
	if err != nil || updated.Revision == draft.Revision || updated.Attachments[0].SHA256 == draft.Attachments[0].SHA256 {
		t.Fatalf("attachment update = %+v, error = %v", updated, err)
	}
	if _, err := service.SendDraft(context.Background(), SendDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision}); errorCode(err) != "draft_revision_conflict" {
		t.Fatalf("stale attachment revision send = %v", err)
	}
	if submitter.calls != 0 || mirror.calls != 0 {
		t.Fatal("stale attachment review reached transport")
	}
	assertNoSendClaim(t, root, draft.Ref)
	result, err := service.SendDraft(context.Background(), SendDraftRequest{Ref: updated.Ref, ExpectedRevision: updated.Revision})
	if err != nil || result.Receipt == nil || result.Receipt.DraftRevision != updated.Revision || submitter.calls != 1 || mirror.calls != 1 {
		t.Fatalf("reviewed attachment send = %+v, error = %v", result, err)
	}
}

func TestReviewedSendChecksEvidenceRevisionBeforeReplay(t *testing.T) {
	for _, test := range []struct {
		name     string
		receipt  bool
		legacy   bool
		wantCode string
	}{
		{"different claim", false, false, "draft_revision_conflict"},
		{"legacy claim", false, true, "draft_revision_unavailable"},
		{"different receipt with draft", true, false, "draft_revision_conflict"},
		{"legacy receipt with draft", true, true, "draft_revision_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			submitter, mirror := sendTransportStubs()
			service := newTransportService(root, submitter, mirror, &stubCredentials{password: "secret"})
			draft := createTransportDraft(t, service)
			evidenceDraft := draft
			evidenceDraft.Body += "changed"
			if err := refreshDraftRevision(&evidenceDraft); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			attempt := SendAttempt{ID: "send_reviewed", DraftRevision: evidenceDraft.Revision,
				StartedAt: now, UpdatedAt: now, Outcome: SendOutcomeObserved,
				InvocationStarted: true, SentStoreObserved: true, ObservedMessageRef: "observed"}
			if test.legacy {
				attempt.DraftRevision = ""
			}
			var err error
			suffix := ".send-claim"
			if test.receipt {
				suffix = ".send-receipt"
				_, err = ensureSendReceipt(root, draft.Ref, attempt)
			} else {
				err = replaceSendAttempt(root, draft.Ref, attempt)
			}
			if err != nil {
				t.Fatal(err)
			}
			draftPath, evidencePath := filepath.Join(root, draft.Ref+".json"), filepath.Join(root, draft.Ref+suffix)
			beforeDraft, beforeEvidence := readRevisionTestFile(t, draftPath), readRevisionTestFile(t, evidencePath)
			if _, err := service.SendDraft(context.Background(), SendDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision}); errorCode(err) != test.wantCode {
				t.Fatalf("evidence revision error = %v, want %s", err, test.wantCode)
			}
			if !bytes.Equal(beforeDraft, readRevisionTestFile(t, draftPath)) || !bytes.Equal(beforeEvidence, readRevisionTestFile(t, evidencePath)) || submitter.calls != 0 || mirror.calls != 0 {
				t.Fatal("revision rejection changed retained evidence or reached transport")
			}
		})
	}
}

func readRevisionTestFile(t *testing.T, path string) []byte {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
