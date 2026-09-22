package mailstore

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

// noFlagReaderOperator wraps an ImapOperator while deliberately not
// implementing transport.FlagStateReader: the embedded interface limits the
// method set to ImapOperator itself.
type noFlagReaderOperator struct{ transport.ImapOperator }

func TestMessageStateReportsServerAndLocalAgreement(t *testing.T) {
	operator := &stubImapOperator{
		flagState: transport.FlagState{Flags: []string{"\\Flagged"}},
	}
	client, original := flagResultFixture(t, operator)
	state, err := client.MessageState(context.Background(), original.Ref)
	if err != nil {
		t.Fatalf("MessageState: %v", err)
	}
	if operator.fetchFlagsCalls != 1 || operator.fetchFlagsUID != state.ServerUID {
		t.Fatalf("FETCH FLAGS calls = %d uid %d, want 1/%d", operator.fetchFlagsCalls, operator.fetchFlagsUID, state.ServerUID)
	}
	if state.ServerState != mail.MessageServerStateObserved || state.ServerUIDValidity != 12345 {
		t.Fatalf("server state = %+v", state)
	}
	if !reflect.DeepEqual(state.ServerFlags, []string{"\\Flagged"}) {
		t.Fatalf("server flags = %v", state.ServerFlags)
	}
	wantLocal := mail.LocalIndexFlags{Read: original.Read, Flagged: original.Flagged, Junk: original.Junk, Deleted: original.Deleted}
	if state.LocalIndexFlags != wantLocal {
		t.Fatalf("local flags = %+v, want %+v", state.LocalIndexFlags, wantLocal)
	}
	if !state.FlagsAgree || state.StalenessNote == "" {
		t.Fatalf("state = %+v", state)
	}
}

func TestMessageStateReportsDivergence(t *testing.T) {
	operator := &stubImapOperator{
		flagState: transport.FlagState{Flags: []string{"\\Seen", "$Junk"}},
	}
	client, original := flagResultFixture(t, operator)
	state, err := client.MessageState(context.Background(), original.Ref)
	if err != nil {
		t.Fatalf("MessageState: %v", err)
	}
	if state.FlagsAgree {
		t.Fatalf("divergent server flags must not agree: %+v", state)
	}
	if !reflect.DeepEqual(state.ServerFlags, []string{"\\Seen", "$Junk"}) || state.LocalIndexFlags.Flagged != original.Flagged {
		t.Fatalf("state lost server or local truth: %+v", state)
	}
}

func TestMessageStateReportsMissingTarget(t *testing.T) {
	operator := &stubImapOperator{flagState: transport.FlagState{Missing: true}}
	client, original := flagResultFixture(t, operator)
	state, err := client.MessageState(context.Background(), original.Ref)
	if err != nil {
		t.Fatalf("MessageState: %v", err)
	}
	if state.ServerState != mail.MessageServerStateMissing || state.FlagsAgree || len(state.ServerFlags) != 0 {
		t.Fatalf("missing state = %+v", state)
	}
}

func TestMessageStatePropagatesFetchFailure(t *testing.T) {
	want := &transport.TransportError{Code: transport.CodeIMAPTimeout, Message: "deadline"}
	operator := &stubImapOperator{flagStateErr: want}
	client, original := flagResultFixture(t, operator)
	state, err := client.MessageState(context.Background(), original.Ref)
	if !errors.Is(err, want) || transport.ErrorCode(err) != transport.CodeIMAPTimeout {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if state.ServerState != "" || state.FlagsAgree {
		t.Fatalf("error path must not fabricate state: %+v", state)
	}
}

func TestMessageStateFailsClosedWithoutFlagReader(t *testing.T) {
	operator := &noFlagReaderOperator{ImapOperator: &stubImapOperator{}}
	client, original := flagResultFixture(t, operator)
	_, err := client.MessageState(context.Background(), original.Ref)
	if err == nil {
		t.Fatal("MessageState without FlagStateReader must fail closed")
	}
	var coded interface{ ErrorCode() string }
	if !errors.As(err, &coded) || coded.ErrorCode() != "imap_flag_read_unsupported" {
		t.Fatalf("error = %v, want imap_flag_read_unsupported", err)
	}
}

func TestMessageStateFailsClosedForUnsupportedProvider(t *testing.T) {
	store, inbox := newSearchFixture(t)
	closeTestResource(t, store, "state store")
	installImapIdentityFixture(t, store, "state-test@custom.example")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inbox, Limit: 1})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("fixture messages: %+v, %v", page, err)
	}
	client := &Client{store: store, send: mail.SendTransport{
		Imap:        &stubImapOperator{},
		Credentials: strictCredentials{"state-test@custom.example": "test-password"},
	}}
	_, err = client.MessageState(context.Background(), page.Messages[0].Ref)
	if transport.ErrorCode(err) != transport.CodeUnsupportedProvider {
		t.Fatalf("error = %v, want %s", err, transport.CodeUnsupportedProvider)
	}
}

func TestMessageStateRejectsStaleReference(t *testing.T) {
	operator := &stubImapOperator{flagState: transport.FlagState{Flags: []string{"\\Flagged"}}}
	client, original := flagResultFixture(t, operator)
	state, err := client.MessageState(context.Background(), original.Ref)
	if err != nil || !state.FlagsAgree {
		t.Fatalf("baseline state = %+v, %v", state, err)
	}
	if _, err := client.MessageState(context.Background(), "stale-ref-value"); err == nil {
		t.Fatal("stale ref must fail before any server read")
	}
	if operator.fetchFlagsCalls != 1 {
		t.Fatalf("stale ref reached the server: %d calls", operator.fetchFlagsCalls)
	}
}
