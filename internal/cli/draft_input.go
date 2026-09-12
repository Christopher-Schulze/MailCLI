package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	stdmail "net/mail"
	"os"
	"strings"

	mailmodel "mailcli/internal/mail"
)

type trackedStringFlag struct {
	value string
	set   bool
}

func (value *trackedStringFlag) String() string {
	return value.value
}

func (value *trackedStringFlag) Set(input string) error {
	value.value = input
	value.set = true
	return nil
}

type repeatableStringFlag []string

func (values *repeatableStringFlag) String() string {
	return strings.Join(*values, ",")
}

func (values *repeatableStringFlag) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("value must not be empty")
	}
	*values = append(*values, value)
	return nil
}

type repeatableRecipientFlag struct {
	values []string
	set    bool
}

func (values *repeatableRecipientFlag) String() string {
	return strings.Join(values.values, ",")
}

func (values *repeatableRecipientFlag) Set(value string) error {
	values.set = true
	if strings.TrimSpace(value) == "" {
		return nil
	}
	values.values = append(values.values, value)
	return nil
}

type draftInputFlags struct {
	input       trackedStringFlag
	account     trackedStringFlag
	from        trackedStringFlag
	subject     trackedStringFlag
	body        trackedStringFlag
	bodyFile    trackedStringFlag
	bodyFormat  trackedStringFlag
	to          repeatableRecipientFlag
	cc          repeatableRecipientFlag
	bcc         repeatableRecipientFlag
	attachments repeatableStringFlag
}

func registerDraftInputFlags(flags *flag.FlagSet) *draftInputFlags {
	options := &draftInputFlags{}
	flags.Var(&options.input, "input", "JSON input file or - for standard input")
	flags.Var(&options.account, "account", "account ref for the sender identity")
	flags.Var(&options.from, "from", "sender address")
	flags.Var(&options.to, "to", "recipient address; repeat for multiple recipients")
	flags.Var(&options.cc, "cc", "CC recipient address; repeat for multiple recipients")
	flags.Var(&options.bcc, "bcc", "BCC recipient address; repeat for multiple recipients")
	flags.Var(&options.subject, "subject", "message subject")
	flags.Var(&options.body, "body", "message body")
	flags.Var(&options.bodyFile, "body-file", "body file or - for standard input")
	flags.Var(&options.bodyFormat, "format", "body format: plain, markdown, or html")
	flags.Var(&options.attachments, "attach", "absolute attachment path; repeat for multiple files")
	return options
}

func (options *draftInputFlags) read() (mailmodel.DraftInput, error) {
	return options.readMode(true)
}

// readUpdate reads partial input for `drafts update`: fields the caller did
// not supply stay unset so the caller can merge them from the stored draft.
func (options *draftInputFlags) readUpdate() (mailmodel.DraftInput, error) {
	return options.readMode(false)
}

func (options *draftInputFlags) readMode(requireBody bool) (mailmodel.DraftInput, error) {
	if !options.nativeMode() {
		path := "-"
		if options.input.set {
			path = options.input.value
		}
		return readDraftInputMode(path, requireBody)
	}
	if options.input.set {
		return mailmodel.DraftInput{}, invalidDraftInput("--input cannot be combined with terminal-native draft flags")
	}
	if options.body.set && options.bodyFile.set {
		return mailmodel.DraftInput{}, invalidDraftInput("terminal-native input requires exactly one of --body or --body-file")
	}
	if requireBody && !options.body.set && !options.bodyFile.set {
		return mailmodel.DraftInput{}, invalidDraftInput("terminal-native input requires exactly one of --body or --body-file")
	}
	body := options.body.value
	if options.bodyFile.set {
		var err error
		body, err = readDraftBody(options.bodyFile.value)
		if err != nil {
			return mailmodel.DraftInput{}, err
		}
	}
	to, err := parseRecipientFlags(options.to.values)
	if err != nil {
		return mailmodel.DraftInput{}, err
	}
	cc, err := parseRecipientFlags(options.cc.values)
	if err != nil {
		return mailmodel.DraftInput{}, err
	}
	bcc, err := parseRecipientFlags(options.bcc.values)
	if err != nil {
		return mailmodel.DraftInput{}, err
	}
	format := mailmodel.DraftBodyPlain
	if options.bodyFormat.set {
		format = mailmodel.DraftBodyFormat(strings.ToLower(strings.TrimSpace(options.bodyFormat.value)))
	}
	switch format {
	case mailmodel.DraftBodyPlain, mailmodel.DraftBodyMarkdown, mailmodel.DraftBodyHTML:
	default:
		return mailmodel.DraftInput{}, invalidDraftInput(
			fmt.Sprintf("invalid body format %q; use plain, markdown, or html", options.bodyFormat.value))
	}
	return mailmodel.DraftInput{
		AccountRef: options.account.value, From: options.from.value, To: to, CC: cc, BCC: bcc,
		Subject: options.subject.value, Body: body, BodyFormat: format,
		Attachments:    append([]string(nil), options.attachments...),
		AccountRefSet:  options.account.set,
		FromSet:        options.from.set,
		ToSet:          options.to.set,
		CCSet:          options.cc.set,
		BCCSet:         options.bcc.set,
		SubjectSet:     options.subject.set,
		BodySet:        options.body.set || options.bodyFile.set,
		BodyFormatSet:  options.bodyFormat.set,
		AttachmentsSet: len(options.attachments) > 0,
	}, nil
}

// mergeDraftUpdateInput applies patch semantics: fields the caller did not
// supply keep the stored draft's values. A body format change requires a new
// body because the stored source belongs to the old format.
func mergeDraftUpdateInput(current mailmodel.Draft, input mailmodel.DraftInput) (mailmodel.DraftInput, error) {
	if !input.AccountRefSet {
		input.AccountRef = current.AccountRef
	}
	if !input.FromSet {
		input.From = current.From
	}
	if !input.ToSet {
		input.To = current.To
	}
	if !input.CCSet {
		input.CC = current.CC
	}
	if !input.BCCSet {
		input.BCC = current.BCC
	}
	if !input.SubjectSet {
		input.Subject = current.Subject
	}
	if !input.AttachmentsSet {
		input.Attachments = make([]string, 0, len(current.Attachments))
		for _, attachment := range current.Attachments {
			input.Attachments = append(input.Attachments, attachment.Path)
		}
	}
	if !input.BodyFormatSet {
		input.BodyFormat = current.BodyFormat
	}
	if !input.BodySet {
		if input.BodyFormatSet {
			return mailmodel.DraftInput{}, invalidDraftInput(
				"changing body format requires a new body; supply --body/--body-file or a JSON body field")
		}
		input.Body = current.BodySource
		if input.Body == "" {
			input.Body = current.Body
		}
	}
	return input, nil
}

func (options *draftInputFlags) nativeMode() bool {
	return options.account.set || options.from.set || options.subject.set || options.body.set || options.bodyFile.set ||
		options.bodyFormat.set || options.to.set || options.cc.set || options.bcc.set || len(options.attachments) > 0
}

func parseRecipientFlags(values []string) ([]mailmodel.Recipient, error) {
	recipients := make([]mailmodel.Recipient, 0, len(values))
	for _, value := range values {
		parsed, err := stdmail.ParseAddress(value)
		if err != nil {
			return nil, invalidDraftInput("invalid recipient address: " + value)
		}
		recipients = append(recipients, mailmodel.Recipient{Name: parsed.Name, Address: mailmodel.MailboxAddrSpec(parsed.Address)})
	}
	return recipients, nil
}

func readDraftBody(path string) (string, error) {
	if path == "" {
		return "", invalidDraftInput("body file path is required")
	}
	if path == "-" {
		return readBoundedDraftBody(os.Stdin)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open draft body: %w", err)
	}
	body, readErr := readBoundedDraftBody(file)
	return body, errors.Join(readErr, file.Close())
}

func readBoundedDraftBody(reader io.Reader) (string, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, mailmodel.MaximumDraftBodyBytes+1))
	if err != nil {
		return "", fmt.Errorf("read draft body: %w", err)
	}
	if len(payload) > mailmodel.MaximumDraftBodyBytes {
		return "", invalidDraftInput("draft body exceeds 4 MiB")
	}
	return string(payload), nil
}

func readDraftInput(path string) (mailmodel.DraftInput, error) {
	return readDraftInputMode(path, true)
}

func readDraftInputMode(path string, requireBody bool) (mailmodel.DraftInput, error) {
	if path == "" {
		return mailmodel.DraftInput{}, &commandError{code: "invalid_argument", message: "input path is required"}
	}
	if path == "-" {
		return decodeDraftInputMode(os.Stdin, requireBody)
	}
	file, err := os.Open(path)
	if err != nil {
		return mailmodel.DraftInput{}, fmt.Errorf("open draft input: %w", err)
	}
	input, decodeErr := decodeDraftInputMode(file, requireBody)
	return input, errors.Join(decodeErr, file.Close())
}

func decodeDraftInput(reader io.Reader) (mailmodel.DraftInput, error) {
	return decodeDraftInputMode(reader, true)
}

func decodeDraftInputMode(reader io.Reader, requireBody bool) (mailmodel.DraftInput, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, maximumDraftInputBytes+1))
	if err != nil {
		return mailmodel.DraftInput{}, fmt.Errorf("read draft input: %w", err)
	}
	if len(payload) == 0 {
		return mailmodel.DraftInput{}, invalidDraftInput(
			"no input received on stdin; pipe JSON or use --input <path>")
	}
	if len(payload) > maximumDraftInputBytes {
		return mailmodel.DraftInput{}, invalidDraftInput("draft input exceeds 16 MiB")
	}
	fields, err := validateInputJSON(payload, inputJSONDraft)
	if err != nil {
		return mailmodel.DraftInput{}, err
	}
	if requireBody && !fields["body"] {
		return mailmodel.DraftInput{}, invalidDraftInput("draft input requires an explicit body field")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var input mailmodel.DraftInput
	if err := decoder.Decode(&input); err != nil {
		return mailmodel.DraftInput{}, inputJSONDecodeError(err)
	}
	input.AccountRefSet = fields["account_ref"]
	input.FromSet = fields["from"]
	input.ToSet = fields["to"]
	input.CCSet = fields["cc"]
	input.BCCSet = fields["bcc"]
	input.SubjectSet = fields["subject"]
	input.BodySet = fields["body"]
	input.BodyFormatSet = fields["body_format"]
	input.AttachmentsSet = fields["attachments"]
	return input, nil
}

func invalidDraftInput(message string) error {
	return &commandError{code: "invalid_input", message: message}
}
