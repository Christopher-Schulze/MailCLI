package cli

import (
	"bytes"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/transport"
)

type stubSetupCredentials struct {
	stored    map[string]string
	loadErr   error
	deleteErr error
}

type setupCredentialInvalidatingImap struct {
	transport.ImapOperator
	configs []transport.ImapConfig
}

func (s *setupCredentialInvalidatingImap) InvalidateCredentials(cfg transport.ImapConfig) {
	s.configs = append(s.configs, cfg)
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
			name: "embedded NUL address", args: []string{"setup", "--from", "alice\x00@example.com", "--json"},
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

func TestSendSetupRejectsUnsupportedProviderBeforeCredentialStore(t *testing.T) {
	previous := sendSetupCredentials
	credentialFactoryCalls := 0
	sendSetupCredentials = func() transport.CredentialStore {
		credentialFactoryCalls++
		return newStubSetupCredentials()
	}
	t.Cleanup(func() { sendSetupCredentials = previous })

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runSend([]string{"setup", "--from", "alice@unknown.example", "--json"}, &stdout, &stderr)
	if code != 1 || credentialFactoryCalls != 0 {
		t.Fatalf("runSend() code = %d, credential factory calls = %d, stdout = %q, stderr = %q", code, credentialFactoryCalls, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"code":"transport_unsupported_provider"`) ||
		!strings.Contains(stdout.String(), transport.ProviderSupportDescription()) {
		t.Fatalf("stdout = %q, want typed provider remediation", stdout.String())
	}
}

func TestSendSetupInvalidatesImapCredentialsAfterSuccessfulChange(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		stdin      string
		setup      func(*stubSetupCredentials)
		wantAction string
	}{
		{
			name: "store", args: []string{"setup", "--from", "alice@icloud.com", "--json"}, stdin: "secret\n",
			wantAction: "stored",
		},
		{
			name: "remove", args: []string{"setup", "--from", "alice@icloud.com", "--remove", "--json"},
			setup:      func(credentials *stubSetupCredentials) { credentials.stored["alice@icloud.com"] = "old" },
			wantAction: "removed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			credentials := newStubSetupCredentials()
			if test.setup != nil {
				test.setup(credentials)
			}
			previousCredentials := sendSetupCredentials
			previousStdin := sendSetupStdin
			sendSetupCredentials = func() transport.CredentialStore { return credentials }
			sendSetupStdin = strings.NewReader(test.stdin)
			t.Cleanup(func() {
				sendSetupCredentials = previousCredentials
				sendSetupStdin = previousStdin
			})

			invalidator := &setupCredentialInvalidatingImap{}
			service := mail.NewServiceWithTransport(nil, "", mail.SendTransport{Imap: invalidator})
			var stdout, stderr bytes.Buffer
			code := runSendWithInvalidator(test.args, &stdout, &stderr, service.InvalidateCredentials)
			if code != 0 || stderr.Len() != 0 || len(invalidator.configs) != 1 {
				t.Fatalf("code = %d, stderr = %q, invalidations = %+v", code, stderr.String(), invalidator.configs)
			}
			if !strings.Contains(stdout.String(), `"action":"`+test.wantAction+`"`) {
				t.Fatalf("stdout = %q, want action %s", stdout.String(), test.wantAction)
			}
			got := invalidator.configs[0]
			if got.Host != "imap.mail.me.com" || got.Port != 993 || got.Username != "alice@icloud.com" || got.Password != "" {
				t.Fatalf("invalidation target = %+v, want password-free iCloud identity", got)
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
