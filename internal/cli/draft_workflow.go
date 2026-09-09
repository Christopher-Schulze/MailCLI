package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mailcli/internal/compose"
	"mailcli/internal/mail"
)

type draftPreview struct {
	Ref                string                   `json:"ref"`
	AccountRef         string                   `json:"account_ref,omitempty"`
	From               string                   `json:"from,omitempty"`
	To                 []mail.Recipient         `json:"to"`
	CC                 []mail.Recipient         `json:"cc"`
	BCC                []mail.Recipient         `json:"bcc"`
	Subject            string                   `json:"subject,omitempty"`
	BodyFormat         mail.DraftBodyFormat     `json:"body_format"`
	View               string                   `json:"view"`
	Body               string                   `json:"body"`
	ContentDiagnostics []mail.ContentDiagnostic `json:"content_diagnostics,omitempty"`
	Attachments        []mail.DraftAttachment   `json:"attachments"`
}

type draftHandoffResult struct {
	DraftRef        string `json:"draft_ref"`
	Opened          bool   `json:"opened"`
	MailApplication string `json:"mail_application"`
	DraftRetained   bool   `json:"draft_retained"`
}

type composeHandoffFunc func(context.Context, compose.Request) (compose.Result, error)

func runDraftHandoff(
	ctx context.Context,
	service *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	return runDraftHandoffWith(ctx, service, args, stdout, stderr, compose.Handoff)
}

func runDraftHandoffWith(
	ctx context.Context,
	service *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	handoff composeHandoffFunc,
) int {
	flags := newFlagSet("drafts handoff", stderr)
	ref := flags.String("ref", "", "local draft ref")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	operationCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	draft, err := service.PrepareDraftHandoffContext(operationCtx, *ref)
	if err != nil {
		return failCommand("drafts.handoff", *jsonOutput, err, stdout, stderr)
	}
	recipients := make([]string, 0, len(draft.To))
	for _, recipient := range draft.To {
		recipients = append(recipients, recipient.Address)
	}
	attachments, cleanup, err := mail.StageDraftAttachments(operationCtx, draft.Attachments)
	if err != nil {
		if cleanup != nil {
			err = errors.Join(err, cleanup())
		}
		return failCommand("drafts.handoff", *jsonOutput, err, stdout, stderr)
	}
	result, err := handoff(operationCtx, compose.Request{
		Recipients: recipients, Subject: draft.Subject, PlainBody: draft.Body,
		HTMLBody: draft.BodyHTML, Attachments: attachments,
	})
	var cleanupErr error
	if cleanup != nil {
		cleanupErr = cleanup()
	}
	if err != nil {
		if cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
		return failCommand("drafts.handoff", *jsonOutput, err, stdout, stderr)
	}
	if cleanupErr != nil {
		return failCommand(
			"drafts.handoff",
			*jsonOutput,
			&commandError{
				code:    "handoff_attachment_cleanup_failed",
				message: fmt.Sprintf("visible compose opened, but private attachment cleanup failed: %v", cleanupErr),
			},
			stdout,
			stderr,
		)
	}
	response := draftHandoffResult{
		DraftRef: draft.Ref, Opened: result.Opened, MailApplication: result.MailApplication,
		DraftRetained: true,
	}
	if *jsonOutput {
		return writeSuccess(stdout, "drafts.handoff", responseData{DraftHandoff: &response})
	}
	writeFormat(stdout, "Opened visible draft in Mail.app\nDraft: %s (retained locally)\n", draft.Ref)
	return 0
}

func runDraftPreview(service *mail.Service, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("drafts preview", stderr)
	ref := flags.String("ref", "", "draft ref")
	view := flags.String("format", "plain", "preview format: plain, source, or html")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	draft, err := service.GetDraft(*ref)
	if err != nil {
		return failCommand("drafts.preview", *jsonOutput, err, stdout, stderr)
	}
	preview, err := makeDraftPreview(draft, strings.ToLower(strings.TrimSpace(*view)))
	if err != nil {
		return failCommand("drafts.preview", *jsonOutput, err, stdout, stderr)
	}
	if *jsonOutput {
		return writeSuccess(stdout, "drafts.preview", responseData{DraftPreview: &preview})
	}
	writeHumanDraftPreview(stdout, preview)
	return 0
}

func makeDraftPreview(draft mail.Draft, view string) (draftPreview, error) {
	body := draft.Body
	switch view {
	case "plain":
	case "source":
		if draft.BodyFormat != mail.DraftBodyPlain {
			body = draft.BodySource
		}
	case "html":
		body = draft.BodyHTML
		if draft.BodyFormat == mail.DraftBodyPlain {
			body = "<pre>" + html.EscapeString(draft.Body) + "</pre>"
		}
	default:
		return draftPreview{}, invalidDraftInput("preview format must be plain, source, or html")
	}
	return draftPreview{
		Ref: draft.Ref, AccountRef: draft.AccountRef, From: draft.From, To: draft.To, CC: draft.CC, BCC: draft.BCC,
		Subject: draft.Subject, BodyFormat: draft.BodyFormat, View: view, Body: body,
		ContentDiagnostics: draft.ContentDiagnostics,
		Attachments:        draft.Attachments,
	}, nil
}

func writeHumanDraftPreview(writer io.Writer, preview draftPreview) {
	if preview.AccountRef != "" {
		writeFormat(writer, "Account: %s\n", preview.AccountRef)
	}
	writeFormat(writer, "From: %s\n", preview.From)
	writeFormat(writer, "To: %s\n", formatRecipients(preview.To))
	if len(preview.CC) > 0 {
		writeFormat(writer, "CC: %s\n", formatRecipients(preview.CC))
	}
	if len(preview.BCC) > 0 {
		writeFormat(writer, "BCC: %s\n", formatRecipients(preview.BCC))
	}
	writeFormat(writer, "Subject: %s\n", preview.Subject)
	writeFormat(writer, "Format: %s (%s preview)\n\n", preview.BodyFormat, preview.View)
	writeRaw(writer, preview.Body)
	if preview.Body != "" && !strings.HasSuffix(preview.Body, "\n") {
		writeLine(writer)
	}
	if len(preview.ContentDiagnostics) > 0 {
		writeLine(writer, "\nContent transformations:")
		for _, diagnostic := range preview.ContentDiagnostics {
			writeFormat(writer, "  %s", diagnostic.Code)
			if diagnostic.Element != "" {
				writeFormat(writer, " element=%s", diagnostic.Element)
			}
			if diagnostic.Attribute != "" {
				writeFormat(writer, " attribute=%s", diagnostic.Attribute)
			}
			writeLine(writer)
		}
	}
	if len(preview.Attachments) > 0 {
		writeLine(writer, "\nAttachments:")
		for _, attachment := range preview.Attachments {
			writeFormat(writer, "  %s (%d bytes)\n", attachment.Path, attachment.Size)
		}
	}
}

func runDraftEdit(
	ctx context.Context,
	service *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	flags := newFlagSet("drafts edit", stderr)
	ref := flags.String("ref", "", "draft ref")
	editor := flags.String("editor", "", "editor executable; defaults to VISUAL, EDITOR, or vi")
	var editorArgs repeatableStringFlag
	flags.Var(&editorArgs, "editor-arg", "editor argument; repeat for multiple arguments")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	outputFlags := addOutputFlags(flags, projectionTargetDraft, defaultDraftOutputView, false)
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	output, err := outputFlags.options(projectionTargetDraft)
	if err != nil {
		return failCommand("drafts.edit", *jsonOutput, err, stdout, stderr)
	}
	output.stderr = stderr
	draft, err := service.GetDraft(*ref)
	if err != nil {
		return failProjectedEmpty("drafts.edit", *jsonOutput, output, err, stdout, stderr)
	}
	input := draftInputFromStored(draft)
	updated, err := editDraftInput(ctx, service, draft.Ref, input, *editor, editorArgs)
	if err != nil {
		return failProjectedEmpty("drafts.edit", *jsonOutput, output, err, stdout, stderr)
	}
	return writeDraftResponse(stdout, "drafts.edit", updated, *jsonOutput, output)
}

func editDraftInput(
	ctx context.Context,
	service *mail.Service,
	ref string,
	input mail.DraftInput,
	editor string,
	editorArgs []string,
) (mail.Draft, error) {
	directory, err := os.MkdirTemp("", "mailcli-draft-edit-")
	if err != nil {
		return mail.Draft{}, fmt.Errorf("create secure draft editor directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(directory) }()
	path := filepath.Join(directory, "draft.json")
	if err := writeDraftEditorFile(path, input); err != nil {
		return mail.Draft{}, err
	}
	command, arguments, err := resolveEditor(editor, editorArgs)
	if err != nil {
		return mail.Draft{}, err
	}
	arguments = append(arguments, path)
	process := exec.CommandContext(ctx, command, arguments...)
	process.Stdin = os.Stdin
	process.Stdout = os.Stdout
	process.Stderr = os.Stderr
	if err := runOwnedProcess(process, 2*time.Second); err != nil {
		return mail.Draft{}, &commandError{code: "editor_failed", message: "draft editor failed: " + err.Error()}
	}
	edited, err := readDraftInput(path)
	if err != nil {
		return mail.Draft{}, fmt.Errorf("validate edited draft: %w", err)
	}
	operationCtx, cancel := context.WithTimeout(ctx, draftUpdateTimeout)
	defer cancel()
	return service.UpdateDraftContext(operationCtx, mail.UpdateDraftRequest{Ref: ref, Input: edited})
}

func writeDraftEditorFile(path string, input mail.DraftInput) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create draft editor file: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(input); err != nil {
		return errors.Join(fmt.Errorf("write draft editor file: %w", err), file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync draft editor file: %w", err), file.Close())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close draft editor file: %w", err)
	}
	return nil
}

func resolveEditor(explicit string, arguments []string) (string, []string, error) {
	if explicit != "" {
		return explicit, append([]string(nil), arguments...), nil
	}
	for _, variable := range []string{"VISUAL", "EDITOR"} {
		fields, err := splitEditorCommand(os.Getenv(variable))
		if err != nil {
			return "", nil, err
		}
		if len(fields) > 0 {
			return fields[0], append(fields[1:], arguments...), nil
		}
	}
	if _, err := os.Stat("/usr/bin/vi"); err != nil {
		return "", nil, &commandError{code: "editor_unavailable", message: "no editor configured; use --editor"}
	}
	return "/usr/bin/vi", append([]string(nil), arguments...), nil
}

func splitEditorCommand(value string) ([]string, error) {
	var fields []string
	var current strings.Builder
	var quote rune
	started := false
	escaped := false
	for _, char := range value {
		if escaped {
			current.WriteRune(char)
			started = true
			escaped = false
			continue
		}
		if char == '\\' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
				continue
			}
			current.WriteRune(char)
			started = true
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
			started = true
		case ' ', '\t', '\n', '\r':
			if started {
				fields = append(fields, current.String())
				current.Reset()
				started = false
			}
		default:
			current.WriteRune(char)
			started = true
		}
	}
	if escaped || quote != 0 {
		return nil, &commandError{code: "invalid_editor", message: fmt.Sprintf("editor command has an unterminated quote or escape: %q", value)}
	}
	if started {
		fields = append(fields, current.String())
	}
	return fields, nil
}

func draftInputFromStored(draft mail.Draft) mail.DraftInput {
	body := draft.Body
	if draft.BodyFormat != mail.DraftBodyPlain {
		body = draft.BodySource
	}
	attachments := make([]string, 0, len(draft.Attachments))
	for _, attachment := range draft.Attachments {
		attachments = append(attachments, attachment.Path)
	}
	return mail.DraftInput{
		AccountRef: draft.AccountRef, From: draft.From, To: draft.To, CC: draft.CC, BCC: draft.BCC,
		Subject: draft.Subject, Body: body, BodyFormat: draft.BodyFormat,
		Attachments: attachments,
	}
}
