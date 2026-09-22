package mail

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	return os.WriteFile(outputPath, content, 0o600)
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
