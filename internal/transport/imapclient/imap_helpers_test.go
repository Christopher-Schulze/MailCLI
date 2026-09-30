package imapclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

func TestPickTrash(t *testing.T) {
	t.Parallel()

	if got := transport.PickTrashMailbox([]transport.MailboxInfo{
		{Name: "INBOX"},
		{Name: "Bin", Flags: []string{"\\Trash"}},
	}); got != "Bin" {
		t.Fatalf("flagged trash = %q, want Bin", got)
	}
	if got := transport.PickTrashMailbox([]transport.MailboxInfo{
		{Name: "INBOX"},
		{Name: "[Gmail]/Trash"},
	}); got != "[Gmail]/Trash" {
		t.Fatalf("named trash = %q", got)
	}
	if got := transport.PickTrashMailbox([]transport.MailboxInfo{{Name: "INBOX"}}); got != "" {
		t.Fatalf("missing trash = %q", got)
	}
}

func TestIsTimeout(t *testing.T) {
	t.Parallel()

	if !isTimeout(context.DeadlineExceeded) {
		t.Fatal("DeadlineExceeded should be timeout")
	}
	if !isTimeout(os.ErrDeadlineExceeded) {
		t.Fatal("os.ErrDeadlineExceeded should be timeout")
	}
	if isTimeout(errors.New("plain")) {
		t.Fatal("plain error should not be timeout")
	}
}

func TestWrapIOError(t *testing.T) {
	t.Parallel()

	if err := wrapIOError(context.Background(), nil, transport.CodeIMAPConnectFailed, "x"); err != nil {
		t.Fatalf("nil err wrapped = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := wrapIOError(ctx, errors.New("closed"), transport.CodeIMAPConnectFailed, "read")
	if transport.ErrorCode(err) != transport.CodeIMAPCanceled || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wrap code = %q", transport.ErrorCode(err))
	}
	err = wrapIOError(context.Background(), context.DeadlineExceeded, transport.CodeIMAPConnectFailed, "read")
	if transport.ErrorCode(err) != transport.CodeIMAPTimeout {
		t.Fatalf("deadline wrap code = %q", transport.ErrorCode(err))
	}
	err = wrapIOError(context.Background(), errors.New("boom"), transport.CodeIMAPConnectFailed, "read")
	if transport.ErrorCode(err) != transport.CodeIMAPConnectFailed {
		t.Fatalf("plain wrap code = %q", transport.ErrorCode(err))
	}
}

func TestReadLineRejectsOversizedResponseWithTypedCause(t *testing.T) {
	t.Parallel()

	sess := &session{
		br: bufio.NewReader(strings.NewReader(
			strings.Repeat("x", maxIMAPResponseLineBytes+1) + "\r\n",
		)),
	}
	_, err := (&Client{}).readLine(sess)
	var malformed *malformedResponseError
	if !errors.As(err, &malformed) {
		t.Fatalf("readLine() error = %v, want malformedResponseError", err)
	}
	if !strings.Contains(err.Error(), "malformed IMAP response") ||
		!strings.Contains(err.Error(), "exceeds") || errors.Unwrap(malformed) == nil {
		t.Fatalf("readLine() error = %v, want malformed response with preserved cause", err)
	}
	if !sess.dirty {
		t.Fatal("readLine() left oversized-response session reusable")
	}
}

func TestWrapDialErrorClassifiesAndPreservesCause(t *testing.T) {
	t.Parallel()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cause := errors.New("dial failed")
	tests := []struct {
		name     string
		ctx      context.Context
		err      error
		wantCode string
	}{
		{name: "canceled", ctx: canceled, err: cause, wantCode: transport.CodeIMAPCanceled},
		{name: "timeout", ctx: context.Background(), err: context.DeadlineExceeded, wantCode: transport.CodeIMAPTimeout},
		{name: "connect", ctx: context.Background(), err: cause, wantCode: transport.CodeIMAPConnectFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := wrapDialError(test.ctx, test.err)
			if transport.ErrorCode(err) != test.wantCode {
				t.Fatalf("wrapDialError() code = %q, want %q", transport.ErrorCode(err), test.wantCode)
			}
			if !errors.Is(err, test.err) {
				t.Fatalf("wrapDialError() error = %v, want cause %v", err, test.err)
			}
		})
	}
}

func TestLoopbackTLSReadRecoveryRequiresVerificationCorrection(t *testing.T) {
	srv := newFakeServer(t, fakeServerConfig{authOK: true, otherMboxes: []string{"INBOX"}})
	certificate, err := x509.ParseCertificate(srv.cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	trusted := x509.NewCertPool()
	trusted.AddCert(certificate)
	for _, test := range []struct {
		name     string
		roots    *x509.CertPool
		hostname string
		failure  bool
	}{
		{name: "untrusted certificate", roots: x509.NewCertPool(), hostname: "localhost", failure: true},
		{name: "wrong hostname", roots: trusted, hostname: "private-host.invalid", failure: true},
		{name: "verified peer", roots: trusted, hostname: "localhost"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, cfg := newFakeClient(t, srv)
			client.TLSConfig = &tls.Config{RootCAs: test.roots, ServerName: test.hostname}
			defer func() {
				if err := client.Close(); err != nil {
					t.Error(err)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			boxes, err := client.ListMailboxes(ctx, cfg)
			if !test.failure {
				if err != nil || len(boxes) != 1 {
					t.Fatalf("verified loopback peer = %v, %v", boxes, err)
				}
				return
			}
			var verification *tls.CertificateVerificationError
			if transport.ErrorCode(err) != transport.CodeIMAPConnectFailed || !errors.As(err, &verification) ||
				!transport.IsConfigurationFailure(err) || transport.IsTransientReadFailure(err) {
				t.Fatalf("real TLS failure classification = %v", err)
			}
			guidance := mail.GuidanceForError("mailboxes.list", err)
			if guidance.Phase != mail.OperationPhaseRead || guidance.EffectCertainty != mail.EffectNone ||
				guidance.Retryability != mail.RetryUserInputRequired || guidance.ReplayAllowed ||
				guidance.Recovery.Action != mail.RecoveryCorrect || !strings.Contains(guidance.Recovery.Instruction, "certificate trust") {
				t.Fatalf("real TLS read guidance = %+v", guidance)
			}
			uncertain := &transport.MutationOutcomeError{Code: transport.CodeIMAPMoveOutcomeUnknown, Err: err}
			if transport.IsConfigurationFailure(uncertain) || transport.IsTransientReadFailure(uncertain) {
				t.Fatal("TLS verification cause erased outcome uncertainty")
			}
		})
	}
	client, cfg := newFakeClient(t, srv)
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = client.ListMailboxes(ctx, cfg)
	if transport.ErrorCode(err) != transport.CodeIMAPConnectFailed || !transport.IsTransientReadFailure(err) || transport.IsConfigurationFailure(err) {
		t.Fatalf("refused loopback connection = %v", err)
	}
	guidance := mail.GuidanceForError("mailboxes.list", err)
	if guidance.Retryability != mail.RetrySafe || !guidance.ReplayAllowed || guidance.Recovery.Action != mail.RecoveryRetry {
		t.Fatalf("refused connection guidance = %+v", guidance)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}
