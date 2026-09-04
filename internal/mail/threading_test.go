package mail

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeriveReplyInputDefaults(t *testing.T) {
	t.Parallel()
	source := ThreadSource{
		Subject:    "Project update",
		From:       "Alice <alice@example.com>",
		ReplyTo:    "Bob <bob@example.com>",
		To:         []Recipient{{Address: "bob@example.com"}},
		MessageID:  "<m1@example.com>",
		References: "<m0@example.com>",
	}
	input, messageID, references, err := DeriveReplyInput(source, DraftKindReply, false, DraftInput{Body: "x"})
	if err != nil {
		t.Fatalf("DeriveReplyInput() error = %v", err)
	}
	if input.Subject != "Re: Project update" {
		t.Fatalf("subject = %q", input.Subject)
	}
	if len(input.To) != 1 || input.To[0].Address != "bob@example.com" || input.To[0].Name != "Bob" {
		t.Fatalf("to = %+v", input.To)
	}
	if messageID != "<m1@example.com>" {
		t.Fatalf("message id = %q", messageID)
	}
	if references != "<m0@example.com> <m1@example.com>" {
		t.Fatalf("references = %q", references)
	}
}

func TestDeriveReplyInputFallsBackToFrom(t *testing.T) {
	t.Parallel()
	source := ThreadSource{Subject: "s", From: "Alice <alice@example.com>", MessageID: "<m1@example.com>"}
	input, _, _, err := DeriveReplyInput(source, DraftKindReply, false, DraftInput{Body: "x"})
	if err != nil {
		t.Fatalf("DeriveReplyInput() error = %v", err)
	}
	if len(input.To) != 1 || input.To[0].Address != "alice@example.com" || input.To[0].Name != "Alice" {
		t.Fatalf("to = %+v", input.To)
	}
}

func TestDeriveReplyInputReplyAllPromotes(t *testing.T) {
	t.Parallel()
	source := ThreadSource{
		Subject: "s", From: "Alice <alice@example.com>",
		To:        []Recipient{{Address: "bob@example.com"}, {Address: "carol@example.com"}},
		CC:        []Recipient{{Address: "dave@example.com"}},
		MessageID: "<m1@example.com>",
	}
	input, _, _, err := DeriveReplyInput(source, DraftKindReply, true, DraftInput{Body: "x"})
	if err != nil {
		t.Fatalf("DeriveReplyInput() error = %v", err)
	}
	if len(input.To) != 1 || input.To[0].Address != "alice@example.com" {
		t.Fatalf("to = %+v", input.To)
	}
	addresses := []string{}
	for _, recipient := range input.CC {
		addresses = append(addresses, recipient.Address)
	}
	// The reply target (alice, from From) is excluded; every other source
	// recipient is promoted.
	if len(addresses) != 3 || addresses[0] != "bob@example.com" ||
		addresses[1] != "carol@example.com" || addresses[2] != "dave@example.com" {
		t.Fatalf("cc = %+v (reply target must be excluded)", addresses)
	}
}

func TestDeriveReplyInputExplicitInputWins(t *testing.T) {
	t.Parallel()
	source := ThreadSource{
		Subject: "s", From: "Alice <alice@example.com>", MessageID: "<m1@example.com>",
		To: []Recipient{{Address: "carol@example.com"}},
	}
	input, _, _, err := DeriveReplyInput(source, DraftKindReply, true, DraftInput{
		Subject: "custom subject", To: []Recipient{{Address: "zoe@example.com"}},
		CC: []Recipient{{Address: "keep@example.com"}}, Body: "x",
	})
	if err != nil {
		t.Fatalf("DeriveReplyInput() error = %v", err)
	}
	if input.Subject != "custom subject" {
		t.Fatalf("subject = %q", input.Subject)
	}
	if len(input.To) != 1 || input.To[0].Address != "zoe@example.com" {
		t.Fatalf("to = %+v", input.To)
	}
	if len(input.CC) != 1 || input.CC[0].Address != "keep@example.com" {
		t.Fatalf("cc = %+v", input.CC)
	}
}

func TestDeriveReplyInputForward(t *testing.T) {
	t.Parallel()
	source := ThreadSource{Subject: "Re: s", MessageID: "<m1@example.com>"}
	input, messageID, references, err := DeriveReplyInput(source, DraftKindForward, false, DraftInput{
		To: []Recipient{{Address: "zoe@example.com"}}, Body: "x",
	})
	if err != nil {
		t.Fatalf("DeriveReplyInput() error = %v", err)
	}
	if input.Subject != "Fwd: s" {
		t.Fatalf("subject = %q", input.Subject)
	}
	if len(input.To) != 1 || input.To[0].Address != "zoe@example.com" {
		t.Fatalf("to = %+v", input.To)
	}
	if messageID != "<m1@example.com>" || references != "<m1@example.com>" {
		t.Fatalf("thread chain = %q / %q", messageID, references)
	}
}

func TestThreadSubjectStripsStackedPrefixes(t *testing.T) {
	t.Parallel()
	if got := threadSubject("Re: FWD: Re:  update ", DraftKindReply); got != "Re: update" {
		t.Fatalf("reply subject = %q", got)
	}
	if got := threadSubject("fw: update", DraftKindForward); got != "Fwd: update" {
		t.Fatalf("forward subject = %q", got)
	}
}

func TestDeriveReplyInputCapsReferencesChain(t *testing.T) {
	t.Parallel()
	var refs []string
	for index := 0; index < 25; index++ {
		refs = append(refs, fmt.Sprintf("<r%d@example.com>", index))
	}
	source := ThreadSource{Subject: "s", From: "a@example.com", References: strings.Join(refs, " "), MessageID: "<new@example.com>"}
	_, _, chain, err := DeriveReplyInput(source, DraftKindReply, false, DraftInput{Body: "x"})
	if err != nil {
		t.Fatalf("DeriveReplyInput() error = %v", err)
	}
	fields := strings.Fields(chain)
	if len(fields) != maximumThreadReferences || fields[len(fields)-1] != "<new@example.com>" || fields[0] != "<r6@example.com>" {
		t.Fatalf("chain = %q", chain)
	}
}

func TestDeriveReplyInputRejectsControlCharacters(t *testing.T) {
	t.Parallel()
	source := ThreadSource{
		Subject: "s", From: "a@example.com", MessageID: "<m1@example.com>",
		References: "<r0@example.com>\r\nBcc: victim@example.com",
	}
	if _, _, _, err := DeriveReplyInput(source, DraftKindReply, false, DraftInput{Body: "x"}); err == nil ||
		errorCode(err) != "invalid_message_source" {
		t.Fatalf("control characters error = %v", err)
	}
}

func TestDeriveReplyInputRequiresReplyTarget(t *testing.T) {
	t.Parallel()
	source := ThreadSource{Subject: "s", MessageID: "<m1@example.com>"}
	if _, _, _, err := DeriveReplyInput(source, DraftKindReply, false, DraftInput{Body: "x"}); err == nil ||
		errorCode(err) != "invalid_message_source" {
		t.Fatalf("missing reply target error = %v", err)
	}
}

type threadSourceGateway struct {
	gatewayStub
	source ThreadSource
}

func (g *threadSourceGateway) MessageThreadSource(ctx context.Context, ref string) (ThreadSource, error) {
	return g.source, nil
}

func TestThreadSourceRequiresStoreBoundProvider(t *testing.T) {
	t.Parallel()
	service := NewService(&gatewayStub{})
	if _, err := service.ThreadSource(context.Background(), "ref"); err == nil ||
		errorCode(err) != "store_bound_reference_required" {
		t.Fatalf("fallback gateway error = %v", err)
	}
	if _, err := service.ThreadSource(context.Background(), ""); err == nil {
		t.Fatal("empty ref: expected error")
	}
}

func TestThreadSourceDelegatesToProvider(t *testing.T) {
	t.Parallel()
	gateway := &threadSourceGateway{source: ThreadSource{Subject: "s", From: "a@example.com", MessageID: "<m1@example.com>"}}
	service := NewServiceWithDraftRoot(gateway, filepath.Join(t.TempDir(), "drafts"))
	source, err := service.ThreadSource(context.Background(), "ref")
	if err != nil || source.MessageID != "<m1@example.com>" {
		t.Fatalf("ThreadSource() = %+v, error = %v", source, err)
	}
}

func TestCreateDraftStoresThreadSource(t *testing.T) {
	t.Parallel()
	service := NewServiceWithDraftRoot(&gatewayStub{}, filepath.Join(t.TempDir(), "drafts"))
	draft, err := service.CreateDraft(CreateDraftRequest{
		Kind: DraftKindReply, SourceRef: storeBoundSourceRef(t),
		SourceMessageID: "<m1@example.com>", SourceReferences: "<m0@example.com> <m1@example.com>",
		Input: DraftInput{To: []Recipient{{Address: "zoe@example.com"}}, Body: "Reply"},
	})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	if draft.SourceMessageID != "<m1@example.com>" ||
		draft.SourceReferences != "<m0@example.com> <m1@example.com>" {
		t.Fatalf("thread source = %q / %q", draft.SourceMessageID, draft.SourceReferences)
	}
	if _, err := service.CreateDraft(CreateDraftRequest{
		Kind: DraftKindReply, SourceRef: storeBoundSourceRef(t),
		SourceMessageID: "<m1@example.com>\r\nX-Injected: 1",
		Input:           DraftInput{To: []Recipient{{Address: "zoe@example.com"}}, Body: "Reply"},
	}); err == nil {
		t.Fatal("control-character source message id: expected error")
	}
}
