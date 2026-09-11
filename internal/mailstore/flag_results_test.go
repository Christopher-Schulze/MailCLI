package mailstore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

type flagBoundaryResult struct {
	evidence transport.MutationEvidence
	code     string
	cause    error
}

type flagResultOperator struct {
	*stubImapOperator
	results    []flagBoundaryResult
	calledUIDs []uint32
	searchUIDs []uint32
}

func (operator *flagResultOperator) SetFlags(_ context.Context, cfg transport.ImapConfig, mailbox string, uid, validity uint32, _, _ []string) (transport.MutationEvidence, error) {
	operator.calledUIDs = append(operator.calledUIDs, uid)
	if len(operator.results) == 0 {
		return transport.MutationEvidence{}, errors.New("unexpected STORE replay")
	}
	result := operator.results[0]
	operator.results = operator.results[1:]
	evidence := result.evidence
	evidence.Command, evidence.SourceAccount, evidence.ExpectedUIDValidity = "STORE", cfg.Username, validity
	evidence.OperationID = "store_observation_test"
	if evidence.UID == 0 {
		evidence.UID = uid
	}
	if evidence.UIDValidity == 0 {
		evidence.UIDValidity = validity
	}
	if evidence.Mailbox == "" {
		evidence.Mailbox = mailbox
	}
	if result.code == "" {
		return evidence, nil
	}
	if result.code == "mailbox_uidvalidity_changed" {
		return evidence, result.cause
	}
	return evidence, &transport.MutationOutcomeError{Code: result.code, Message: "controlled flag boundary", Evidence: evidence, Err: result.cause}
}

func (operator *flagResultOperator) SearchUID(ctx context.Context, cfg transport.ImapConfig, mailbox, messageID string) (uint32, uint32, int, error) {
	if len(operator.searchUIDs) > 0 {
		operator.uid = operator.searchUIDs[0]
		operator.searchUIDs = operator.searchUIDs[1:]
	}
	return operator.stubImapOperator.SearchUID(ctx, cfg, mailbox, messageID)
}

func flagResultFixture(t *testing.T, operator transport.ImapOperator) (*Client, mail.MessageSummary) {
	t.Helper()
	store, inbox := newSearchFixture(t)
	closeTestResource(t, store, "flag result store")
	installImapIdentityFixture(t, store, "flag-results@gmail.com")
	page, err := store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: inbox, Limit: 1})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("fixture messages: %+v, %v", page, err)
	}
	return &Client{store: store, send: mail.SendTransport{Imap: operator, Credentials: strictCredentials{"flag-results@gmail.com": "test-password"}}}, page.Messages[0]
}

func TestMarkMessagePreservesActualFlagResults(t *testing.T) {
	tests := []struct {
		name                                         string
		result                                       flagBoundaryResult
		wantCode                                     string
		wantRead, wantFlagged, wantJunk, wantDeleted bool
		wantState                                    transport.FlagObservationState
	}{
		{name: "all actual flags replace cached booleans", result: flagBoundaryResult{evidence: transport.MutationEvidence{Outcome: transport.MutationOutcomeCompleted, FlagsState: transport.FlagObservationObserved, ActualFlags: []string{"\\sEeN", "$JUNK", "\\Deleted"}, FlagsSource: "STORE"}}, wantRead: true, wantJunk: true, wantDeleted: true, wantState: transport.FlagObservationObserved},
		{name: "contradictory observed empty set", result: flagBoundaryResult{evidence: transport.MutationEvidence{Outcome: transport.MutationOutcomeObserved, FlagsState: transport.FlagObservationObserved, ActualFlags: []string{}, FlagsSource: "STORE"}, code: transport.CodeIMAPFlagsMismatch}, wantCode: transport.CodeIMAPFlagsMismatch, wantState: transport.FlagObservationObserved},
		{name: "missing UID retains clearly local flags", result: flagBoundaryResult{evidence: transport.MutationEvidence{Outcome: transport.MutationOutcomeUnknown, FlagsState: transport.FlagObservationMissing, FlagsSource: "FETCH"}, code: transport.CodeIMAPMessageNotFound}, wantCode: transport.CodeIMAPMessageNotFound, wantFlagged: true, wantState: transport.FlagObservationMissing},
		{name: "post-write rollover cannot replay", result: flagBoundaryResult{evidence: transport.MutationEvidence{Outcome: transport.MutationOutcomeUnknown, FlagsState: transport.FlagObservationUnverified, FlagsSource: "FETCH"}, code: transport.CodeIMAPFlagsOutcomeUnknown, cause: uidValidityChangedErrorForTest()}, wantCode: transport.CodeIMAPFlagsOutcomeUnknown, wantFlagged: true, wantState: transport.FlagObservationUnverified},
		{name: "success without observation fails closed", result: flagBoundaryResult{evidence: transport.MutationEvidence{Outcome: transport.MutationOutcomeCompleted}}, wantCode: transport.CodeIMAPFlagsOutcomeUnknown, wantFlagged: true, wantState: transport.FlagObservationUnverified},
		{name: "wrong UID proof fails closed", result: flagBoundaryResult{evidence: transport.MutationEvidence{Outcome: transport.MutationOutcomeCompleted, UID: 999, FlagsState: transport.FlagObservationObserved, ActualFlags: []string{"\\Seen"}}}, wantCode: transport.CodeIMAPFlagsOutcomeUnknown, wantFlagged: true, wantState: transport.FlagObservationUnverified},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operator := &flagResultOperator{stubImapOperator: &stubImapOperator{}, results: []flagBoundaryResult{test.result}}
			client, original := flagResultFixture(t, operator)
			read := true
			summary, err := client.MarkMessage(context.Background(), mail.MarkMessageRequest{Ref: original.Ref, Read: &read, AllowDraftMutation: true})
			if transport.ErrorCode(err) != test.wantCode {
				t.Fatalf("error = %v, want %s", err, test.wantCode)
			}
			if summary.Read != test.wantRead || summary.Flagged != test.wantFlagged || summary.Junk != test.wantJunk || summary.Deleted != test.wantDeleted {
				t.Fatalf("incorrect actual/local flag projection: %+v", summary)
			}
			if summary.ServerTruth == nil || summary.ServerTruth.FlagsState != string(test.wantState) || summary.Subject != original.Subject || summary.Sender != original.Sender {
				t.Fatalf("missing observation or preserved metadata: %+v", summary)
			}
			if len(operator.calledUIDs) != 1 {
				t.Fatalf("STORE calls = %v, want exactly one", operator.calledUIDs)
			}
			if test.wantState != transport.FlagObservationObserved && !strings.Contains(summary.StalenessNote, "local cached") {
				t.Fatalf("unverified booleans were not labeled local: %+v", summary)
			}
			local, err := client.store.ListMessages(context.Background(), mail.ListMessagesRequest{MailboxRef: original.MailboxRef, Limit: 1})
			if err != nil || len(local.Messages) != 1 || local.Messages[0].Read != original.Read || local.Messages[0].Flagged != original.Flagged {
				t.Fatalf("local cached state changed: %+v, %v", local, err)
			}
		})
	}
}

func TestMarkMessageRetryUsesNewResolvedSummaryIdentity(t *testing.T) {
	operator := &flagResultOperator{
		stubImapOperator: &stubImapOperator{}, searchUIDs: []uint32{101, 202},
		results: []flagBoundaryResult{
			{evidence: transport.MutationEvidence{Outcome: transport.MutationOutcomeNotStarted}, code: "mailbox_uidvalidity_changed", cause: uidValidityChangedErrorForTest()},
			{evidence: transport.MutationEvidence{Outcome: transport.MutationOutcomeCompleted, FlagsState: transport.FlagObservationObserved, ActualFlags: []string{"\\Seen"}, FlagsSource: "STORE"}},
		},
	}
	client, original := flagResultFixture(t, operator)
	read := true
	summary, err := client.MarkMessage(context.Background(), mail.MarkMessageRequest{Ref: original.Ref, Read: &read, AllowDraftMutation: true})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := mailref.DecodeMessage(summary.Ref)
	if err != nil || ref.ExpectedIMAPUID != 202 || summary.ServerTruth == nil || summary.ServerTruth.UID != 202 || !reflect.DeepEqual(operator.calledUIDs, []uint32{101, 202}) {
		t.Fatalf("retry retained stale identity: ref %+v, summary %+v, calls %v, error %v", ref, summary, operator.calledUIDs, err)
	}
}

func TestBatchMarkRetainsMissingAndConflictingEvidence(t *testing.T) {
	for _, test := range []struct {
		name, code, outcome, wantState string
		flagsState                     transport.FlagObservationState
	}{
		{"conflicting", transport.CodeIMAPFlagsMismatch, transport.MutationOutcomeObserved, mail.BatchItemFailed, transport.FlagObservationObserved},
		{"missing", transport.CodeIMAPMessageNotFound, transport.MutationOutcomeUnknown, mail.BatchItemUncertain, transport.FlagObservationMissing},
		{"unverified", transport.CodeIMAPFlagsOutcomeUnknown, transport.MutationOutcomeUnknown, mail.BatchItemUncertain, transport.FlagObservationUnverified},
	} {
		t.Run(test.name, func(t *testing.T) {
			operator := &flagResultOperator{stubImapOperator: &stubImapOperator{}, results: []flagBoundaryResult{{evidence: transport.MutationEvidence{Outcome: test.outcome, FlagsState: test.flagsState, FlagsSource: "FETCH"}, code: test.code}}}
			client, original := flagResultFixture(t, operator)
			read := true
			result, err := mail.NewService(client).ExecuteBatch(context.Background(), mail.BatchRequest{Operation: mail.BatchOperationMark, Items: []mail.BatchItem{{ID: "mark", Ref: original.Ref, Read: &read, AllowDraftMutation: true}}})
			if err != nil || len(result.Items) != 1 {
				t.Fatalf("batch = %+v, %v", result, err)
			}
			item := result.Items[0]
			if item.State != test.wantState || item.MessageState == nil || item.MessageState.ServerTruth.FlagsState != string(test.flagsState) || item.Error == nil || item.Error.Guidance == nil || item.Error.Guidance.ReplayAllowed || item.Error.Retryable || item.Error.Guidance.Recovery.OperationID == "" {
				t.Fatalf("batch lost evidence or allowed replay: %+v", item)
			}
			if len(operator.calledUIDs) != 1 {
				t.Fatalf("batch STORE replay: %v", operator.calledUIDs)
			}
		})
	}
}
