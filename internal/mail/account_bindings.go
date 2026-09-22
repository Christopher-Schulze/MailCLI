package mail

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdmail "net/mail"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mailcli/internal/transport"
)

const (
	AccountBindingVersion       = 1
	maximumAccountBindingBytes  = 256 * 1024
	maximumAccountBindings      = 128
	maximumSenderAliasesPerBind = 64
)

// AccountBinding connects one stable Mail account identity to the sender
// aliases that may be used for it and to the Keychain lookup identity. It
// never contains a password or any other secret. The optional explicit
// endpoint fields pin SMTP submission and IMAP access to validated public
// hosts; when present they take precedence over the provider domain table.
type AccountBinding struct {
	AccountID         string   `json:"account_id"`
	SenderAliases     []string `json:"sender_aliases"`
	CredentialAccount string   `json:"credential_account"`
	SMTPHost          string   `json:"smtp_host,omitempty"`
	SMTPPort          int      `json:"smtp_port,omitempty"`
	IMAPHost          string   `json:"imap_host,omitempty"`
	IMAPPort          int      `json:"imap_port,omitempty"`
}

// AccountBindingFile is the private, versioned on-disk binding document.
type AccountBindingFile struct {
	Version  int              `json:"version"`
	Bindings []AccountBinding `json:"bindings"`
}

// AccountBindingStore persists explicit account identity bindings.
type AccountBindingStore interface {
	LoadAccountBindings() (AccountBindingFile, error)
	UpsertAccountBinding(AccountBinding) error
}

// AccountBindingError is a typed failure for binding state or validation.
type AccountBindingError struct {
	Code    string
	Message string
	Err     error
}

func (e *AccountBindingError) Error() string {
	if e.Err == nil {
		return e.Code + ": " + e.Message
	}
	return e.Code + ": " + e.Message + ": " + e.Err.Error()
}

func (e *AccountBindingError) Unwrap() error { return e.Err }

func (e *AccountBindingError) ErrorCode() string { return e.Code }

// NewAccountBindingStore returns a file-backed binding store. An empty path
// resolves to the user's private MailCLI application-support directory.
func NewAccountBindingStore(path string) AccountBindingStore {
	return &fileAccountBindingStore{path: path}
}

// DefaultAccountBindingStore returns the standard private binding store.
func DefaultAccountBindingStore() AccountBindingStore {
	return NewAccountBindingStore("")
}

// NormalizeAccountBinding validates and canonicalizes one binding before it
// is persisted or used. Provider aliases and the credential identity must
// belong to the same supported provider family.
func NormalizeAccountBinding(binding AccountBinding) (AccountBinding, error) {
	binding.AccountID = strings.ToUpper(strings.TrimSpace(binding.AccountID))
	if binding.AccountID == "" || strings.ContainsAny(binding.AccountID, "\r\n\x00") {
		return AccountBinding{}, &AccountBindingError{
			Code:    "account_binding_invalid",
			Message: "account binding requires a stable account ID without control characters",
		}
	}
	if len(binding.SenderAliases) == 0 || len(binding.SenderAliases) > maximumSenderAliasesPerBind {
		return AccountBinding{}, &AccountBindingError{
			Code:    "account_binding_invalid",
			Message: fmt.Sprintf("account binding requires 1 to %d sender aliases", maximumSenderAliasesPerBind),
		}
	}
	credential, err := normalizeBindingAddress(binding.CredentialAccount)
	if err != nil {
		return AccountBinding{}, &AccountBindingError{
			Code:    "account_binding_invalid",
			Message: "credential account is invalid",
			Err:     err,
		}
	}
	binding.CredentialAccount = credential
	binding, err = normalizeBindingEndpoints(binding)
	if err != nil {
		return AccountBinding{}, err
	}
	explicitHosts := binding.SMTPHost != "" || binding.IMAPHost != ""
	seen := make(map[string]struct{}, len(binding.SenderAliases))
	aliases := make([]string, 0, len(binding.SenderAliases))
	var providerHost string
	var providerPort int
	for _, value := range binding.SenderAliases {
		alias, err := normalizeBindingAddress(value)
		if err != nil {
			return AccountBinding{}, &AccountBindingError{
				Code:    "account_binding_invalid",
				Message: "sender alias is invalid",
				Err:     err,
			}
		}
		key := strings.ToLower(alias)
		if _, exists := seen[key]; exists {
			return AccountBinding{}, &AccountBindingError{
				Code:    "account_binding_invalid",
				Message: "sender aliases must be unique case-insensitively",
			}
		}
		seen[key] = struct{}{}
		if !explicitHosts {
			_, _, imapHost, imapPort, providerErr := transport.ProviderHosts(alias)
			if providerErr != nil {
				return AccountBinding{}, providerBindingError("sender alias", alias, providerErr)
			}
			if providerHost == "" {
				providerHost, providerPort = imapHost, imapPort
			} else if providerHost != imapHost || providerPort != imapPort {
				return AccountBinding{}, &AccountBindingError{
					Code:    "account_binding_provider_mismatch",
					Message: "sender aliases must use one supported provider",
				}
			}
		}
		aliases = append(aliases, alias)
	}
	if !explicitHosts {
		_, _, credentialHost, credentialPort, providerErr := transport.ProviderHosts(credential)
		if providerErr != nil {
			return AccountBinding{}, providerBindingError("credential account", credential, providerErr)
		}
		if providerHost != credentialHost || providerPort != credentialPort {
			return AccountBinding{}, &AccountBindingError{
				Code:    "account_binding_provider_mismatch",
				Message: "credential account must use the same supported provider as its sender aliases",
			}
		}
	}
	sort.Slice(aliases, func(left, right int) bool {
		return strings.ToLower(aliases[left]) < strings.ToLower(aliases[right])
	})
	binding.SenderAliases = aliases
	return binding, nil
}

// ValidateBindingHosts checks one explicit endpoint set without touching a
// binding. CLI validation runs it before credentials are prompted or stored.
func ValidateBindingHosts(smtpHost string, smtpPort int, imapHost string, imapPort int) error {
	_, err := normalizeBindingEndpoints(AccountBinding{
		SMTPHost: smtpHost, SMTPPort: smtpPort,
		IMAPHost: imapHost, IMAPPort: imapPort,
	})
	return err
}

// normalizeBindingEndpoints validates the optional explicit host/port pairs.
// Each leg is independent: a host requires a usable port and a port requires
// a host. Hosts must be public DNS names or public IP literals; loopback,
// private, link-local, multicast, and unspecified targets are rejected.
func normalizeBindingEndpoints(binding AccountBinding) (AccountBinding, error) {
	var err error
	if binding.SMTPHost, binding.SMTPPort, err = normalizeBindingEndpoint("smtp", binding.SMTPHost, binding.SMTPPort); err != nil {
		return AccountBinding{}, err
	}
	if binding.IMAPHost, binding.IMAPPort, err = normalizeBindingEndpoint("imap", binding.IMAPHost, binding.IMAPPort); err != nil {
		return AccountBinding{}, err
	}
	return binding, nil
}

func normalizeBindingEndpoint(protocol, host string, port int) (string, int, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		if port != 0 {
			return "", 0, &AccountBindingError{
				Code:    "account_binding_host_invalid",
				Message: protocol + " port requires an explicit host",
			}
		}
		return "", 0, nil
	}
	if port < 1 || port > 65535 {
		return "", 0, &AccountBindingError{
			Code:    "account_binding_host_invalid",
			Message: protocol + " port must be between 1 and 65535",
		}
	}
	normalized, err := normalizeBindingHost(host)
	if err != nil {
		return "", 0, &AccountBindingError{
			Code:    "account_binding_host_invalid",
			Message: protocol + " host is invalid",
			Err:     err,
		}
	}
	return normalized, port, nil
}

func normalizeBindingHost(host string) (string, error) {
	if len(host) > 253 || strings.ContainsAny(host, " \t\r\n\x00/@") {
		return "", errors.New("host contains invalid characters")
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
			addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
			return "", errors.New("host must not be a private, loopback, link-local, multicast, or unspecified address")
		}
		return addr.String(), nil
	}
	if strings.ContainsAny(host, ":") {
		return "", errors.New("host is not a valid DNS name or IP literal")
	}
	if host == "localhost" {
		return "", errors.New("host must not be a private or reserved name")
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".home.arpa", ".lan", ".corp"} {
		if strings.HasSuffix(host, suffix) {
			return "", errors.New("host must not be a private or reserved name")
		}
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "", errors.New("host must be a fully qualified DNS name or a public IP literal")
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return "", errors.New("host label is empty or too long")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("host label must not start or end with a hyphen")
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return "", errors.New("host contains invalid characters")
			}
		}
	}
	return host, nil
}

// ResolveTransportHosts resolves the SMTP and IMAP endpoints for a sender,
// preferring explicit binding hosts per leg and falling back to the provider
// domain table. Explicit hosts arrive pre-validated by binding normalization;
// when the provider lookup fails, both legs must be explicit to proceed.
func ResolveTransportHosts(email string, binding *AccountBinding) (smtpHost string, smtpPort int, imapHost string, imapPort int, err error) {
	smtpHost, smtpPort, imapHost, imapPort, err = transport.ProviderHosts(email)
	if binding != nil {
		if binding.SMTPHost != "" {
			smtpHost, smtpPort = binding.SMTPHost, binding.SMTPPort
		}
		if binding.IMAPHost != "" {
			imapHost, imapPort = binding.IMAPHost, binding.IMAPPort
		}
	}
	if err != nil && (smtpHost == "" || imapHost == "") {
		return "", 0, "", 0, err
	}
	return smtpHost, smtpPort, imapHost, imapPort, nil
}

func normalizeBindingAddress(value string) (string, error) {
	parsed, err := stdmail.ParseAddress(strings.TrimSpace(value))
	if err != nil || parsed.Address == "" {
		if err == nil {
			err = errors.New("address is empty")
		}
		return "", err
	}
	if strings.ContainsAny(parsed.Address, "\r\n\x00") {
		return "", errors.New("address contains control characters")
	}
	return MailboxAddrSpec(parsed.Address), nil
}

func providerBindingError(kind, address string, cause error) error {
	code := transport.ErrorCode(cause)
	if code == "" {
		code = "account_binding_provider_invalid"
	}
	return &AccountBindingError{
		Code:    code,
		Message: fmt.Sprintf("%s %s is not supported", kind, address),
		Err:     cause,
	}
}

func validateAccountBindingFile(document AccountBindingFile) (AccountBindingFile, error) {
	if document.Version != AccountBindingVersion {
		return AccountBindingFile{}, &AccountBindingError{
			Code:    "account_binding_version_unsupported",
			Message: fmt.Sprintf("account binding file version %d is unsupported; expected %d", document.Version, AccountBindingVersion),
		}
	}
	if len(document.Bindings) > maximumAccountBindings {
		return AccountBindingFile{}, &AccountBindingError{
			Code:    "account_binding_invalid",
			Message: fmt.Sprintf("account binding file contains more than %d bindings", maximumAccountBindings),
		}
	}
	seen := make(map[string]struct{}, len(document.Bindings))
	bindings := make([]AccountBinding, 0, len(document.Bindings))
	for _, binding := range document.Bindings {
		normalized, err := NormalizeAccountBinding(binding)
		if err != nil {
			return AccountBindingFile{}, err
		}
		if _, exists := seen[normalized.AccountID]; exists {
			return AccountBindingFile{}, &AccountBindingError{
				Code:    "account_binding_invalid",
				Message: "account binding file contains duplicate account IDs",
			}
		}
		seen[normalized.AccountID] = struct{}{}
		bindings = append(bindings, normalized)
	}
	sort.Slice(bindings, func(left, right int) bool {
		return bindings[left].AccountID < bindings[right].AccountID
	})
	document.Bindings = bindings
	return document, nil
}

// ResolveAccountBinding finds the binding for an alias. A missing binding is
// reported by found=false so callers can retain the legacy sender fallback.
// Without an account ID, multiple matching accounts fail closed.
func ResolveAccountBinding(
	document AccountBindingFile,
	alias string,
	accountID string,
) (binding AccountBinding, found bool, err error) {
	document, err = validateAccountBindingFile(document)
	if err != nil {
		return AccountBinding{}, false, err
	}
	canonical, err := normalizeBindingAddress(alias)
	if err != nil {
		return AccountBinding{}, false, &AccountBindingError{
			Code:    "account_binding_invalid",
			Message: "sender alias is invalid",
			Err:     err,
		}
	}
	accountID = strings.ToUpper(strings.TrimSpace(accountID))
	matches := make([]AccountBinding, 0, 2)
	for _, candidate := range document.Bindings {
		if accountID != "" && candidate.AccountID != accountID {
			continue
		}
		for _, candidateAlias := range candidate.SenderAliases {
			if strings.EqualFold(candidateAlias, canonical) {
				matches = append(matches, candidate)
				break
			}
		}
	}
	if len(matches) == 0 {
		return AccountBinding{}, false, nil
	}
	if len(matches) > 1 {
		return AccountBinding{}, false, &AccountBindingError{
			Code:    "account_binding_ambiguous",
			Message: fmt.Sprintf("sender alias %s is bound to multiple Mail accounts; provide an explicit account ref", canonical),
		}
	}
	return matches[0], true, nil
}

// FindAccountBinding returns the binding for one stable account identity.
// It validates the complete document before selecting the entry so callers
// never use a partially trusted mapping.
func FindAccountBinding(
	document AccountBindingFile,
	accountID string,
) (binding AccountBinding, found bool, err error) {
	document, err = validateAccountBindingFile(document)
	if err != nil {
		return AccountBinding{}, false, err
	}
	accountID = strings.ToUpper(strings.TrimSpace(accountID))
	if accountID == "" {
		return AccountBinding{}, false, &AccountBindingError{
			Code:    "account_binding_invalid",
			Message: "account binding lookup requires a stable account ID",
		}
	}
	for _, candidate := range document.Bindings {
		if candidate.AccountID == accountID {
			return candidate, true, nil
		}
	}
	return AccountBinding{}, false, nil
}

type fileAccountBindingStore struct {
	path string
}

func (s *fileAccountBindingStore) bindingPath(create bool) (string, error) {
	path := s.path
	if path == "" {
		configRoot, err := os.UserConfigDir()
		if err != nil {
			return "", &AccountBindingError{Code: "account_binding_unavailable", Message: "resolve MailCLI application-support directory", Err: err}
		}
		path = filepath.Join(configRoot, "MailCLI", "account-bindings.json")
	}
	if !filepath.IsAbs(path) {
		return "", &AccountBindingError{Code: "account_binding_invalid", Message: "account binding path must be absolute"}
	}
	directory := filepath.Dir(path)
	if create {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return "", &AccountBindingError{Code: "account_binding_unavailable", Message: "create private account-binding directory", Err: err}
		}
	}
	info, err := os.Lstat(directory)
	if err != nil {
		if !create && os.IsNotExist(err) {
			return path, nil
		}
		return "", &AccountBindingError{Code: "account_binding_unavailable", Message: "inspect account-binding directory", Err: err}
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", &AccountBindingError{Code: "account_binding_unavailable", Message: "account-binding directory must be a real directory"}
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", &AccountBindingError{Code: "account_binding_unavailable", Message: "restrict account-binding directory", Err: err}
	}
	return path, nil
}

func (s *fileAccountBindingStore) LoadAccountBindings() (AccountBindingFile, error) {
	path, err := s.bindingPath(false)
	if err != nil {
		return AccountBindingFile{}, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return AccountBindingFile{Version: AccountBindingVersion, Bindings: []AccountBinding{}}, nil
	}
	if err != nil {
		return AccountBindingFile{}, &AccountBindingError{Code: "account_binding_unavailable", Message: "inspect account-binding file", Err: err}
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maximumAccountBindingBytes {
		return AccountBindingFile{}, &AccountBindingError{Code: "account_binding_invalid", Message: "account-binding file must be a bounded regular file"}
	}
	payload, err := readBoundedRegularFile(path, info, maximumAccountBindingBytes)
	if err != nil {
		return AccountBindingFile{}, &AccountBindingError{Code: "account_binding_unavailable", Message: "read account-binding file", Err: err}
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var document AccountBindingFile
	if err := decoder.Decode(&document); err != nil {
		return AccountBindingFile{}, &AccountBindingError{Code: "account_binding_invalid", Message: "decode account-binding file", Err: err}
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return AccountBindingFile{}, &AccountBindingError{Code: "account_binding_invalid", Message: "account-binding file must contain exactly one JSON object", Err: err}
	}
	return validateAccountBindingFile(document)
}

func (s *fileAccountBindingStore) UpsertAccountBinding(binding AccountBinding) error {
	normalized, err := NormalizeAccountBinding(binding)
	if err != nil {
		return err
	}
	document, err := s.LoadAccountBindings()
	if err != nil {
		return err
	}
	replaced := false
	for index := range document.Bindings {
		if document.Bindings[index].AccountID == normalized.AccountID {
			document.Bindings[index] = normalized
			replaced = true
			break
		}
	}
	if !replaced {
		document.Bindings = append(document.Bindings, normalized)
	}
	document, err = validateAccountBindingFile(document)
	if err != nil {
		return err
	}
	payload, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return &AccountBindingError{Code: "account_binding_invalid", Message: "encode account-binding file", Err: err}
	}
	payload = append(payload, '\n')
	if len(payload) > maximumAccountBindingBytes {
		return &AccountBindingError{Code: "account_binding_invalid", Message: "account-binding file exceeds 256 KiB"}
	}
	path, err := s.bindingPath(true)
	if err != nil {
		return err
	}
	temporary, err := attachmentTemporaryPath(path)
	if err != nil {
		return &AccountBindingError{Code: "account_binding_unavailable", Message: "create account-binding temporary path", Err: err}
	}
	defer func() { _ = removeIfPresent(temporary) }()
	if err := writePrivateFile(temporary, payload); err != nil {
		return &AccountBindingError{Code: "account_binding_unavailable", Message: "write account-binding file", Err: err}
	}
	if err := os.Rename(temporary, path); err != nil {
		return &AccountBindingError{Code: "account_binding_unavailable", Message: "publish account-binding file", Err: err}
	}
	return syncDirectory(filepath.Dir(path))
}
