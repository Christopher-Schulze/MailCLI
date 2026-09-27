package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

type hydrationMessageGateway struct {
	testGateway
	message mail.Message
	err     error
}

func (g hydrationMessageGateway) GetMessage(context.Context, string) (mail.Message, error) {
	return g.message, g.err
}

func (g hydrationMessageGateway) OpenDraft(context.Context, string) (mail.Message, error) {
	return g.message, g.err
}

func failedHydrationMessage() mail.Message {
	return mail.Message{
		Summary:         mail.MessageSummary{Ref: "msg_ref", Subject: "Partial"},
		Content:         "available text",
		ContentSource:   "emlx_partial",
		ContentComplete: false,
		MissingParts:    []string{"mime-decoding"},
		Hydration: &mail.HydrationDiagnostic{
			State:           mail.HydrationStateFailed,
			AttemptedSource: "imap",
			Local:           &mail.HydrationCause{Code: "raw_source_partial", Message: "local EMLX source is partial"},
			Remote:          &mail.HydrationCause{Code: "imap_auth_failed", Message: "IMAP authentication failed"},
			Remediation:     "restore the credential, then retry `mailcli messages get --ref REF`",
		},
	}
}

func TestMessagesGetJSONRetainsPartialMessageOnHydrationFailure(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	message := failedHydrationMessage()
	service := mail.NewService(hydrationMessageGateway{
		message: message,
		err:     &testCodedError{code: "imap_auth_failed", message: "AUTHENTICATIONFAILED secret-token"},
	})
	code := Run(context.Background(), service, []string{"messages", "get", "--ref", "msg_ref", "--json"}, &stdout, &stderr)
	if code != 1 || stderr.Len() != 0 {
		t.Fatalf("Run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output = %q", err, stdout.String())
	}
	if response.OK || response.Error == nil || response.Error.Code != "imap_auth_failed" || response.Data.Message == nil {
		t.Fatalf("response = %+v", response)
	}
	if response.Data.Message.Content != "available text" || response.Data.Message.ContentComplete ||
		response.Data.Message.Hydration == nil || response.Data.Message.Hydration.Remote == nil ||
		response.Data.Message.Hydration.Remote.Code != "imap_auth_failed" {
		t.Fatalf("partial message = %+v", response.Data.Message)
	}
	if strings.Contains(stdout.String(), "secret-token") {
		t.Fatalf("JSON leaked raw authentication text: %s", stdout.String())
	}
}

func TestMessagesGetUntypedHydrationFailureKeepsUnknownOrigin(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	message := failedHydrationMessage()
	message.Hydration.Remote = &mail.HydrationCause{Code: "hydration_failed", Message: "IMAP hydration failed"}
	message.Hydration.Remediation = "resolve the IMAP failure, then retry `mailcli messages get --ref REF`"
	service := mail.NewService(hydrationMessageGateway{
		message: message,
		err:     &testCodedError{code: "hydration_failed", message: "remote spool failure secret-token"},
	})
	code := Run(context.Background(), service, []string{"messages", "get", "--ref", "msg_ref", "--json"}, &stdout, &stderr)
	if code != 1 || stderr.Len() != 0 {
		t.Fatalf("Run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output = %q", err, stdout.String())
	}
	if response.OK || response.Error == nil || response.Error.Code != "hydration_failed" || response.Data.Message == nil {
		t.Fatalf("response = %+v", response)
	}
	if response.Error.Guidance == nil || response.Error.Guidance.Phase != mail.OperationPhaseHydration ||
		response.Error.Guidance.EffectCertainty != mail.EffectNone ||
		response.Error.Guidance.Retryability != mail.RetryObserveRequired || response.Error.Guidance.ReplayAllowed ||
		response.Error.Guidance.Recovery.Action != mail.RecoveryInspect {
		t.Fatalf("unknown hydration guidance = %+v", response.Error.Guidance)
	}
	message = *response.Data.Message
	if message.Summary.Ref != "msg_ref" || message.Content != "available text" || message.ContentComplete ||
		message.Hydration == nil || message.Hydration.AttemptedSource != "imap" ||
		message.Hydration.Local == nil || message.Hydration.Local.Code != "raw_source_partial" ||
		message.Hydration.Remote == nil || message.Hydration.Remote.Code != "hydration_failed" {
		t.Fatalf("unknown-origin partial message = %+v", message)
	}
	if strings.Contains(stdout.String(), "secret-token") || strings.Contains(stdout.String(), "remote spool failure") {
		t.Fatalf("serialized failure leaked raw remote details: %s", stdout.String())
	}
}

func TestMessagesGetHumanRetainsPartialMessageOnHydrationFailure(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	message := failedHydrationMessage()
	message.Hydration.Remote = &mail.HydrationCause{
		Code: "imap_timeout", Message: "IMAP hydration timed out",
	}
	service := mail.NewService(hydrationMessageGateway{
		message: message,
		err:     &testCodedError{code: "imap_timeout", message: "raw protocol timeout secret-token"},
	})
	code := Run(context.Background(), service, []string{"messages", "get", "--ref", "msg_ref"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("Run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	for _, wanted := range []string{
		"Content complete: false",
		"Hydration state: failed",
		"Hydration source: imap",
		"Hydration remote: imap_timeout: IMAP hydration timed out",
		"available text",
	} {
		if !strings.Contains(stdout.String(), wanted) {
			t.Fatalf("stdout = %q, want %q", stdout.String(), wanted)
		}
	}
	if !strings.Contains(stderr.String(), "message content is incomplete") || strings.Contains(stderr.String(), "secret-token") {
		t.Fatalf("stderr = %q, want safe incomplete-content failure", stderr.String())
	}
}

func TestDraftsOpenUsesPartialHydrationFailureEnvelope(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	service := mail.NewService(hydrationMessageGateway{
		message: failedHydrationMessage(),
		err:     &testCodedError{code: "operation_canceled", message: "canceled"},
	})
	code := Run(context.Background(), service, []string{"drafts", "open", "--ref", "msg_ref", "--json"}, &stdout, &stderr)
	if code != 1 || stderr.Len() != 0 {
		t.Fatalf("Run() code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if response.OK || response.Command != "drafts.open" || response.Data.Message == nil || response.Error == nil {
		t.Fatalf("response = %+v", response)
	}
}

type testCodedError struct {
	code    string
	message string
}

func (e *testCodedError) Error() string     { return e.message }
func (e *testCodedError) ErrorCode() string { return e.code }

func TestHydrationEnvelopePreservesCausePolicyAndSanitizesDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name                string
		err                 error
		retry               mail.Retryability
		action              mail.RecoveryAction
		effect              mail.EffectCertainty
		instructionContains string
	}{
		{name: "TLS verification", err: &transport.TransportError{Code: transport.CodeIMAPConnectFailed,
			Err: &tls.CertificateVerificationError{Err: errors.New("private-host.invalid certificate secret-token")}},
			retry: mail.RetryUserInputRequired, action: mail.RecoveryCorrect, effect: mail.EffectNone},
		{name: "invalid source", err: &transport.TransportError{Code: transport.CodeIMAPFetchFailed},
			retry: mail.RetryTerminal, action: mail.RecoveryInspect, effect: mail.EffectNone},
		{name: "FETCH disconnect", err: &transport.TransportError{Code: transport.CodeIMAPFetchFailed, Err: io.EOF},
			retry: mail.RetrySafe, action: mail.RecoveryRetry, effect: mail.EffectNone},
		{name: "unknown UID identity", err: &transport.TransportError{Code: transport.CodeIMAPMessageUIDUnknown,
			Message: "no independently verified IMAP UID is available"},
			retry: mail.RetryUserInputRequired, action: mail.RecoveryCorrect, effect: mail.EffectNone,
			instructionContains: "verified mailbox UID"},
		{name: "uncertain outcome", err: &transport.MutationOutcomeError{Code: transport.CodeIMAPMoveOutcomeUnknown,
			Err: &transport.TransportError{Code: transport.CodeIMAPConnectFailed,
				Err: &tls.CertificateVerificationError{Err: errors.New("secret-token")}}},
			retry: mail.RetryObserveRequired, action: mail.RecoveryObserve, effect: mail.EffectUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := failedHydrationMessage()
			message.Hydration.Remote = &mail.HydrationCause{Code: transport.ErrorCode(test.err), Message: "IMAP hydration failed"}
			message.Hydration.Remediation = "Inspect the IMAP configuration before retrying."
			wrapped := hydrationCommandError(message.Hydration, test.err)
			if !errors.Is(wrapped, test.err) {
				t.Fatal("hydration command discarded its original cause")
			}
			for _, command := range []string{"messages.get", "drafts.open"} {
				var stdout, stderr bytes.Buffer
				if code := failMessageRead(command, true, message, test.err, &stdout, &stderr); code != 1 || stderr.Len() != 0 {
					t.Fatalf("failure code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				var response envelope
				if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				guidance := response.Error.Guidance
				if response.OK || response.Error.Code != transport.ErrorCode(test.err) || guidance == nil ||
					guidance.Retryability != test.retry || guidance.Recovery.Action != test.action ||
					guidance.EffectCertainty != test.effect || guidance.ReplayAllowed != (test.retry == mail.RetrySafe) ||
					(test.retry == mail.RetryUserInputRequired && guidance.Recovery.Instruction == "") ||
					(test.instructionContains != "" && !strings.Contains(guidance.Recovery.Instruction, test.instructionContains)) {
					t.Fatalf("hydration policy = %+v", response.Error)
				}
				if test.name == "unknown UID identity" {
					argument := "--ref"
					if command == "drafts.open" {
						argument = "--ref"
					}
					if guidance.Phase != mail.OperationPhaseHydration || guidance.Recovery.Command != command ||
						!equalStrings(guidance.Recovery.Args, []string{argument, "msg_ref", "--json"}) {
						t.Fatalf("hydration identity recovery = %+v", guidance)
					}
				}
				if strings.Contains(stdout.String(), "secret-token") || strings.Contains(stdout.String(), "private-host.invalid") {
					t.Fatalf("raw cause leaked: %s", stdout.String())
				}
				if test.name == "TLS verification" || test.name == "uncertain outcome" {
					stdout.Reset()
					if code := failCommand(command, true, test.err, &stdout, &stderr); code != 1 ||
						strings.Contains(stdout.String(), "secret-token") || strings.Contains(stdout.String(), "private-host.invalid") {
						t.Fatalf("direct JSON exposed raw TLS cause: code=%d %s", code, stdout.String())
					}
					if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					if response.Error.Code != transport.ErrorCode(test.err) || response.Error.Guidance.Retryability != test.retry || response.Error.Guidance.EffectCertainty != test.effect {
						t.Fatalf("direct TLS policy changed: %+v", response.Error)
					}
					if code := failCommand(command, false, test.err, &stdout, &stderr); code != 1 || strings.Contains(stderr.String(), "secret-token") {
						t.Fatalf("human failure exposed TLS cause: code=%d %s", code, stderr.String())
					}
					stderr.Reset()
				}
			}
		})
	}
}
