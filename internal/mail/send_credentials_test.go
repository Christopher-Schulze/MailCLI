package mail

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/keychain"
	"mailcli/internal/transport"
)

type credentialBoundaryIMAP struct {
	*reconcileImapStub
	lists int
}

func (o *credentialBoundaryIMAP) ListMailboxes(ctx context.Context, cfg transport.ImapConfig) ([]transport.MailboxInfo, error) {
	o.lists++
	return o.reconcileImapStub.ListMailboxes(ctx, cfg)
}

func TestSendCredentialErrorsPreserveCauseAndClaims(t *testing.T) {
	cause := errors.New("credential backend unavailable")
	for _, test := range []struct {
		name, code string
		err        error
		missing    bool
	}{
		{"read failure", keychain.CodeLoadFailed, &keychain.KeychainError{Code: keychain.CodeLoadFailed, Message: "read failed", Err: cause}, false},
		{"unsupported", keychain.CodeUnsupported, &keychain.KeychainError{Code: keychain.CodeUnsupported, Message: "unsupported", Err: cause}, false},
		{"untyped backend failure", "", os.ErrPermission, false},
		{"missing item", "smtp_credentials_missing", &keychain.KeychainError{Code: keychain.CodeNotFound, Message: "missing"}, true},
		{"empty password", "smtp_credentials_missing", nil, true},
	} {
		for _, boundary := range []string{"send", "delivery", "unknown reconcile", "mirror reconcile"} {
			t.Run(test.name+"/"+boundary, func(t *testing.T) {
				root := filepath.Join(t.TempDir(), "drafts")
				submitter, mirror := sendTransportStubs()
				credentials := &stubCredentials{password: "secret"}
				service := newTransportService(root, submitter, mirror, credentials)
				draft := createTransportDraft(t, service)
				operator := &credentialBoundaryIMAP{reconcileImapStub: &reconcileImapStub{}}
				if boundary == "unknown reconcile" {
					beginUnknownClaim(t, root, draft)
					service.send.Imap = operator
				}
				if boundary == "mirror reconcile" {
					mirror.err = &transport.TransportError{Code: transport.CodeIMAPAppendFailed, Message: "mirror rejected"}
					result, err := service.SendDraft(context.Background(), SendDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision})
					if err == nil || result.Outcome != SendOutcomeMirrorPending {
						t.Fatalf("prepare accepted submission: result=%+v error=%v", result, err)
					}
					service.send.Imap = operator
				}
				before, err := service.GetDraft(draft.Ref)
				if err != nil {
					t.Fatal(err)
				}
				previousSubmits, previousMirrors := submitter.calls, mirror.calls
				credentials.password, credentials.loadErr = "", test.err
				command := "drafts.send"
				var result SendResult
				switch boundary {
				case "send":
					result, err = service.SendDraft(context.Background(), SendDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision})
				case "delivery":
					_, err = DeliverViaTransport(context.Background(), service.send, draft)
				default:
					command = "drafts.reconcile"
					result, err = service.ReconcileDraft(context.Background(), draft.Ref)
				}
				if err == nil || transport.ErrorCode(err) != test.code {
					t.Fatalf("credential code=%q error=%v; want %q", transport.ErrorCode(err), err, test.code)
				}
				if test.missing {
					if !strings.Contains(err.Error(), "mailcli send setup --from sender@icloud.com") {
						t.Fatalf("missing setup guidance: %v", err)
					}
				} else if !errors.Is(err, test.err) || test.code != "" && !errors.Is(err, cause) {
					t.Fatalf("credential error lost its cause: %v", err)
				}
				if test.code != "" {
					guidance := GuidanceForError(command, err)
					if guidance.Recovery.Action != RecoveryCorrect || guidance.EffectCertainty != EffectNone || guidance.ReplayAllowed {
						t.Fatalf("credential repair guidance=%+v", guidance)
					}
				}
				if submitter.calls != previousSubmits || mirror.calls != previousMirrors || operator.lists != 0 || operator.searchCalls != 0 || operator.fetchCalls != 0 {
					t.Fatalf("credential failure dispatched transport: submits=%d mirrors=%d IMAP=%+v", submitter.calls, mirror.calls, operator)
				}
				after, err := service.GetDraft(draft.Ref)
				if err != nil || !reflect.DeepEqual(after.SendAttempt, before.SendAttempt) {
					t.Fatalf("credential failure changed retained claim: before=%+v after=%+v error=%v", before.SendAttempt, after.SendAttempt, err)
				}
				if before.SendAttempt == nil {
					assertNoSendClaim(t, root, draft.Ref)
					if result.AttemptID != "" {
						t.Fatalf("preflight created attempt: %+v", result)
					}
				} else if !result.DraftRetained || result.Outcome != before.SendAttempt.Outcome {
					t.Fatalf("retained outcome lost: %+v", result)
				}
			})
		}
	}
}
