package cli

import (
	"bytes"
	"encoding/json"
	stdmail "net/mail"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

func TestQuotedMailboxNativeAndJSONDraftInputs(t *testing.T) {
	for _, address := range []string{`"A B"@example.com`, `"Jörg Smith"@example.com`, `"A\"B"@example.com`, `"A@B"@example.com`} {
		t.Run(address, func(t *testing.T) {
			flags := newFlagSet("quoted", &bytes.Buffer{})
			options := registerDraftInputFlags(flags)
			if err := flags.Parse([]string{"--from", "sender@icloud.com", "--to", "Jörg <" + address + ">", "--body", "Body"}); err != nil {
				t.Fatal(err)
			}
			input, err := options.read()
			if err != nil || len(input.To) != 1 || input.To[0].Address != address || input.To[0].Name != "Jörg" {
				t.Fatalf("native input = %+v, %v", input, err)
			}
			payload, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			fromJSON, err := decodeDraftInput(bytes.NewReader(payload))
			if err != nil || !reflect.DeepEqual(input.To, fromJSON.To) {
				t.Fatalf("JSON input = %+v, %v", fromJSON, err)
			}
			for _, source := range []mail.DraftInput{input, fromJSON} {
				service := mail.NewServiceWithTransport(nil, t.TempDir(), mail.SendTransport{})
				draft, err := service.CreateDraft(mail.CreateDraftRequest{Input: source})
				if err != nil {
					t.Fatal(err)
				}
				stored, err := service.GetDraft(draft.Ref)
				if err != nil || stored.Revision != draft.Revision || !reflect.DeepEqual(stored.To, source.To) {
					t.Fatalf("stored input = %+v, %v", stored, err)
				}
				message, err := mail.BuildMessage(stored, "<flags@example.com>")
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := stdmail.ReadMessage(bytes.NewReader(message))
				if err != nil {
					t.Fatal(err)
				}
				to, err := decoded.Header.AddressList("To")
				want, parseErr := stdmail.ParseAddress(address)
				if err != nil || parseErr != nil || len(to) != 1 || to[0].Address != want.Address || to[0].Name != "Jörg" {
					t.Fatalf("composed input = %+v, %v", to, err)
				}
			}
		})
	}
}

func TestQuotedMailboxSetupUsesStableCredentialIdentity(t *testing.T) {
	for _, address := range []string{`"A B"@icloud.com`, `"Jörg Smith"@icloud.com`, `"A\"B"@icloud.com`, `"A@B"@icloud.com`} {
		t.Run(address, func(t *testing.T) {
			credentials := newStubSetupCredentials()
			bindings := mail.NewAccountBindingStore(filepath.Join(t.TempDir(), "bindings.json"))
			account, err := mailref.EncodeAccount("QUOTED")
			if err != nil {
				t.Fatal(err)
			}
			previousCredentials, previousStdin := sendSetupCredentials, sendSetupStdin
			sendSetupCredentials = func() transport.CredentialStore { return credentials }
			t.Cleanup(func() { sendSetupCredentials, sendSetupStdin = previousCredentials, previousStdin })
			credential := `"Login Box"@icloud.com`
			for _, action := range []string{"store", "rotate", "remove"} {
				// Each prompt owns its reader; no buffered leftovers cross invocations.
				sendSetupStdin = strings.NewReader("test-secret\n")
				args := []string{"setup", "--from", "Sender <" + address + ">", "--account", account, "--json"}
				switch action {
				case "store":
					args = append(args, "--credential-account", credential)
				case "remove":
					args = append(args, "--remove")
				}
				var stdout, stderr bytes.Buffer
				invalidated := ""
				code := runSendWithBindings(args, &stdout, &stderr, func(value string) { invalidated = value }, bindings)
				if code != 0 || stderr.Len() != 0 || invalidated != credential {
					t.Fatalf("%s: code=%d output=%s error=%s invalidated=%q", action, code, stdout.String(), stderr.String(), invalidated)
				}
				if action == "remove" {
					if len(credentials.stored) != 0 {
						t.Fatalf("credentials remain after removal: %d", len(credentials.stored))
					}
					continue
				}
				if len(credentials.stored) != 1 || credentials.stored[credential] != "test-secret" {
					t.Fatal("setup used a different credential identity")
				}
				document, err := bindings.LoadAccountBindings()
				if err != nil {
					t.Fatal(err)
				}
				binding, found, err := mail.ResolveAccountBinding(document, address, "QUOTED")
				if err != nil || !found || binding.CredentialAccount != credential || !reflect.DeepEqual(binding.SenderAliases, []string{address}) {
					t.Fatalf("binding = %+v, found=%t, %v", binding, found, err)
				}
			}
		})
	}
}
