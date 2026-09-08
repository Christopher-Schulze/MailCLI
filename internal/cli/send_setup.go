package cli

import (
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
	return runSendWithInvalidator(args, stdout, stderr, nil)
}

func runSendWithInvalidator(
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	invalidateCredentials func(string),
) int {
	return runSendWithBindings(args, stdout, stderr, invalidateCredentials, nil)
}

func runSendWithBindings(
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
		writeFormat(stdout, "Usage:\n  mailcli send setup --from <email> [--account <ref>] [--credential-account <email>] [--remove] [--json]\n\n%s\n", transport.ProviderSupportDescription())
		return 0
	case "setup":
		if bindings == nil {
			bindings = sendSetupBindings()
		}
		return runSendSetup(args[1:], stdout, stderr, invalidateCredentials, bindings)
	default:
		writeFormat(stderr, "unknown send command %q\n", args[0])
		return 2
	}
}

func runSendSetup(
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
	remove := flags.Bool("remove", false, "remove the stored app-specific password")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if code := parseFlags(flags, args, stdout, stderr); code >= 0 {
		return code
	}
	parsed, err := stdmail.ParseAddress(strings.TrimSpace(*from))
	if err != nil || parsed.Address == "" {
		return failCommand(
			"send.setup", *jsonOutput,
			&commandError{code: "invalid_argument", message: "send setup requires a valid --from <email>"},
			stdout, stderr,
		)
	}
	account := parsed.Address
	if _, _, _, _, err := transport.ProviderHosts(account); err != nil {
		return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
	}
	stableAccountID := ""
	if strings.TrimSpace(*accountRef) != "" {
		ref, err := mailref.DecodeAccount(strings.TrimSpace(*accountRef))
		if err != nil {
			return failCommand("send.setup", *jsonOutput, &commandError{code: "invalid_argument", message: "send setup requires a valid --account ref: " + err.Error()}, stdout, stderr)
		}
		stableAccountID = ref.AccountID
	}
	credential := account
	if stableAccountID != "" && strings.TrimSpace(*credentialAccount) == "" {
		binding, found, err := loadSendBinding(bindings, stableAccountID)
		if err != nil {
			return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
		}
		if found {
			credential = binding.CredentialAccount
		}
	}
	if strings.TrimSpace(*credentialAccount) != "" {
		credentialParsed, err := stdmail.ParseAddress(strings.TrimSpace(*credentialAccount))
		if err != nil || credentialParsed.Address == "" {
			return failCommand("send.setup", *jsonOutput, &commandError{code: "invalid_argument", message: "send setup requires a valid --credential-account <email>"}, stdout, stderr)
		}
		credential = credentialParsed.Address
		if _, _, _, _, err := transport.ProviderHosts(credential); err != nil {
			return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
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
		if err := upsertSendBinding(bindings, stableAccountID, account, credential); err != nil {
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

func upsertSendBinding(store mail.AccountBindingStore, accountID, alias, credential string) error {
	if store == nil {
		return &commandError{code: "account_binding_unavailable", message: "account binding store is unavailable"}
	}
	document, err := store.LoadAccountBindings()
	if err != nil {
		return err
	}
	binding, found, err := mail.FindAccountBinding(document, accountID)
	if err != nil {
		return err
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
	return store.UpsertAccountBinding(binding)
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
