package mailstore

import (
	"context"
	"crypto/tls"
	"errors"
	"testing"

	"mailcli/internal/transport"
)

func TestHydrationErrorCodePreservesRemoteOrigin(t *testing.T) {
	untypedRemote := errors.New("remote spool failure secret-token")
	tests := []struct {
		name      string
		localCode string
		remote    error
		wantCode  string
	}{
		{name: "content incomplete with untyped remote", localCode: localContentIncompleteCode, remote: untypedRemote, wantCode: "hydration_failed"},
		{name: "raw source partial with untyped remote", localCode: "raw_source_partial", remote: untypedRemote, wantCode: "hydration_failed"},
		{name: "local content incomplete only", localCode: localContentIncompleteCode, wantCode: localContentIncompleteCode},
		{name: "local raw source partial only", localCode: "raw_source_partial", wantCode: "raw_source_partial"},
		{name: "typed timeout remote", localCode: localContentIncompleteCode,
			remote: &transport.TransportError{Code: transport.CodeIMAPTimeout}, wantCode: transport.CodeIMAPTimeout},
		{name: "typed cancellation remote", localCode: localContentIncompleteCode,
			remote: &transport.TransportError{Code: transport.CodeIMAPCanceled, Err: context.Canceled}, wantCode: transport.CodeIMAPCanceled},
		{name: "typed disconnect remote", localCode: localContentIncompleteCode,
			remote: &transport.TransportError{Code: transport.CodeIMAPDisconnected}, wantCode: transport.CodeIMAPDisconnected},
		{name: "TLS connection remote", localCode: localContentIncompleteCode,
			remote: &transport.TransportError{Code: transport.CodeIMAPConnectFailed,
				Err: &tls.CertificateVerificationError{Err: errors.New("certificate verification failed")}},
			wantCode: transport.CodeIMAPConnectFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			local := operationError(test.localCode, "local message content is incomplete")
			combined := newHydrationError("read message", local, test.remote)
			if got := transport.ErrorCode(combined); got != test.wantCode {
				t.Fatalf("hydration error code = %q, want %q", got, test.wantCode)
			}
			if test.remote != nil && !errors.Is(combined, test.remote) {
				t.Fatalf("combined hydration error lost remote cause %v", test.remote)
			}
		})
	}
}
