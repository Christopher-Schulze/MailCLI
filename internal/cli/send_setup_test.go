package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

type stubSetupCredentials struct {
	stored    map[string]string
	loadErr   error
	storeErr  error
	deleteErr error
}

type setupBindingPublicationError struct {
	status mail.AccountBindingPublicationStatus
}

func (e *setupBindingPublicationError) Error() string {
	return "injected account-binding publication failure"
}

func (e *setupBindingPublicationError) ErrorCode() string { return "account_binding_unavailable" }

func (e *setupBindingPublicationError) BindingPublicationStatus() mail.AccountBindingPublicationStatus {
	return e.status
}

type setupBindingPublicationFaultStore struct {
	store        mail.AccountBindingStore
	status       mail.AccountBindingPublicationStatus
	beforeUpdate func()
}

func (s *setupBindingPublicationFaultStore) LoadAccountBindings() (mail.AccountBindingFile, error) {
	return s.store.LoadAccountBindings()
}

func (s *setupBindingPublicationFaultStore) UpdateAccountBindings(
	ctx context.Context,
	update mail.AccountBindingUpdate,
) error {
	if s.beforeUpdate != nil {
		s.beforeUpdate()
	}
	if s.status == mail.AccountBindingPublicationUnknown {
		if err := s.store.UpdateAccountBindings(ctx, update); err != nil {
			return err
		}
	}
	return &setupBindingPublicationError{status: s.status}
}

func (s *setupBindingPublicationFaultStore) UpsertAccountBinding(binding mail.AccountBinding) error {
	return s.store.UpsertAccountBinding(binding)
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
	if c.storeErr != nil {
		return c.storeErr
	}
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

func TestSendSetupPersistsExplicitAccountBinding(t *testing.T) {
	credentials := newStubSetupCredentials()
	bindings := mail.NewAccountBindingStore(filepath.Join(t.TempDir(), "account-bindings.json"))
	accountRef, err := mailref.EncodeAccount("ACCOUNT-1")
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	previousCredentials := sendSetupCredentials
	previousStdin := sendSetupStdin
	sendSetupCredentials = func() transport.CredentialStore { return credentials }
	sendSetupStdin = strings.NewReader("secret\n")
	t.Cleanup(func() {
		sendSetupCredentials = previousCredentials
		sendSetupStdin = previousStdin
	})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runSendWithBindings([]string{
		"setup", "--from", "alias@icloud.com", "--account", accountRef,
		"--credential-account", "login@icloud.com", "--json",
	}, &stdout, &stderr, nil, bindings)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code = %d, stderr = %q, stdout = %q", code, stderr.String(), stdout.String())
	}
	if credentials.stored["login@icloud.com"] != "secret" || credentials.stored["alias@icloud.com"] != "" {
		t.Fatalf("stored credentials = %#v", credentials.stored)
	}
	document, err := bindings.LoadAccountBindings()
	if err != nil {
		t.Fatalf("LoadAccountBindings() error = %v", err)
	}
	binding, found, err := mail.FindAccountBinding(document, "ACCOUNT-1")
	if err != nil || !found || binding.CredentialAccount != "login@icloud.com" || len(binding.SenderAliases) != 1 || binding.SenderAliases[0] != "alias@icloud.com" {
		t.Fatalf("binding = %+v, found=%t, error=%v", binding, found, err)
	}
}

func TestSendSetupBindingPublicationFailureReportsPartialEffects(t *testing.T) {
	tests := []struct {
		name   string
		status mail.AccountBindingPublicationStatus
	}{
		{name: "before rename", status: mail.AccountBindingPublicationNone},
		{name: "after rename", status: mail.AccountBindingPublicationUnknown},
	}
	for _, test := range tests {
		for _, jsonOutput := range []bool{true, false} {
			mode := "human"
			if jsonOutput {
				mode = "json"
			}
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				runSendSetupBindingPublicationFailure(t, test.status, jsonOutput)
			})
		}
	}
}

func runSendSetupBindingPublicationFailure(t *testing.T, status mail.AccountBindingPublicationStatus, jsonOutput bool) {
	t.Helper()
	path, store, before, accountRef := seedSetupBinding(t, []string{"old@icloud.com"})
	previousBindings := sendSetupBindings
	sendSetupBindings = func() mail.AccountBindingStore {
		return &setupBindingPublicationFaultStore{store: store, status: status}
	}
	t.Cleanup(func() { sendSetupBindings = previousBindings })
	credentials := newStubSetupCredentials()
	args := []string{
		"setup", "--from", "alice@icloud.com", "--account", accountRef,
		"--credential-account", "login@icloud.com",
	}
	if jsonOutput {
		args = append(args, "--json")
	}
	code, stdout, stderr := runSendSetupWithStub(t, credentials, "credential-secret\n", args)
	if code != 1 || credentials.stored["login@icloud.com"] != "credential-secret" {
		t.Fatalf("code = %d, credentials = %#v, stdout = %q, stderr = %q", code, credentials.stored, stdout.String(), stderr.String())
	}
	assertNoSetupCredentialLeak(t, stdout, stderr)
	assertSendSetupPublicationOutput(t, status, jsonOutput, stdout, stderr)
	assertSendSetupBindingState(t, store, path, before, status)
}

func seedSetupBinding(t *testing.T, aliases []string) (string, mail.AccountBindingStore, []byte, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "account-bindings.json")
	store := mail.NewAccountBindingStore(path)
	if err := store.UpsertAccountBinding(mail.AccountBinding{
		AccountID: "ACCOUNT-1", SenderAliases: aliases, CredentialAccount: "login@icloud.com",
	}); err != nil {
		t.Fatalf("seed UpsertAccountBinding() error = %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	accountRef, err := mailref.EncodeAccount("ACCOUNT-1")
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	return path, store, before, accountRef
}

func assertNoSetupCredentialLeak(t *testing.T, stdout, stderr *bytes.Buffer) {
	t.Helper()
	if strings.Contains(stdout.String(), "credential-secret") || strings.Contains(stderr.String(), "credential-secret") {
		t.Fatal("the credential leaked into command output")
	}
}

func assertSendSetupPublicationOutput(
	t *testing.T,
	status mail.AccountBindingPublicationStatus,
	jsonOutput bool,
	stdout, stderr *bytes.Buffer,
) {
	t.Helper()
	if !jsonOutput {
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "binding_publish: "+string(status)) ||
			!strings.Contains(stderr.String(), "mailcli accounts list --json") ||
			!strings.Contains(stderr.String(), "explicit send setup decision") {
			t.Fatalf("human failure output stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
		return
	}
	var output struct {
		Data struct {
			PartialEffects []sendSetupPartialEffect `json:"partial_effects"`
		} `json:"data"`
		Error struct {
			Guidance mail.OperationGuidance `json:"guidance"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode error response: %v, output=%q", err, stdout.String())
	}
	wantEffects := []sendSetupPartialEffect{
		{Step: "keychain_store", Status: "complete"},
		{Step: "binding_publish", Status: status},
	}
	if !reflect.DeepEqual(output.Data.PartialEffects, wantEffects) {
		t.Fatalf("partial_effects = %+v, want %+v", output.Data.PartialEffects, wantEffects)
	}
	guidance := output.Error.Guidance
	if guidance.EffectCertainty != mail.EffectPartial || guidance.Retryability != mail.RetryObserveRequired ||
		guidance.ReplayAllowed || guidance.Recovery.Action != mail.RecoveryObserve ||
		guidance.Recovery.Command != "accounts.list" || !reflect.DeepEqual(guidance.Recovery.Args, []string{"--json"}) {
		t.Fatalf("recovery guidance = %+v", guidance)
	}
}

func assertSendSetupBindingState(
	t *testing.T,
	store mail.AccountBindingStore,
	path string,
	before []byte,
	status mail.AccountBindingPublicationStatus,
) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if status == mail.AccountBindingPublicationNone && !bytes.Equal(after, before) {
		t.Fatalf("pre-rename failure changed binding file: before=%s after=%s", before, after)
	}
	if status == mail.AccountBindingPublicationUnknown && bytes.Equal(after, before) {
		t.Fatal("post-rename failure did not retain the published binding")
	}
	document, err := store.LoadAccountBindings()
	if err != nil {
		t.Fatalf("LoadAccountBindings() error = %v", err)
	}
	binding, found, err := mail.FindAccountBinding(document, "ACCOUNT-1")
	if err != nil || !found {
		t.Fatalf("FindAccountBinding() = %+v, found=%t, error=%v", binding, found, err)
	}
	wantAliases := []string{"old@icloud.com"}
	if status == mail.AccountBindingPublicationUnknown {
		wantAliases = []string{"alice@icloud.com", "old@icloud.com"}
	}
	if !reflect.DeepEqual(binding.SenderAliases, wantAliases) {
		t.Fatalf("retained binding = %+v, want aliases %v", binding, wantAliases)
	}
}

func TestSendSetupCredentialStoreFailureReportsNoPartialEffects(t *testing.T) {
	path, store, before, accountRef := seedSetupBinding(t, []string{"alice@icloud.com"})
	previousBindings := sendSetupBindings
	sendSetupBindings = func() mail.AccountBindingStore { return store }
	t.Cleanup(func() { sendSetupBindings = previousBindings })
	credentials := newStubSetupCredentials()
	credentials.storeErr = errors.New("injected Keychain write failure")
	code, stdout, stderr := runSendSetupWithStub(t, credentials, "credential-secret\n", []string{
		"setup", "--from", "alice@icloud.com", "--account", accountRef,
		"--credential-account", "login@icloud.com", "--json",
	})
	if code != 1 || len(credentials.stored) != 0 || stderr.Len() != 0 {
		t.Fatalf("code = %d, credentials = %#v, stdout = %q, stderr = %q", code, credentials.stored, stdout.String(), stderr.String())
	}
	assertNoSetupCredentialLeak(t, stdout, stderr)
	assertSendSetupNoEffectError(t, stdout)
	assertSetupBindingBytes(t, path, before)
}

func assertSendSetupNoEffectError(t *testing.T, stdout *bytes.Buffer) {
	t.Helper()
	var output struct {
		Data struct {
			PartialEffects []sendSetupPartialEffect `json:"partial_effects"`
		} `json:"data"`
		Error struct {
			Guidance mail.OperationGuidance `json:"guidance"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode error response: %v, output=%q", err, stdout.String())
	}
	if len(output.Data.PartialEffects) != 0 || output.Error.Guidance.EffectCertainty != mail.EffectNone {
		t.Fatalf("Keychain failure response partial_effects=%+v guidance=%+v", output.Data.PartialEffects, output.Error.Guidance)
	}
}

func assertSetupBindingBytes(t *testing.T, path string, before []byte) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("Keychain failure changed the binding file")
	}
}

func TestSendSetupPreservesExistingBindingCredential(t *testing.T) {
	credentials := newStubSetupCredentials()
	bindings := mail.NewAccountBindingStore(filepath.Join(t.TempDir(), "account-bindings.json"))
	accountRef, err := mailref.EncodeAccount("ACCOUNT-1")
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	if err := bindings.UpsertAccountBinding(mail.AccountBinding{
		AccountID: "ACCOUNT-1", SenderAliases: []string{"alias@icloud.com"}, CredentialAccount: "login@icloud.com",
	}); err != nil {
		t.Fatalf("UpsertAccountBinding() error = %v", err)
	}
	previousCredentials := sendSetupCredentials
	previousStdin := sendSetupStdin
	sendSetupCredentials = func() transport.CredentialStore { return credentials }
	sendSetupStdin = strings.NewReader("rotated\n")
	t.Cleanup(func() {
		sendSetupCredentials = previousCredentials
		sendSetupStdin = previousStdin
	})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runSendWithBindings([]string{"setup", "--from", "alias@icloud.com", "--account", accountRef, "--json"}, &stdout, &stderr, nil, bindings)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code = %d, stderr = %q, stdout = %q", code, stderr.String(), stdout.String())
	}
	if credentials.stored["login@icloud.com"] != "rotated" {
		t.Fatalf("existing binding credential was not rotated: %#v", credentials.stored)
	}
	if _, exists := credentials.stored["alias@icloud.com"]; exists {
		t.Fatalf("setup unexpectedly created an alias credential: %#v", credentials.stored)
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

func TestSendSetupPersistsExplicitBindingHosts(t *testing.T) {
	credentials := newStubSetupCredentials()
	bindings := mail.NewAccountBindingStore(filepath.Join(t.TempDir(), "account-bindings.json"))
	accountRef, err := mailref.EncodeAccount("ACCOUNT-1")
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	previousCredentials := sendSetupCredentials
	previousStdin := sendSetupStdin
	sendSetupCredentials = func() transport.CredentialStore { return credentials }
	sendSetupStdin = strings.NewReader("secret\n")
	t.Cleanup(func() {
		sendSetupCredentials = previousCredentials
		sendSetupStdin = previousStdin
	})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runSendWithBindings([]string{
		"setup", "--from", "user@corp.example", "--account", accountRef,
		"--smtp-host", "smtp.corp.example", "--smtp-port", "587",
		"--imap-host", "imap.corp.example", "--imap-port", "993", "--json",
	}, &stdout, &stderr, nil, bindings)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code = %d, stderr = %q, stdout = %q", code, stderr.String(), stdout.String())
	}
	document, err := bindings.LoadAccountBindings()
	if err != nil {
		t.Fatalf("LoadAccountBindings() error = %v", err)
	}
	binding, found, err := mail.FindAccountBinding(document, "ACCOUNT-1")
	if err != nil || !found {
		t.Fatalf("FindAccountBinding() = %+v, found=%t, error=%v", binding, found, err)
	}
	if binding.SMTPHost != "smtp.corp.example" || binding.SMTPPort != 587 ||
		binding.IMAPHost != "imap.corp.example" || binding.IMAPPort != 993 {
		t.Fatalf("binding hosts = %+v", binding)
	}
}

func TestSendSetupRejectsHostFlagsWithoutAccount(t *testing.T) {
	credentials := newStubSetupCredentials()
	previousCredentials := sendSetupCredentials
	sendSetupCredentials = func() transport.CredentialStore { return credentials }
	t.Cleanup(func() { sendSetupCredentials = previousCredentials })
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runSend([]string{
		"setup", "--from", "user@corp.example", "--imap-host", "imap.corp.example", "--json",
	}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stdout.String(), "require --account") {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestSendSetupRejectsInvalidExplicitHost(t *testing.T) {
	credentials := newStubSetupCredentials()
	bindings := mail.NewAccountBindingStore(filepath.Join(t.TempDir(), "account-bindings.json"))
	accountRef, err := mailref.EncodeAccount("ACCOUNT-1")
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	previousCredentials := sendSetupCredentials
	previousStdin := sendSetupStdin
	sendSetupCredentials = func() transport.CredentialStore { return credentials }
	sendSetupStdin = strings.NewReader("secret\n")
	t.Cleanup(func() {
		sendSetupCredentials = previousCredentials
		sendSetupStdin = previousStdin
	})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runSendWithBindings([]string{
		"setup", "--from", "user@corp.example", "--account", accountRef,
		"--imap-host", "localhost", "--imap-port", "993", "--json",
	}, &stdout, &stderr, nil, bindings)
	if code != 1 || !strings.Contains(stdout.String(), "account_binding_host_invalid") {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if len(credentials.stored) != 0 {
		t.Fatalf("credentials stored on rejected host: %+v", credentials.stored)
	}
}

func TestSendSetupPreservesBindingHostsWithoutFlags(t *testing.T) {
	credentials := newStubSetupCredentials()
	bindings := mail.NewAccountBindingStore(filepath.Join(t.TempDir(), "account-bindings.json"))
	accountRef, err := mailref.EncodeAccount("ACCOUNT-1")
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	if err := bindings.UpsertAccountBinding(mail.AccountBinding{
		AccountID:         "ACCOUNT-1",
		SenderAliases:     []string{"user@corp.example"},
		CredentialAccount: "user@corp.example",
		SMTPHost:          "smtp.corp.example", SMTPPort: 587,
		IMAPHost: "imap.corp.example", IMAPPort: 993,
	}); err != nil {
		t.Fatalf("UpsertAccountBinding() error = %v", err)
	}
	previousCredentials := sendSetupCredentials
	previousStdin := sendSetupStdin
	sendSetupCredentials = func() transport.CredentialStore { return credentials }
	sendSetupStdin = strings.NewReader("secret\n")
	t.Cleanup(func() {
		sendSetupCredentials = previousCredentials
		sendSetupStdin = previousStdin
	})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runSendWithBindings([]string{
		"setup", "--from", "user@corp.example", "--account", accountRef, "--json",
	}, &stdout, &stderr, nil, bindings)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q, stdout = %q", code, stderr.String(), stdout.String())
	}
	document, err := bindings.LoadAccountBindings()
	if err != nil {
		t.Fatalf("LoadAccountBindings() error = %v", err)
	}
	binding, found, err := mail.FindAccountBinding(document, "ACCOUNT-1")
	if err != nil || !found || binding.SMTPHost != "smtp.corp.example" || binding.IMAPHost != "imap.corp.example" {
		t.Fatalf("binding = %+v, found=%t, error=%v", binding, found, err)
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

func TestSendSetupMergeRejectsChangedImplicitFields(t *testing.T) {
	base := mail.AccountBinding{AccountID: "ACCOUNT-1", SenderAliases: []string{"old@icloud.com"}, CredentialAccount: "old@icloud.com", SMTPHost: "smtp.example.com", SMTPPort: 587, IMAPHost: "imap.example.com", IMAPPort: 993}
	for _, test := range []struct {
		name               string
		change             func(*mail.AccountBinding)
		explicitCredential bool
		hosts              *bindingHosts
		wantConflict       bool
	}{
		{"unchanged", func(*mail.AccountBinding) {}, false, nil, false},
		{"credential changed", func(b *mail.AccountBinding) { b.CredentialAccount = "new@icloud.com" }, false, nil, true},
		{"smtp changed", func(b *mail.AccountBinding) { b.SMTPHost = "smtp.new.example.com" }, false, nil, true},
		{"imap port changed", func(b *mail.AccountBinding) { b.IMAPPort = 143 }, true, nil, true},
		{"explicit credential wins", func(b *mail.AccountBinding) { b.CredentialAccount = "new@icloud.com" }, true, nil, false},
		{"explicit hosts win", func(b *mail.AccountBinding) { b.SMTPHost = "smtp.new.example.com" }, false, &bindingHosts{smtpHost: "smtp.explicit.example.com", smtpPort: 587, imapHost: "imap.explicit.example.com", imapPort: 993}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			locked := base
			test.change(&locked)
			document := mail.AccountBindingFile{Version: mail.AccountBindingVersion, Bindings: []mail.AccountBinding{locked}}
			merged, err := mergeSendBinding(document, base.AccountID, "alias@icloud.com", "explicit@icloud.com", test.hosts, sendBindingDecision{snapshot: base, found: true, credentialExplicit: test.explicitCredential})
			if test.wantConflict {
				if errorCode(err) != "account_binding_changed" {
					t.Fatalf("merge error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			binding := merged.Bindings[0]
			wantCredential := locked.CredentialAccount
			if test.explicitCredential {
				wantCredential = "explicit@icloud.com"
			}
			if binding.CredentialAccount != wantCredential || !reflect.DeepEqual(binding.SenderAliases, []string{"old@icloud.com", "alias@icloud.com"}) {
				t.Fatalf("merge = %+v", binding)
			}
			if test.hosts == nil {
				if binding.SMTPHost != locked.SMTPHost || binding.SMTPPort != locked.SMTPPort || binding.IMAPHost != locked.IMAPHost || binding.IMAPPort != locked.IMAPPort {
					t.Fatalf("locked hosts changed: %+v", binding)
				}
			} else if binding.SMTPHost != test.hosts.smtpHost || binding.SMTPPort != test.hosts.smtpPort || binding.IMAPHost != test.hosts.imapHost || binding.IMAPPort != test.hosts.imapPort {
				t.Fatalf("explicit hosts lost: %+v", binding)
			}
		})
	}
	for _, found := range []bool{false, true} {
		document := mail.AccountBindingFile{Version: mail.AccountBindingVersion}
		if found {
			document.Bindings = []mail.AccountBinding{base}
		}
		_, err := mergeSendBinding(document, base.AccountID, "alias@icloud.com", "old@icloud.com", nil, sendBindingDecision{snapshot: base, found: !found})
		if errorCode(err) != "account_binding_changed" {
			t.Fatalf("presence changed found=%t: %v", found, err)
		}
	}
}

func TestSendSetupConflictGuidanceRequiresExplicitDecision(t *testing.T) {
	err := &mail.AccountBindingError{Code: "account_binding_changed", Message: "binding changed"}
	guidance := sendSetupErrorGuidance(mail.GuidanceForError("send.setup", err), responseData{PartialEffects: []sendSetupPartialEffect{{Step: "keychain_store", Status: "complete"}, {Step: "binding_publish", Status: "none"}}}, err)
	if guidance.EffectCertainty != mail.EffectPartial || guidance.Retryability != mail.RetryUserInputRequired || guidance.ReplayAllowed || guidance.Recovery.Command != "accounts.list" || !reflect.DeepEqual(guidance.Recovery.Args, []string{"--json"}) {
		t.Fatalf("conflict guidance = %+v", guidance)
	}
}

func TestSendSetupInvalidatesBeforeBindingPublication(t *testing.T) {
	for _, storeFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("store fails %t", storeFails), func(t *testing.T) {
			_, bindings, _, accountRef := seedSetupBinding(t, []string{"alice@icloud.com"})
			credentials := newStubSetupCredentials()
			if storeFails {
				credentials.storeErr = errors.New("store failed")
			}
			previousCredentials, previousStdin := sendSetupCredentials, sendSetupStdin
			sendSetupCredentials = func() transport.CredentialStore { return credentials }
			sendSetupStdin = strings.NewReader("new-secret\n")
			t.Cleanup(func() { sendSetupCredentials, sendSetupStdin = previousCredentials, previousStdin })
			calls := 0
			var stdout, stderr bytes.Buffer
			code := runSendWithBindings([]string{"setup", "--from", "alice@icloud.com", "--account", accountRef, "--json"}, &stdout, &stderr, func(account string) {
				calls++
				if account != "login@icloud.com" || credentials.stored[account] != "new-secret" {
					t.Fatalf("wrong invalidation boundary for %s", account)
				}
			}, &setupBindingPublicationFaultStore{store: bindings, status: mail.AccountBindingPublicationNone, beforeUpdate: func() {
				if calls != 1 {
					t.Fatalf("publication began before invalidation: %d", calls)
				}
			}})
			wantCalls := 1
			if storeFails {
				wantCalls = 0
			}
			if code != 1 || calls != wantCalls {
				t.Fatalf("code=%d invalidations=%d output=%s", code, calls, stdout.String())
			}
		})
	}
}

type setupLockReleaseError struct{ setupBindingPublicationError }

func (*setupLockReleaseError) BindingLockReleaseFailed() bool { return true }

func TestSendSetupLockReleaseReportsCompletedPublication(t *testing.T) {
	err := &setupLockReleaseError{setupBindingPublicationError{status: mail.AccountBindingPublicationComplete}}
	for _, jsonOutput := range []bool{true, false} {
		var stdout, stderr bytes.Buffer
		status := sendSetupBindingPublicationStatus(err)
		if status != mail.AccountBindingPublicationComplete {
			t.Fatalf("publish status = %s", status)
		}
		if code := failSendSetupBindingUpdate(jsonOutput, err, status, &stdout, &stderr); code != 1 {
			t.Fatalf("exit = %d", code)
		}
		if !jsonOutput {
			if !strings.Contains(stderr.String(), "binding_publish: complete") || !strings.Contains(stderr.String(), "warning: lock_release: failed") {
				t.Fatalf("warning = %s", stderr.String())
			}
			continue
		}
		var output struct {
			Data struct {
				PartialEffects []sendSetupPartialEffect `json:"partial_effects"`
			} `json:"data"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
			t.Fatal(err)
		}
		want := []sendSetupPartialEffect{{Step: "keychain_store", Status: "complete"}, {Step: "binding_publish", Status: "complete"}, {Step: "lock_release", Status: "failed"}}
		if !reflect.DeepEqual(output.Data.PartialEffects, want) {
			t.Fatalf("effects = %+v", output.Data.PartialEffects)
		}
	}
}

type setupProcessPromptReader struct{ announced bool }

func (r *setupProcessPromptReader) Read(payload []byte) (int, error) {
	if !r.announced {
		r.announced = true
		if _, err := fmt.Fprintln(os.Stdout, "MAILCLI_SETUP_PROMPT"); err != nil {
			return 0, err
		}
	}
	return os.Stdin.Read(payload)
}

func TestSendSetupConcurrentProcessHelper(t *testing.T) {
	role := os.Getenv("MAILCLI_SETUP_PROCESS_ROLE")
	if role == "" {
		return
	}
	store := mail.NewAccountBindingStore(os.Getenv("MAILCLI_SETUP_PROCESS_PATH"))
	if role == "update" {
		if err := store.UpdateAccountBindings(context.Background(), func(document mail.AccountBindingFile) (mail.AccountBindingFile, error) {
			document.Bindings[0].CredentialAccount = "new@icloud.com"
			return document, nil
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	credentials := newStubSetupCredentials()
	sendSetupCredentials = func() transport.CredentialStore { return credentials }
	sendSetupStdin = &setupProcessPromptReader{}
	accountRef, err := mailref.EncodeAccount("ACCOUNT-1")
	if err != nil {
		t.Fatal(err)
	}
	code := runSendWithBindings([]string{"setup", "--from", "alice@icloud.com", "--account", accountRef, "--json"}, os.Stdout, os.Stderr, nil, store)
	if code != 1 || credentials.stored["login@icloud.com"] != "new-secret" {
		t.Fatalf("setup code=%d credentials=%+v", code, credentials.stored)
	}
}

func TestSendSetupConcurrentCredentialChangePreservesOtherProcess(t *testing.T) {
	path, store, _, _ := seedSetupBinding(t, []string{"alice@icloud.com"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSendSetupConcurrentProcessHelper$", "-test.v")
	command.Env = append(os.Environ(), "MAILCLI_SETUP_PROCESS_ROLE=setup", "MAILCLI_SETUP_PROCESS_PATH="+path)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	t.Cleanup(func() {
		cancel()
		if !finished {
			if err := command.Wait(); err == nil {
				t.Error("abandoned setup helper exited successfully")
			}
		}
	})
	reader := bufio.NewReader(output)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("prompt handshake: %v stderr=%s", err, stderr.String())
		}
		if strings.TrimSpace(line) == "MAILCLI_SETUP_PROMPT" {
			break
		}
	}
	writer := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSendSetupConcurrentProcessHelper$", "-test.v")
	writer.Env = append(os.Environ(), "MAILCLI_SETUP_PROCESS_ROLE=update", "MAILCLI_SETUP_PROCESS_PATH="+path)
	if output, err := writer.CombinedOutput(); err != nil {
		t.Fatalf("concurrent writer: %v, %s", err, output)
	}
	if _, err := io.WriteString(input, "new-secret\n"); err != nil {
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Error struct {
			Code     string                 `json:"code"`
			Guidance mail.OperationGuidance `json:"guidance"`
		} `json:"error"`
		Data struct {
			PartialEffects []sendSetupPartialEffect `json:"partial_effects"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(line), &response); err != nil {
		t.Fatalf("setup output: %v, %s", err, line)
	}
	want := []sendSetupPartialEffect{{Step: "keychain_store", Status: "complete"}, {Step: "binding_publish", Status: "none"}}
	if response.Error.Code != "account_binding_changed" || !reflect.DeepEqual(response.Data.PartialEffects, want) {
		t.Fatalf("conflict response = %+v", response)
	}
	guidance := response.Error.Guidance
	if guidance.Phase != mail.OperationPhaseExecution || guidance.EffectCertainty != mail.EffectPartial ||
		guidance.Retryability != mail.RetryUserInputRequired || guidance.ReplayAllowed ||
		guidance.Recovery.Action != mail.RecoveryObserve || guidance.Recovery.Command != "accounts.list" ||
		!reflect.DeepEqual(guidance.Recovery.Args, []string{"--json"}) {
		t.Fatalf("concurrent setup guidance = %+v", guidance)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	err = command.Wait()
	finished = true
	if err != nil {
		t.Fatalf("setup process: %v stderr=%s", err, stderr.String())
	}
	document, err := store.LoadAccountBindings()
	if err != nil || document.Bindings[0].CredentialAccount != "new@icloud.com" {
		t.Fatalf("concurrent binding overwritten: %+v, %v", document, err)
	}
}
