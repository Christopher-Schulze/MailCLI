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

type draftInputFlags struct {
	input       trackedStringFlag
	from        trackedStringFlag
	subject     trackedStringFlag
	body        trackedStringFlag
	bodyFile    trackedStringFlag
	bodyFormat  trackedStringFlag
	to          repeatableStringFlag
	cc          repeatableStringFlag
	bcc         repeatableStringFlag
	attachments repeatableStringFlag
}

func registerDraftInputFlags(flags *flag.FlagSet) *draftInputFlags {
	options := &draftInputFlags{}
	flags.Var(&options.input, "input", "JSON input file or - for standard input")
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
	if !options.nativeMode() {
		path := "-"
		if options.input.set {
			path = options.input.value
		}
		return readDraftInput(path)
	}
	if options.input.set {
		return mailmodel.DraftInput{}, invalidDraftInput("--input cannot be combined with terminal-native draft flags")
	}
	if options.body.set == options.bodyFile.set {
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
	to, err := parseRecipientFlags(options.to)
	if err != nil {
		return mailmodel.DraftInput{}, err
	}
	cc, err := parseRecipientFlags(options.cc)
	if err != nil {
		return mailmodel.DraftInput{}, err
	}
	bcc, err := parseRecipientFlags(options.bcc)
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
		From: options.from.value, To: to, CC: cc, BCC: bcc,
		Subject: options.subject.value, Body: body, BodyFormat: format,
		Attachments: append([]string(nil), options.attachments...),
	}, nil
}

func (options *draftInputFlags) nativeMode() bool {
	return options.from.set || options.subject.set || options.body.set || options.bodyFile.set ||
		options.bodyFormat.set || len(options.to)+len(options.cc)+len(options.bcc)+len(options.attachments) > 0
}

func parseRecipientFlags(values []string) ([]mailmodel.Recipient, error) {
	recipients := make([]mailmodel.Recipient, 0, len(values))
	for _, value := range values {
		parsed, err := stdmail.ParseAddress(value)
		if err != nil {
			return nil, invalidDraftInput("invalid recipient address: " + value)
		}
		recipients = append(recipients, mailmodel.Recipient{Name: parsed.Name, Address: parsed.Address})
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
	if path == "" {
		return mailmodel.DraftInput{}, &commandError{code: "invalid_argument", message: "input path is required"}
	}
	if path == "-" {
		return decodeDraftInput(os.Stdin)
	}
	file, err := os.Open(path)
	if err != nil {
		return mailmodel.DraftInput{}, fmt.Errorf("open draft input: %w", err)
	}
	input, decodeErr := decodeDraftInput(file)
	return input, errors.Join(decodeErr, file.Close())
}

func decodeDraftInput(reader io.Reader) (mailmodel.DraftInput, error) {
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
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
		return mailmodel.DraftInput{}, invalidDraftInput("draft input must be one JSON object")
	}
	body, present := fields["body"]
	if !present {
		return mailmodel.DraftInput{}, invalidDraftInput("draft input requires an explicit body field")
	}
	if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		return mailmodel.DraftInput{}, invalidDraftInput("draft input body must be a string")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var input mailmodel.DraftInput
	if err := decoder.Decode(&input); err != nil {
		return mailmodel.DraftInput{}, invalidDraftInput("decode draft JSON: " + err.Error())
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return mailmodel.DraftInput{}, invalidDraftInput("draft input must contain exactly one JSON object")
	}
	return input, nil
}

func invalidDraftInput(message string) error {
	return &commandError{code: "invalid_input", message: message}
}
