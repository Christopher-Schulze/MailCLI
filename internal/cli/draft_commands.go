package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"mailcli/internal/mail"
)

const (
	maximumDraftInputBytes       = 16 * 1024 * 1024
	draftUpdateTimeout           = 15 * time.Second
	draftDiscardTimeout          = 15 * time.Second
	draftPruneTimeout            = 2 * time.Minute
	draftReconcileTimeout        = draftSendTimeout
	draftHandoffReconcileTimeout = 15 * time.Second
	draftSaveTimeout             = 2 * time.Minute
	draftSendTimeout             = 15 * time.Minute
	pruneDayDuration             = 24 * time.Hour
	maxPruneAgeDays              = int64((1<<63 - 1) / int64(pruneDayDuration))
)

type commandError struct {
	code    string
	message string
}

func (e *commandError) Error() string {
	return e.message
}

func (e *commandError) ErrorCode() string {
	return e.code
}

func runDrafts(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		writeLine(stderr, "Usage:\n  mailcli drafts <create|list|inspect|preview|edit|handoff|handoff-reconcile|update|save|open|send|reconcile|discard|prune> [options]")
		return 2
	}
	switch args[0] {
	case "help", "--help", "-h":
		writeLine(stdout, "Usage:\n  mailcli drafts <create|list|inspect|preview|edit|handoff|handoff-reconcile|update|save|open|send|reconcile|discard|prune> [options]")
		return 0
	case "create":
		return runDraftCreateContext(ctx, service, args[1:], stdout, stderr)
	case "list":
		return runDraftList(service, args[1:], stdout, stderr)
	case "inspect":
		return runDraftInspect(service, args[1:], stdout, stderr)
	case "preview":
		return runDraftPreview(service, args[1:], stdout, stderr)
	case "edit":
		return runDraftEdit(ctx, service, args[1:], stdout, stderr)
	case "handoff":
		return runDraftHandoff(ctx, service, args[1:], stdout, stderr)
	case "handoff-reconcile":
		return runDraftHandoffReconcile(ctx, service, args[1:], stdout, stderr)
	case "update":
		return runDraftUpdate(ctx, service, args[1:], stdout, stderr)
	case "save":
		return runDraftSave(ctx, service, args[1:], stdout, stderr)
	case "open":
		return runMailDraftOpen(ctx, service, args[1:], stdout, stderr)
	case "send":
		return runDraftSend(ctx, service, args[1:], stdout, stderr)
	case "reconcile":
		return runDraftReconcile(ctx, service, args[1:], stdout, stderr)
	case "discard":
		return runDraftDiscard(ctx, service, args[1:], stdout, stderr)
	case "prune":
		return runDraftPrune(ctx, service, args[1:], stdout, stderr)
	default:
		writeFormat(stderr, "unknown drafts command %q\n", args[0])
		return 2
	}
}

func runDraftHandoffReconcile(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("drafts handoff-reconcile", stderr)
	ref := flags.String("ref", "", "draft ref with an unresolved handoff")
	attempt := flags.String("attempt", "", "retained handoff attempt ID")
	outcome := flags.String("outcome", "", "observed outcome: opened or failed")
	confirm := flags.Bool("confirm", false, "confirm cleanup of the retained handoff evidence")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if strings.TrimSpace(*ref) == "" {
		return failCommand("drafts.handoff-reconcile", *jsonOutput, invalidDraftInput("missing required --ref"), stdout, stderr)
	}
	if strings.TrimSpace(*attempt) == "" {
		return failCommand("drafts.handoff-reconcile", *jsonOutput, invalidDraftInput("missing required --attempt"), stdout, stderr)
	}
	if !*confirm {
		return failCommand("drafts.handoff-reconcile", *jsonOutput, confirmationRequired("handoff reconciliation"), stdout, stderr)
	}
	resolution := mail.HandoffResolution(strings.TrimSpace(*outcome))
	if resolution != mail.HandoffResolutionOpened && resolution != mail.HandoffResolutionFailed {
		return failCommand("drafts.handoff-reconcile", *jsonOutput, invalidDraftInput("--outcome must be opened or failed"), stdout, stderr)
	}
	operationCtx, cancel := context.WithTimeout(ctx, draftHandoffReconcileTimeout)
	defer cancel()
	result, err := service.ReconcileDraftHandoffContext(operationCtx, *ref, *attempt, resolution)
	if err != nil {
		return failCommandWithData("drafts.handoff-reconcile", *jsonOutput, responseData{HandoffReconcile: &result}, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "drafts.handoff-reconcile", responseData{HandoffReconcile: &result})
	}
	writeFormat(stdout, "handoff reconciled\t%s\t%s\n", result.DraftRef, result.Outcome)
	return 0
}

func runDraftReconcile(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("drafts reconcile", stderr)
	ref := flags.String("ref", "", "draft ref with an existing send attempt")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	operationCtx, cancel := context.WithTimeout(ctx, draftReconcileTimeout)
	defer cancel()
	result, err := service.ReconcileDraft(operationCtx, *ref)
	if err != nil {
		if result.AttemptID != "" {
			return failCommandWithData(
				"drafts.reconcile", *jsonOutput, responseData{SendResult: &result}, err, stdout, stderr,
			)
		}
		return failCommand("drafts.reconcile", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "drafts.reconcile", responseData{SendResult: &result})
	}
	writeHumanSendResult(stdout, result)
	return 0
}

func runDraftSave(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("drafts save", stderr)
	ref := flags.String("ref", "", "local draft ref")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	operationCtx, cancel := context.WithTimeout(ctx, draftSaveTimeout)
	defer cancel()
	saved, err := service.SaveDraft(operationCtx, *ref)
	if err != nil {
		if saved.Message.Ref != "" {
			return failCommandWithData(
				"drafts.save", *jsonOutput, responseData{SavedDraft: &saved}, err, stdout, stderr,
			)
		}
		return failCommand("drafts.save", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "drafts.save", responseData{SavedDraft: &saved})
	}
	writeFormat(stdout, "%s\t%s\n", saved.Message.Ref, oneLine(saved.Message.Subject))
	return 0
}

func runMailDraftOpen(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("drafts open", stderr)
	messageRef := flags.String("message", "", "Mail.app draft message ref")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	outputFlags := addOutputFlags(flags, projectionTargetMessage, defaultMessageOutputView, false)
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	output, err := outputFlags.options(projectionTargetMessage)
	if err != nil {
		return failCommand("drafts.open", *jsonOutput, err, stdout, stderr)
	}
	output.stderr = stderr
	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	message, err := service.OpenDraft(operationCtx, *messageRef)
	if err != nil {
		return failMessageRead("drafts.open", *jsonOutput, message, err, stdout, stderr, output)
	}
	if *jsonOutput {
		return writeProjectedSuccess(stdout, "drafts.open", responseData{Message: &message}, output)
	}
	if err := writeMessage(stdout, message); err != nil {
		return 1
	}
	return 0
}

func runDraftCreate(service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	return runDraftCreateContext(context.Background(), service, args, stdout, stderr)
}

func runDraftCreateContext(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("drafts create", stderr)
	inputFlags := registerDraftInputFlags(flags)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	outputFlags := addOutputFlags(flags, projectionTargetDraft, defaultDraftOutputView, false)
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	output, err := outputFlags.options(projectionTargetDraft)
	if err != nil {
		return failCommand("drafts.create", *jsonOutput, err, stdout, stderr)
	}
	output.stderr = stderr
	input, err := inputFlags.read()
	if err != nil {
		return failProjectedEmpty("drafts.create", *jsonOutput, output, err, stdout, stderr)
	}
	draft, err := service.CreateDraftContext(ctx, mail.CreateDraftRequest{Kind: mail.DraftKindNew, Input: input})
	if err != nil {
		return failProjectedEmpty("drafts.create", *jsonOutput, output, err, stdout, stderr)
	}
	return writeDraftResponse(stdout, "drafts.create", draft, *jsonOutput, output)
}

type draftListEntry struct {
	mail.DraftSummary
	AgeDays int `json:"age_days"`
}

func runDraftList(service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("drafts list", stderr)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	drafts, err := service.ListDrafts()
	if err != nil {
		return failCommand("drafts.list", *jsonOutput, err, stdout, stderr)
	}
	entries := make([]draftListEntry, 0, len(drafts))
	for _, draft := range drafts {
		entries = append(entries, draftListEntry{
			DraftSummary: draft,
			AgeDays:      int(time.Since(draft.UpdatedAt).Hours() / 24),
		})
	}
	if *jsonOutput {
		return writeSuccess(stdout, "drafts.list", responseData{Drafts: &entries})
	}
	rows := make([][]string, 0, len(entries))
	for _, entry := range entries {
		sendMarker := "none"
		if entry.EverSent {
			sendMarker = "attempt"
		}
		rows = append(rows, []string{
			entry.Ref, string(entry.Kind), entry.UpdatedAt.Format(time.RFC3339),
			fmt.Sprintf("%dd", entry.AgeDays), sendMarker, oneLine(entry.Subject),
		})
	}
	if writeTerminalTable(stdout, []string{"REF", "TYPE", "UPDATED", "AGE", "SEND ATTEMPT", "SUBJECT"}, rows) {
		return 0
	}
	for _, entry := range entries {
		sendMarker := "none"
		if entry.EverSent {
			sendMarker = "attempt"
		}
		writeFormat(stdout, "%s\t%s\t%s\t%dd\t%s\t%s\n",
			entry.Ref, entry.Kind, entry.UpdatedAt.Format(time.RFC3339), entry.AgeDays, sendMarker, oneLine(entry.Subject))
	}
	return 0
}

func runDraftPrune(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("drafts prune", stderr)
	olderThan := flags.Int("older-than", 30, "age threshold in days for never-sent drafts (minimum 1)")
	confirm := flags.Bool("confirm", false, "delete the listed stale drafts")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if *olderThan < 1 {
		return failCommand("drafts.prune", *jsonOutput, &commandError{
			code: "invalid_argument",
			message: fmt.Sprintf(
				"--older-than must be at least 1 day (got %d); every never-sent draft would be pruned at lower values",
				*olderThan,
			),
		}, stdout, stderr)
	}
	if int64(*olderThan) > maxPruneAgeDays {
		return failCommand("drafts.prune", *jsonOutput, &commandError{
			code: "invalid_argument",
			message: fmt.Sprintf(
				"--older-than must be at most %d days to fit the supported duration (got %d)",
				maxPruneAgeDays, *olderThan,
			),
		}, stdout, stderr)
	}
	operationCtx, cancel := context.WithTimeout(ctx, draftPruneTimeout)
	defer cancel()
	result, err := service.PruneDraftsContext(operationCtx, mail.PruneDraftsRequest{
		OlderThan: time.Duration(*olderThan) * pruneDayDuration,
		Confirm:   *confirm,
	})
	if err != nil {
		return failCommandWithData(
			"drafts.prune", *jsonOutput, responseData{PruneResult: &result}, err, stdout, stderr,
		)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "drafts.prune", responseData{PruneResult: &result})
	}
	if len(result.Candidates) == 0 && len(result.ExpiredReceipts) == 0 && len(result.SweptLocks) == 0 {
		writeLine(stdout, "no stale never-sent drafts")
		return 0
	}
	if !*confirm {
		for _, candidate := range result.Candidates {
			writeFormat(stdout, "would remove\t%s\t%d days\t%s\n", candidate.Ref, candidate.AgeDays, oneLine(candidate.Subject))
		}
		for _, ref := range result.ExpiredReceipts {
			writeFormat(stdout, "would remove receipt\t%s\n", ref)
		}
		return 0
	}
	for _, ref := range result.Removed {
		writeFormat(stdout, "removed\t%s\n", ref)
	}
	for _, ref := range result.SweptLocks {
		writeFormat(stdout, "swept_lock\t%s\n", ref)
	}
	for _, ref := range result.ExpiredReceipts {
		writeFormat(stdout, "removed receipt\t%s\n", ref)
	}
	for _, failure := range result.Failed {
		writeFormat(stdout, "failed\t%s\t%s\n", failure.Ref, failure.Error)
	}
	return 0
}

func runDraftInspect(service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("drafts inspect", stderr)
	ref := flags.String("ref", "", "draft ref")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	outputFlags := addOutputFlags(flags, projectionTargetDraft, outputViewMetadata, true)
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	output, err := outputFlags.options(projectionTargetDraft)
	if err != nil {
		return failCommand("drafts.inspect", *jsonOutput, err, stdout, stderr)
	}
	output.stderr = stderr
	if *ref == "" {
		return failProjectedEmpty("drafts.inspect", *jsonOutput, output,
			invalidDraftInput("missing required --ref"), stdout, stderr)
	}
	draft, err := service.GetDraft(*ref)
	if err != nil {
		var operation interface{ ErrorCode() string }
		if errors.As(err, &operation) && operation.ErrorCode() == "not_found" {
			receipt, receiptErr := service.GetSendReceipt(*ref)
			if receiptErr == nil {
				if output.exportPath != "" {
					return failProjectedEmpty("drafts.inspect", *jsonOutput, output,
						&commandError{code: "content_unavailable", message: "consumed send receipt has no draft body to export"}, stdout, stderr)
				}
				if *jsonOutput {
					data := responseData{SendReceipt: &receipt, Projection: &projectionInfo{View: output.view, Fields: []string{}}}
					return writeProjectedSuccess(stdout, "drafts.inspect", data, output)
				}
				return writeSendReceiptResponse(stdout, "drafts.inspect", receipt, false)
			}
			var receiptOperation interface{ ErrorCode() string }
			if !errors.As(receiptErr, &receiptOperation) || receiptOperation.ErrorCode() != "not_found" {
				return failProjectedEmpty("drafts.inspect", *jsonOutput, output, receiptErr, stdout, stderr)
			}
		}
		return failProjectedEmpty("drafts.inspect", *jsonOutput, output, err, stdout, stderr)
	}
	return writeDraftResponse(stdout, "drafts.inspect", draft, *jsonOutput, output)
}

func writeSendReceiptResponse(stdout io.Writer, command string, receipt mail.SendReceipt, jsonOutput bool) int {
	if jsonOutput {
		return writeSuccess(stdout, command, responseData{SendReceipt: &receipt})
	}
	writeFormat(stdout, "Draft: %s\nOutcome: %s\nAttempt: %s\nSMTP submission accepted: %t\nSent copy observed: %t\nStarted: %s\nCompleted: %s\nExpires: %s\n",
		receipt.DraftRef, receipt.Outcome, receipt.AttemptID, receipt.SubmissionAccepted, receipt.SentCopyObserved,
		receipt.StartedAt.Format(time.RFC3339), receipt.CompletedAt.Format(time.RFC3339),
		receipt.ExpiresAt.Format(time.RFC3339))
	if receipt.DraftRevision != "" {
		writeFormat(stdout, "Reviewed revision: %s\n", receipt.DraftRevision)
	}
	if receipt.MessageID != "" {
		writeFormat(stdout, "Message-ID: %s\n", receipt.MessageID)
	}
	if receipt.ServerResponse != "" {
		writeFormat(stdout, "SMTP response: %s\n", receipt.ServerResponse)
	}
	if receipt.SentMailbox != "" {
		writeFormat(stdout, "Sent mailbox: %s\n", receipt.SentMailbox)
	}
	if receipt.UIDValidity != 0 || receipt.UID != 0 {
		writeFormat(stdout, "Sent UIDVALIDITY/UID: %d/%d\n", receipt.UIDValidity, receipt.UID)
	}
	return 0
}

func runDraftUpdate(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("drafts update", stderr)
	ref := flags.String("ref", "", "draft ref")
	expectedRevision := flags.String("expected-revision", "", "required revision from the inspected draft; rejects concurrent changes")
	inputFlags := registerDraftInputFlags(flags)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	outputFlags := addOutputFlags(flags, projectionTargetDraft, defaultDraftOutputView, false)
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	output, err := outputFlags.options(projectionTargetDraft)
	if err != nil {
		return failCommand("drafts.update", *jsonOutput, err, stdout, stderr)
	}
	output.stderr = stderr
	if *ref == "" {
		return failProjectedEmpty("drafts.update", *jsonOutput, output,
			invalidDraftInput("missing required --ref"), stdout, stderr)
	}
	if *expectedRevision == "" {
		return failProjectedEmpty("drafts.update", *jsonOutput, output,
			invalidDraftInput("missing required --expected-revision; inspect the draft before updating"), stdout, stderr)
	}
	input, err := inputFlags.read()
	if err != nil {
		return failProjectedEmpty("drafts.update", *jsonOutput, output, err, stdout, stderr)
	}
	operationCtx, cancel := context.WithTimeout(ctx, draftUpdateTimeout)
	defer cancel()
	draft, err := service.UpdateDraftContext(operationCtx, mail.UpdateDraftRequest{Ref: *ref, ExpectedRevision: *expectedRevision, Input: input})
	if err != nil {
		return failProjectedEmpty("drafts.update", *jsonOutput, output, err, stdout, stderr)
	}
	return writeDraftResponse(stdout, "drafts.update", draft, *jsonOutput, output)
}

func runDraftSend(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("drafts send", stderr)
	ref := flags.String("ref", "", "draft ref")
	expectedRevision := flags.String("expected-revision", "", "required revision of the reviewed content; rejects concurrent changes")
	confirm := flags.Bool("confirm", false, "confirm sending the draft via direct SMTP")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if !*confirm {
		return failCommand("drafts.send", *jsonOutput, confirmationRequired("draft send"), stdout, stderr)
	}
	if *expectedRevision == "" {
		return failCommand("drafts.send", *jsonOutput, invalidDraftInput("missing required --expected-revision; review the draft before sending"), stdout, stderr)
	}
	operationCtx, cancel := context.WithTimeout(ctx, draftSendTimeout)
	defer cancel()
	result, err := service.SendDraft(operationCtx, mail.SendDraftRequest{Ref: *ref, ExpectedRevision: *expectedRevision})
	if err != nil {
		if result.AttemptID != "" {
			return failCommandWithData(
				"drafts.send", *jsonOutput, responseData{SendResult: &result}, err, stdout, stderr,
			)
		}
		return failCommand("drafts.send", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "drafts.send", responseData{SendResult: &result})
	}
	writeHumanSendResult(stdout, result)
	return 0
}

func writeHumanSendResult(stdout io.Writer, result mail.SendResult) {
	writeFormat(stdout, "%s\t%s\tsubmission_accepted=%t\tsent_copy_observed=%t\n",
		result.Outcome, result.DraftRef, result.SubmissionAccepted, result.SentCopyObserved)
}

func runDraftDiscard(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("drafts discard", stderr)
	ref := flags.String("ref", "", "draft ref")
	confirm := flags.Bool("confirm", false, "confirm local draft removal")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if !*confirm {
		return failCommand("drafts.discard", *jsonOutput, confirmationRequired("draft discard"), stdout, stderr)
	}
	operationCtx, cancel := context.WithTimeout(ctx, draftDiscardTimeout)
	defer cancel()
	if err := service.DiscardDraftContext(operationCtx, *ref); err != nil {
		return failCommand("drafts.discard", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "drafts.discard", responseData{})
	}
	writeLine(stdout, "draft discarded")
	return 0
}

func runMessageReply(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	return runDerivedDraft(ctx, service, mail.DraftKindReply, args, stdout, stderr)
}

func runMessageForward(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	return runDerivedDraft(ctx, service, mail.DraftKindForward, args, stdout, stderr)
}

// runDerivedDraft resolves the source message's thread headers from the Mail
// store, derives subject/recipients/thread chain (explicit input wins), and
// creates the local review draft.
func runDerivedDraft(ctx context.Context, service *mail.Service, kind mail.DraftKind, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("messages "+string(kind), stderr)
	messageRef := flags.String("message", "", "source message ref")
	inputFlags := registerDraftInputFlags(flags)
	replyAll := false
	if kind == mail.DraftKindReply {
		flags.BoolVar(&replyAll, "all", false, "reply to all recipients")
	}
	jsonOutput := flags.Bool("json", false, "emit JSON")
	outputFlags := addOutputFlags(flags, projectionTargetDraft, defaultDraftOutputView, false)
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	output, err := outputFlags.options(projectionTargetDraft)
	if err != nil {
		return failCommand("messages."+string(kind), *jsonOutput, err, stdout, stderr)
	}
	output.stderr = stderr
	input, err := inputFlags.read()
	if err != nil {
		return failProjectedEmpty("messages."+string(kind), *jsonOutput, output, err, stdout, stderr)
	}
	source, err := service.ThreadSource(ctx, *messageRef)
	if err != nil {
		return failProjectedEmpty("messages."+string(kind), *jsonOutput, output, err, stdout, stderr)
	}
	derived, sourceMessageID, references, err := mail.DeriveReplyInput(source, kind, replyAll, input)
	if err != nil {
		return failProjectedEmpty("messages."+string(kind), *jsonOutput, output, err, stdout, stderr)
	}
	draft, err := service.CreateDraftContext(ctx, mail.CreateDraftRequest{
		Kind: kind, SourceRef: *messageRef, ReplyAll: replyAll, Input: derived,
		SourceMessageID: sourceMessageID, SourceReferences: references,
	})
	if err != nil {
		return failProjectedEmpty("messages."+string(kind), *jsonOutput, output, err, stdout, stderr)
	}
	return writeDraftResponse(stdout, "messages."+string(kind), draft, *jsonOutput, output)
}

func writeDraftResponse(stdout io.Writer, command string, draft mail.Draft, jsonOutput bool, options ...outputOptions) int {
	output := outputOptions{target: projectionTargetDraft, view: defaultDraftOutputView, maxBytes: defaultJSONOutputBytes}
	if len(options) > 0 {
		output = options[0]
	}
	if output.stderr == nil {
		output.stderr = io.Discard
	}
	var exported *mail.ContentExport
	if output.exportPath != "" {
		if int64(len(draft.Body)) > maximumContentExportBytes {
			return failProjectedDraft(command, jsonOutput, draft, output,
				&commandError{code: "content_export_too_large", message: "draft body exceeds the 64 MiB export limit"}, stdout, output.stderr)
		}
		value, err := mail.WriteExclusiveContent(output.exportPath, func(writer io.Writer) error {
			_, writeErr := io.WriteString(writer, draft.Body)
			return writeErr
		})
		if err != nil {
			return failProjectedDraft(command, jsonOutput, draft, output, err, stdout, output.stderr)
		}
		exported = &value
	}
	if jsonOutput {
		return writeProjectedSuccess(stdout, command, responseData{Draft: &draft, ContentExport: exported}, output)
	}
	if output.exportPath != "" {
		writeFormat(stdout, "%s\t%d\t%s\n", exported.Path, exported.Size, exported.SHA256)
		return 0
	}
	writeFormat(stdout, "%s\t%s\t%s\n", draft.Ref, draft.Kind, oneLine(draft.Subject))
	writeFormat(stdout, "Revision: %s\n", draft.Revision)
	return 0
}

func confirmationRequired(action string) error {
	return &commandError{code: "confirmation_required", message: action + " requires --confirm"}
}
