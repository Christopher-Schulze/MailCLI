package mail

import (
	"context"
	"fmt"
	stdmail "net/mail"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/mailref"
)

func TestInferDerivedSenderUsesTheAccountThatHoldsTheSourceMessage(t *testing.T) {
	t.Parallel()
	accountRef := func(id string) string {
		ref, err := mailref.EncodeAccount(id)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	messageRef := func(id string) string {
		ref, err := mailref.EncodeMessage(mailref.Message{
			AccountID: id, MailboxPath: []string{"INBOX"}, LibraryID: "1", ExpectedMessageID: "m",
		})
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	gateway := &gatewayStub{accounts: []Account{
		{Ref: accountRef("ACC-GMAIL"), State: "ok", EmailAddresses: []string{"me@gmail.com"},
			ConfiguredSenderAliases: []string{"alias@example.com"}},
		{Ref: accountRef("ACC-ICLOUD"), State: "ok", EmailAddresses: []string{"me@icloud.com"}},
		{Ref: accountRef("ACC-MANY"), State: "ok", EmailAddresses: []string{"a@many.example", "b@many.example"}},
		{Ref: accountRef("ACC-DEGRADED"), State: "degraded", EmailAddresses: []string{"x@degraded.example"}},
	}}
	service := NewService(gateway)
	for _, test := range []struct {
		name        string
		account     string
		source      ThreadSource
		wantFrom    string
		wantAccount string
	}{
		{"addressee alias wins", "ACC-GMAIL", ThreadSource{To: []Recipient{{Address: "other@example.com"}, {Address: "alias@Example.com"}}}, "alias@example.com", accountRef("ACC-GMAIL")},
		{"cc counts as addressee", "ACC-MANY", ThreadSource{To: []Recipient{{Address: "x@elsewhere.example"}}, CC: []Recipient{{Address: "b@many.example"}}}, "b@many.example", accountRef("ACC-MANY")},
		{"single address is the fallback", "ACC-ICLOUD", ThreadSource{To: []Recipient{{Address: "list@example.com"}}}, "me@icloud.com", accountRef("ACC-ICLOUD")},
		{"several addresses without a match stay open", "ACC-MANY", ThreadSource{To: []Recipient{{Address: "list@example.com"}}}, "", ""},
		{"degraded account stays open", "ACC-DEGRADED", ThreadSource{To: []Recipient{{Address: "x@degraded.example"}}}, "", ""},
		{"unknown account stays open", "ACC-NONE", ThreadSource{To: []Recipient{{Address: "me@gmail.com"}}}, "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			from, account, _ := service.InferDerivedSender(context.Background(), messageRef(test.account), test.source)
			if from != test.wantFrom || account != test.wantAccount {
				t.Fatalf("InferDerivedSender() = %q, %q; want %q, %q", from, account, test.wantFrom, test.wantAccount)
			}
		})
	}
	if from, account, own := service.InferDerivedSender(context.Background(), "msg_not-a-ref", ThreadSource{}); from != "" || account != "" || len(own) != 0 {
		t.Fatalf("invalid ref inferred %q, %q", from, account)
	}
}

func TestSendBlockersNameWhatStopsASend(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		draft Draft
		want  []string
	}{
		{"no content blocker", Draft{From: "me@example.com", To: []Recipient{{Address: "you@example.com"}}}, []string{}},
		{"no sender", Draft{To: []Recipient{{Address: "you@example.com"}}}, []string{"from_missing"}},
		{"blank sender and no recipients", Draft{From: "  "}, []string{"from_missing", "recipients_missing"}},
		{"bcc counts as a recipient", Draft{From: "me@example.com", BCC: []Recipient{{Address: "you@example.com"}}}, []string{}},
	} {
		got := SendBlockers(test.draft)
		if got == nil || !reflect.DeepEqual(got, test.want) {
			t.Errorf("%s: SendBlockers() = %#v, want %#v", test.name, got, test.want)
		}
	}
}

func TestDerivedSenderIdentitiesRequireOneDistinctMatch(t *testing.T) {
	accountRef, err := mailref.EncodeAccount("SOURCE")
	if err != nil {
		t.Fatal(err)
	}
	messageRef, err := mailref.EncodeMessage(mailref.Message{AccountID: "SOURCE", MailboxPath: []string{"INBOX"}, LibraryID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, state, wantFrom string
		source                ThreadSource
		wantOwn               []string
	}{
		{"two own To", "ok", "", ThreadSource{To: []Recipient{{Address: "Me@example.com"}, {Address: "alias@example.com"}}}, []string{"Me@example.com", "alias@example.com", `"A B"@example.com`}},
		{"reversed own To", "ok", "", ThreadSource{To: []Recipient{{Address: "alias@example.com"}, {Address: "Me@example.com"}}}, []string{"Me@example.com", "alias@example.com", `"A B"@example.com`}},
		{"own To and CC", "ok", "", ThreadSource{To: []Recipient{{Address: "Me@example.com"}}, CC: []Recipient{{Address: "alias@example.com"}}}, []string{"Me@example.com", "alias@example.com", `"A B"@example.com`}},
		{"duplicate domain casing", "ok", "alias@example.com", ThreadSource{To: []Recipient{{Address: "alias@EXAMPLE.COM"}}, CC: []Recipient{{Address: "alias@example.com"}}}, []string{"Me@example.com", "alias@example.com", `"A B"@example.com`}},
		{"local case is distinct", "ok", "", ThreadSource{To: []Recipient{{Address: "me@example.com"}}}, []string{"Me@example.com", "alias@example.com", `"A B"@example.com`}},
		{"quoted discovered sender", "ok", `"A B"@example.com`, ThreadSource{CC: []Recipient{{Address: `"A B"@EXAMPLE.COM`}}}, []string{"Me@example.com", "alias@example.com", `"A B"@example.com`}},
		{"malformed recipient", "ok", "", ThreadSource{To: []Recipient{{Address: "alias@example.com"}, {Address: "broken"}}}, []string{"Me@example.com", "alias@example.com", `"A B"@example.com`}},
		{"parser loss", "ok", "", ThreadSource{CC: []Recipient{{Address: "alias@example.com"}}, RecipientParseError: fmt.Errorf("malformed To")}, []string{"Me@example.com", "alias@example.com", `"A B"@example.com`}},
		{"degraded", "degraded", "", ThreadSource{}, nil},
		{"disabled", "disabled", "", ThreadSource{}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := NewService(&gatewayStub{accounts: []Account{{Ref: accountRef, State: test.state,
				EmailAddresses: []string{"Me@EXAMPLE.COM", "invalid"}, ConfiguredSenderAliases: []string{"alias@example.com", "alias@EXAMPLE.COM"},
				DiscoveredSenderIdentities: []string{`"A B"@example.com`}}}})
			from, account, own := service.InferDerivedSender(context.Background(), messageRef, test.source)
			wantAccount := ""
			if test.wantFrom != "" {
				wantAccount = accountRef
			}
			if from != test.wantFrom || account != wantAccount || !reflect.DeepEqual(own, test.wantOwn) {
				t.Fatalf("sender = %q/%q, own = %#v; want %q/%q, %#v", from, account, own, test.wantFrom, wantAccount, test.wantOwn)
			}
		})
	}
}

func TestReplyAllOwnIdentitiesAndRecipientCase(t *testing.T) {
	source := ThreadSource{From: "target@example.com", MessageID: "<source@example.com>",
		To: []Recipient{{Address: "Me@EXAMPLE.COM"}, {Address: "User@EXAMPLE.COM"}, {Address: "user@example.com"}},
		CC: []Recipient{{Address: "alias@example.com"}, {Address: "User@example.com"}, {Address: `"A B"@example.com`}}}
	own := []string{"Me@example.com", "alias@example.com", `"A B"@example.com`}
	for _, test := range []struct {
		name   string
		input  DraftInput
		own    []string
		wantCC []Recipient
	}{
		{"automatic", DraftInput{Body: "Reply"}, own, []Recipient{{Address: "User@EXAMPLE.COM"}, {Address: "user@example.com"}}},
		{"no verified identities", DraftInput{Body: "Reply"}, nil, []Recipient{{Address: "Me@EXAMPLE.COM"}, {Address: "User@EXAMPLE.COM"}, {Address: "user@example.com"}, {Address: "alias@example.com"}, {Address: `"A B"@example.com`}}},
		{"explicit own roles", DraftInput{Body: "Reply", ToSet: true, To: []Recipient{{Address: "Me@example.com"}}, CCSet: true, CC: []Recipient{{Address: "alias@example.com"}}, BCC: []Recipient{{Address: `"A B"@example.com`}}}, own, []Recipient{{Address: "alias@example.com"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input, id, refs, err := DeriveReplyInput(source, DraftKindReply, true, test.input, test.own)
			if err != nil || !reflect.DeepEqual(input.CC, test.wantCC) || !reflect.DeepEqual(input.BCC, test.input.BCC) {
				t.Fatalf("input = %+v, error = %v", input, err)
			}
			if test.input.ToSet && !reflect.DeepEqual(input.To, test.input.To) {
				t.Fatalf("explicit To changed: %+v", input.To)
			}
			service := NewServiceWithDraftRoot(&gatewayStub{}, t.TempDir())
			draft, err := service.CreateDraft(CreateDraftRequest{Kind: DraftKindReply, SourceRef: storeBoundSourceRef(t), ReplyAll: true, Input: input, SourceMessageID: id, SourceReferences: refs})
			if err != nil {
				t.Fatal(err)
			}
			payload, err := BuildMessage(draft, "<reply@example.com>")
			if err != nil {
				t.Fatal(err)
			}
			message := readComposedHeaderTestMessage(t, payload)
			cc, err := message.Header.AddressList("Cc")
			if err != nil || len(cc) != len(test.wantCC) {
				t.Fatalf("composed CC = %+v, error = %v", cc, err)
			}
			for index, recipient := range test.wantCC {
				parsed, err := stdmail.ParseAddress(recipient.Address)
				if err != nil || cc[index].Address != parsed.Address {
					t.Fatalf("composed CC[%d] = %+v, expected %+v, error = %v", index, cc[index], parsed, err)
				}
			}
		})
	}
}

func TestThreadSubjectLocalizedPrefixesPreserveOriginalOffsets(t *testing.T) {
	for _, test := range []struct{ source, want string }{
		{"AW[2]: Wg: Mixed CASE", "Mixed CASE"},
		{"[Kİ List] Re[12]: Sv: Vs: Tr: Odp: Res: Rv: Update", "[Kİ List] Update"},
		{"Re: [K Team] AW: FWD: Update", "[K Team] Update"},
		{"[First] Re: [Second] AW: Update", "[First] [Second] Update"},
		{"[List] Unprefixed CASE", "[List] Unprefixed CASE"},
		{"Awareness: Keep", "Awareness: Keep"},
	} {
		for _, kind := range []DraftKind{DraftKindReply, DraftKindForward} {
			t.Run(string(kind)+"/"+test.source, func(t *testing.T) {
				prefix := "Re: "
				if kind == DraftKindForward {
					prefix = "Fwd: "
				}
				if got := threadSubject(test.source, kind); got != prefix+test.want {
					t.Fatalf("subject = %q, want %q", got, prefix+test.want)
				}
			})
		}
	}
}

func TestDeriveReplyInputDefaults(t *testing.T) {
	t.Parallel()
	source := ThreadSource{
		Subject:    "Project update",
		From:       "Alice <alice@example.com>",
		ReplyTo:    []Recipient{{Name: "Bob", Address: "bob@example.com"}},
		To:         []Recipient{{Address: "bob@example.com"}},
		MessageID:  "<m1@example.com>",
		References: "<m0@example.com>",
	}
	input, messageID, references, err := DeriveReplyInput(source, DraftKindReply, false, DraftInput{Body: "x"}, nil)
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
	input, _, _, err := DeriveReplyInput(source, DraftKindReply, false, DraftInput{Body: "x"}, nil)
	if err != nil {
		t.Fatalf("DeriveReplyInput() error = %v", err)
	}
	if len(input.To) != 1 || input.To[0].Address != "alice@example.com" || input.To[0].Name != "Alice" {
		t.Fatalf("to = %+v", input.To)
	}
}

func TestDeriveReplyInputUsesCompleteReplyToList(t *testing.T) {
	t.Parallel()
	source := ThreadSource{
		Subject: "s", From: "Alice <alice@example.com>",
		ReplyTo: []Recipient{
			{Name: "Bob", Address: "bob@example.com"},
			{Name: "Carol", Address: "carol@example.com"},
		},
		MessageID: "<m1@example.com>",
	}
	input, _, _, err := DeriveReplyInput(source, DraftKindReply, false, DraftInput{Body: "x"}, nil)
	if err != nil {
		t.Fatalf("DeriveReplyInput() error = %v", err)
	}
	if len(input.To) != 2 || input.To[0].Address != "bob@example.com" || input.To[1].Address != "carol@example.com" {
		t.Fatalf("to = %+v, want complete Reply-To list", input.To)
	}
	service := NewServiceWithDraftRoot(&gatewayStub{}, filepath.Join(t.TempDir(), "drafts"))
	draft, err := service.CreateDraft(CreateDraftRequest{
		Kind: DraftKindReply, SourceRef: storeBoundSourceRef(t), Input: input,
		SourceMessageID: "<m1@example.com>", SourceReferences: "<m1@example.com>",
	})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	payload, err := BuildMessage(draft, "<reply@example.com>")
	if err != nil {
		t.Fatalf("BuildMessage() error = %v", err)
	}
	message, err := stdmail.ReadMessage(strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}
	addresses, err := message.Header.AddressList("To")
	if err != nil || len(addresses) != 2 || addresses[0].Address != "bob@example.com" || addresses[1].Address != "carol@example.com" {
		t.Fatalf("composed To = %+v, error = %v", addresses, err)
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
	input, _, _, err := DeriveReplyInput(source, DraftKindReply, true, DraftInput{Body: "x"}, nil)
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
	}, nil)
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

func TestDeriveReplyInputPreservesExplicitEmptyFields(t *testing.T) {
	t.Parallel()
	source := ThreadSource{
		Subject: "s", From: "Alice <alice@example.com>",
		To: []Recipient{{Address: "bob@example.com"}}, MessageID: "<m1@example.com>",
	}
	input, _, _, err := DeriveReplyInput(source, DraftKindReply, true, DraftInput{
		SubjectSet: true, CCSet: true, Subject: "", CC: []Recipient{}, Body: "x",
	}, nil)
	if err != nil {
		t.Fatalf("DeriveReplyInput() error = %v", err)
	}
	if input.Subject != "" {
		t.Fatalf("subject = %q, want explicit empty subject", input.Subject)
	}
	if input.CC == nil || len(input.CC) != 0 {
		t.Fatalf("cc = %#v, want explicit empty CC", input.CC)
	}
}

func TestDeriveReplyInputReplyAllDeduplicatesExplicitTo(t *testing.T) {
	t.Parallel()
	source := ThreadSource{
		Subject: "s", From: "Alice <alice@example.com>",
		To: []Recipient{
			{Name: "Bob", Address: "bob@example.com"},
			{Address: "carol@example.com"},
		},
		CC: []Recipient{{Address: "dave@example.com"}}, MessageID: "<m1@example.com>",
	}
	input, _, _, err := DeriveReplyInput(source, DraftKindReply, true, DraftInput{
		ToSet: true, To: []Recipient{{Name: "Bob", Address: "bob@EXAMPLE.COM"}}, Body: "x",
	}, nil)
	if err != nil {
		t.Fatalf("DeriveReplyInput() error = %v", err)
	}
	if len(input.To) != 1 || input.To[0].Address != "bob@EXAMPLE.COM" {
		t.Fatalf("to = %+v", input.To)
	}
	if len(input.CC) != 2 || input.CC[0].Address != "carol@example.com" || input.CC[1].Address != "dave@example.com" {
		t.Fatalf("cc = %+v, want source recipients except explicit To", input.CC)
	}
}

func TestDeriveReplyInputForward(t *testing.T) {
	t.Parallel()
	source := ThreadSource{Subject: "Re: s", MessageID: "<m1@example.com>"}
	input, messageID, references, err := DeriveReplyInput(source, DraftKindForward, false, DraftInput{
		To: []Recipient{{Address: "zoe@example.com"}}, Body: "x",
	}, nil)
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
	_, _, chain, err := DeriveReplyInput(source, DraftKindReply, false, DraftInput{Body: "x"}, nil)
	if err != nil {
		t.Fatalf("DeriveReplyInput() error = %v", err)
	}
	fields := strings.Fields(chain)
	if len(fields) != maximumThreadReferences || fields[len(fields)-1] != "<new@example.com>" || fields[0] != "<r6@example.com>" {
		t.Fatalf("chain = %q", chain)
	}
}

func TestDeriveReplyInputDeduplicatesReferencesAndKeepsParentLast(t *testing.T) {
	t.Parallel()
	parent := "<parent@example.com>"
	source := ThreadSource{
		Subject: "s", From: "a@example.com", MessageID: parent,
		References: "<first@example.com> <second@example.com> <first@example.com> " + parent + " <third@example.com> <second@example.com>",
	}
	_, _, chain, err := DeriveReplyInput(source, DraftKindReply, false, DraftInput{Body: "x"}, nil)
	if err != nil {
		t.Fatalf("DeriveReplyInput() error = %v", err)
	}
	want := "<first@example.com> <second@example.com> <third@example.com> " + parent
	if chain != want {
		t.Fatalf("chain = %q, want %q", chain, want)
	}
}

func TestDeriveReplyInputRejectsMalformedMessageIDs(t *testing.T) {
	t.Parallel()
	tests := []ThreadSource{
		{Subject: "s", From: "a@example.com", MessageID: "parent@example.com"},
		{Subject: "s", From: "a@example.com", MessageID: "<parent@example.com>", References: "<valid@example.com> invalid"},
	}
	for _, source := range tests {
		if _, _, _, err := DeriveReplyInput(source, DraftKindReply, false, DraftInput{Body: "x"}, nil); errorCode(err) != "invalid_message_source" {
			t.Fatalf("DeriveReplyInput(%+v) error = %v, want invalid_message_source", source, err)
		}
	}
}

func TestDerivedReplyDraftComposesCanonicalThreading(t *testing.T) {
	t.Parallel()
	parent := "<parent@example.com>"
	var historical []string
	for index := 0; index < maximumThreadReferences+4; index++ {
		historical = append(historical, fmt.Sprintf("<history-%d@example.com>", index))
	}
	source := ThreadSource{
		Subject: "Project update", From: "Alice <alice@example.com>", MessageID: parent,
		References: strings.Join(append(historical, parent, historical[0]), " "),
	}
	input, sourceMessageID, references, err := DeriveReplyInput(source, DraftKindReply, false, DraftInput{Body: "Reply"}, nil)
	if err != nil {
		t.Fatalf("DeriveReplyInput() error = %v", err)
	}
	service := NewServiceWithDraftRoot(&gatewayStub{}, filepath.Join(t.TempDir(), "drafts"))
	draft, err := service.CreateDraft(CreateDraftRequest{
		Kind: DraftKindReply, SourceRef: storeBoundSourceRef(t), Input: input,
		SourceMessageID: sourceMessageID, SourceReferences: references,
	})
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	message, err := BuildMessage(draft, "<reply@example.com>")
	if err != nil {
		t.Fatalf("BuildMessage() error = %v", err)
	}
	parsed, err := stdmail.ReadMessage(strings.NewReader(string(message)))
	if err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}
	finalReferences := strings.Fields(parsed.Header.Get("References"))
	if len(finalReferences) != maximumThreadReferences || finalReferences[len(finalReferences)-1] != parent {
		t.Fatalf("final References = %q, want %d entries ending in %q", parsed.Header.Get("References"), maximumThreadReferences, parent)
	}
	parentCount := 0
	for _, reference := range finalReferences {
		if reference == parent {
			parentCount++
		}
	}
	if parentCount != 1 || parsed.Header.Get("In-Reply-To") != parent {
		t.Fatalf("thread headers = References %q / In-Reply-To %q, want one parent %q", parsed.Header.Get("References"), parsed.Header.Get("In-Reply-To"), parent)
	}
}

func TestDeriveReplyInputRejectsControlCharacters(t *testing.T) {
	t.Parallel()
	source := ThreadSource{
		Subject: "s", From: "a@example.com", MessageID: "<m1@example.com>",
		References: "<r0@example.com>\r\nBcc: victim@example.com",
	}
	if _, _, _, err := DeriveReplyInput(source, DraftKindReply, false, DraftInput{Body: "x"}, nil); err == nil ||
		errorCode(err) != "invalid_message_source" {
		t.Fatalf("control characters error = %v", err)
	}
}

func TestDeriveReplyInputRequiresReplyTarget(t *testing.T) {
	t.Parallel()
	source := ThreadSource{Subject: "s", MessageID: "<m1@example.com>"}
	if _, _, _, err := DeriveReplyInput(source, DraftKindReply, false, DraftInput{Body: "x"}, nil); err == nil ||
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
