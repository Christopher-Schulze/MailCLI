package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	stdmail "net/mail"
	"strings"

	"mailcli/internal/keychain"
	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

// sendSetupCredentials is overridable in tests so tests never touch the
// real keychain.
var sendSetupCredentials = keychain.New

// sendSetupBindings is overridable in tests so setup tests never touch the
// user's private account-binding file.
var sendSetupBindings = mail.DefaultAccountBindingStore

// sendSetupStdin is overridable in tests so the password prompt can be fed
// from a pipe instead of the terminal.
var sendSetupStdin io.Reader = osStdin

// sendSetupAccounts lists the Mail account catalog when setup runs without a
// Mail service; it is nil in production, where the service provides it, and
// overridable in tests.
var sendSetupAccounts func(context.Context) ([]mail.Account, error)

// sendSetupResult reports one keychain mutation for the send.setup command.
type sendSetupResult struct {
	Account           string `json:"account"`
	AccountRef        string `json:"account_ref,omitempty"`
	CredentialAccount string `json:"credential_account,omitempty"`
	Action            string `json:"action"`
}

type sendSetupPartialEffect struct {
	Step   string                               `json:"step"`
	Status mail.AccountBindingPublicationStatus `json:"status"`
}

func runSend(args []string, stdout io.Writer, stderr io.Writer) int {
	return runSendWithBindingsContext(context.Background(), args, stdout, stderr, nil, nil)
}

func runSendWithInvalidator(
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	invalidateCredentials func(string),
) int {
	return runSendWithBindingsContext(context.Background(), args, stdout, stderr, invalidateCredentials, nil)
}

func runSendWithBindings(
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	invalidateCredentials func(string),
	bindings mail.AccountBindingStore,
) int {
	return runSendWithBindingsContext(context.Background(), args, stdout, stderr, invalidateCredentials, bindings)
}

func runSendWithBindingsContext(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	invalidateCredentials func(string),
	bindings mail.AccountBindingStore,
) int {
	handler := func(ctx context.Context, _ *mail.Service, args []string, stdout, stderr io.Writer) int {
		if bindings == nil {
			bindings = sendSetupBindings()
		}
		return runSendSetup(ctx, args, stdout, stderr, invalidateCredentials, bindings, sendSetupAccounts)
	}
	return runCommandFamily(ctx, nil, "send", args, stdout, stderr, handler)
}

func runSendSetupCommand(
	ctx context.Context,
	service *mail.Service,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	var invalidateCredentials func(string)
	var bindings mail.AccountBindingStore
	listAccounts := sendSetupAccounts
	if service != nil {
		invalidateCredentials = service.InvalidateCredentials
		bindings = service.AccountBindingStore()
		listAccounts = service.ListAccounts
	}
	if bindings == nil {
		bindings = sendSetupBindings()
	}
	return runSendSetup(ctx, args, stdout, stderr, invalidateCredentials, bindings, listAccounts)
}

type sendSetupOptions struct {
	from, accountRef, credentialAccount, smtpHost, imapHost *string
	smtpPort, imapPort                                      *int
	remove, jsonOutput                                      *bool
}

func newSendSetupFlags(stderr io.Writer) (*flag.FlagSet, sendSetupOptions) {
	flags := newFlagSet("send setup", stderr)
	return flags, sendSetupOptions{
		from:              flags.String("from", "", "sender email address"),
		accountRef:        flags.String("account", "", "account ref to bind to the sender"),
		credentialAccount: flags.String("credential-account", "", "keychain account used for this sender"),
		smtpHost:          flags.String("smtp-host", "", "explicit SMTP submission host for the account binding"),
		smtpPort:          flags.Int("smtp-port", 0, "explicit SMTP submission port for the account binding"),
		imapHost:          flags.String("imap-host", "", "explicit IMAP host for the account binding"),
		imapPort:          flags.Int("imap-port", 0, "explicit IMAP port for the account binding"),
		remove:            flags.Bool("remove", false, "remove the stored app-specific password"),
		jsonOutput:        flags.Bool("json", false, "emit JSON"),
	}
}

// sendSetupCommandRequired reports whether setup must open the Mail service:
// storing a password without --account resolves the owning account from the
// catalog. Unparseable arguments open nothing and fail in the handler.
func sendSetupCommandRequired(args []string) bool {
	if len(args) == 0 || helpOnly(args) {
		return false
	}
	flags, options := newSendSetupFlags(io.Discard)
	if err := flags.Parse(args); err != nil {
		return false
	}
	return strings.TrimSpace(*options.accountRef) == "" && !*options.remove && strings.TrimSpace(*options.from) != ""
}

func runSendSetup(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	invalidateCredentials func(string),
	bindings mail.AccountBindingStore,
	listAccounts func(context.Context) ([]mail.Account, error),
) int {
	flags, options := newSendSetupFlags(stderr)
	from, accountRef, credentialAccount := options.from, options.accountRef, options.credentialAccount
	smtpHost, smtpPort, imapHost, imapPort := options.smtpHost, options.smtpPort, options.imapHost, options.imapPort
	remove, jsonOutput := options.remove, options.jsonOutput
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	if err := ctx.Err(); err != nil {
		return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
	}
	hostFlags, credentialFlag := false, false
	flags.Visit(func(value *flag.Flag) {
		switch value.Name {
		case "smtp-host", "smtp-port", "imap-host", "imap-port":
			hostFlags = true
		case "credential-account":
			credentialFlag = true
		}
	})
	parsed, err := stdmail.ParseAddress(strings.TrimSpace(*from))
	if err != nil || parsed.Address == "" {
		return failCommand(
			"send.setup", *jsonOutput,
			&commandError{code: "invalid_argument", message: "send setup requires a valid --from <email>"},
			stdout, stderr,
		)
	}
	account := mail.MailboxAddrSpec(parsed.Address)
	stableAccountID := ""
	explicitAccount := strings.TrimSpace(*accountRef) != ""
	if explicitAccount {
		ref, err := mailref.DecodeAccount(strings.TrimSpace(*accountRef))
		if err != nil {
			return failCommand("send.setup", *jsonOutput, &commandError{code: "invalid_argument", message: "send setup requires a valid --account ref: " + err.Error()}, stdout, stderr)
		}
		stableAccountID = ref.AccountID
	}
	if hostFlags && stableAccountID == "" {
		return failCommand(
			"send.setup", *jsonOutput,
			&commandError{code: "invalid_argument", message: "explicit binding hosts require --account so the binding carries them"},
			stdout, stderr,
		)
	}
	if hostFlags {
		if err := mail.ValidateBindingHosts(*smtpHost, *smtpPort, *imapHost, *imapPort); err != nil {
			return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
		}
	}
	if !explicitAccount && !*remove {
		resolvedID, resolvedRef, err := resolveSendSetupAccount(ctx, listAccounts, account)
		if err != nil {
			return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
		}
		stableAccountID, *accountRef = resolvedID, resolvedRef
	}
	var existingBinding mail.AccountBinding
	var bindingDocument mail.AccountBindingFile
	bindingFound := false
	if stableAccountID != "" {
		var err error
		bindingDocument, existingBinding, bindingFound, err = loadSendBinding(bindings, stableAccountID)
		if err != nil {
			return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
		}
	}
	explicitHosts := hostFlags || (bindingFound && (existingBinding.SMTPHost != "" || existingBinding.IMAPHost != ""))
	if !explicitHosts {
		if _, _, _, _, err := transport.ProviderHosts(account); err != nil {
			return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
		}
	}
	credential := account
	if bindingFound && strings.TrimSpace(*credentialAccount) == "" {
		credential = existingBinding.CredentialAccount
	}
	if credentialFlag {
		credentialParsed, err := stdmail.ParseAddress(strings.TrimSpace(*credentialAccount))
		if err != nil || credentialParsed.Address == "" {
			return failCommand("send.setup", *jsonOutput, &commandError{code: "invalid_argument", message: "send setup requires a valid --credential-account <email>"}, stdout, stderr)
		}
		credential = mail.MailboxAddrSpec(credentialParsed.Address)
		if !explicitHosts {
			if _, _, _, _, err := transport.ProviderHosts(credential); err != nil {
				return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
			}
		}
	}
	if !explicitAccount && !strings.EqualFold(account, credential) {
		return failCommand("send.setup", *jsonOutput, &commandError{code: "invalid_argument", message: "--credential-account requires --account so the alias binding is explicit"}, stdout, stderr)
	}
	var hosts *bindingHosts
	if hostFlags {
		hosts = &bindingHosts{smtpHost: *smtpHost, smtpPort: *smtpPort, imapHost: *imapHost, imapPort: *imapPort}
	}
	decision := sendBindingDecision{snapshot: existingBinding, found: bindingFound, credentialExplicit: credentialFlag}
	if stableAccountID != "" && !*remove {
		// Validate the exact planned document without changing the observed
		// snapshot. Publication still repeats the merge under its owning lock.
		bindingDocument.Bindings = append([]mail.AccountBinding(nil), bindingDocument.Bindings...)
		planned, err := mergeSendBinding(bindingDocument, stableAccountID, account, credential, hosts, decision)
		if err == nil {
			_, _, err = mail.FindAccountBinding(planned, stableAccountID)
		}
		if err != nil {
			return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
		}
	}
	credentials := sendSetupCredentials()
	if *remove {
		if err := ctx.Err(); err != nil {
			return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
		}
		if err := credentials.Delete(credential); err != nil {
			return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
		}
		if invalidateCredentials != nil {
			invalidateCredentials(credential)
		}
		return writeSendSetupResult(stdout, "send.setup", *jsonOutput, sendSetupResult{
			Account: account, AccountRef: *accountRef, CredentialAccount: credential, Action: "removed",
		})
	}
	password, err := readPasswordLine(ctx, transport.ProviderSupportDescription()+"\nApp-specific password for "+account+": ")
	if err != nil {
		return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
	}
	if password == "" {
		return failCommand(
			"send.setup", *jsonOutput,
			&commandError{code: "invalid_argument", message: "app-specific password is required"},
			stdout, stderr,
		)
	}
	if err := ctx.Err(); err != nil {
		return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
	}
	if err := credentials.Store(credential, password); err != nil {
		return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
	}
	if invalidateCredentials != nil {
		invalidateCredentials(credential)
	}
	if stableAccountID != "" {
		if err := ctx.Err(); err != nil {
			return failSendSetupBindingUpdate(*jsonOutput, err, mail.AccountBindingPublicationNone, stdout, stderr)
		}
		if err := upsertSendBinding(ctx, bindings, stableAccountID, account, credential, hosts, decision); err != nil {
			return failSendSetupBindingUpdate(
				*jsonOutput, err, sendSetupBindingPublicationStatus(err), stdout, stderr,
			)
		}
	}
	return writeSendSetupResult(stdout, "send.setup", *jsonOutput, sendSetupResult{
		Account: account, AccountRef: *accountRef, CredentialAccount: credential, Action: "stored",
	})
}

// resolveSendSetupAccount selects the one Mail account that owns sender when
// setup runs without --account, so setup always publishes the binding that
// reply and account-scoped drafts require before they can be sent.
func resolveSendSetupAccount(
	ctx context.Context,
	listAccounts func(context.Context) ([]mail.Account, error),
	sender string,
) (string, string, error) {
	const remediation = "; pass --account ACCOUNT_REF from 'mailcli accounts list --json'"
	if listAccounts == nil {
		return "", "", &commandError{code: "account_catalog_incomplete", message: "the Mail account catalog is unavailable to bind " + sender + remediation}
	}
	accounts, err := listAccounts(ctx)
	if err != nil {
		return "", "", &commandError{code: "account_catalog_incomplete", message: "cannot read the Mail account catalog to bind " + sender + ": " + err.Error() + remediation, cause: err}
	}
	var refs []string
	accountID := ""
	for _, candidate := range accounts {
		owned := false
		for _, address := range candidate.EmailAddresses {
			if strings.EqualFold(mail.MailboxAddrSpec(address), sender) {
				owned = true
				break
			}
		}
		if !owned {
			continue
		}
		ref, err := mailref.DecodeAccount(candidate.Ref)
		if err != nil {
			return "", "", &commandError{code: "account_reference_invalid", message: "account ref " + candidate.Ref + " cannot be decoded: " + err.Error(), cause: err}
		}
		refs = append(refs, candidate.Ref)
		accountID = ref.AccountID
	}
	switch len(refs) {
	case 1:
		return accountID, refs[0], nil
	case 0:
		return "", "", &commandError{code: "account_identity_missing", message: "no Mail account has a provable sender identity " + sender + remediation}
	default:
		return "", "", &commandError{code: "account_binding_ambiguous", message: "sender " + sender + " belongs to several Mail accounts (" + strings.Join(refs, ", ") + ")" + remediation}
	}
}

// bindingHosts carries the optional explicit endpoint flags. A nil value
// preserves an existing binding's host fields; a non-nil value replaces all
// four fields as one coherent endpoint set.
type bindingHosts struct {
	smtpHost string
	smtpPort int
	imapHost string
	imapPort int
}

type sendBindingDecision struct {
	snapshot           mail.AccountBinding
	found              bool
	credentialExplicit bool
}

func upsertSendBinding(ctx context.Context, store mail.AccountBindingStore, accountID, alias, credential string, hosts *bindingHosts, decision sendBindingDecision) error {
	if store == nil {
		return &commandError{code: "account_binding_unavailable", message: "account binding store is unavailable"}
	}
	return store.UpdateAccountBindings(ctx, func(document mail.AccountBindingFile) (mail.AccountBindingFile, error) {
		return mergeSendBinding(document, accountID, alias, credential, hosts, decision)
	})
}

func mergeSendBinding(document mail.AccountBindingFile, accountID string, alias string, credential string, hosts *bindingHosts, decision sendBindingDecision) (mail.AccountBindingFile, error) {
	binding, found, err := mail.FindAccountBinding(document, accountID)
	if err != nil {
		return mail.AccountBindingFile{}, err
	}
	if sendBindingImplicitFieldsChanged(binding, found, hosts, decision) {
		return mail.AccountBindingFile{}, &mail.AccountBindingError{Code: "account_binding_changed", Message: "implicit account-binding fields changed during password entry; inspect accounts list --json and rerun send setup"}
	}
	if !found {
		binding = mail.AccountBinding{AccountID: accountID, SenderAliases: []string{alias}, CredentialAccount: credential}
	} else {
		if decision.credentialExplicit {
			binding.CredentialAccount = credential
		}
		known := false
		for _, value := range binding.SenderAliases {
			if strings.EqualFold(value, alias) {
				known = true
				break
			}
		}
		if !known {
			binding.SenderAliases = append(binding.SenderAliases, alias)
		}
	}
	if hosts != nil {
		binding.SMTPHost = hosts.smtpHost
		binding.SMTPPort = hosts.smtpPort
		binding.IMAPHost = hosts.imapHost
		binding.IMAPPort = hosts.imapPort
	}
	for index := range document.Bindings {
		if document.Bindings[index].AccountID == binding.AccountID {
			document.Bindings[index] = binding
			return document, nil
		}
	}
	document.Bindings = append(document.Bindings, binding)
	return document, nil
}

func sendBindingImplicitFieldsChanged(binding mail.AccountBinding, found bool, hosts *bindingHosts, decision sendBindingDecision) bool {
	if !decision.credentialExplicit || hosts == nil {
		if found != decision.found {
			return true
		}
	}
	if !decision.credentialExplicit && binding.CredentialAccount != decision.snapshot.CredentialAccount {
		return true
	}
	return hosts == nil && (binding.SMTPHost != decision.snapshot.SMTPHost || binding.SMTPPort != decision.snapshot.SMTPPort ||
		binding.IMAPHost != decision.snapshot.IMAPHost || binding.IMAPPort != decision.snapshot.IMAPPort)
}

func sendSetupErrorGuidance(guidance mail.OperationGuidance, data responseData, err error) mail.OperationGuidance {
	if len(data.PartialEffects) == 0 {
		guidance.EffectCertainty = mail.EffectNone
		return guidance
	}
	retryability := mail.RetryObserveRequired
	if errorCode(err) == "account_binding_changed" {
		retryability = mail.RetryUserInputRequired
	}
	return mail.OperationGuidance{
		Phase: mail.OperationPhaseExecution, EffectCertainty: mail.EffectPartial, Retryability: retryability,
		Recovery: mail.RecoveryGuidance{Action: mail.RecoveryObserve, Command: "accounts.list", Args: []string{"--json"}},
	}
}

func sendSetupBindingPublicationStatus(err error) mail.AccountBindingPublicationStatus {
	var publication interface {
		BindingPublicationStatus() mail.AccountBindingPublicationStatus
	}
	if errors.As(err, &publication) {
		status := publication.BindingPublicationStatus()
		if status == mail.AccountBindingPublicationUnknown || status == mail.AccountBindingPublicationComplete {
			return status
		}
	}
	return mail.AccountBindingPublicationNone
}

func failSendSetupBindingUpdate(
	jsonOutput bool,
	err error,
	bindingStatus mail.AccountBindingPublicationStatus,
	stdout io.Writer,
	stderr io.Writer,
) int {
	effects := []sendSetupPartialEffect{
		{Step: "keychain_store", Status: "complete"},
		{Step: "binding_publish", Status: bindingStatus},
	}
	var release interface{ BindingLockReleaseFailed() bool }
	if errors.As(err, &release) && release.BindingLockReleaseFailed() {
		effects = append(effects, sendSetupPartialEffect{Step: "lock_release", Status: "failed"})
	}
	if jsonOutput {
		return failCommandWithData("send.setup", true, responseData{PartialEffects: effects}, err, stdout, stderr)
	}
	writeLine(stderr, err)
	writeFormat(stderr, "partial effects: keychain_store: complete; binding_publish: %s\n", bindingStatus)
	if len(effects) == 3 {
		writeLine(stderr, "warning: lock_release: failed; the binding publication status is unchanged")
	}
	writeLine(stderr, "recovery: run `mailcli accounts list --json` to observe the binding, then make an explicit send setup decision. MailCLI will not retry or roll back the Keychain credential automatically.")
	return commandExitCodeFor("send.setup", err, true)
}

func loadSendBinding(store mail.AccountBindingStore, accountID string) (mail.AccountBindingFile, mail.AccountBinding, bool, error) {
	if store == nil {
		return mail.AccountBindingFile{}, mail.AccountBinding{}, false, &commandError{code: "account_binding_unavailable", message: "account binding store is unavailable"}
	}
	document, err := store.LoadAccountBindings()
	if err != nil {
		return mail.AccountBindingFile{}, mail.AccountBinding{}, false, err
	}
	binding, found, err := mail.FindAccountBinding(document, accountID)
	return document, binding, found, err
}

func writeSendSetupResult(stdout io.Writer, command string, jsonOutput bool, result sendSetupResult) int {
	if jsonOutput {
		return writeSuccess(stdout, command, responseData{SendSetup: &result})
	}
	writeFormat(stdout, "%s app-specific password for %s\n", result.Action, result.Account)
	return 0
}
