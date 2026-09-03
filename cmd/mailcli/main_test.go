package main

import "testing"

// TestSendTransportWiring pins the dependency wiring for sends and IMAP
// mutations: every transport role must be backed by a real client.
// It performs no I/O; construction is side-effect free by contract.
func TestSendTransportWiring(t *testing.T) {
	transport := sendTransport()
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
}
