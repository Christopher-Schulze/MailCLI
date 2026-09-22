package main

import "testing"

// TestSendTransportWiring pins the dependency wiring for sends and IMAP
// mutations: every transport role must be backed by a real client.
// It performs no I/O; construction is side-effect free by contract.
func TestSendTransportWiring(t *testing.T) {
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
