package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/transport"
)

func TestGuidanceForProtocolFailuresBeforeTerminators(t *testing.T) {
	tests := []struct {
		name          string
		command       string
		code          string
		phase         OperationPhase
		effect        EffectCertainty
		retryability  Retryability
		replayAllowed bool
		recovery      RecoveryAction
	}{
		{
			name: "mutation lock setup requires correction", command: "drafts.send",
			code: transport.CodeIMAPLockUnavailable, phase: OperationPhaseValidation,
			effect: EffectNone, retryability: RetryUserInputRequired, recovery: RecoveryCorrect,
		},
		{
			name: "mutation lock contention permits retry", command: "messages.mark",
			code: transport.CodeIMAPAccountBusy, phase: OperationPhaseMutation,
			effect: EffectNone, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry,
		},
		{
			name: "SMTP source integrity requires inspection", command: "drafts.send",
			code: transport.CodeSMTPSourceInvalid, phase: OperationPhaseSubmission,
			effect: EffectNone, retryability: RetryObserveRequired, recovery: RecoveryInspect,
		},
		{
			name:          "SMTP DATA incomplete before terminator",
			command:       "drafts.send",
			code:          transport.CodeSMTPDataIncomplete,
			phase:         OperationPhaseSubmission,
			effect:        EffectNone,
			retryability:  RetrySafe,
			replayAllowed: true,
			recovery:      RecoveryRetry,
		},
		{
			name:          "SMTP submission unknown after terminator",
			command:       "drafts.send",
			code:          transport.CodeSMTPSubmissionUnknown,
			phase:         OperationPhaseSubmission,
			effect:        EffectUnknown,
			retryability:  RetryObserveRequired,
			replayAllowed: false,
			recovery:      RecoveryReconcile,
		},
		{
			name:          "APPEND incomplete during send",
			command:       "drafts.send",
			code:          transport.CodeIMAPAppendIncomplete,
			phase:         OperationPhaseMirror,
			effect:        EffectPartial,
			retryability:  RetryObserveRequired,
			replayAllowed: false,
			recovery:      RecoveryReconcile,
		},
		{
			name:          "APPEND incomplete during reconcile",
			command:       "drafts.reconcile",
			code:          transport.CodeIMAPAppendIncomplete,
			phase:         OperationPhaseMirror,
			effect:        EffectNone,
			retryability:  RetrySafe,
			replayAllowed: true,
			recovery:      RecoveryRetry,
		},
		{
			name:          "APPEND outcome unknown after terminator",
			command:       "drafts.reconcile",
			code:          transport.CodeIMAPAppendOutcomeUnknown,
			phase:         OperationPhaseMirror,
			effect:        EffectPartial,
			retryability:  RetryObserveRequired,
			replayAllowed: false,
			recovery:      RecoveryReconcile,
		},
		{
			name:          "composite command stays conservative",
			command:       "batch",
			code:          transport.CodeSMTPDataIncomplete,
			phase:         OperationPhaseExecution,
			effect:        EffectUnknown,
			retryability:  RetryObserveRequired,
			replayAllowed: false,
			recovery:      RecoveryInspect,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := GuidanceForError(test.command, &transport.TransportError{Code: test.code, Message: "injected protocol failure"})
			if got.Phase != test.phase || got.EffectCertainty != test.effect ||
				got.Retryability != test.retryability || got.ReplayAllowed != test.replayAllowed ||
				got.Recovery.Action != test.recovery {
				t.Fatalf("GuidanceForError(%q, %s) = %+v", test.command, test.code, got)
			}
		})
	}
}

func TestAttachmentSaveGuidanceRequiresClassifiedTransientCause(t *testing.T) {
	type guidanceCase struct {
		name         string
		cause        error
		certainty    EffectCertainty
		effect       EffectCertainty
		retryability Retryability
		replay       bool
		recovery     RecoveryAction
	}

	check := func(t *testing.T, test guidanceCase) {
		t.Helper()
		if test.certainty == "" {
			test.certainty = EffectNone
		}
		outcome := &AttachmentSaveOutcomeError{
			Cause: test.cause, Phase: OperationPhaseExecution, EffectCertainty: test.certainty,
		}
		single := GuidanceForError("attachments.save", outcome)
		batchItem := (&batchExecution{request: BatchRequest{Operation: BatchOperationAttachmentSave}}).itemError(outcome)
		if batchItem.Guidance == nil || !reflect.DeepEqual(batchItem.Guidance, &single) {
			t.Fatalf("single guidance = %+v, batch guidance = %+v", single, batchItem.Guidance)
		}
		if single.EffectCertainty != test.effect || single.Retryability != test.retryability ||
			single.ReplayAllowed != test.replay || single.Recovery.Action != test.recovery ||
			batchItem.Retryable != test.replay {
			t.Fatalf("single guidance = %+v, batch retryable = %t", single, batchItem.Retryable)
		}
	}

	var cases []guidanceCase
	addCodes := func(codes []string, retryability Retryability, replay bool, recovery RecoveryAction) {
		for _, code := range codes {
			cases = append(cases, guidanceCase{
				name: code, cause: &OperationError{Code: code, Message: "attachment save failure"},
				certainty: EffectNone, effect: EffectNone, retryability: retryability,
				replay: replay, recovery: recovery,
			})
		}
	}
	addCodes([]string{
		"invalid_argument", "invalid_input", "missing_required", "not_found", "ambiguous_attachment",
		"attachment_not_downloaded", "invalid_reference", "ambiguous_reference", "stale_reference",
		"store_bound_reference_required", "message_source_missing", "raw_source_partial", "content_incomplete",
		"local_only_mailbox", "mailbox_uidvalidity_changed", "account_disabled", "account_degraded",
		"account_identity_missing", "account_binding_invalid", "account_binding_host_invalid",
		"account_binding_provider_mismatch", "account_binding_version_unsupported", "account_binding_unavailable",
		"account_binding_stale", "account_reference_version_unsupported", "mail_store_unavailable",
		"mail_store_preferences_unavailable", "mail_store_preferences_invalid", "safe_mailbox_listing_unavailable",
		"safe_search_unavailable", "mail_automation_denied", "mail_not_running", "mail_recovery_required",
		transport.CodeIMAPMessageUIDUnknown, transport.CodeIMAPAmbiguousMessageID,
		transport.CodeIMAPMailboxNotFound, transport.CodeIMAPMessageNotFound, transport.CodeIMAPAmbiguousMailbox,
	}, RetryUserInputRequired, false, RecoveryCorrect)
	addCodes([]string{
		"attachment_resource_limit", "account_catalog_incomplete", "ambiguous_mail_store_generation",
		"account_reference_corrupt", "account_reference_invalid", "ambiguous_message_source",
		"content_export_changed", "imap_flag_read_unsupported", "invalid_emlx", "invalid_mailbox_cache",
		"invalid_message_source", "mail_store_not_read_only", "mailbox_cache_malformed", "mail_store_path_mismatch",
		"mailbox_catalog_incomplete", "mailbox_info_malformed", "mime_resource_limit", "raw_source_too_large",
		"store_changed", "unsafe_message_source", "unsupported_mail_store_schema", "search_unavailable",
		"draft_state_error", "send_receipt_invalid", transport.CodeIMAPMessageUIDMismatch,
		transport.CodeIMAPResponseMalformed, transport.CodeIMAPUIDValidityUnknown, "attachment_changed",
		transport.CodeIMAPFetchFailed, transport.CodeIMAPResourceLimitExceeded,
	}, RetryTerminal, false, RecoveryInspect)
	addCodes([]string{
		transport.CodeSMTPAuthFailed, transport.CodeSMTPCredentialsMissing, transport.CodeIMAPAuthFailed,
		transport.CodeSMTPTLSFailed, transport.CodeUnsupportedProvider, transport.CodeSMTPUTF8Unsupported,
	}, RetryUserInputRequired, false, RecoveryCorrect)
	addCodes([]string{
		transport.CodeIMAPConnectFailed, transport.CodeIMAPCanceled, transport.CodeIMAPDisconnected,
		transport.CodeIMAPTimeout, "operation_canceled", "operation_timeout", "mail_busy",
		"mail_automation_timeout", "mail_process_changed",
	}, RetrySafe, true, RecoveryRetry)
	addCodes([]string{"operation_failed", "future_attachment_failure", "invalid_future_attachment_failure"},
		RetryObserveRequired, false, RecoveryInspect)
	cases = append(cases,
		guidanceCase{
			name: "canceled context", cause: context.Canceled, certainty: EffectNone,
			effect: EffectNone, retryability: RetrySafe, replay: true, recovery: RecoveryRetry,
		},
		guidanceCase{
			name: "deadline context", cause: context.DeadlineExceeded, certainty: EffectNone,
			effect: EffectNone, retryability: RetrySafe, replay: true, recovery: RecoveryRetry,
		},
		guidanceCase{
			name: "network-truncated FETCH", cause: &transport.TransportError{
				Code: transport.CodeIMAPFetchFailed, Err: io.ErrUnexpectedEOF,
			}, certainty: EffectNone, effect: EffectNone, retryability: RetrySafe, replay: true, recovery: RecoveryRetry,
		},
		guidanceCase{
			name: "TLS verification failure", cause: &transport.TransportError{
				Code: transport.CodeIMAPConnectFailed,
				Err:  &tls.CertificateVerificationError{Err: errors.New("untrusted certificate")},
			}, certainty: EffectNone, effect: EffectNone, retryability: RetryUserInputRequired,
			replay: false, recovery: RecoveryCorrect,
		},
		guidanceCase{
			name: "filesystem permission", cause: fs.ErrPermission, certainty: EffectNone,
			effect: EffectNone, retryability: RetryUserInputRequired, replay: false, recovery: RecoveryCorrect,
		},
		guidanceCase{
			name: "untyped filesystem failure", cause: errors.New("read failed"), certainty: EffectNone,
			effect: EffectNone, retryability: RetryObserveRequired, replay: false, recovery: RecoveryInspect,
		},
		guidanceCase{
			name: "untyped cancellation with other code", cause: &OperationError{
				Code: "operation_failed", Message: "operation stopped", Err: context.Canceled,
			}, certainty: EffectNone, effect: EffectNone, retryability: RetrySafe, replay: true, recovery: RecoveryRetry,
		},
	)

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) { check(t, test) })
	}
	for _, certainty := range []EffectCertainty{EffectComplete, EffectPartial, EffectUnknown} {
		t.Run(string(certainty)+" outcome blocks replay", func(t *testing.T) {
			check(t, guidanceCase{
				name: string(certainty), cause: context.DeadlineExceeded, certainty: certainty,
				effect: certainty, retryability: RetryObserveRequired, replay: false, recovery: RecoveryInspect,
			})
		})
	}
	t.Run("invalid outcome certainty fails closed", func(t *testing.T) {
		check(t, guidanceCase{
			name: "invalid certainty", cause: errors.New("unknown outcome"), certainty: "future",
			effect: EffectUnknown, retryability: RetryObserveRequired, replay: false, recovery: RecoveryInspect,
		})
	})
}

func TestResourceLimitGuidancePreservesDispatchUncertainty(t *testing.T) {
	resource := &transport.TransportError{
		Code: transport.CodeIMAPResourceLimitExceeded, Message: "bounded response exceeded",
		Limit: &transport.ResourceLimit{Name: "untagged logical response lines", Value: 10000}, ObservedAtLeast: 10001,
	}
	for _, command := range []string{"mailboxes.list", "messages.delete", "drafts.reconcile", BatchOperationMark} {
		t.Run(command, func(t *testing.T) {
			guidance := GuidanceForError(command, fmt.Errorf("lookup failed: %w", resource))
			if guidance.EffectCertainty != EffectNone || guidance.Retryability != RetryTerminal ||
				guidance.ReplayAllowed || guidance.Recovery.Action != RecoveryInspect {
				t.Fatalf("resource guidance = %+v", guidance)
			}
		})
	}
	for _, test := range []struct {
		name    string
		command string
		err     error
		effect  EffectCertainty
	}{
		{name: "SMTP", command: "drafts.send", err: &transport.SubmissionError{Stage: "reply", Err: resource}, effect: EffectUnknown},
		{name: "APPEND", command: "drafts.reconcile", err: errors.Join(resource, &transport.TransportError{Code: transport.CodeIMAPAppendOutcomeUnknown}), effect: EffectPartial},
		{name: "MOVE", command: "messages.move", err: &transport.MutationOutcomeError{Code: transport.CodeIMAPMoveOutcomeUnknown, Evidence: transport.MutationEvidence{Command: "MOVE"}, Err: resource}, effect: EffectUnknown},
		{name: "partial STORE", command: "messages.mark", err: &transport.MutationOutcomeError{Code: transport.CodeIMAPFlagsPartial, Evidence: transport.MutationEvidence{Command: "STORE", Outcome: transport.MutationOutcomePartial}, Err: resource}, effect: EffectPartial},
		{name: "composite batch", command: "batch", err: resource, effect: EffectUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			guidance := GuidanceForError(test.command, test.err)
			if guidance.EffectCertainty != test.effect || guidance.Retryability != RetryObserveRequired || guidance.ReplayAllowed {
				t.Fatalf("resource cause erased dispatch evidence: %+v", guidance)
			}
		})
	}
}

type searchBudgetGuidanceError struct {
	requiredBytes int64
}

func (e *searchBudgetGuidanceError) Error() string {
	return "search candidate exceeds byte budget"
}

func (e *searchBudgetGuidanceError) ErrorCode() string {
	return "search_budget_too_small"
}

func (e *searchBudgetGuidanceError) RequiredBytes() int64 {
	return e.requiredBytes
}

func TestGuidanceForSearchBudgetTooSmallRequiresCorrectedRestart(t *testing.T) {
	for _, command := range []string{"messages.search", "messages.filter"} {
		got := GuidanceForError(command, &searchBudgetGuidanceError{requiredBytes: 4096})
		if got.Phase != OperationPhaseRead || got.EffectCertainty != EffectNone ||
			got.Retryability != RetryUserInputRequired || got.ReplayAllowed ||
			got.Recovery.Action != RecoveryCorrect || got.Recovery.Command != "" || len(got.Recovery.Args) != 0 {
			t.Fatalf("GuidanceForError(%q) = %+v", command, got)
		}
	}
}

func TestGuidanceForNotFoundRequiresFreshInputForReads(t *testing.T) {
	err := &transport.TransportError{Code: "not_found", Message: "missing message"}
	tests := []struct {
		name    string
		command string
	}{
		{name: "messages.get", command: "messages.get"},
		{name: "messages.raw", command: "messages.raw"},
		{name: "drafts.inspect", command: "drafts.inspect"},
		{name: "drafts.open", command: "drafts.open"},
		{name: "attachments.list", command: "attachments.list"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := GuidanceForError(test.command, err)
			if got.Phase != OperationPhaseRead || got.EffectCertainty != EffectNone ||
				got.Retryability != RetryUserInputRequired || got.ReplayAllowed ||
				got.Recovery.Action != RecoveryCorrect || got.Recovery.Command != "" || len(got.Recovery.Args) != 0 {
				t.Fatalf("GuidanceForError(%q, not_found) = %+v", test.command, got)
			}
		})
	}
}

func TestGuidanceForDraftAdoptionTracksPublicationBoundary(t *testing.T) {
	tests := []struct {
		name          string
		adoption      *DraftAdoptionError
		phase         OperationPhase
		effect        EffectCertainty
		retryability  Retryability
		replayAllowed bool
		recovery      RecoveryAction
		command       string
		args          []string
	}{
		{
			name:     "missing source before publication",
			adoption: &DraftAdoptionError{Err: &OperationError{Code: "not_found", Message: "source draft missing"}},
			phase:    OperationPhaseRead, effect: EffectNone, retryability: RetryUserInputRequired,
			recovery: RecoveryCorrect,
		},
		{
			name:     "incomplete source needs materialization",
			adoption: &DraftAdoptionError{Err: &OperationError{Code: "adopt_source_incomplete", Message: "source content incomplete"}},
			phase:    OperationPhaseRead, effect: EffectNone, retryability: RetryUserInputRequired,
			recovery: RecoveryCorrect,
		},
		{
			name:     "transient source read before publication",
			adoption: &DraftAdoptionError{Err: &OperationError{Code: "operation_timeout", Message: "source read timed out"}},
			phase:    OperationPhaseRead, effect: EffectNone, retryability: RetrySafe,
			replayAllowed: true, recovery: RecoveryRetry,
		},
		{
			name:     "retained staging blocks replay",
			adoption: &DraftAdoptionError{Ref: "draft_adopted", StagingPath: "/tmp/staging", StagingRetained: true, Err: errors.New("cleanup failed")},
			phase:    OperationPhaseExecution, effect: EffectUnknown, retryability: RetryObserveRequired,
			recovery: RecoveryInspect, command: "drafts.list", args: []string{"--json"},
		},
		{
			name:     "publication blocks replay even for not found cause",
			adoption: &DraftAdoptionError{Ref: "draft_adopted", PublicationStarted: true, Err: &OperationError{Code: "not_found", Message: "post-publication failure"}},
			phase:    OperationPhaseExecution, effect: EffectUnknown, retryability: RetryObserveRequired,
			recovery: RecoveryInspect, command: "drafts.list", args: []string{"--json"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := GuidanceForError("drafts.adopt", test.adoption)
			if got.Phase != test.phase || got.EffectCertainty != test.effect ||
				got.Retryability != test.retryability || got.ReplayAllowed != test.replayAllowed ||
				got.Recovery.Action != test.recovery || got.Recovery.Command != test.command ||
				!reflect.DeepEqual(got.Recovery.Args, test.args) {
				t.Fatalf("GuidanceForError(drafts.adopt) = %+v", got)
			}
			if test.name == "incomplete source needs materialization" &&
				!strings.Contains(got.Recovery.Instruction, "Open the source draft in Mail.app") {
				t.Fatalf("incomplete-source instruction = %q", got.Recovery.Instruction)
			}
		})
	}
}

func TestGuidanceForKnownReadErrorsHasExplicitPolicy(t *testing.T) {
	tests := []struct {
		command             string
		code                string
		phase               OperationPhase
		retryability        Retryability
		replayAllowed       bool
		recovery            RecoveryAction
		instruction         bool
		instructionContains string
	}{
		{code: transport.CodeIMAPConnectFailed, phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{code: transport.CodeIMAPCanceled, phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{code: transport.CodeIMAPDisconnected, phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{code: transport.CodeIMAPTimeout, phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{command: "doctor", code: "mail_automation_timeout", phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{command: "doctor", code: "mail_busy", phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{command: "messages.list", code: "mail_process_changed", phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{code: transport.CodeIMAPFetchFailed, phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "operation_canceled", phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{code: "draft_operation_canceled", phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{code: "operation_timeout", phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{code: "mail_store_unavailable", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "mail_store_preferences_unavailable", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "mail_store_preferences_invalid", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "safe_mailbox_listing_unavailable", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "safe_search_unavailable", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "account_disabled", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "Enable this account in Mail.app"},
		{code: "account_degraded", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "mailcli accounts list --json"},
		{code: "account_identity_missing", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "mailcli send setup"},
		{code: "account_binding_invalid", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "preserve existing binding data"},
		{code: "account_binding_host_invalid", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "host and port"},
		{code: "account_binding_provider_mismatch", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "same supported provider"},
		{code: "account_binding_version_unsupported", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "preserve the binding file"},
		{code: "account_reference_version_unsupported", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "supports the account-reference version"},
		{code: "account_binding_unavailable", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "preserve the file contents"},
		{code: "account_binding_stale", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "enabled account"},
		{code: "mail_automation_denied", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "Privacy & Security > Automation"},
		{code: "mail_not_running", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "Open Mail.app"},
		{code: "mail_recovery_required", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "Quit and reopen Mail.app"},
		{code: transport.CodeIMAPAuthFailed, phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "ambiguous_reference", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "current reference"},
		{code: "stale_cursor", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "fresh next_cursor"},
		{code: "ambiguous_attachment", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "search_count_limit_exceeded", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "invalid_reference", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "opaque reference"},
		{code: "stale_reference", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "message_source_missing", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: "raw_source_partial", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "complete source"},
		{code: transport.CodeIMAPMessageUIDUnknown, phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "verified mailbox UID"},
		{code: transport.CodeIMAPAmbiguousMessageID, phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "ambiguous message matches"},
		{code: "content_unavailable", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "remove `--export`"},
		{code: "content_incomplete", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "finish downloading this message"},
		{code: "local_only_mailbox", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "local store"},
		{code: "store_bound_reference_required", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "store-bound message reference"},
		{code: "mailbox_uidvalidity_changed", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "fresh message reference"},
		{code: transport.CodeIMAPMailboxNotFound, phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: transport.CodeIMAPMessageNotFound, phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: transport.CodeIMAPAmbiguousMailbox, phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true},
		{code: transport.CodeIMAPMutationFailed, command: "messages.get", phase: OperationPhaseRead, retryability: RetryObserveRequired, recovery: RecoveryInspect},
		{code: transport.CodeIMAPSentMailboxNotFound, command: "messages.get", phase: OperationPhaseRead, retryability: RetryObserveRequired, recovery: RecoveryInspect},
		{code: "mail_error", command: "messages.list", phase: OperationPhaseRead, retryability: RetryObserveRequired, recovery: RecoveryInspect},
		{code: "bridge_cleanup_failed", command: "messages.list", phase: OperationPhaseRead, retryability: RetryObserveRequired, recovery: RecoveryInspect},
		{code: "mail_automation_failed", command: "doctor", phase: OperationPhaseRead, retryability: RetryObserveRequired, recovery: RecoveryInspect},
		{code: "mail_access_gate_failed", command: "doctor", phase: OperationPhaseRead, retryability: RetryObserveRequired, recovery: RecoveryInspect},
		{code: "mail_app_unavailable", command: "doctor", phase: OperationPhaseRead, retryability: RetryObserveRequired, recovery: RecoveryInspect},
		{code: "mail_scripting_unavailable", command: "doctor", phase: OperationPhaseRead, retryability: RetryObserveRequired, recovery: RecoveryInspect},
		{code: "osascript_unavailable", command: "doctor", phase: OperationPhaseRead, retryability: RetryObserveRequired, recovery: RecoveryInspect},
		{code: "environment_unhealthy", command: "doctor", phase: OperationPhaseRead, retryability: RetryObserveRequired, recovery: RecoveryInspect},
		{code: "operation_failed", command: "doctor", phase: OperationPhaseRead, retryability: RetryObserveRequired, recovery: RecoveryInspect},
		{code: "invalid_request", command: "messages.list", phase: OperationPhaseRead, retryability: RetryObserveRequired, recovery: RecoveryInspect},
		{code: "mail_access_gate_corrupt", command: "doctor", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect, instruction: true, instructionContains: "Quit Mail.app"},
		{code: "mail_access_gate_unsafe", command: "doctor", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect, instruction: true, instructionContains: "No Mail.app action was dispatched"},
		{code: "unsupported_platform", command: "doctor", phase: OperationPhaseRead, retryability: RetryObserveRequired, recovery: RecoveryInspect},
		{code: "unsupported_architecture", command: "doctor", phase: OperationPhaseRead, retryability: RetryObserveRequired, recovery: RecoveryInspect},
		{code: "account_reference_corrupt", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "account_reference_invalid", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "mail_store_path_mismatch", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "content_export_changed", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "unsupported_mail_store_schema", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "ambiguous_mail_store_generation", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "unsafe_message_source", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "raw_source_too_large", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "attachment_resource_limit", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "mime_resource_limit", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "invalid_emlx", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "invalid_message_source", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "ambiguous_message_source", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "store_changed", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "mail_store_not_read_only", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "invalid_mailbox_cache", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "mailbox_cache_malformed", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "imap_flag_read_unsupported", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "account_catalog_incomplete", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "mailbox_catalog_incomplete", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "mailbox_info_malformed", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "imap_message_uid_mismatch", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "draft_state_error", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "send_receipt_invalid", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "imap_response_malformed", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "mailbox_uidvalidity_unknown", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "search_unavailable", phase: OperationPhaseRead, retryability: RetryTerminal, recovery: RecoveryInspect},
		{code: "search_cursor_stale", phase: OperationPhaseRead, retryability: RetryUserInputRequired, recovery: RecoveryCorrect},
		{code: "search_index_changed", phase: OperationPhaseRead, retryability: RetrySafe, replayAllowed: true, recovery: RecoveryRetry},
		{code: "invalid_argument", phase: OperationPhaseValidation, retryability: RetryUserInputRequired, recovery: RecoveryCorrect},
		{code: "invalid_cursor", phase: OperationPhaseValidation, retryability: RetryUserInputRequired, recovery: RecoveryCorrect},
	}
	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			command := test.command
			if command == "" {
				command = "messages.get"
			}
			got := GuidanceForError(command, &transport.TransportError{Code: test.code, Message: "injected read failure"})
			if got.Phase != test.phase || got.EffectCertainty != EffectNone ||
				got.Retryability != test.retryability || got.ReplayAllowed != test.replayAllowed ||
				got.Recovery.Action != test.recovery || (got.Recovery.Instruction != "") != test.instruction ||
				(test.instructionContains != "" && !strings.Contains(got.Recovery.Instruction, test.instructionContains)) {
				t.Fatalf("GuidanceForError(%q) = %+v", test.code, got)
			}
		})
	}
	permission := GuidanceForError("messages.get", fs.ErrPermission)
	if permission.Phase != OperationPhaseRead || permission.EffectCertainty != EffectNone ||
		permission.Retryability != RetryUserInputRequired || permission.ReplayAllowed ||
		permission.Recovery.Action != RecoveryCorrect || permission.Recovery.Instruction == "" {
		t.Fatalf("permission guidance = %+v", permission)
	}
	unknown := GuidanceForError("messages.get", &transport.TransportError{Code: "future_read_failure", Message: "new failure"})
	if unknown.Phase != OperationPhaseRead || unknown.EffectCertainty != EffectNone ||
		unknown.Retryability != RetryObserveRequired || unknown.ReplayAllowed || unknown.Recovery.Action != RecoveryInspect {
		t.Fatalf("unknown read guidance = %+v", unknown)
	}
}

func TestAccessGatePreflightGuidanceProvesNoEffectForMutations(t *testing.T) {
	tests := []struct {
		code              string
		instructionPhrase string
		retryability      Retryability
		recovery          RecoveryAction
	}{
		{code: "mail_not_running", instructionPhrase: "no action was dispatched", retryability: RetryUserInputRequired, recovery: RecoveryCorrect},
		{code: "mail_access_gate_corrupt", instructionPhrase: "Never delete or replace mail-access.lock", retryability: RetryUserInputRequired, recovery: RecoveryCorrect},
		{code: "mail_access_gate_unsafe", instructionPhrase: "use a verified recovery plan", retryability: RetryTerminal, recovery: RecoveryInspect},
	}
	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			guidance := GuidanceForError("drafts.send", &OperationError{Code: test.code, Message: "preflight failure"})
			if guidance.Phase != OperationPhaseExecution || guidance.EffectCertainty != EffectNone ||
				guidance.Retryability != test.retryability || guidance.ReplayAllowed ||
				guidance.Recovery.Action != test.recovery ||
				!strings.Contains(guidance.Recovery.Instruction, test.instructionPhrase) {
				t.Fatalf("access-gate guidance = %+v", guidance)
			}
		})
	}
}

func TestDraftStateReadPreservesPermissionCause(t *testing.T) {
	root := t.TempDir()
	service := NewServiceWithDraftRoot(nil, root)
	draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{
		To: []Recipient{{Address: "recipient@example.com"}}, Subject: "Permission", Body: "Body",
	}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, draft.Ref+".json")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Error(err)
		}
	})
	_, err = service.GetDraft(draft.Ref)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("draft permission cause lost: %v", err)
	}
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "delete") {
		t.Fatalf("unsafe permission diagnostic: %v", err)
	}
	guidance := GuidanceForError("drafts.inspect", err)
	if guidance.Phase != OperationPhaseRead || guidance.EffectCertainty != EffectNone || guidance.ReplayAllowed ||
		guidance.Retryability != RetryUserInputRequired || guidance.Recovery.Action != RecoveryCorrect || guidance.Recovery.Instruction == "" {
		t.Fatalf("wrapped permission guidance = %+v", guidance)
	}
}

func TestGuidanceForAmbiguousMailboxMutationRequiresCorrection(t *testing.T) {
	ambiguous := &transport.TransportError{
		Code: transport.CodeIMAPAmbiguousMailbox, Message: "multiple mailbox identities match the requested path",
	}
	for _, command := range []string{
		"messages.mark", "messages.move", "messages.copy", "messages.delete",
		BatchOperationMark, BatchOperationMove, BatchOperationCopy, BatchOperationDelete,
	} {
		t.Run(command, func(t *testing.T) {
			got := GuidanceForError(command, ambiguous)
			if got.Phase != OperationPhaseValidation || got.EffectCertainty != EffectNone ||
				got.Retryability != RetryUserInputRequired || got.ReplayAllowed ||
				got.Recovery.Action != RecoveryCorrect ||
				!strings.Contains(got.Recovery.Instruction, "Correct the conflicting IMAP mailbox identities") {
				t.Fatalf("GuidanceForError(%q) = %+v", command, got)
			}
		})
	}

	batch := GuidanceForError("batch", ambiguous)
	if batch.EffectCertainty != EffectUnknown || batch.Retryability != RetryObserveRequired ||
		batch.ReplayAllowed || batch.Recovery.Action != RecoveryInspect {
		t.Fatalf("batch guidance = %+v; want conservative inspection", batch)
	}
	for _, operation := range []BatchOperation{
		BatchOperationMark, BatchOperationMove, BatchOperationCopy, BatchOperationDelete,
	} {
		t.Run("batch item "+operation, func(t *testing.T) {
			item := (&batchExecution{request: BatchRequest{Operation: operation}}).itemError(ambiguous)
			if item.Retryable || item.Guidance == nil ||
				item.Guidance.Phase != OperationPhaseValidation || item.Guidance.EffectCertainty != EffectNone ||
				item.Guidance.Retryability != RetryUserInputRequired || item.Guidance.ReplayAllowed ||
				item.Guidance.Recovery.Action != RecoveryCorrect ||
				!strings.Contains(item.Guidance.Recovery.Instruction, "duplicate special-use assignments") {
				t.Fatalf("batch item guidance = %+v; want pre-dispatch correction guidance", item)
			}
		})
	}

	for _, cause := range []error{ambiguous, &transport.TransportError{
		Code: transport.CodeIMAPAmbiguousMessageID, Message: "duplicate message IDs",
	}} {
		uncertain := &transport.MutationOutcomeError{
			Code:     transport.CodeIMAPMoveOutcomeUnknown,
			Evidence: transport.MutationEvidence{Command: "MOVE", OperationID: "op_1"},
			Err:      cause,
		}
		got := GuidanceForError("messages.move", uncertain)
		if got.EffectCertainty != EffectUnknown || got.Retryability != RetryObserveRequired ||
			got.ReplayAllowed || got.Recovery.Action == RecoveryCorrect {
			t.Fatalf("wrapped uncertain mutation guidance = %+v for cause %v; identity correction must not erase outcome uncertainty", got, cause)
		}
	}
}
