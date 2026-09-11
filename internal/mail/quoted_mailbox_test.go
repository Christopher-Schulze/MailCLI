package mail

import (
	"bytes"
	"context"
	stdmail "net/mail"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/transport"
)

func TestQuotedMailboxCompositionPreservesIdentity(t *testing.T) {
	for _, test := range []struct{ name, address, identity string }{
		{"ordinary", "Mixed.Case@example.com", "Mixed.Case@example.com"},
		{"quoted atom", `"Mixed.Case"@example.com`, "Mixed.Case@example.com"},
		{"space", `"A B"@example.com`, "A B@example.com"},
		{"unicode space", `"Jörg Smith"@example.com`, "Jörg Smith@example.com"},
		{"escaped quote", `"A\"B"@example.com`, `A"B@example.com`},
		{"escaped slash", `"A\\B"@example.com`, `A\B@example.com`},
		{"embedded at", `"A@B"@example.com`, "A@B@example.com"},
		{"consecutive dots", `"A..B"@example.com`, "A..B@example.com"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, name := range []string{"", "Display", "Jörg", "Name, with punctuation"} {
				draft := Draft{From: "Sender <" + test.address + ">", To: []Recipient{{Name: name, Address: test.address}}, Body: "Body"}
				payload, err := BuildMessage(draft, "<quoted@example.com>")
				if err != nil {
					t.Fatal(err)
				}
				message := readComposedHeaderTestMessage(t, payload)
				for _, header := range []string{"From", "To"} {
					addresses, err := message.Header.AddressList(header)
					if err != nil || len(addresses) != 1 || addresses[0].Address != test.identity {
						t.Errorf("%s identity with name %q: %+v, %v; want %q", header, name, addresses, err, test.identity)
					}
					if header == "To" && err == nil && len(addresses) == 1 && addresses[0].Name != name {
						t.Errorf("display name = %q, want %q", addresses[0].Name, name)
					}
				}
				if err := validateStoredDraftAddresses(draft); err != nil {
					t.Errorf("valid quoted recipient rejected: %v", err)
				}
				key, err := recipientAddressKey(draft.To[0])
				if err != nil || key != strings.ToLower(test.identity) {
					t.Errorf("deduplication identity with name %q = %q, %v", name, key, err)
				}
			}
		})
	}
}

func TestQuotedMailboxDuplicateNamesDoNotChangeIdentity(t *testing.T) {
	for _, address := range []string{`"A B"@example.com`, `"Jörg Smith"@example.com`, `"A\"B"@example.com`, `"A@B"@example.com`, `"Mixed"@example.com`} {
		t.Run(address, func(t *testing.T) {
			input := DraftInput{To: []Recipient{{Address: address}}, CC: []Recipient{{Name: "Other display", Address: address}}}
			if err := validateDraftAddresses(input); err == nil || !strings.Contains(err.Error(), "duplicate") {
				t.Errorf("create validation did not reject duplicate: %v", err)
			}
			if err := validateStoredDraftAddresses(Draft{To: input.To, CC: input.CC}); err == nil || !strings.Contains(err.Error(), "duplicate") {
				t.Errorf("stored validation did not reject duplicate: %v", err)
			}
		})
	}
}

func TestRecipientDisplayNameDoesNotRepairMalformedAddress(t *testing.T) {
	for _, address := range []string{"A B@example.com", "A@B@example.com", "A..B@example.com", "one@example.com, two@example.com", `"unterminated@example.com`} {
		t.Run(address, func(t *testing.T) {
			for _, name := range []string{"", "Display", "Jörg"} {
				input := DraftInput{To: []Recipient{{Name: name, Address: address}}}
				if err := validateDraftAddresses(input); err == nil {
					t.Errorf("accepted malformed input with display name %q", name)
				}
			}
		})
	}
}

func TestQuotedMailboxSenderBindingsRoundTrip(t *testing.T) {
	for _, address := range []string{`"A B"@icloud.com`, `"Jörg Smith"@gmail.com`, `"A\"B"@icloud.com`, `"A@B"@gmail.com`} {
		t.Run(address, func(t *testing.T) {
			parsed, err := stdmail.ParseAddress(address)
			if err != nil {
				t.Fatal(err)
			}
			sender, err := sendSender("Sender <" + address + ">")
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, _, err := transport.ProviderHosts(sender); err != nil {
				t.Errorf("sender lost provider-resolvable syntax: %q, %v", sender, err)
			}
			store := NewAccountBindingStore(filepath.Join(t.TempDir(), "bindings.json"))
			if err := store.UpsertAccountBinding(AccountBinding{AccountID: "quoted", SenderAliases: []string{address}, CredentialAccount: address}); err != nil {
				t.Fatal(err)
			}
			document, err := store.LoadAccountBindings()
			if err != nil {
				t.Fatal(err)
			}
			binding, found, err := ResolveAccountBinding(document, sender, "QUOTED")
			if err != nil || !found {
				t.Fatalf("resolve persisted alias: found=%t, %v", found, err)
			}
			for _, value := range []string{binding.CredentialAccount, binding.SenderAliases[0]} {
				actual, err := stdmail.ParseAddress(value)
				if err != nil || actual.Address != parsed.Address {
					t.Errorf("persisted identity = %q, %v", value, err)
				}
			}
		})
	}
}

func TestQuotedMailboxDirectTransportMatchesHeaders(t *testing.T) {
	for _, address := range []string{`"A B"@icloud.com`, `"Jörg Smith"@icloud.com`, `"A\"B"@icloud.com`, `"A@B"@icloud.com`} {
		t.Run(address, func(t *testing.T) {
			submitter, mirror := sendTransportStubs()
			draft := Draft{From: "Jörg <" + address + ">", To: []Recipient{{Name: "Recipient", Address: address}}, Body: "Body"}
			evidence, err := DeliverViaTransport(context.Background(), SendTransport{Submitter: submitter, Mirror: mirror, Credentials: &stubCredentials{password: "test"}}, draft)
			if err != nil || !evidence.SubmissionAccepted || submitter.calls != 1 || mirror.calls != 1 {
				t.Fatalf("direct transport: %+v, %v; calls %d/%d", evidence, err, submitter.calls, mirror.calls)
			}
			if submitter.lastFrom != address || len(submitter.lastTo) != 1 || submitter.lastTo[0] != address {
				t.Errorf("envelope = %q, %q; want %q", submitter.lastFrom, submitter.lastTo, address)
			}
			message := readComposedHeaderTestMessage(t, submitter.lastMessage)
			for _, header := range []string{"From", "To"} {
				actual, err := message.Header.AddressList(header)
				want, parseErr := stdmail.ParseAddress(address)
				if err != nil || parseErr != nil || len(actual) != 1 || actual[0].Address != want.Address {
					t.Errorf("%s identity: %v, %v", header, actual, err)
				}
			}
		})
	}
}

func TestQuotedMailboxStoredInvalidInputStopsBeforeSMTP(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Draft)
	}{
		{"duplicate with different name", func(draft *Draft) { draft.CC = []Recipient{{Name: "Other", Address: draft.To[0].Address}} }},
		{"unquoted space with name", func(draft *Draft) { draft.To[0] = Recipient{Name: "Name", Address: "A B@example.com"} }},
		{"unquoted embedded at", func(draft *Draft) { draft.To[0] = Recipient{Name: "Name", Address: "A@B@example.com"} }},
		{"address list with name", func(draft *Draft) { draft.To[0] = Recipient{Name: "Name", Address: "one@example.com, two@example.com"} }},
		{"recipient raw control", func(draft *Draft) { draft.To[0].Address += "\n" }},
		{"recipient name control", func(draft *Draft) { draft.To[0].Name = "Unsafe\x00Name" }},
		{"recipient decoded name control", func(draft *Draft) { draft.To[0].Address = "=?UTF-8?b?eA0KeQ==?= <recipient@example.com>" }},
		{"BCC raw control", func(draft *Draft) { draft.BCC = []Recipient{{Address: "hidden@example.com\n"}} }},
		{"BCC name control", func(draft *Draft) { draft.BCC = []Recipient{{Name: "Unsafe\x00Name", Address: "hidden@example.com"}} }},
		{"BCC decoded name control", func(draft *Draft) { draft.BCC = []Recipient{{Address: "=?UTF-8?b?eA0KeQ==?= <hidden@example.com>"}} }},
		{"sender raw control", func(draft *Draft) { draft.From += "\n" }},
		{"sender decoded name control", func(draft *Draft) { draft.From = "=?UTF-8?b?eA0KeQ==?= <sender@icloud.com>" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			submitter, mirror := sendTransportStubs()
			service := newTransportService(root, submitter, mirror, &stubCredentials{password: "test"})
			draft, err := service.CreateDraft(CreateDraftRequest{Input: DraftInput{From: "sender@icloud.com", To: []Recipient{{Address: `"A B"@example.com`}}, Body: "Body"}})
			if err != nil {
				t.Fatal(err)
			}
			test.change(&draft)
			if err := refreshDraftRevision(&draft); err != nil {
				t.Fatal(err)
			}
			if err := writeDraftFile(root, draft); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, draft.Ref+".json")
			before := readRevisionTestFile(t, path)
			if _, err := service.SendDraft(context.Background(), SendDraftRequest{Ref: draft.Ref, ExpectedRevision: draft.Revision}); errorCode(err) != "invalid_argument" || submitter.calls != 0 || mirror.calls != 0 {
				t.Fatalf("invalid stored draft: %v; transport calls=%d/%d", err, submitter.calls, mirror.calls)
			}
			if !bytes.Equal(before, readRevisionTestFile(t, path)) {
				t.Fatal("invalid input altered the stored draft")
			}
			assertNoSendClaim(t, root, draft.Ref)
		})
	}
}

func TestQuotedMailboxNativeSenderCatalogComparison(t *testing.T) {
	for _, address := range []string{`"A B"@icloud.com`, `"Jörg Smith"@icloud.com`, `"A\"B"@icloud.com`, `"A@B"@icloud.com`} {
		t.Run(address, func(t *testing.T) {
			service := NewService(&gatewayStub{accounts: []Account{{EmailAddresses: []string{address}}}})
			if err := service.validateDraftSender(context.Background(), "Sender <"+address+">"); err != nil {
				t.Fatalf("configured mailbox rejected: %v", err)
			}
			if err := service.validateDraftSender(context.Background(), "other@icloud.com"); err == nil {
				t.Fatal("unconfigured mailbox accepted")
			}
		})
	}
}

func TestQuotedMailboxCatalogMatchUsesParsedIdentity(t *testing.T) {
	for _, test := range []struct {
		name, candidate, sender string
		want                    bool
	}{
		{"quoted atom", `"Alias"@icloud.com`, "Alias@icloud.com", true},
		{"quoted pair", `"A\ B"@icloud.com`, `"A B"@icloud.com`, true},
		{"name and casing", `Display <"A B"@ICLOUD.COM>`, `"a b"@icloud.com`, true},
		{"different mailbox", `"A B"@icloud.com`, `"A C"@icloud.com`, false},
		{"invalid catalog input", "A B@icloud.com", `"A B"@icloud.com`, false},
		{"invalid sender input", `"A B"@icloud.com`, "A B@icloud.com", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, account := range []Account{{EmailAddresses: []string{test.candidate}}, {DiscoveredSenderIdentities: []string{test.candidate}}, {ConfiguredSenderAliases: []string{test.candidate}}} {
				if got := accountContainsAddress(account, test.sender); got != test.want {
					t.Errorf("catalog comparison = %t, want %t", got, test.want)
				}
			}
		})
	}
}
