package mail

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

// SendTransport bundles the direct-send dependencies: an SMTP submitter, an
// IMAP Sent-mailbox mirror, and the credential store for app-specific
// passwords. A Service created without them rejects sends with
// send_transport_unavailable instead of panicking.
type SendTransport struct {
	Submitter       transport.Submitter
	Mirror          transport.SentMirror
	Credentials     transport.CredentialStore
	AccountBindings AccountBindingStore
	Imap            transport.ImapOperator
}

// InvalidateCredentials advances the IMAP pool generation for account after
// its credential store has successfully changed it. Pooled sessions may be
// keyed by a provider-table endpoint or by an explicit binding host, so every
// resolvable identity is invalidated.
func (t SendTransport) InvalidateCredentials(account string) {
	invalidator, ok := t.ImapClient().(transport.CredentialInvalidator)
	if !ok {
		return
	}
	configs := map[transport.ImapConfig]struct{}{}
	if _, _, imapHost, imapPort, err := transport.ProviderHosts(account); err == nil {
		configs[transport.ImapConfig{Host: imapHost, Port: imapPort, Username: account}] = struct{}{}
	}
	if t.AccountBindings != nil {
		if document, err := t.AccountBindings.LoadAccountBindings(); err == nil {
			for _, binding := range document.Bindings {
				if binding.IMAPHost == "" || !strings.EqualFold(binding.CredentialAccount, account) {
					continue
				}
				configs[transport.ImapConfig{Host: binding.IMAPHost, Port: binding.IMAPPort, Username: account}] = struct{}{}
			}
		}
	}
	for config := range configs {
		invalidator.InvalidateCredentials(config)
	}
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

func (t SendTransport) CheckMutationLock(ctx context.Context, cfg transport.ImapConfig) error {
	checker, ok := t.ImapClient().(interface {
		CheckMutationLock(context.Context, transport.ImapConfig) error
	})
	if !ok {
		return nil
	}
	return checker.CheckMutationLock(ctx, cfg)
}

type Service struct {
	gateway   Gateway
	draftRoot string
	send      SendTransport
	// contentObserver is optional per-service render instrumentation; normal
	// callers leave it nil.
	contentObserver draftContentObserver
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

// InvalidateCredentials propagates a successful credential change to the
// configured direct-send transport.
func (s *Service) InvalidateCredentials(account string) {
	s.send.InvalidateCredentials(account)
}

// AccountBindingStore exposes the configured private account identity store
// to command setup code without exposing the transport internals.
func (s *Service) AccountBindingStore() AccountBindingStore {
	return s.send.AccountBindings
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

// StoreProfile reports the opened Mail-store profile state when the gateway
// exposes it; an absent gateway or closed store reports false.
func (s *Service) StoreProfile() (StoreProfile, bool) {
	if s == nil || s.gateway == nil {
		return StoreProfile{}, false
	}
	profiler, supported := s.gateway.(StoreProfiler)
	if !supported {
		return StoreProfile{}, false
	}
	return profiler.StoreProfile()
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
	catalog, err := s.ListAccountCatalog(ctx)
	return catalog.Accounts, err
}

func (s *Service) ListAccountCatalog(ctx context.Context) (AccountCatalog, error) {
	var catalog AccountCatalog
	if reader, ok := s.gateway.(AccountCatalogReader); ok {
		var err error
		catalog, err = reader.ListAccountCatalog(ctx)
		if err != nil {
			return catalog, err
		}
	} else {
		accounts, err := s.gateway.ListAccounts(ctx)
		if err != nil {
			return AccountCatalog{}, err
		}
		for index := range accounts {
			if accounts[index].IdentityCoverage.State != "" {
				continue
			}
			accounts[index].IdentityCoverage = SenderIdentityCoverage{
				Source: SenderIdentityCoverageSourceUnknown,
				State:  SenderIdentityCoverageStateUnavailable,
			}
		}
		catalog = AccountCatalog{Accounts: accounts, Complete: true}
	}
	s.annotateDirectOpsSupport(catalog.Accounts)
	return catalog, nil
}

// annotateDirectOpsSupport fills DirectOpsSupported/DirectOpsReason per
// account using the same binding document and ResolveTransportHosts the
// mutation and send paths use. Bindings apply to IMAP accounts only,
// mirroring catalog assembly. A binding-load failure means every direct
// operation fails the same lookup, so all accounts report
// unsupported_provider in that case.
func (s *Service) annotateDirectOpsSupport(accounts []Account) {
	var document AccountBindingFile
	if s.send.AccountBindings != nil {
		loaded, err := s.send.AccountBindings.LoadAccountBindings()
		if err != nil {
			for index := range accounts {
				accounts[index].DirectOpsSupported = false
				accounts[index].DirectOpsReason = DirectOpsReasonUnsupportedProvider
			}
			return
		}
		document = loaded
	}
	for index := range accounts {
		var binding *AccountBinding
		if accounts[index].Type == AccountTypeIMAP {
			if ref, err := mailref.DecodeAccount(accounts[index].Ref); err == nil {
				if resolved, found, err := FindAccountBinding(document, ref.AccountID); err == nil && found {
					binding = &resolved
				}
			}
		}
		accounts[index].DirectOpsSupported, accounts[index].DirectOpsReason =
			AccountDirectOpsSupport(accounts[index], binding)
	}
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

// MessageState reads the verified server-side flag snapshot for one message
// and pairs it with the local index projection. It performs no mutation.
func (s *Service) MessageState(ctx context.Context, ref string) (MessageState, error) {
	if ref == "" {
		return MessageState{}, validationError("message ref is required")
	}
	return s.gateway.MessageState(ctx, ref)
}

// MessageThread returns the bounded chronological member list of the
// Envelope Index conversation containing the referenced message.
func (s *Service) MessageThread(ctx context.Context, request MessageThreadRequest) (MessageThread, error) {
	if strings.TrimSpace(request.Ref) == "" {
		return MessageThread{}, validationError("message ref is required")
	}
	return s.gateway.MessageThread(ctx, request)
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
	result, err := s.gateway.DeleteMessage(ctx, request)
	return result, err
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
