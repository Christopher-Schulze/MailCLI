package mailstore

import (
	"context"
	"errors"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

func TestTransferCopyNotStartedSkipsDestinationReconciliation(t *testing.T) {
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "identity@gmail.com")
	deadlineErr := &transport.TransportError{
		Code:    transport.CodeIMAPTimeout,
		Message: "injected COPY deadline failure",
	}
	fakeImap := &notStartedCopyOperator{stubImapOperator: &stubImapOperator{
		boxes:                  []transport.MailboxInfo{{Name: "INBOX"}, {Name: "Archive"}},
		searchMatchesByMailbox: map[string][]int{"Archive": []int{0}},
		mutationErrs:           []error{deadlineErr},
	}}
	client := &Client{
		store: store,
		send: mail.SendTransport{
			Imap:        fakeImap,
			Credentials: stubCredentials{"identity@gmail.com": "secret"},
		},
	}
	destinationRef, err := mailref.EncodeMailbox(testAccountID, []string{"Archive"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{
		MailboxRef: inboxRef, Limit: 1,
	})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("ListMessages() = %+v, error = %v", page, err)
	}

	_, err = client.TransferMessage(context.Background(), mail.TransferMessageRequest{
		Ref: page.Messages[0].Ref, DestinationMailbox: destinationRef, Copy: true,
	})
	if transport.ErrorCode(err) != transport.CodeIMAPTimeout {
		t.Fatalf("TransferMessage() error = %v, want %s", err, transport.CodeIMAPTimeout)
	}
	var outcomeErr *transport.MutationOutcomeError
	if !errors.As(err, &outcomeErr) || outcomeErr.Evidence.Outcome != transport.MutationOutcomeNotStarted ||
		outcomeErr.Evidence.Command != "COPY" || outcomeErr.Evidence.OperationID == "" ||
		outcomeErr.Evidence.UIDValidity != 12345 || outcomeErr.Evidence.ExpectedUIDValidity != 12345 ||
		outcomeErr.Evidence.ServerResponse != "" {
		t.Fatalf("pre-dispatch COPY evidence = %+v, want not-started UIDVALIDITY evidence: %v", outcomeErr, err)
	}
	if fakeImap.archiveSearches != 1 {
		t.Fatalf("destination searches = %d, want only the pre-dispatch safety check", fakeImap.archiveSearches)
	}
	if fakeImap.mutationCalls != 1 {
		t.Fatalf("mutation calls = %d, want 1", fakeImap.mutationCalls)
	}
	if len(client.copyAttempts) != 0 {
		t.Fatalf("retained COPY attempts = %d, want none after not-started failure", len(client.copyAttempts))
	}
}

type notStartedCopyOperator struct {
	*stubImapOperator
	archiveSearches int
}

func (operator *notStartedCopyOperator) SearchUID(
	ctx context.Context,
	cfg transport.ImapConfig,
	mailbox string,
	messageID string,
) (uint32, uint32, int, error) {
	if mailbox == "Archive" {
		operator.archiveSearches++
	}
	return operator.stubImapOperator.SearchUID(ctx, cfg, mailbox, messageID)
}

func (operator *notStartedCopyOperator) CopyMessage(
	ctx context.Context,
	cfg transport.ImapConfig,
	srcMailbox string,
	uid uint32,
	expectedUIDValidity uint32,
	dstMailbox string,
) (transport.MutationEvidence, error) {
	evidence, err := operator.stubImapOperator.CopyMessage(
		ctx, cfg, srcMailbox, uid, expectedUIDValidity, dstMailbox,
	)
	if err == nil {
		return evidence, nil
	}
	evidence.Outcome = transport.MutationOutcomeNotStarted
	evidence.ServerResponse = ""
	return evidence, &transport.MutationOutcomeError{
		Code:     transport.ErrorCode(err),
		Message:  "IMAP COPY was not dispatched; no server-side effect occurred",
		Evidence: evidence,
		Err:      err,
	}
}
