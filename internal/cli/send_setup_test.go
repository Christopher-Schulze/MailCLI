package cli

import (
	"bytes"
	"strings"
	"testing"

	"mailcli/internal/transport"
)

type stubSetupCredentials struct {
	stored    map[string]string
	loadErr   error
	deleteErr error
}

func newStubSetupCredentials() *stubSetupCredentials {
	return &stubSetupCredentials{stored: map[string]string{}}
}

func (c *stubSetupCredentials) Load(account string) (string, error) {
	if c.loadErr != nil {
		return "", c.loadErr
	}
	return c.stored[account], nil
}

func (c *stubSetupCredentials) Store(account string, password string) error {
	c.stored[account] = password
	return nil
}

func (c *stubSetupCredentials) Delete(account string) error {
	if c.deleteErr != nil {
		return c.deleteErr
	}
	if _, exists := c.stored[account]; !exists {
		return &commandError{code: "keychain_item_not_found", message: "no stored password"}
	}
	delete(c.stored, account)
	return nil
}

func runSendSetupWithStub(
	t *testing.T,
	credentials *stubSetupCredentials,
	stdinContent string,
	args []string,
) (int, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	previousCredentials := sendSetupCredentials
	previousStdin := sendSetupStdin
	sendSetupCredentials = func() transport.CredentialStore { return credentials }
	sendSetupStdin = strings.NewReader(stdinContent)
	t.Cleanup(func() {
		sendSetupCredentials = previousCredentials
		sendSetupStdin = previousStdin
	})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runSend(args, &stdout, &stderr)
	return code, &stdout, &stderr
}

func TestSendSetupStoresPasswordWithoutEchoingIt(t *testing.T) {
	credentials := newStubSetupCredentials()
	code, stdout, stderr := runSendSetupWithStub(t, credentials, "app-specific-secret\n",
		[]string{"setup", "--from", "alice@icloud.com", "--json"})
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"command":"send.setup"`) ||
		!strings.Contains(stdout.String(), `"action":"stored"`) ||
		!strings.Contains(stdout.String(), `"account":"alice@icloud.com"`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if credentials.stored["alice@icloud.com"] != "app-specific-secret" {
		t.Fatalf("stored password = %q", credentials.stored["alice@icloud.com"])
	}
	if strings.Contains(stdout.String(), "app-specific-secret") ||
		strings.Contains(stderr.String(), "app-specific-secret") {
		t.Fatal("the secret leaked into command output")
	}
}

func TestSendSetupHumanOutput(t *testing.T) {
	credentials := newStubSetupCredentials()
	code, stdout, _ := runSendSetupWithStub(t, credentials, "secret\n",
		[]string{"setup", "--from", "alice@icloud.com"})
	if code != 0 || stdout.String() != "stored app-specific password for alice@icloud.com\n" {
		t.Fatalf("code = %d, stdout = %q", code, stdout.String())
	}
}

func TestSendSetupRemoveDeletesStoredPassword(t *testing.T) {
	credentials := newStubSetupCredentials()
	credentials.stored["alice@icloud.com"] = "secret"
	code, stdout, _ := runSendSetupWithStub(t, credentials, "",
		[]string{"setup", "--from", "alice@icloud.com", "--remove", "--json"})
	if code != 0 || !strings.Contains(stdout.String(), `"action":"removed"`) {
		t.Fatalf("code = %d, stdout = %q", code, stdout.String())
	}
	if _, exists := credentials.stored["alice@icloud.com"]; exists {
		t.Fatal("stored password survived --remove")
	}
}

func TestSendSetupRemoveMissingPasswordFailsTyped(t *testing.T) {
	credentials := newStubSetupCredentials()
	code, stdout, _ := runSendSetupWithStub(t, credentials, "",
		[]string{"setup", "--from", "alice@icloud.com", "--remove", "--json"})
	if code != 1 || !strings.Contains(stdout.String(), `"code":"keychain_item_not_found"`) {
		t.Fatalf("code = %d, stdout = %q", code, stdout.String())
	}
}

func TestSendSetupRejectsInvalidInput(t *testing.T) {
	credentials := newStubSetupCredentials()
	tests := []struct {
		name     string
		args     []string
		wantCode string
	}{
		{
			name: "missing from", args: []string{"setup", "--json"},
			wantCode: "invalid_argument",
		},
		{
			name: "invalid address", args: []string{"setup", "--from", "not-an-address", "--json"},
			wantCode: "invalid_argument",
		},
		{
			name: "unsupported provider", args: []string{"setup", "--from", "alice@unknown.example", "--json"},
			wantCode: "transport_unsupported_provider",
		},
		{
			name: "empty password", args: []string{"setup", "--from", "alice@icloud.com", "--json"},
			wantCode: "invalid_argument",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, _ := runSendSetupWithStub(t, credentials, "\n", test.args)
			// Usage errors (invalid_argument) return exit 2; transport errors return 1.
			wantExit := 1
			if test.wantCode == "invalid_argument" {
				wantExit = 2
			}
			if code != wantExit || !strings.Contains(stdout.String(), `"code":"`+test.wantCode+`"`) {
				t.Fatalf("code = %d (want %d), stdout = %q", code, wantExit, stdout.String())
			}
		})
	}
}

func TestSendUnknownSubcommandFails(t *testing.T) {
	credentials := newStubSetupCredentials()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	previous := sendSetupCredentials
	sendSetupCredentials = func() transport.CredentialStore { return credentials }
	t.Cleanup(func() { sendSetupCredentials = previous })
	if code := runSend([]string{"missing"}, &stdout, &stderr); code != 2 {
		t.Fatalf("runSend() code = %d", code)
	}
}
