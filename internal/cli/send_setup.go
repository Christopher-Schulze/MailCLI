package cli

import (
	"context"
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

// sendSetupResult reports one keychain mutation for the send.setup command.
type sendSetupResult struct {
	Account           string `json:"account"`
	AccountRef        string `json:"account_ref,omitempty"`
	CredentialAccount string `json:"credential_account,omitempty"`
	Action            string `json:"action"`
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
	if len(args) == 0 {
		writeLine(stderr, "Usage:\n  mailcli send <setup> [options]")
		return 2
	}
	switch args[0] {
	case "help", "--help", "-h":
		writeFormat(stdout, "Usage:\n  mailcli send setup --from <email> [--account <ref>] [--credential-account <email>] [--smtp-host <host> --smtp-port <port>] [--imap-host <host> --imap-port <port>] [--remove] [--json]\n\n%s\n", transport.ProviderSupportDescription())
		return 0
	case "setup":
		if bindings == nil {
			bindings = sendSetupBindings()
		}
		return runSendSetup(ctx, args[1:], stdout, stderr, invalidateCredentials, bindings)
	default:
		writeFormat(stderr, "unknown send command %q\n", args[0])
		return 2
	}
}

func runSendSetup(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	invalidateCredentials func(string),
	bindings mail.AccountBindingStore,
) int {
	flags := newFlagSet("send setup", stderr)
	from := flags.String("from", "", "sender email address")
	accountRef := flags.String("account", "", "account ref to bind to the sender")
	credentialAccount := flags.String("credential-account", "", "keychain account used for this sender")
	smtpHost := flags.String("smtp-host", "", "explicit SMTP submission host for the account binding")
	smtpPort := flags.Int("smtp-port", 0, "explicit SMTP submission port for the account binding")
	imapHost := flags.String("imap-host", "", "explicit IMAP host for the account binding")
	imapPort := flags.Int("imap-port", 0, "explicit IMAP port for the account binding")
	remove := flags.Bool("remove", false, "remove the stored app-specific password")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	hostFlags := *smtpHost != "" || *smtpPort != 0 || *imapHost != "" || *imapPort != 0
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
	if strings.TrimSpace(*accountRef) != "" {
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
	var existingBinding mail.AccountBinding
	bindingFound := false
	if stableAccountID != "" {
		var err error
		existingBinding, bindingFound, err = loadSendBinding(bindings, stableAccountID)
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
	if strings.TrimSpace(*credentialAccount) != "" {
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
	if stableAccountID == "" && !strings.EqualFold(account, credential) {
		return failCommand("send.setup", *jsonOutput, &commandError{code: "invalid_argument", message: "--credential-account requires --account so the alias binding is explicit"}, stdout, stderr)
	}
	credentials := sendSetupCredentials()
	if *remove {
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
	password, err := readPasswordLine(transport.ProviderSupportDescription() + "\nApp-specific password for " + account + ": ")
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
	if err := credentials.Store(credential, password); err != nil {
		return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
	}
	if stableAccountID != "" {
		var hosts *bindingHosts
		if hostFlags {
			hosts = &bindingHosts{
				smtpHost: *smtpHost, smtpPort: *smtpPort,
				imapHost: *imapHost, imapPort: *imapPort,
			}
		}
		if err := upsertSendBinding(ctx, bindings, stableAccountID, account, credential, hosts); err != nil {
			return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
		}
	}
	if invalidateCredentials != nil {
		invalidateCredentials(credential)
	}
	return writeSendSetupResult(stdout, "send.setup", *jsonOutput, sendSetupResult{
		Account: account, AccountRef: *accountRef, CredentialAccount: credential, Action: "stored",
	})
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

func upsertSendBinding(ctx context.Context, store mail.AccountBindingStore, accountID, alias, credential string, hosts *bindingHosts) error {
	if store == nil {
		return &commandError{code: "account_binding_unavailable", message: "account binding store is unavailable"}
	}
	return store.UpdateAccountBindings(ctx, func(document mail.AccountBindingFile) (mail.AccountBindingFile, error) {
		return mergeSendBinding(document, accountID, alias, credential, hosts)
	})
}

func mergeSendBinding(document mail.AccountBindingFile, accountID string, alias string, credential string, hosts *bindingHosts) (mail.AccountBindingFile, error) {
	binding, found, err := mail.FindAccountBinding(document, accountID)
	if err != nil {
		return mail.AccountBindingFile{}, err
	}
	if !found {
		binding = mail.AccountBinding{AccountID: accountID, SenderAliases: []string{alias}, CredentialAccount: credential}
	} else {
		binding.CredentialAccount = credential
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

func loadSendBinding(store mail.AccountBindingStore, accountID string) (mail.AccountBinding, bool, error) {
	if store == nil {
		return mail.AccountBinding{}, false, &commandError{code: "account_binding_unavailable", message: "account binding store is unavailable"}
	}
	document, err := store.LoadAccountBindings()
	if err != nil {
		return mail.AccountBinding{}, false, err
	}
	return mail.FindAccountBinding(document, accountID)
}

func writeSendSetupResult(stdout io.Writer, command string, jsonOutput bool, result sendSetupResult) int {
	if jsonOutput {
		return writeSuccess(stdout, command, responseData{SendSetup: &result})
	}
	writeFormat(stdout, "%s app-specific password for %s\n", result.Action, result.Account)
	return 0
}
