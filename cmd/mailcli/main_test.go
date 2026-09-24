package main

import (
	"errors"
	"path/filepath"
	"testing"

	"mailcli/internal/transport"
	"mailcli/internal/transport/imapclient"
)

// TestSendTransportWiring pins the dependency wiring for sends and IMAP
// mutations: every transport role must be backed by a real client.
// It performs no I/O; construction is side-effect free by contract.
func TestSendTransportWiring(t *testing.T) {
	t.Setenv("MAILCLI_IMAP_MUTATION_LOCK", "on")
	transport := newInvocationTransport()
	t.Cleanup(func() {
		if err := transport.Close(); err != nil {
			t.Errorf("close invocation transport: %v", err)
		}
	})
	if transport.Submitter == nil {
		t.Error("SendTransport.Submitter = nil, want SMTP client")
	}
	if transport.Mirror == nil {
		t.Error("SendTransport.Mirror = nil, want IMAP client")
	}
	if transport.Credentials == nil {
		t.Error("SendTransport.Credentials = nil, want keychain store")
	}
	if transport.Imap == nil {
		t.Error("SendTransport.Imap = nil, want IMAP operator")
	}
	if transport.Imap != transport.Mirror {
		t.Error("SendTransport.Imap and Mirror do not share the invocation client")
	}
}

func TestInvocationImapClientFailsClosedWhenConfigDirectoryUnavailable(t *testing.T) {
	t.Setenv("MAILCLI_IMAP_MUTATION_LOCK", "on")
	setupErr := errors.New("user config directory unavailable")
	var clientFactoryCalls int
	client := newInvocationImapClientWith(
		func() (string, error) { return "", setupErr },
		func(imapclient.ClientOptions) (*imapclient.Client, error) {
			clientFactoryCalls++
			return nil, errors.New("client factory must not run")
		},
	)
	if clientFactoryCalls != 0 {
		t.Fatalf("client factory calls = %d, want 0", clientFactoryCalls)
	}
	if got := transport.ErrorCode(client.MutationLockSetupError()); got != transport.CodeIMAPLockUnavailable {
		t.Fatalf("MutationLockSetupError code = %q, want %q", got, transport.CodeIMAPLockUnavailable)
	}
}

func TestInvocationImapClientFailsClosedWhenLockClientInitializationFails(t *testing.T) {
	t.Setenv("MAILCLI_IMAP_MUTATION_LOCK", "on")
	setupErr := errors.New("lock client initialization failed")
	wantLockDir := filepath.Join("/test-config", "MailCLI")
	var gotLockDir string
	client := newInvocationImapClientWith(
		func() (string, error) { return "/test-config", nil },
		func(options imapclient.ClientOptions) (*imapclient.Client, error) {
			gotLockDir = options.MutationLockDir
			return nil, setupErr
		},
	)
	if gotLockDir != wantLockDir {
		t.Fatalf("MutationLockDir = %q, want %q", gotLockDir, wantLockDir)
	}
	if got := transport.ErrorCode(client.MutationLockSetupError()); got != transport.CodeIMAPLockUnavailable {
		t.Fatalf("MutationLockSetupError code = %q, want %q", got, transport.CodeIMAPLockUnavailable)
	}
}

func TestInvocationImapClientKeepsNormalLockAndExplicitOptOutSeparate(t *testing.T) {
	t.Setenv("MAILCLI_IMAP_MUTATION_LOCK", "on")
	wantLockDir := filepath.Join("/test-config", "MailCLI")
	var gotLockDir string
	client := newInvocationImapClientWith(
		func() (string, error) { return "/test-config", nil },
		func(options imapclient.ClientOptions) (*imapclient.Client, error) {
			gotLockDir = options.MutationLockDir
			return imapclient.NewWithOptions(options)
		},
	)
	if gotLockDir != wantLockDir || client.MutationLockSetupError() != nil {
		t.Fatalf("normal lock setup = %q, error %v; want %q and nil", gotLockDir, client.MutationLockSetupError(), wantLockDir)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close normal client: %v", err)
	}

	t.Setenv("MAILCLI_IMAP_MUTATION_LOCK", "off")
	var configDirCalls, clientFactoryCalls int
	optedOut := newInvocationImapClientWith(
		func() (string, error) {
			configDirCalls++
			return "", errors.New("must not be checked after explicit opt-out")
		},
		func(imapclient.ClientOptions) (*imapclient.Client, error) {
			clientFactoryCalls++
			return nil, errors.New("must not construct a locked client after explicit opt-out")
		},
	)
	if configDirCalls != 0 || clientFactoryCalls != 0 || optedOut.MutationLockSetupError() != nil {
		t.Fatalf("explicit opt-out called config/client factories %d/%d times or retained setup error %v",
			configDirCalls, clientFactoryCalls, optedOut.MutationLockSetupError())
	}
	if err := optedOut.Close(); err != nil {
		t.Fatalf("close opted-out client: %v", err)
	}
}

// TestMutationLockOptOut pins the MAILCLI_IMAP_MUTATION_LOCK kill switch:
// only explicit falsy values disable the cross-process account lock.
func TestMutationLockOptOut(t *testing.T) {
	for _, value := range []string{"0", "off", "OFF", "false", " no "} {
		t.Setenv("MAILCLI_IMAP_MUTATION_LOCK", value)
		if !mutationLockDisabled() {
			t.Errorf("mutationLockDisabled() = false for %q", value)
		}
	}
	for _, value := range []string{"", "1", "on", "true", "yes", "anything"} {
		t.Setenv("MAILCLI_IMAP_MUTATION_LOCK", value)
		if mutationLockDisabled() {
			t.Errorf("mutationLockDisabled() = true for %q", value)
		}
	}
}
