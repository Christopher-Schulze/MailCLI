package mail

import (
	"context"
	"fmt"
	"io"
	"slices"
	"time"

	"mailcli/internal/transport"
)

// SendTransport bundles the direct-send dependencies: an SMTP submitter, an
// IMAP Sent-mailbox mirror, and the credential store for app-specific
// passwords. A Service created without them rejects sends with
// send_transport_unavailable instead of panicking.
type SendTransport struct {
	Submitter   transport.Submitter
	Mirror      transport.SentMirror
	Credentials transport.CredentialStore
	Imap        transport.ImapOperator
}

func (t SendTransport) ImapClient() transport.ImapOperator {
	if t.Imap != nil {
		return t.Imap
	}
	if op, ok := t.Mirror.(transport.ImapOperator); ok {
		return op
	}
	return nil
}

type Service struct {
	gateway   Gateway
	draftRoot string
	send      SendTransport
}

type ValidationError struct {
	Message string
}

type OperationError struct {
	Code    string
	Message string
}

func NewService(gateway Gateway) *Service {
	return &Service{gateway: gateway}
}

func NewServiceWithDraftRoot(gateway Gateway, draftRoot string) *Service {
	return &Service{gateway: gateway, draftRoot: draftRoot}
}

func NewServiceWithTransport(gateway Gateway, draftRoot string, send SendTransport) *Service {
	return &Service{gateway: gateway, draftRoot: draftRoot, send: send}
}

func (e *ValidationError) Error() string {
	return e.Message
}

func (e *ValidationError) ErrorCode() string {
	return "invalid_argument"
}

func (e *OperationError) Error() string {
	return e.Message
}

func (e *OperationError) ErrorCode() string {
	return e.Code
}

func (s *Service) Probe(ctx context.Context, live bool) DiagnosticReport {
	return s.gateway.Probe(ctx, live)
}

func (s *Service) ProbeWithDiagnostics(ctx context.Context, live bool) (DiagnosticReport, []DiagnosticTiming) {
	if prober, ok := s.gateway.(interface {
		ProbeWithDiagnostics(context.Context, bool) (DiagnosticReport, []DiagnosticTiming)
	}); ok {
		return prober.ProbeWithDiagnostics(ctx, live)
	}
	started := time.Now()
	report := s.Probe(ctx, live)
	return report, []DiagnosticTiming{{Phase: "probe", Milliseconds: elapsedMilliseconds(started)}}
}

func elapsedMilliseconds(started time.Time) float64 {
	return float64(time.Since(started).Microseconds()) / 1000
}

func (s *Service) ListAccounts(ctx context.Context) ([]Account, error) {
	return s.gateway.ListAccounts(ctx)
}

func (s *Service) ListMailboxes(ctx context.Context, request ListMailboxesRequest) ([]Mailbox, error) {
	return s.gateway.ListMailboxes(ctx, request)
}

func (s *Service) ResolveMailbox(ctx context.Context, accountRef string, path []string) (Mailbox, error) {
	if accountRef == "" || len(path) == 0 {
		return Mailbox{}, validationError("account ref and at least one mailbox path segment are required")
	}
	mailboxes, err := s.gateway.ListMailboxes(ctx, ListMailboxesRequest{AccountRef: accountRef})
	if err != nil {
		return Mailbox{}, err
	}
	for _, mailbox := range mailboxes {
		if slices.Equal(mailbox.Path, path) {
			return mailbox, nil
		}
	}
	return Mailbox{}, validationError("mailbox path was not found in the selected account")
}

func (s *Service) ListMessages(ctx context.Context, request ListMessagesRequest) (MessagePage, error) {
	if request.MailboxRef == "" {
		return MessagePage{}, validationError("mailbox ref is required")
	}
	limit, err := normalizeLimit(request.Limit)
	if err != nil {
		return MessagePage{}, err
	}
	request.Limit = limit
	return s.gateway.ListMessages(ctx, request)
}

func (s *Service) GetMessage(ctx context.Context, ref string) (Message, error) {
	if ref == "" {
		return Message{}, validationError("message ref is required")
	}
	return s.gateway.GetMessage(ctx, ref)
}

func (s *Service) OpenDraft(ctx context.Context, ref string) (Message, error) {
	if ref == "" {
		return Message{}, validationError("draft message ref is required")
	}
	return s.gateway.OpenDraft(ctx, ref)
}

func (s *Service) GetRawSource(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", validationError("message ref is required")
	}
	return s.gateway.GetRawSource(ctx, ref)
}

func (s *Service) WriteRawSource(ctx context.Context, ref string, writer io.Writer) error {
	if ref == "" {
		return validationError("message ref is required")
	}
	if streamer, ok := s.gateway.(interface {
		WriteRawSource(context.Context, string, io.Writer) error
	}); ok {
		return streamer.WriteRawSource(ctx, ref, writer)
	}
	raw, err := s.gateway.GetRawSource(ctx, ref)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(writer, raw); err != nil {
		return fmt.Errorf("write RFC message source: %w", err)
	}
	return nil
}

func (s *Service) MarkMessage(ctx context.Context, request MarkMessageRequest) (MessageSummary, error) {
	if request.Ref == "" {
		return MessageSummary{}, validationError("message ref is required")
	}
	if request.Read == nil && request.Flagged == nil && request.Junk == nil {
		return MessageSummary{}, validationError("at least one message state is required")
	}
	return s.gateway.MarkMessage(ctx, request)
}

func (s *Service) TransferMessage(ctx context.Context, request TransferMessageRequest) (MessageSummary, error) {
	if request.Ref == "" || request.DestinationMailbox == "" {
		return MessageSummary{}, validationError("message ref and destination mailbox ref are required")
	}
	return s.gateway.TransferMessage(ctx, request)
}

func (s *Service) DeleteMessage(ctx context.Context, request DeleteMessageRequest) (DeleteResult, error) {
	if request.Ref == "" {
		return DeleteResult{}, validationError("message ref is required")
	}
	if err := s.gateway.DeleteMessage(ctx, request); err != nil {
		return DeleteResult{}, err
	}
	return DeleteResult{MessageRef: request.Ref, Deleted: true}, nil
}

func (s *Service) Sync(ctx context.Context, accountRef string) (SyncResult, error) {
	if err := s.gateway.Sync(ctx, accountRef); err != nil {
		return SyncResult{}, err
	}
	return SyncResult{AccountRef: accountRef, Triggered: true}, nil
}

type SyncChecker interface {
	SyncCheck(ctx context.Context, accountRef string) (SyncCheckResult, error)
}

func (s *Service) SyncCheck(ctx context.Context, accountRef string) (SyncCheckResult, error) {
	if checker, ok := s.gateway.(SyncChecker); ok {
		return checker.SyncCheck(ctx, accountRef)
	}
	return SyncCheckResult{}, fmt.Errorf("sync --check is not supported by the active mail gateway")
}
func IsHealthy(report DiagnosticReport) bool {
	for _, check := range report.Checks {
		if check.Status == "fail" {
			return false
		}
	}
	return true
}

func normalizeLimit(limit int) (int, error) {
	if limit == 0 {
		return DefaultPageLimit, nil
	}
	if limit < 0 || limit > MaximumPageLimit {
		return 0, validationError(fmt.Sprintf("limit must be between 1 and %d", MaximumPageLimit))
	}
	return limit, nil
}

func validationError(message string) error {
	return &ValidationError{Message: message}
}
