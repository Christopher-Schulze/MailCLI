package mailstore

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"mailcli/internal/keychain"
	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

type credentialProbe struct {
	strictCredentials
	loads    []string
	failures map[string]error
}

func (p *credentialProbe) Load(account string) (string, error) {
	p.loads = append(p.loads, account)
	if err := p.failures[account]; err != nil {
		return "", err
	}
	return p.strictCredentials.Load(account)
}

func TestIMAPCredentialResolutionBoundary(t *testing.T) {
	accountRef, err := mailref.EncodeAccount(testAccountID)
	if err != nil {
		t.Fatal(err)
	}
	missing := fmt.Errorf("credential lookup: %w", &keychain.KeychainError{Code: keychain.CodeNotFound, Message: "no item"})
	denied := &keychain.KeychainError{Code: keychain.CodeLoadFailed, Message: "access denied", Err: errors.New("credential access denied")}
	plain := errors.New("credential helper failed")
	for _, route := range []string{"targeted", "sync/discovery"} {
		for _, test := range []struct {
			name                 string
			aliases              []string
			binding              *mail.AccountBinding
			passwords            strictCredentials
			failures             map[string]error
			nilStore             bool
			wantLoads            []string
			wantSender, username string
			host, code           string
			cause                error
		}{
			{name: "one unbound load", aliases: []string{"alpha@gmail.com"}, passwords: strictCredentials{"alpha@gmail.com": "generated-password"}, wantLoads: []string{"alpha@gmail.com"}, wantSender: "alpha@gmail.com", username: "alpha@gmail.com", host: "imap.gmail.com"},
			{name: "missing alias then success", aliases: []string{"beta@gmail.com", "alpha@gmail.com"}, passwords: strictCredentials{"beta@gmail.com": "generated-password"}, failures: map[string]error{"alpha@gmail.com": missing}, wantLoads: []string{"alpha@gmail.com", "beta@gmail.com"}, wantSender: "beta@gmail.com", username: "beta@gmail.com", host: "imap.gmail.com"},
			{name: "empty alias then success", aliases: []string{"alpha@gmail.com", "beta@gmail.com"}, passwords: strictCredentials{"beta@gmail.com": "generated-password"}, wantLoads: []string{"alpha@gmail.com", "beta@gmail.com"}, wantSender: "beta@gmail.com", username: "beta@gmail.com", host: "imap.gmail.com"},
			{name: "bound login differs from sender", aliases: []string{"alias@icloud.com"}, binding: &mail.AccountBinding{AccountID: testAccountID, SenderAliases: []string{"alias@icloud.com"}, CredentialAccount: "login@icloud.com"}, passwords: strictCredentials{"login@icloud.com": "generated-password"}, wantLoads: []string{"login@icloud.com"}, wantSender: "alias@icloud.com", username: "login@icloud.com", host: "imap.mail.me.com"},
			{name: "custom bound endpoints", aliases: []string{"alias@example.com"}, binding: &mail.AccountBinding{AccountID: testAccountID, SenderAliases: []string{"alias@example.com"}, CredentialAccount: "login@example.com", SMTPHost: "smtp.example.com", SMTPPort: 465, IMAPHost: "imap.example.com", IMAPPort: 993}, passwords: strictCredentials{"login@example.com": "generated-password"}, wantLoads: []string{"login@example.com"}, wantSender: "alias@example.com", username: "login@example.com", host: "imap.example.com"},
			{name: "invalid bound endpoint skips secure store", aliases: []string{"alias@icloud.com"}, binding: &mail.AccountBinding{AccountID: testAccountID, SenderAliases: []string{"alias@icloud.com"}, CredentialAccount: "login@icloud.com", IMAPHost: "imap.mail.me.com"}, passwords: strictCredentials{"login@icloud.com": "generated-password"}, code: "account_binding_host_invalid"},
			{name: "unsupported skips secure store", aliases: []string{"alpha@example.com"}, code: transport.CodeUnsupportedProvider},
			{name: "unsupported without store", aliases: []string{"alpha@example.com"}, nilStore: true, code: transport.CodeUnsupportedProvider},
			{name: "missing store", aliases: []string{"alpha@gmail.com"}, nilStore: true, code: "imap_credentials_missing"},
			{name: "missing item", aliases: []string{"alpha@gmail.com"}, failures: map[string]error{"alpha@gmail.com": missing}, wantLoads: []string{"alpha@gmail.com"}, code: "imap_credentials_missing", cause: missing},
			{name: "bound missing item", aliases: []string{"alias@icloud.com"}, binding: &mail.AccountBinding{AccountID: testAccountID, SenderAliases: []string{"alias@icloud.com"}, CredentialAccount: "login@icloud.com"}, failures: map[string]error{"login@icloud.com": missing}, wantLoads: []string{"login@icloud.com"}, code: "imap_credentials_missing", cause: missing},
			{name: "bound missing store", aliases: []string{"alias@icloud.com"}, binding: &mail.AccountBinding{AccountID: testAccountID, SenderAliases: []string{"alias@icloud.com"}, CredentialAccount: "login@icloud.com"}, nilStore: true, code: "imap_credentials_missing"},
			{name: "empty password beats later unsupported alias", aliases: []string{"alpha@gmail.com", "zeta@example.com"}, wantLoads: []string{"alpha@gmail.com"}, code: "imap_credentials_missing"},
			{name: "denied stops before successful alias", aliases: []string{"alpha@gmail.com", "beta@gmail.com"}, passwords: strictCredentials{"beta@gmail.com": "generated-password"}, failures: map[string]error{"alpha@gmail.com": denied}, wantLoads: []string{"alpha@gmail.com"}, code: keychain.CodeLoadFailed, cause: denied},
			{name: "plain failure stops before successful alias", aliases: []string{"alpha@gmail.com", "beta@gmail.com"}, passwords: strictCredentials{"beta@gmail.com": "generated-password"}, failures: map[string]error{"alpha@gmail.com": plain}, wantLoads: []string{"alpha@gmail.com"}, cause: plain},
			{name: "bound denied keeps cause", aliases: []string{"alias@icloud.com"}, binding: &mail.AccountBinding{AccountID: testAccountID, SenderAliases: []string{"alias@icloud.com"}, CredentialAccount: "login@icloud.com"}, failures: map[string]error{"login@icloud.com": denied}, wantLoads: []string{"login@icloud.com"}, code: keychain.CodeLoadFailed, cause: denied},
			{name: "stale bound alias", aliases: []string{"alpha@gmail.com"}, binding: &mail.AccountBinding{AccountID: testAccountID, SenderAliases: []string{"other@gmail.com"}, CredentialAccount: "alpha@gmail.com"}, passwords: strictCredentials{"alpha@gmail.com": "generated-password"}, code: accountBindingStaleCode},
		} {
			t.Run(route+"/"+test.name, func(t *testing.T) {
				probe := &credentialProbe{strictCredentials: test.passwords, failures: test.failures}
				var credentials transport.CredentialStore = probe
				if test.nilStore {
					credentials = nil
				}
				account := mail.Account{Ref: accountRef, State: "ok", EmailAddresses: test.aliases}
				bindings := mail.AccountBindingFile{Version: mail.AccountBindingVersion}
				if test.binding != nil {
					bindings.Bindings = []mail.AccountBinding{*test.binding}
				}
				resolve := func() (string, transport.ImapConfig, error) {
					if route == "targeted" {
						sender, cfg, _, err := resolveAccountIdentityFromCatalog(context.Background(), []mail.Account{account}, testAccountID, credentials, bindings)
						return sender, cfg, err
					}
					return imapConfigForAccount(context.Background(), account, credentials, bindings)
				}
				sender, cfg, err := resolve()
				if !reflect.DeepEqual(probe.loads, test.wantLoads) {
					t.Fatalf("loads=%v, want %v", probe.loads, test.wantLoads)
				}
				if test.wantSender != "" {
					if err != nil || sender != test.wantSender || cfg.Username != test.username || cfg.Host != test.host || cfg.Port != 993 || cfg.Password != "generated-password" {
						t.Fatalf("sender=%q username=%q host=%q port=%d error=%v", sender, cfg.Username, cfg.Host, cfg.Port, err)
					}
					probe.strictCredentials[test.username] = "fresh-generated-password"
					_, fresh, err := resolve()
					if err != nil || fresh.Password != "fresh-generated-password" || !reflect.DeepEqual(probe.loads, append(append([]string(nil), test.wantLoads...), test.wantLoads...)) {
						t.Fatalf("credential was reloaded twice or cached across attempts: loads=%v error=%v", probe.loads, err)
					}
					return
				}
				if err == nil || transport.ErrorCode(err) != test.code || sender != "" || cfg != (transport.ImapConfig{}) {
					t.Fatalf("failure changed: sender=%q code=%q error=%v", sender, transport.ErrorCode(err), err)
				}
				if test.cause != nil && !errors.Is(err, test.cause) {
					t.Fatalf("credential cause lost: %v", err)
				}
				if test.code == keychain.CodeLoadFailed {
					var typed *keychain.KeychainError
					if !errors.As(err, &typed) || typed != denied || !errors.Is(err, denied.Err) {
						t.Fatalf("typed denied cause lost: %v", err)
					}
				}
				if test.code == "imap_credentials_missing" && !strings.Contains(err.Error(), "mailcli send setup") {
					t.Fatalf("missing IMAP repair guidance: %v", err)
				}
			})
		}
	}
}

func TestIMAPCredentialFailureStopsNetworkDispatch(t *testing.T) {
	for _, route := range []string{"targeted", "sync", "discovery"} {
		t.Run(route, func(t *testing.T) {
			fixture := newMutationIdentityFixture(t)
			store, client, _, _ := openMutationIdentityClient(t, fixture)
			operator := &recentImapOperator{stubImapOperator: &stubImapOperator{}}
			denied := &keychain.KeychainError{Code: keychain.CodeLoadFailed, Message: "access denied"}
			probe := &credentialProbe{failures: map[string]error{"identity@gmail.com": denied}}
			client.send.Credentials, client.send.Imap = probe, operator
			accountRef, err := mailref.EncodeAccount(testAccountID)
			if err != nil {
				t.Fatal(err)
			}
			switch route {
			case "targeted":
				refs := mutationTargetReferences(t, store, fixture.searchFixtureData, 1)
				_, err := client.resolveImapTargetForMutation(context.Background(), refs[0])
				if !errors.Is(err, denied) || transport.ErrorCode(err) != keychain.CodeLoadFailed {
					t.Fatalf("targeted failure changed: %v", err)
				}
			case "sync":
				result, err := client.SyncCheck(context.Background(), accountRef)
				if err != nil || result.Complete || len(result.Failures) != 1 || result.Failures[0].Code != keychain.CodeLoadFailed {
					t.Fatalf("sync failure changed: %+v error=%v", result, err)
				}
			case "discovery":
				result, err := client.NewMessages(context.Background(), mail.NewMessagesRequest{AccountRef: accountRef, Limit: 1})
				if err != nil || result.Complete || len(result.Failures) != 1 || result.Failures[0].Code != keychain.CodeLoadFailed {
					t.Fatalf("discovery failure changed: %+v error=%v", result, err)
				}
			}
			if !reflect.DeepEqual(probe.loads, []string{"identity@gmail.com"}) || operator.listCalls != 0 || operator.searchCalls != 0 || len(operator.calls) != 0 {
				t.Fatalf("credential failure dispatched work: loads=%v LIST=%d SEARCH=%d recent=%v", probe.loads, operator.listCalls, operator.searchCalls, operator.calls)
			}
		})
	}
}
