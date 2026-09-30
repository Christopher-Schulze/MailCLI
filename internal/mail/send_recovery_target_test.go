package mail

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/transport"
)

func TestSendRecoveryRejectsChangedTargetBeforeIO(t *testing.T) {
	for _, outcome := range []SendOutcome{SendOutcomeUnknown, SendOutcomeMirrorPending} {
		for _, test := range []struct {
			name   string
			change func(*AccountBinding)
		}{
			{"host", func(b *AccountBinding) { b.IMAPHost = "other.example.com" }},
			{"port", func(b *AccountBinding) { b.IMAPPort = 994 }},
			{"username", func(b *AccountBinding) { b.CredentialAccount = "other@icloud.com" }},
			{"account", func(b *AccountBinding) { b.AccountID = "account-b" }},
		} {
			t.Run(string(outcome)+"/"+test.name, func(t *testing.T) {
				service, draft, binding, credentials, submitter, mirror := pendingRecoveryDraft(t, outcome)
				claimPath := filepath.Join(service.draftRoot, draft.Ref+".send-claim")
				before := readRecoveryFixture(t, claimPath)
				spoolPath := filepath.Join(service.draftRoot, draft.Ref+".send-spool")
				spoolBefore := readRecoveryFixture(t, spoolPath)
				test.change(&binding)
				if err := service.send.AccountBindings.UpdateAccountBindings(context.Background(), func(doc AccountBindingFile) (AccountBindingFile, error) {
					doc.Bindings = []AccountBinding{binding}
					return doc, nil
				}); err != nil {
					t.Fatal(err)
				}
				imap := &recoveryImap{reconcileImapStub: reconcileImapStub{mailboxes: []transport.MailboxInfo{{Name: "Sent", Flags: []string{"\\Sent"}}}}}
				service.send.Imap = imap
				loads := len(credentials.lookups)
				result, err := service.ReconcileDraft(context.Background(), draft.Ref)
				if transport.ErrorCode(err) != "send_identity_unverifiable" || !result.DraftRetained || result.Outcome != outcome {
					t.Fatalf("reconcile = %+v, error = %v; want retained target refusal", result, err)
				}
				if len(credentials.lookups) != loads || imap.listCalls != 0 || imap.searchCalls != 0 || imap.fetchCalls != 0 || submitter.calls != 1 || mirror.calls != 1 {
					t.Fatalf("target refusal contacted credentials or transport: loads %v, search/fetch %d/%d, SMTP/APPEND %d/%d", credentials.lookups, imap.searchCalls, imap.fetchCalls, submitter.calls, mirror.calls)
				}
				if !bytes.Equal(before, readRecoveryFixture(t, claimPath)) || !bytes.Equal(spoolBefore, readRecoveryFixture(t, spoolPath)) {
					t.Fatal("target refusal changed retained claim or spool")
				}
			})
		}
	}
}

type recoveryImap struct {
	reconcileImapStub
	listCalls int
	config    transport.ImapConfig
}

func (i *recoveryImap) ListMailboxes(ctx context.Context, cfg transport.ImapConfig) ([]transport.MailboxInfo, error) {
	i.listCalls++
	i.config = cfg
	return i.reconcileImapStub.ListMailboxes(ctx, cfg)
}

type recoveryMirror struct {
	*stubMirror
	config transport.ImapConfig
}

func (m *recoveryMirror) AppendToSent(ctx context.Context, cfg transport.ImapConfig, raw []byte, id string) (transport.AppendEvidence, error) {
	m.config = cfg
	return m.stubMirror.AppendToSent(ctx, cfg, raw, id)
}

func TestSendRecoveryAllowsPasswordRotationAndTerminalReplay(t *testing.T) {
	for _, outcome := range []SendOutcome{SendOutcomeUnknown, SendOutcomeMirrorPending} {
		t.Run(string(outcome), func(t *testing.T) {
			service, draft, _, credentials, submitter, mirror := pendingRecoveryDraft(t, outcome)
			if err := credentials.Store("sender@icloud.com", "rotated-password"); err != nil {
				t.Fatal(err)
			}
			imap := &recoveryImap{reconcileImapStub: reconcileImapStub{
				mailboxes: []transport.MailboxInfo{{Name: "Sent", Flags: []string{"\\Sent"}}},
				uid:       42, fetchRaw: readRecoveryFixture(t, filepath.Join(service.draftRoot, draft.Ref+".send-spool")),
			}}
			wrapped := &recoveryMirror{stubMirror: mirror}
			service.send.Mirror = wrapped
			mirror.err = nil
			wantAppendCalls := 2
			if outcome == SendOutcomeUnknown {
				service.send.Imap = imap
				wantAppendCalls = 1
			}
			result, err := service.ReconcileDraft(context.Background(), draft.Ref)
			if err != nil || result.Outcome != SendOutcomeSent || result.DraftRetained || submitter.calls != 1 || mirror.calls != wantAppendCalls {
				t.Fatalf("recovery = %+v, error = %v, SMTP/APPEND %d/%d", result, err, submitter.calls, mirror.calls)
			}
			config := wrapped.config
			if outcome == SendOutcomeUnknown {
				config = imap.config
			}
			if config.Password != "rotated-password" || config.Username != "sender@icloud.com" || config.Host != "imap.mail.me.com" || config.Port != 993 {
				t.Fatal("recovery did not use the original target and rotated password")
			}
			service.send = SendTransport{}
			replayed, err := service.ReconcileDraft(context.Background(), draft.Ref)
			if err != nil || replayed.Receipt == nil || replayed.Receipt.AttemptID != result.AttemptID || !replayed.Replayed {
				t.Fatalf("terminal recovery = %+v, error = %v", replayed, err)
			}
		})
	}
}

func TestSendRecoveryLegacyTargetRemainsUnverifiable(t *testing.T) {
	for _, outcome := range []SendOutcome{SendOutcomeUnknown, SendOutcomeMirrorPending} {
		t.Run(string(outcome), func(t *testing.T) {
			service, draft, _, credentials, submitter, mirror := pendingRecoveryDraft(t, outcome)
			attempt, err := readSendAttempt(service.draftRoot, draft.Ref)
			if err != nil || attempt == nil {
				t.Fatalf("read attempt = %+v, error = %v", attempt, err)
			}
			attempt.RecoveryIdentity = nil
			if err := replaceSendAttempt(service.draftRoot, draft.Ref, *attempt); err != nil {
				t.Fatal(err)
			}
			claimPath := filepath.Join(service.draftRoot, draft.Ref+".send-claim")
			before := readRecoveryFixture(t, claimPath)
			imap := &recoveryImap{}
			service.send.Imap = imap
			loads := len(credentials.lookups)
			result, err := service.ReconcileDraft(context.Background(), draft.Ref)
			if transport.ErrorCode(err) != "send_identity_unverifiable" || !strings.Contains(err.Error(), "no original IMAP target") || !result.DraftRetained {
				t.Fatalf("legacy recovery = %+v, error = %v", result, err)
			}
			if len(credentials.lookups) != loads || imap.listCalls != 0 || submitter.calls != 1 || mirror.calls != 1 || !bytes.Equal(before, readRecoveryFixture(t, claimPath)) {
				t.Fatal("legacy recovery guessed a target or changed retained evidence")
			}
		})
	}
}

func TestSendRecoveryRejectsMalformedStoredIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*SendRecoveryIdentity)
	}{
		{"host", func(i *SendRecoveryIdentity) { i.Host = "localhost" }},
		{"port", func(i *SendRecoveryIdentity) { i.Port = 0 }},
		{"username", func(i *SendRecoveryIdentity) { i.Username = "" }},
		{"account", func(i *SendRecoveryIdentity) { i.AccountID = "bad\raccount" }},
		{"noncanonical", func(i *SendRecoveryIdentity) { i.Host = "IMAP.MAIL.ME.COM." }},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, draft, _, _, _, _ := pendingRecoveryDraft(t, SendOutcomeMirrorPending)
			attempt, err := readSendAttempt(service.draftRoot, draft.Ref)
			if err != nil || attempt == nil || attempt.RecoveryIdentity == nil {
				t.Fatalf("read attempt = %+v, error = %v", attempt, err)
			}
			test.change(attempt.RecoveryIdentity)
			if err := replaceSendAttempt(service.draftRoot, draft.Ref, *attempt); err != nil {
				t.Fatal(err)
			}
			if _, err := readSendAttempt(service.draftRoot, draft.Ref); err == nil {
				t.Fatal("malformed recovery identity was accepted")
			}
		})
	}
}

func pendingRecoveryDraft(t *testing.T, outcome SendOutcome) (*Service, Draft, AccountBinding, *bindingCredentials, *stubSubmitter, *stubMirror) {
	t.Helper()
	root := t.TempDir()
	store := NewAccountBindingStore(filepath.Join(root, "bindings.json"))
	binding := AccountBinding{AccountID: "account-a", SenderAliases: []string{"sender@icloud.com"}, CredentialAccount: "sender@icloud.com", SMTPHost: "smtp.mail.me.com", SMTPPort: 587, IMAPHost: "imap.mail.me.com", IMAPPort: 993}
	if err := store.UpsertAccountBinding(binding); err != nil {
		t.Fatal(err)
	}
	credentials := &bindingCredentials{passwords: map[string]string{"sender@icloud.com": "first-password"}}
	submitter, mirror := sendTransportStubs()
	mirror.err = &transport.TransportError{Code: transport.CodeIMAPAppendFailed, Message: "before APPEND"}
	service := NewServiceWithTransport(nil, filepath.Join(root, "drafts"), SendTransport{Submitter: submitter, Mirror: mirror, Credentials: credentials, AccountBindings: store})
	draft := createTransportDraft(t, service)
	submitter.submitHook = func(context.Context) {
		attempt, err := readSendAttempt(service.draftRoot, draft.Ref)
		want := SendRecoveryIdentity{AccountID: "ACCOUNT-A", Host: "imap.mail.me.com", Port: 993, Username: "sender@icloud.com"}
		if err != nil || attempt == nil || attempt.RecoveryIdentity == nil || *attempt.RecoveryIdentity != want {
			t.Fatalf("original recovery target was not durable before SMTP: %+v, error = %v", attempt, err)
		}
		projection := draftSendAttemptSummaryFrom(attempt)
		if projection.RecoveryIdentity == nil || *projection.RecoveryIdentity != want || projection.RecoveryIdentity == attempt.RecoveryIdentity {
			t.Fatal("metadata projection lost or aliased original recovery target")
		}
	}
	result, err := service.SendDraft(context.Background(), SendDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision})
	if transport.ErrorCode(err) != transport.CodeIMAPAppendFailed || result.Outcome != SendOutcomeMirrorPending {
		t.Fatalf("initial send = %+v, error = %v", result, err)
	}
	if outcome == SendOutcomeUnknown {
		attempt, err := readSendAttempt(service.draftRoot, draft.Ref)
		if err != nil || attempt == nil {
			t.Fatalf("read attempt = %+v, error = %v", attempt, err)
		}
		attempt.Outcome, attempt.AcceptedByMail, attempt.Transport = SendOutcomeUnknown, false, nil
		if err := replaceSendAttempt(service.draftRoot, draft.Ref, *attempt); err != nil {
			t.Fatal(err)
		}
	}
	return service, draft, binding, credentials, submitter, mirror
}

func readRecoveryFixture(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
