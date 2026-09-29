package mailstore

import (
	"context"
	"errors"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

// guardedCopyOperator makes check and COPY one call, like the real IMAP client
// does under the account lock; the legacy CopyMessage must stay unused.
type guardedCopyOperator struct {
	*stubImapOperator
	match         transport.DestinationMatch
	guardErr      error
	guardCalls    int
	guardedID     string
	legacyCopies  int
	copiedUnderIt int
	// destinationSearches counts SearchUID calls on the destination mailbox,
	// the legacy pre-dispatch check and the verification after the COPY.
	destinationSearches int
}

func (o *guardedCopyOperator) CopyMessageIfAbsent(
	ctx context.Context, cfg transport.ImapConfig, srcMailbox string, uid uint32, expectedUIDValidity uint32, dstMailbox string, messageID string,
) (transport.MutationEvidence, transport.DestinationMatch, error) {
	o.guardCalls++
	o.guardedID = messageID
	notStarted := transport.MutationEvidence{
		OperationID: "guarded-copy", Outcome: transport.MutationOutcomeNotStarted, Command: "COPY",
		Mailbox: srcMailbox, TargetMailbox: dstMailbox, UID: uid, ExpectedUIDValidity: expectedUIDValidity,
	}
	if o.guardErr != nil {
		return notStarted, transport.DestinationMatch{}, &transport.CopyGuardError{Err: o.guardErr}
	}
	if o.match.Count > 0 {
		return notStarted, o.match, nil
	}
	o.copiedUnderIt++
	evidence, err := o.stubImapOperator.CopyMessage(ctx, cfg, srcMailbox, uid, expectedUIDValidity, dstMailbox)
	return evidence, transport.DestinationMatch{}, err
}

func (o *guardedCopyOperator) SearchUID(
	ctx context.Context, cfg transport.ImapConfig, mailbox string, messageID string,
) (uint32, uint32, int, error) {
	if mailbox == "Sent" {
		o.destinationSearches++
	}
	return o.stubImapOperator.SearchUID(ctx, cfg, mailbox, messageID)
}

func (o *guardedCopyOperator) CopyMessage(
	ctx context.Context, cfg transport.ImapConfig, srcMailbox string, uid uint32, expectedUIDValidity uint32, dstMailbox string,
) (transport.MutationEvidence, error) {
	o.legacyCopies++
	return o.stubImapOperator.CopyMessage(ctx, cfg, srcMailbox, uid, expectedUIDValidity, dstMailbox)
}

func guardedCopyFixture(t *testing.T, operator *guardedCopyOperator) (*Client, mail.TransferMessageRequest) {
	t.Helper()
	store, inboxRef := newSearchFixture(t)
	closeTestResource(t, store, "test store")
	installImapIdentityFixture(t, store, "identity@gmail.com")
	client := &Client{
		store: store,
		send:  mail.SendTransport{Imap: operator, Credentials: stubCredentials{"identity@gmail.com": "secret"}},
	}
	destinationRef, err := mailref.EncodeMailbox(testAccountID, []string{"Sent"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inboxRef, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	messageRef := messageRefWithSubject(t, page.Messages, "Status Update")
	return client, mail.TransferMessageRequest{Ref: messageRef, DestinationMailbox: destinationRef, Copy: true}
}

func newGuardedCopyOperator() *guardedCopyOperator {
	return &guardedCopyOperator{stubImapOperator: &stubImapOperator{
		boxes: []transport.MailboxInfo{{Name: "INBOX"}, {Name: "Sent", Flags: []string{"\\Sent"}}},
		uid:   102,
		// The only destination search is the verification after the COPY.
		searchMatchesByMailbox: map[string][]int{"Sent": {1}},
	}}
}

func TestGuardedCopyDecidesUnderTheLockAndSendsTheCopyOnce(t *testing.T) {
	operator := newGuardedCopyOperator()
	client, request := guardedCopyFixture(t, operator)
	summary, err := client.TransferMessage(context.Background(), request)
	if err != nil || summary.ServerTruth == nil || summary.ServerTruth.Command != "COPY" {
		t.Fatalf("TransferMessage() = %+v, error = %v", summary, err)
	}
	if operator.guardCalls != 1 || operator.copiedUnderIt != 1 || operator.legacyCopies != 0 || operator.guardedID == "" {
		t.Fatalf("guard calls=%d copies under guard=%d legacy copies=%d message id=%q; want one guarded COPY",
			operator.guardCalls, operator.copiedUnderIt, operator.legacyCopies, operator.guardedID)
	}
	if operator.destinationSearches != 1 {
		t.Fatalf("destination searches = %d, want only the verification after the COPY", operator.destinationSearches)
	}
	if len(client.copyAttempts) != 0 {
		t.Fatalf("retained COPY attempts = %d, want none after a verified COPY", len(client.copyAttempts))
	}
}

func TestGuardedCopyDoesNotCopyWhenTheDestinationAlreadyHoldsTheMessage(t *testing.T) {
	operator := newGuardedCopyOperator()
	operator.match = transport.DestinationMatch{UID: 55, UIDValidity: 12345, Count: 1}
	client, request := guardedCopyFixture(t, operator)
	_, err := client.TransferMessage(context.Background(), request)
	var outcome *transport.MutationOutcomeError
	if !errors.As(err, &outcome) || outcome.Evidence.DestinationUID != 55 || outcome.Evidence.DestinationUIDValidity != 12345 {
		t.Fatalf("TransferMessage() error = %v, want unknown-outcome evidence with the destination UID", err)
	}
	if transport.ErrorCode(err) != transport.CodeIMAPCopyOutcomeUnknown {
		t.Fatalf("error code = %s, want %s", transport.ErrorCode(err), transport.CodeIMAPCopyOutcomeUnknown)
	}
	if operator.copiedUnderIt != 0 || operator.legacyCopies != 0 || operator.mutationCalls != 0 {
		t.Fatalf("a COPY was sent: guarded=%d legacy=%d mutations=%d", operator.copiedUnderIt, operator.legacyCopies, operator.mutationCalls)
	}
	if len(client.copyAttempts) != 0 {
		t.Fatalf("retained COPY attempts = %d, want none when no COPY started", len(client.copyAttempts))
	}
}

func TestGuardedCopyReportsAFailedDestinationCheckWithoutCopying(t *testing.T) {
	operator := newGuardedCopyOperator()
	operator.guardErr = &transport.TransportError{Code: transport.CodeIMAPTimeout, Message: "IMAP SEARCH deadline"}
	client, request := guardedCopyFixture(t, operator)
	_, err := client.TransferMessage(context.Background(), request)
	if transport.ErrorCode(err) != transport.CodeIMAPCopyOutcomeUnknown {
		t.Fatalf("TransferMessage() error = %v, want %s", err, transport.CodeIMAPCopyOutcomeUnknown)
	}
	var guardTimeout *transport.TransportError
	if !errors.As(err, &guardTimeout) {
		t.Fatalf("error %v does not keep the destination check failure", err)
	}
	if operator.copiedUnderIt != 0 || operator.legacyCopies != 0 || len(client.copyAttempts) != 0 {
		t.Fatalf("guarded=%d legacy=%d attempts=%d; want no COPY and no retained attempt", operator.copiedUnderIt, operator.legacyCopies, len(client.copyAttempts))
	}
}
