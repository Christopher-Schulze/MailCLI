package mail

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mailcli/internal/transport"
	"mailcli/internal/transport/imapclient"
)

func TestSendDraftFailsBeforeSMTPWhenMutationLockSetupFailed(t *testing.T) {
	root := t.TempDir()
	submitter, mirror := sendTransportStubs()
	credentialLoads := 0
	credentials := &stubCredentials{password: "secret", loadHook: func() { credentialLoads++ }}
	service := newTransportService(root, submitter, mirror, credentials)
	draft := createTransportDraft(t, service)
	imap := imapclient.NewWithMutationLockSetupError(errors.New("config directory unavailable"))
	service.send.Imap = imap

	result, err := service.SendDraft(context.Background(), SendDraftRequest{
		Ref: draft.Ref, ExpectedRevision: draft.Revision,
	})
	sendErr := err
	if got := transport.ErrorCode(sendErr); got != transport.CodeIMAPLockUnavailable {
		t.Fatalf("SendDraft() error code = %q, error %v; want %q", got, sendErr, transport.CodeIMAPLockUnavailable)
	}
	if result.AttemptID != "" || submitter.calls != 0 || mirror.calls != 0 || credentialLoads != 0 {
		t.Fatalf("SendDraft() result=%+v, submitter calls=%d, mirror calls=%d, credential loads=%d; want no dispatch",
			result, submitter.calls, mirror.calls, credentialLoads)
	}
	retained, err := service.GetDraft(draft.Ref)
	if err != nil {
		t.Fatalf("GetDraft() after preflight failure: %v", err)
	}
	if retained.SendAttempt != nil {
		t.Fatalf("preflight failure created send attempt: %+v", retained.SendAttempt)
	}
	guidance := GuidanceForError("drafts.send", sendErr)
	if guidance.Phase != OperationPhaseMutation || guidance.EffectCertainty != EffectNone ||
		guidance.Retryability != RetrySafe || !guidance.ReplayAllowed ||
		guidance.Recovery.Action != RecoveryRetry {
		t.Fatalf("lock setup guidance = %+v for %T %v (code %q), want safe retry with no effect",
			guidance, sendErr, sendErr, transport.ErrorCode(sendErr))
	}
}

func TestSendDraftFailsBeforeSMTPWhenMutationLockInitializationFails(t *testing.T) {
	root := t.TempDir()
	submitter, mirror := sendTransportStubs()
	credentialLoads := 0
	credentials := &stubCredentials{password: "secret", loadHook: func() { credentialLoads++ }}
	service := newTransportService(root, submitter, mirror, credentials)
	draft := createTransportDraft(t, service)
	lockParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(lockParent, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	imap, err := imapclient.NewWithOptions(imapclient.ClientOptions{MutationLockDir: lockParent})
	if err != nil {
		t.Fatalf("NewWithOptions() = %v, want client that reports lock failure during preflight", err)
	}
	service.send.Imap = imap

	result, err := service.SendDraft(context.Background(), SendDraftRequest{
		Ref: draft.Ref, ExpectedRevision: draft.Revision,
	})
	if got := transport.ErrorCode(err); got != transport.CodeIMAPLockUnavailable {
		t.Fatalf("SendDraft() error code = %q, error %v; want %q", got, err, transport.CodeIMAPLockUnavailable)
	}
	if result.AttemptID != "" || submitter.calls != 0 || mirror.calls != 0 || credentialLoads != 0 {
		t.Fatalf("SendDraft() result=%+v, submitter calls=%d, mirror calls=%d, credential loads=%d; want no dispatch",
			result, submitter.calls, mirror.calls, credentialLoads)
	}
	retained, err := service.GetDraft(draft.Ref)
	if err != nil {
		t.Fatalf("GetDraft() after preflight failure: %v", err)
	}
	if retained.SendAttempt != nil {
		t.Fatalf("preflight failure created send attempt: %+v", retained.SendAttempt)
	}
}

func TestDeliverViaTransportFailsBeforeSubmissionWhenMutationLockSetupFailed(t *testing.T) {
	submitter, mirror := sendTransportStubs()
	credentialLoads := 0
	credentials := &stubCredentials{password: "secret", loadHook: func() { credentialLoads++ }}
	imap := imapclient.NewWithMutationLockSetupError(errors.New("config directory unavailable"))
	_, err := DeliverViaTransport(context.Background(), SendTransport{
		Submitter: submitter, Mirror: mirror, Credentials: credentials,
		Imap: imap,
	}, Draft{
		From: "sender@icloud.com",
		To:   []Recipient{{Address: "recipient@example.com"}},
	})
	if got := transport.ErrorCode(err); got != transport.CodeIMAPLockUnavailable {
		t.Fatalf("DeliverViaTransport() error code = %q, error %v; want %q",
			got, err, transport.CodeIMAPLockUnavailable)
	}
	if submitter.calls != 0 || mirror.calls != 0 || credentialLoads != 0 {
		t.Fatalf("submitter calls=%d, mirror calls=%d, credential loads=%d; want no dispatch",
			submitter.calls, mirror.calls, credentialLoads)
	}
}
