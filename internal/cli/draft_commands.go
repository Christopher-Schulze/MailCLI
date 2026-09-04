package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"mailcli/internal/mail"
)

const maximumDraftInputBytes = 16 * 1024 * 1024

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
		writeLine(stderr, "Usage:\n  mailcli drafts <create|list|inspect|preview|edit|handoff|update|save|open|send|reconcile|discard|prune> [options]")
		return 2
	}
	switch args[0] {
	case "help", "--help", "-h":
		writeLine(stdout, "Usage:\n  mailcli drafts <create|list|inspect|preview|edit|handoff|update|save|open|send|reconcile|discard|prune> [options]")
		return 0
	case "create":
		return runDraftCreate(service, args[1:], stdout, stderr)
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
	case "update":
		return runDraftUpdate(service, args[1:], stdout, stderr)
	case "save":
		return runDraftSave(ctx, service, args[1:], stdout, stderr)
	case "open":
		return runMailDraftOpen(ctx, service, args[1:], stdout, stderr)
	case "send":
		return runDraftSend(ctx, service, args[1:], stdout, stderr)
	case "reconcile":
		return runDraftReconcile(ctx, service, args[1:], stdout, stderr)
	case "discard":
		return runDraftDiscard(service, args[1:], stdout, stderr)
	case "prune":
		return runDraftPrune(service, args[1:], stdout, stderr)
	default:
		writeFormat(stderr, "unknown drafts command %q\n", args[0])
		return 2
	}
}

func runDraftReconcile(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("drafts reconcile", stderr)
	ref := flags.String("ref", "", "draft ref with an existing send attempt")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	operationCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
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
	writeFormat(stdout, "%s\t%s\n", result.Outcome, result.DraftRef)
	return 0
}

func runDraftSave(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("drafts save", stderr)
	ref := flags.String("ref", "", "local draft ref")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	operationCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
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
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	operationCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	message, err := service.OpenDraft(operationCtx, *messageRef)
	if err != nil {
		return failCommand("drafts.open", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "drafts.open", responseData{Message: &message})
	}
	if err := writeMessage(stdout, message); err != nil {
		return 1
	}
	return 0
}

func runDraftCreate(service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("drafts create", stderr)
	inputFlags := registerDraftInputFlags(flags)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	input, err := inputFlags.read()
	if err != nil {
		return failCommand("drafts.create", *jsonOutput, err, stdout, stderr)
	}
	draft, err := service.CreateDraft(mail.CreateDraftRequest{Kind: mail.DraftKindNew, Input: input})
	if err != nil {
		return failCommand("drafts.create", *jsonOutput, err, stdout, stderr)
	}
	return writeDraftResponse(stdout, "drafts.create", draft, *jsonOutput)
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

func runDraftPrune(service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("drafts prune", stderr)
	olderThan := flags.Int("older-than", 30, "age threshold in days for never-sent drafts")
	confirm := flags.Bool("confirm", false, "delete the listed stale drafts")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	result, err := service.PruneDrafts(mail.PruneDraftsRequest{
		OlderThan: time.Duration(*olderThan) * 24 * time.Hour,
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
	if len(result.Candidates) == 0 {
		writeLine(stdout, "no stale never-sent drafts")
		return 0
	}
	if !*confirm {
		for _, candidate := range result.Candidates {
			writeFormat(stdout, "would remove\t%s\t%d days\t%s\n", candidate.Ref, candidate.AgeDays, oneLine(candidate.Subject))
		}
		return 0
	}
	for _, ref := range result.Removed {
		writeFormat(stdout, "removed\t%s\n", ref)
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
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if *ref == "" {
		return failCommand("drafts.inspect", *jsonOutput, invalidDraftInput("missing required --ref"), stdout, stderr)
	}
	draft, err := service.GetDraft(*ref)
	if err != nil {
		return failCommand("drafts.inspect", *jsonOutput, err, stdout, stderr)
	}
	return writeDraftResponse(stdout, "drafts.inspect", draft, *jsonOutput)
}

func runDraftUpdate(service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("drafts update", stderr)
	ref := flags.String("ref", "", "draft ref")
	inputFlags := registerDraftInputFlags(flags)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if *ref == "" {
		return failCommand("drafts.update", *jsonOutput, invalidDraftInput("missing required --ref"), stdout, stderr)
	}
	input, err := inputFlags.read()
	if err != nil {
		return failCommand("drafts.update", *jsonOutput, err, stdout, stderr)
	}
	draft, err := service.UpdateDraft(mail.UpdateDraftRequest{Ref: *ref, Input: input})
	if err != nil {
		return failCommand("drafts.update", *jsonOutput, err, stdout, stderr)
	}
	return writeDraftResponse(stdout, "drafts.update", draft, *jsonOutput)
}

func runDraftSend(ctx context.Context, service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("drafts send", stderr)
	ref := flags.String("ref", "", "draft ref")
	confirm := flags.Bool("confirm", false, "confirm sending the draft via direct SMTP")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if !*confirm {
		return failCommand("drafts.send", *jsonOutput, confirmationRequired("draft send"), stdout, stderr)
	}
	operationCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	result, err := service.SendDraft(operationCtx, *ref)
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
	writeFormat(stdout, "%s\t%s\n", result.Outcome, result.DraftRef)
	return 0
}

func runDraftDiscard(service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
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
	if err := service.DiscardDraft(*ref); err != nil {
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
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	input, err := inputFlags.read()
	if err != nil {
		return failCommand("messages."+string(kind), *jsonOutput, err, stdout, stderr)
	}
	source, err := service.ThreadSource(ctx, *messageRef)
	if err != nil {
		return failCommand("messages."+string(kind), *jsonOutput, err, stdout, stderr)
	}
	derived, sourceMessageID, references, err := mail.DeriveReplyInput(source, kind, replyAll, input)
	if err != nil {
		return failCommand("messages."+string(kind), *jsonOutput, err, stdout, stderr)
	}
	draft, err := service.CreateDraft(mail.CreateDraftRequest{
		Kind: kind, SourceRef: *messageRef, ReplyAll: replyAll, Input: derived,
		SourceMessageID: sourceMessageID, SourceReferences: references,
	})
	if err != nil {
		return failCommand("messages."+string(kind), *jsonOutput, err, stdout, stderr)
	}
	return writeDraftResponse(stdout, "messages."+string(kind), draft, *jsonOutput)
}

func writeDraftResponse(stdout io.Writer, command string, draft mail.Draft, jsonOutput bool) int {
	if jsonOutput {
		return writeSuccess(stdout, command, responseData{Draft: &draft})
	}
	writeFormat(stdout, "%s\t%s\t%s\n", draft.Ref, draft.Kind, oneLine(draft.Subject))
	return 0
}

func confirmationRequired(action string) error {
	return &commandError{code: "confirmation_required", message: action + " requires --confirm"}
}
