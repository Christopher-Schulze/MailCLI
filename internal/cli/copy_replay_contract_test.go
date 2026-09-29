package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

func TestCopyReplayDocumentationDoesNotPromiseExactlyOnce(t *testing.T) {
	for _, path := range []string{"README.md", "docs/documentation.md", "skills/mailcli/SKILL.md", "skills/mailcli/references/mutations.md"} {
		content := strings.Join(strings.Fields(readRepositoryFile(t, path)), " ")
		for _, claim := range []string{"so a retry never duplicates a message", "No duplicate send or copy", "concurrent processes cannot both copy", "two processes cannot both see an empty destination and both copy"} {
			if strings.Contains(content, claim) {
				t.Errorf("%s promises unproven COPY replay safety: %q", path, claim)
			}
		}
		if path == "docs/documentation.md" {
			for _, required := range []string{"replay_allowed:false", "a subsequent process cannot inherit", "an absent immediate match does not prove no COPY occurred", "no cross-process exactly-once guarantee"} {
				if !strings.Contains(content, required) {
					t.Errorf("COPY manual omits %q", required)
				}
			}
		}
	}
}

func TestCopyUnknownResponseForbidsGeneratedWriteReplay(t *testing.T) {
	for _, outcome := range []string{transport.MutationOutcomeNotStarted, transport.MutationOutcomeUnknown} {
		t.Run(outcome, func(t *testing.T) {
			gateway := &moveFlagResultGateway{err: &transport.MutationOutcomeError{
				Code: transport.CodeIMAPCopyOutcomeUnknown, Message: "COPY requires destination observation",
				Evidence: transport.MutationEvidence{Command: "COPY", OperationID: "copy_retained", Outcome: outcome},
			}}
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), mail.NewService(gateway), []string{"messages", "copy", "--ref", "msg_ref", "--mailbox", "mbx_ref", "--json"}, &stdout, &stderr)
			var response envelope
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if code != 1 || response.OK || response.Command != "messages.copy" || stderr.Len() != 0 || gateway.calls != 1 || response.Error == nil || response.Error.Code != transport.CodeIMAPCopyOutcomeUnknown {
				t.Fatalf("COPY failure response: exit=%d stdout=%s stderr=%s calls=%d", code, stdout.String(), stderr.String(), gateway.calls)
			}
			guidance := response.Error.Guidance
			if guidance == nil || guidance.ReplayAllowed || guidance.Retryability != mail.RetryObserveRequired || guidance.Recovery.Action != mail.RecoveryObserve || guidance.Recovery.OperationID != "copy_retained" || guidance.Recovery.Command != "" || len(guidance.Recovery.Args) != 0 {
				t.Fatalf("COPY guidance permits replay or loses evidence: %+v", guidance)
			}
			if response.Next == nil || response.Next.Do != "check_state" || response.Next.Command != "" || len(response.Next.Args) != 0 {
				t.Fatalf("COPY next action repeats a write: %+v", response.Next)
			}
		})
	}
}
