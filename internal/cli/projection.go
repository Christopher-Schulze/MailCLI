package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"mailcli/internal/mail"
)

const (
	outputViewMetadata = "metadata"
	outputViewPlain    = "plain"
	outputViewFull     = "full"

	defaultMessageOutputView = outputViewMetadata
	defaultDraftOutputView   = outputViewPlain
	defaultRawOutputView     = outputViewFull

	defaultJSONOutputBytes    = int64(1 << 20)
	maximumJSONOutputBytes    = mail.MaximumRawSourceBytes
	maximumContentExportBytes = mail.MaximumRawSourceBytes
)

type projectionTarget string

const (
	projectionTargetMessage    projectionTarget = "message"
	projectionTargetDraft      projectionTarget = "draft"
	projectionTargetAttachment projectionTarget = "attachment"
	projectionTargetRaw        projectionTarget = "raw"
)

type outputFlagState struct {
	flags    *flag.FlagSet
	view     *string
	fields   *string
	export   *string
	maxBytes *int64
}

type outputOptions struct {
	target         projectionTarget
	view           string
	fields         map[string]struct{}
	fieldsProvided bool
	exportPath     string
	maxBytes       int64
	stderr         io.Writer
}

type projectionInfo struct {
	View   string   `json:"view"`
	Fields []string `json:"fields"`
}

type serializedProjection struct {
	message     *messageProjection
	draft       *draftProjection
	attachments *[]attachmentProjection
	hideRaw     bool
}

type messageProjection struct {
	Summary         mail.MessageSummary       `json:"summary"`
	ReplyTo         *string                   `json:"reply_to,omitempty"`
	To              *[]mail.Recipient         `json:"to,omitempty"`
	CC              *[]mail.Recipient         `json:"cc,omitempty"`
	BCC             *[]mail.Recipient         `json:"bcc,omitempty"`
	Headers         *string                   `json:"headers,omitempty"`
	Content         *string                   `json:"content,omitempty"`
	ContentSource   string                    `json:"content_source"`
	ContentComplete bool                      `json:"content_complete"`
	MissingParts    *[]string                 `json:"missing_parts"`
	Hydration       *mail.HydrationDiagnostic `json:"hydration,omitempty"`
	Attachments     *[]mail.Attachment        `json:"attachments,omitempty"`
}

type draftProjection struct {
	Ref                *string                       `json:"ref,omitempty"`
	Revision           *string                       `json:"revision,omitempty"`
	Kind               *mail.DraftKind               `json:"kind,omitempty"`
	AccountRef         *string                       `json:"account_ref,omitempty"`
	SourceRef          *string                       `json:"source_ref,omitempty"`
	ReplyAll           *bool                         `json:"reply_all,omitempty"`
	SourceMessageID    *string                       `json:"source_message_id,omitempty"`
	SourceReferences   *string                       `json:"source_references,omitempty"`
	From               *string                       `json:"from,omitempty"`
	To                 *[]mail.Recipient             `json:"to,omitempty"`
	CC                 *[]mail.Recipient             `json:"cc,omitempty"`
	BCC                *[]mail.Recipient             `json:"bcc,omitempty"`
	Subject            *string                       `json:"subject,omitempty"`
	Body               *string                       `json:"body,omitempty"`
	BodyFormat         *mail.DraftBodyFormat         `json:"body_format,omitempty"`
	BodySource         *string                       `json:"body_source,omitempty"`
	BodyHTML           *string                       `json:"body_html,omitempty"`
	ContentDiagnostics *[]mail.ContentDiagnostic     `json:"content_diagnostics,omitempty"`
	Attachments        *[]mail.DraftAttachment       `json:"attachments,omitempty"`
	AttachmentCount    int                           `json:"attachment_count"`
	CreatedAt          *time.Time                    `json:"created_at,omitempty"`
	UpdatedAt          *time.Time                    `json:"updated_at,omitempty"`
	SendAttempt        *mail.DraftSendAttemptSummary `json:"send_attempt,omitempty"`
	SaveAttempt        *mail.DraftSaveAttemptSummary `json:"save_attempt,omitempty"`
}

type attachmentProjection struct {
	ID         *string `json:"id,omitempty"`
	Name       *string `json:"name,omitempty"`
	MIMEType   *string `json:"mime_type,omitempty"`
	Size       *int64  `json:"size,omitempty"`
	SizeKnown  *bool   `json:"size_known,omitempty"`
	Downloaded *bool   `json:"downloaded,omitempty"`
}

type outputTooLargeError struct {
	actual int
	limit  int64
	target string
}

func (e *outputTooLargeError) Error() string {
	return fmt.Sprintf(
		"%s JSON output is %d bytes, above the %d-byte limit; use --export PATH for complete content or raise --max-bytes",
		e.target, e.actual, e.limit,
	)
}

func (e *outputTooLargeError) ErrorCode() string {
	return "output_too_large"
}

func addOutputFlags(flags *flag.FlagSet, target projectionTarget, defaultView string, allowExport bool) outputFlagState {
	state := outputFlagState{flags: flags}
	viewDescription := "JSON view: metadata, plain, or full"
	switch target {
	case projectionTargetAttachment:
		viewDescription = "JSON view: metadata or full"
	case projectionTargetRaw:
		viewDescription = "JSON view: full"
	}
	state.view = flags.String("view", defaultView, viewDescription)
	state.fields = flags.String("fields", "", "comma-separated JSON fields; mutually exclusive with --view")
	if allowExport {
		state.export = flags.String("export", "", "exclusive absolute path for complete content export")
	}
	state.maxBytes = flags.Int64("max-bytes", defaultJSONOutputBytes, "maximum JSON response bytes")
	return state
}

func (s outputFlagState) options(target projectionTarget) (outputOptions, error) {
	options := outputOptions{target: target, view: strings.ToLower(strings.TrimSpace(*s.view)), maxBytes: *s.maxBytes}
	viewProvided, fieldsProvided, exportProvided := false, false, false
	s.flags.Visit(func(option *flag.Flag) {
		switch option.Name {
		case "view":
			viewProvided = true
		case "fields":
			fieldsProvided = true
		case "export":
			exportProvided = true
		}
	})
	options.fieldsProvided = fieldsProvided
	if options.maxBytes <= 0 || options.maxBytes > maximumJSONOutputBytes {
		return outputOptions{}, &commandError{code: "invalid_argument", message: fmt.Sprintf(
			"--max-bytes must be between 1 and %d", maximumJSONOutputBytes,
		)}
	}
	if viewProvided && fieldsProvided {
		return outputOptions{}, &commandError{code: "invalid_argument", message: "--view and --fields cannot be combined"}
	}
	if fieldsProvided {
		fields, err := parseProjectionFields(target, *s.fields)
		if err != nil {
			return outputOptions{}, err
		}
		options.fields = fields
		options.view = "custom"
	} else if !validProjectionView(target, options.view) {
		return outputOptions{}, &commandError{code: "invalid_argument", message: projectionViewError(target, options.view)}
	}
	if s.export != nil {
		options.exportPath = strings.TrimSpace(*s.export)
		if exportProvided && options.exportPath == "" {
			return outputOptions{}, &commandError{code: "invalid_argument", message: "--export requires an absolute path"}
		}
		if options.exportPath != "" && target != projectionTargetRaw && !options.includes("content") && !options.includes("body") {
			return outputOptions{}, &commandError{code: "invalid_argument", message: "--export requires a content-capable view or field"}
		}
		if options.exportPath != "" && options.view == outputViewMetadata {
			return outputOptions{}, &commandError{code: "invalid_argument", message: "--export cannot be combined with the metadata view"}
		}
		if options.exportPath != "" {
			if err := validateContentExportPath(options.exportPath); err != nil {
				return outputOptions{}, err
			}
		}
	}
	return options, nil
}

func validateContentExportPath(path string) error {
	if !filepath.IsAbs(path) {
		return &commandError{code: "invalid_argument", message: "content export path must be absolute"}
	}
	if _, err := os.Lstat(path); err == nil {
		return &commandError{code: "invalid_argument", message: "content export path already exists"}
	} else if !os.IsNotExist(err) {
		return &commandError{code: "invalid_argument", message: fmt.Sprintf("inspect content export path: %v", err)}
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return &commandError{code: "invalid_argument", message: fmt.Sprintf("inspect content export directory: %v", err)}
	}
	if !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
		return &commandError{code: "invalid_argument", message: "content export parent is not a directory"}
	}
	return nil
}

func validProjectionView(target projectionTarget, view string) bool {
	switch target {
	case projectionTargetMessage, projectionTargetDraft:
		return view == outputViewMetadata || view == outputViewPlain || view == outputViewFull
	case projectionTargetAttachment:
		return view == outputViewMetadata || view == outputViewFull
	case projectionTargetRaw:
		return view == outputViewFull
	default:
		return false
	}
}

func projectionViewError(target projectionTarget, view string) string {
	switch target {
	case projectionTargetRaw:
		return fmt.Sprintf("unknown raw view %q; only full is supported", view)
	case projectionTargetAttachment:
		return fmt.Sprintf("unknown attachment view %q; choose metadata or full", view)
	default:
		return fmt.Sprintf("unknown %s view %q; choose metadata, plain, or full", target, view)
	}
}

func parseProjectionFields(target projectionTarget, value string) (map[string]struct{}, error) {
	if strings.TrimSpace(value) == "" {
		return nil, &commandError{code: "invalid_argument", message: "--fields must contain at least one field"}
	}
	allowed := projectionFieldNames(target)
	fields := make(map[string]struct{})
	for _, raw := range strings.Split(value, ",") {
		field := strings.ToLower(strings.TrimSpace(raw))
		if field == "" {
			return nil, &commandError{code: "invalid_argument", message: "--fields contains an empty field"}
		}
		if !slices.Contains(allowed, field) {
			return nil, &commandError{code: "invalid_argument", message: fmt.Sprintf(
				"unknown %s field %q; allowed fields: %s", target, field, strings.Join(allowed, ", "),
			)}
		}
		if _, duplicate := fields[field]; duplicate {
			return nil, &commandError{code: "invalid_argument", message: fmt.Sprintf("duplicate %s field %q", target, field)}
		}
		fields[field] = struct{}{}
	}
	return fields, nil
}

func projectionFieldNames(target projectionTarget) []string {
	switch target {
	case projectionTargetMessage:
		return []string{"summary", "reply_to", "to", "cc", "bcc", "headers", "content", "content_source", "content_complete", "missing_parts", "hydration", "attachments"}
	case projectionTargetDraft:
		return []string{"ref", "revision", "kind", "account_ref", "source_ref", "reply_all", "source_message_id", "source_references", "from", "to", "cc", "bcc", "subject", "body", "body_format", "body_source", "body_html", "content_diagnostics", "attachments", "attachment_count", "created_at", "updated_at", "send_attempt", "save_attempt"}
	case projectionTargetAttachment:
		return []string{"id", "name", "mime_type", "size", "size_known", "downloaded"}
	case projectionTargetRaw:
		return []string{"raw_source"}
	default:
		return nil
	}
}

//go:noinline
func (o outputOptions) includes(field string) bool {
	if o.fieldsProvided {
		_, ok := o.fields[field]
		return ok
	}
	switch o.view {
	case outputViewFull:
		return true
	case outputViewPlain:
		return field != "headers" && field != "body_source" && field != "body_html"
	case outputViewMetadata:
		return field != "headers" && field != "content" && field != "body" && field != "body_source" && field != "body_html"
	default:
		return false
	}
}

func projectionFields(target projectionTarget, options outputOptions, contentRetained bool) []string {
	names := projectionFieldNames(target)
	fields := make([]string, 0, len(names))
	for _, field := range names {
		if options.exportPath != "" && (field == "content" || field == "body" || field == "body_source" || field == "body_html") {
			continue
		}
		if options.includes(field) || requiredProjectionField(target, field, contentRetained) {
			fields = append(fields, field)
		}
	}
	sort.Strings(fields)
	return fields
}

//go:noinline
func requiredProjectionField(target projectionTarget, field string, contentRetained bool) bool {
	switch target {
	case projectionTargetMessage:
		return field == "summary" || field == "content_source" || field == "content_complete" ||
			field == "missing_parts" || field == "hydration" || (contentRetained && field == "content")
	case projectionTargetDraft:
		switch field {
		case "ref", "revision", "kind", "account_ref", "subject", "body_format", "attachment_count", "created_at", "updated_at", "send_attempt", "save_attempt":
			return true
		}
	}
	return false
}

func messageProjectionFor(message mail.Message, options outputOptions, retainContent bool) *messageProjection {
	projection := &messageProjection{
		Summary: message.Summary, ContentSource: message.ContentSource,
		ContentComplete: message.ContentComplete, MissingParts: &message.MissingParts,
		Hydration: message.Hydration,
	}
	if options.includes("reply_to") {
		projection.ReplyTo = &message.ReplyTo
	}
	if options.includes("to") {
		projection.To = &message.To
	}
	if options.includes("cc") {
		projection.CC = &message.CC
	}
	if options.includes("bcc") {
		projection.BCC = &message.BCC
	}
	if options.includes("headers") {
		projection.Headers = &message.Headers
	}
	if (options.includes("content") || retainContent) && options.exportPath == "" {
		projection.Content = &message.Content
	}
	if options.includes("attachments") {
		projection.Attachments = &message.Attachments
	}
	return projection
}

func draftProjectionFor(draft mail.Draft, options outputOptions) *draftProjection {
	projection := &draftProjection{AttachmentCount: len(draft.Attachments)}
	for _, field := range projectionFieldNames(projectionTargetDraft) {
		if options.exportPath != "" && (field == "body" || field == "body_source" || field == "body_html") {
			continue
		}
		if !options.includes(field) && !requiredProjectionField(projectionTargetDraft, field, false) {
			continue
		}
		switch field {
		case "ref":
			projection.Ref = &draft.Ref
		case "revision":
			projection.Revision = &draft.Revision
		case "kind":
			projection.Kind = &draft.Kind
		case "account_ref":
			projection.AccountRef = &draft.AccountRef
		case "source_ref":
			projection.SourceRef = &draft.SourceRef
		case "reply_all":
			projection.ReplyAll = &draft.ReplyAll
		case "source_message_id":
			projection.SourceMessageID = &draft.SourceMessageID
		case "source_references":
			projection.SourceReferences = &draft.SourceReferences
		case "from":
			projection.From = &draft.From
		case "to":
			projection.To = &draft.To
		case "cc":
			projection.CC = &draft.CC
		case "bcc":
			projection.BCC = &draft.BCC
		case "subject":
			projection.Subject = &draft.Subject
		case "body":
			projection.Body = &draft.Body
		case "body_format":
			projection.BodyFormat = &draft.BodyFormat
		case "body_source":
			projection.BodySource = &draft.BodySource
		case "body_html":
			projection.BodyHTML = &draft.BodyHTML
		case "content_diagnostics":
			projection.ContentDiagnostics = &draft.ContentDiagnostics
		case "attachments":
			projection.Attachments = &draft.Attachments
		case "attachment_count":
			projection.AttachmentCount = len(draft.Attachments)
		case "created_at":
			projection.CreatedAt = &draft.CreatedAt
		case "updated_at":
			projection.UpdatedAt = &draft.UpdatedAt
		case "send_attempt":
			projection.SendAttempt = draftSendAttemptProjection(draft.SendAttempt)
		case "save_attempt":
			projection.SaveAttempt = draftSaveAttemptProjection(draft.SaveAttempt)
		}
	}
	return projection
}

func draftSendAttemptProjection(attempt *mail.SendAttempt) *mail.DraftSendAttemptSummary {
	if attempt == nil {
		return nil
	}
	summary := &mail.DraftSendAttemptSummary{
		ID: attempt.ID, StartedAt: attempt.StartedAt, UpdatedAt: attempt.UpdatedAt,
		DraftRevision: attempt.DraftRevision,
		MessageID:     attempt.MessageID, EnvelopeFingerprint: attempt.EnvelopeFingerprint,
		MIMEFingerprint: attempt.MIMEFingerprint, Outcome: attempt.Outcome,
		InvocationStarted: attempt.InvocationStarted, AcceptedByMail: attempt.AcceptedByMail,
		SentStoreObserved: attempt.SentStoreObserved,
		SentCopyObserved:  attempt.SentStoreObserved, ObservedMessageRef: attempt.ObservedMessageRef,
	}
	if attempt.Transport != nil {
		transport := *attempt.Transport
		summary.Transport = &transport
		summary.SubmissionAccepted = transport.SubmissionAccepted || strings.TrimSpace(transport.ServerResponse) != ""
	}
	if attempt.ObservationBaseline != nil {
		baseline := *attempt.ObservationBaseline
		baseline.SentMailboxIDs = append([]int64(nil), attempt.ObservationBaseline.SentMailboxIDs...)
		summary.ObservationBaseline = &baseline
	}
	return summary
}

func draftSaveAttemptProjection(attempt *mail.DraftSaveAttempt) *mail.DraftSaveAttemptSummary {
	if attempt == nil {
		return nil
	}
	return &mail.DraftSaveAttemptSummary{
		ID: attempt.ID, StartedAt: attempt.StartedAt, UpdatedAt: attempt.UpdatedAt,
		InvocationStarted: attempt.InvocationStarted, AcceptedByMail: attempt.AcceptedByMail,
		ObservedMessageRef: attempt.ObservedMessageRef, ObservationBaseline: attempt.ObservationBaseline,
	}
}

func attachmentProjections(values []mail.Attachment, options outputOptions) []attachmentProjection {
	result := make([]attachmentProjection, 0, len(values))
	allFields := !options.fieldsProvided
	for index := range values {
		value := &values[index]
		projection := attachmentProjection{}
		if allFields || options.includes("id") {
			projection.ID = &value.ID
		}
		if allFields || options.includes("name") {
			projection.Name = &value.Name
		}
		if allFields || options.includes("mime_type") {
			projection.MIMEType = value.MIMEType
		}
		if allFields || options.includes("size") {
			projection.Size = &value.Size
		}
		if allFields || options.includes("size_known") {
			projection.SizeKnown = &value.SizeKnown
		}
		if allFields || options.includes("downloaded") {
			projection.Downloaded = &value.Downloaded
		}
		result = append(result, projection)
	}
	return result
}

func projectedMessageData(data responseData, message mail.Message, options outputOptions, failed bool) responseData {
	retainContent := failed && options.exportPath == "" && message.Hydration != nil && !message.ContentComplete && message.Content != ""
	data.Projection = &projectionInfo{View: options.view, Fields: projectionFields(projectionTargetMessage, options, retainContent)}
	data.serialization = &serializedProjection{message: messageProjectionFor(message, options, retainContent)}
	return data
}

func projectedDraftData(data responseData, draft mail.Draft, options outputOptions) responseData {
	data.Projection = &projectionInfo{View: options.view, Fields: projectionFields(projectionTargetDraft, options, false)}
	data.serialization = &serializedProjection{draft: draftProjectionFor(draft, options)}
	return data
}

func projectedAttachmentData(data responseData, attachments []mail.Attachment, options outputOptions) responseData {
	data.Projection = &projectionInfo{View: options.view, Fields: projectionFields(projectionTargetAttachment, options, false)}
	values := attachmentProjections(attachments, options)
	data.serialization = &serializedProjection{attachments: &values}
	return data
}

func projectedRawData(data responseData, options outputOptions, hideRaw bool) responseData {
	fields := []string{}
	if data.RawSource != nil && !hideRaw {
		fields = projectionFields(projectionTargetRaw, options, false)
	}
	data.Projection = &projectionInfo{View: options.view, Fields: fields}
	data.serialization = &serializedProjection{hideRaw: hideRaw || !options.includes("raw_source")}
	return data
}

func failProjectedMessage(
	command string,
	jsonOutput bool,
	message mail.Message,
	options outputOptions,
	err error,
	stdout io.Writer,
	stderr io.Writer,
) int {
	if !jsonOutput {
		return failCommand(command, false, err, stdout, stderr)
	}
	return writeProjectedFailure(stdout, command, responseData{Message: &message}, options, err, true)
}

func failProjectedDraft(
	command string,
	jsonOutput bool,
	draft mail.Draft,
	options outputOptions,
	err error,
	stdout io.Writer,
	stderr io.Writer,
) int {
	if !jsonOutput {
		writeLine(stderr, err)
		return commandExitCode(err)
	}
	return writeProjectedFailure(stdout, command, responseData{Draft: &draft}, options, err, false)
}

func failProjectedEmpty(
	command string,
	jsonOutput bool,
	options outputOptions,
	err error,
	stdout io.Writer,
	stderr io.Writer,
) int {
	if !jsonOutput {
		return failCommand(command, false, err, stdout, stderr)
	}
	return writeProjectedFailure(stdout, command, responseData{}, options, err, false)
}

func exportMessageBody(message mail.Message, path string) (mail.ContentExport, error) {
	if !message.ContentComplete {
		return mail.ContentExport{}, &commandError{
			code: "content_incomplete", message: "message content is incomplete; export requires content_complete:true",
		}
	}
	if int64(len(message.Content)) > maximumContentExportBytes {
		return mail.ContentExport{}, &commandError{
			code: "content_export_too_large", message: "message content exceeds the 64 MiB export limit",
		}
	}
	return mail.WriteExclusiveContent(path, func(writer io.Writer) error {
		_, err := io.WriteString(writer, message.Content)
		return err
	})
}

func writeProjectedSuccess(stdout io.Writer, command string, data responseData, options outputOptions) int {
	data = dataForProjection(data, options, false)
	payload, err := marshalEnvelope(envelope{SchemaVersion: schemaVersion, OK: true, Command: command, Data: data})
	if err != nil {
		return 1
	}
	if int64(len(payload)) > options.maxBytes {
		return writeProjectedFailure(stdout, command, data, options, &outputTooLargeError{
			actual: len(payload), limit: options.maxBytes, target: string(options.target),
		}, false)
	}
	return writeEnvelopeBytes(stdout, payload)
}

func writeProjectedFailure(stdout io.Writer, command string, data responseData, options outputOptions, err error, failed bool) int {
	data = dataForProjection(data, options, failed)
	payload, marshalErr := marshalEnvelope(envelope{
		SchemaVersion: schemaVersion, OK: false, Command: command, Data: data,
		Error: newErrorData(command, data, err),
	})
	if marshalErr != nil || int64(len(payload)) > options.maxBytes {
		// An error envelope must remain parseable even when the requested view is
		// too large. Keep the identity and recovery evidence that fits without
		// replaying the omitted body or headers.
		fallbackOptions := options
		fallbackOptions.fields = map[string]struct{}{}
		fallbackOptions.fieldsProvided = true
		fallback := dataForProjection(data, fallbackOptions, false)
		fallback.Projection = &projectionInfo{View: options.view, Fields: fallback.Projection.Fields}
		payload, marshalErr = marshalEnvelope(envelope{
			SchemaVersion: schemaVersion, OK: false, Command: command, Data: fallback,
			Error: newErrorData(command, fallback, err),
		})
	}
	if marshalErr != nil {
		return 1
	}
	if code := writeEnvelopeBytes(stdout, payload); code != 0 {
		return code
	}
	return commandExitCode(err)
}

func errorCode(err error) string {
	var typed codedError
	if errors.As(err, &typed) && typed.ErrorCode() != "" {
		return typed.ErrorCode()
	}
	return "operation_failed"
}

func dataForProjection(data responseData, options outputOptions, failed bool) responseData {
	switch options.target {
	case projectionTargetMessage:
		if data.Message != nil {
			return projectedMessageData(data, *data.Message, options, failed)
		}
		data.Projection = &projectionInfo{View: options.view, Fields: []string{}}
	case projectionTargetDraft:
		if data.Draft != nil {
			return projectedDraftData(data, *data.Draft, options)
		}
		data.Projection = &projectionInfo{View: options.view, Fields: []string{}}
	case projectionTargetAttachment:
		if data.Attachments != nil {
			return projectedAttachmentData(data, *data.Attachments, options)
		}
		data.Projection = &projectionInfo{View: options.view, Fields: []string{}}
	case projectionTargetRaw:
		return projectedRawData(data, options, options.exportPath != "")
	}
	return data
}

func marshalEnvelope(value envelope) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writeEnvelopeBytes(writer io.Writer, payload []byte) int {
	written, err := writer.Write(payload)
	if err != nil || written != len(payload) {
		return 1
	}
	return 0
}

func (data responseData) MarshalJSON() ([]byte, error) {
	type responseDataAlias responseData
	copy := data
	copy.Message, copy.Draft, copy.RawSource, copy.Attachments = nil, nil, nil, nil
	var messageRaw, draftRaw, rawSourceRaw, attachmentsRaw json.RawMessage
	if data.Message != nil {
		var encoded []byte
		var err error
		if data.serialization != nil && data.serialization.message != nil {
			encoded, err = json.Marshal(data.serialization.message)
		} else {
			encoded, err = json.Marshal(data.Message)
		}
		if err != nil {
			return nil, err
		}
		messageRaw = encoded
	}
	if data.Draft != nil {
		var encoded []byte
		var err error
		if data.serialization != nil && data.serialization.draft != nil {
			encoded, err = json.Marshal(data.serialization.draft)
		} else {
			encoded, err = json.Marshal(data.Draft)
		}
		if err != nil {
			return nil, err
		}
		draftRaw = encoded
	}
	if data.RawSource != nil && (data.serialization == nil || !data.serialization.hideRaw) {
		encoded, err := json.Marshal(data.RawSource)
		if err != nil {
			return nil, err
		}
		rawSourceRaw = encoded
	}
	if data.Attachments != nil {
		var encoded []byte
		var err error
		if data.serialization != nil && data.serialization.attachments != nil {
			encoded, err = json.Marshal(data.serialization.attachments)
		} else {
			encoded, err = json.Marshal(data.Attachments)
		}
		if err != nil {
			return nil, err
		}
		attachmentsRaw = encoded
	}
	return json.Marshal(struct {
		*responseDataAlias
		Message     json.RawMessage `json:"message,omitempty"`
		Draft       json.RawMessage `json:"draft,omitempty"`
		RawSource   json.RawMessage `json:"raw_source,omitempty"`
		Attachments json.RawMessage `json:"attachments,omitempty"`
	}{
		responseDataAlias: (*responseDataAlias)(&copy),
		Message:           messageRaw,
		Draft:             draftRaw,
		RawSource:         rawSourceRaw,
		Attachments:       attachmentsRaw,
	})
}
