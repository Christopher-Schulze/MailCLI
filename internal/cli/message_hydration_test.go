package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"mailcli/internal/mail"
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
	code := Run(context.Background(), service, []string{"drafts", "open", "--message", "msg_ref", "--json"}, &stdout, &stderr)
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
