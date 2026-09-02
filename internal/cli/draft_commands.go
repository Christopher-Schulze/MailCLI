package cli

import (
	"context"
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
		writeLine(stderr, "Usage:\n  mailcli drafts <create|list|inspect|preview|edit|handoff|update|save|open|send|reconcile|discard> [options]")
		return 2
	}
	switch args[0] {
	case "help", "--help", "-h":
		writeLine(stdout, "Usage:\n  mailcli drafts <create|list|inspect|preview|edit|handoff|update|save|open|send|reconcile|discard> [options]")
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
	writeMessage(stdout, message)
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
	if *jsonOutput {
		return writeSuccess(stdout, "drafts.list", responseData{Drafts: &drafts})
	}
	rows := make([][]string, 0, len(drafts))
	for _, draft := range drafts {
		rows = append(rows, []string{draft.Ref, string(draft.Kind), draft.UpdatedAt.Format(time.RFC3339), draft.Subject})
	}
	if writeTerminalTable(stdout, []string{"REF", "TYPE", "UPDATED", "SUBJECT"}, rows) {
		return 0
	}
	for _, draft := range drafts {
		writeFormat(stdout, "%s\t%s\t%s\t%s\n", draft.Ref, draft.Kind, draft.UpdatedAt.Format(time.RFC3339), oneLine(draft.Subject))
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

func runMessageReply(service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	return runDerivedDraft(service, mail.DraftKindReply, args, stdout, stderr)
}

func runMessageForward(service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	return runDerivedDraft(service, mail.DraftKindForward, args, stdout, stderr)
}

func runDerivedDraft(service *mail.Service, kind mail.DraftKind, args []string, stdout io.Writer, stderr io.Writer) int {
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
	draft, err := service.CreateDraft(mail.CreateDraftRequest{
		Kind: kind, SourceRef: *messageRef, ReplyAll: replyAll, Input: input,
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
