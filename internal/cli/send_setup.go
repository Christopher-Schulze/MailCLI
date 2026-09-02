package cli

import (
	"io"
	stdmail "net/mail"
	"strings"

	"mailcli/internal/keychain"
	"mailcli/internal/transport"
)

// sendSetupCredentials is overridable in tests so tests never touch the
// real keychain.
var sendSetupCredentials = keychain.New

// sendSetupStdin is overridable in tests so the password prompt can be fed
// from a pipe instead of the terminal.
var sendSetupStdin io.Reader = osStdin

// sendSetupResult reports one keychain mutation for the send.setup command.
type sendSetupResult struct {
	Account string `json:"account"`
	Action  string `json:"action"`
}

func runSend(args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		writeLine(stderr, "Usage:\n  mailcli send <setup> [options]")
		return 2
	}
	switch args[0] {
	case "help", "--help", "-h":
		writeLine(stdout, "Usage:\n  mailcli send setup --from <email> [--remove] [--json]")
		return 0
	case "setup":
		return runSendSetup(args[1:], stdout, stderr)
	default:
		writeFormat(stderr, "unknown send command %q\n", args[0])
		return 2
	}
}

func runSendSetup(args []string, stdout io.Writer, stderr io.Writer) int {
	flags := newFlagSet("send setup", stderr)
	from := flags.String("from", "", "sender email address")
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
	credentials := sendSetupCredentials()
	if *remove {
		if err := credentials.Delete(account); err != nil {
			return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
		}
		return writeSendSetupResult(stdout, "send.setup", *jsonOutput, sendSetupResult{
			Account: account, Action: "removed",
		})
	}
	password, err := readPasswordLine("App-specific password for " + account + ": ")
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
	if err := credentials.Store(account, password); err != nil {
		return failCommand("send.setup", *jsonOutput, err, stdout, stderr)
	}
	return writeSendSetupResult(stdout, "send.setup", *jsonOutput, sendSetupResult{
		Account: account, Action: "stored",
	})
}

func writeSendSetupResult(stdout io.Writer, command string, jsonOutput bool, result sendSetupResult) int {
	if jsonOutput {
		return writeSuccess(stdout, command, responseData{SendSetup: &result})
	}
	writeFormat(stdout, "%s app-specific password for %s\n", result.Action, result.Account)
	return 0
}
