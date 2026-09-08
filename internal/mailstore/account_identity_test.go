package mailstore

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
	"mailcli/internal/transport"
)

func TestResolveAccountEmailFromCatalog(t *testing.T) {
	validRef, err := mailref.EncodeAccount("TARGET-ACCOUNT")
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	unsupportedRef := "acct_" + base64.RawURLEncoding.EncodeToString(
		[]byte(`{"version":99,"account_id":"OTHER-ACCOUNT"}`),
	)
	tests := []struct {
		name        string
		accounts    []mail.Account
		credentials strictCredentials
		wantEmail   string
		wantCode    string
		wantText    string
	}{
		{
			name:     "empty catalog is disabled account",
			wantCode: accountDisabledCode,
			wantText: "not enabled",
		},
		{
			name:        "successful resolution",
			accounts:    []mail.Account{{Ref: validRef, EmailAddresses: []string{"target@gmail.com"}, State: "ok"}},
			credentials: strictCredentials{"target@gmail.com": "secret"},
			wantEmail:   "target@gmail.com",
		},
		{
			name:        "unrelated corrupt reference does not block valid target",
			accounts:    []mail.Account{{Ref: "acct_not-valid"}, {Ref: validRef, EmailAddresses: []string{"target@gmail.com"}, State: "ok"}},
			credentials: strictCredentials{"target@gmail.com": "secret"},
			wantEmail:   "target@gmail.com",
		},
		{
			name:     "all references preserve categories",
			accounts: []mail.Account{{Ref: "acct_not-valid"}, {Ref: unsupportedRef}},
			wantCode: accountReferenceInvalidCode,
			wantText: "corrupt account reference",
		},
		{
			name:     "unsupported references preserve version category",
			accounts: []mail.Account{{Ref: unsupportedRef}},
			wantCode: accountReferenceVersionUnsupportedCode,
			wantText: "unsupported account reference version",
		},
		{
			name:     "corrupt references preserve corruption category",
			accounts: []mail.Account{{Ref: "acct_not-valid"}, {Ref: "acct_also-not-valid"}},
			wantCode: accountReferenceCorruptCode,
			wantText: "corrupt account reference",
		},
		{
			name:     "disabled account is distinct",
			accounts: []mail.Account{{Ref: validRef, State: "disabled", EmailAddresses: []string{"target@example.com"}}},
			wantCode: accountDisabledCode,
			wantText: "disabled",
		},
		{
			name:     "missing identity is distinct",
			accounts: []mail.Account{{Ref: validRef, State: "ok"}},
			wantCode: accountIdentityMissingCode,
			wantText: "no provable sender identity",
		},
		{
			name:        "unsupported provider is typed",
			accounts:    []mail.Account{{Ref: validRef, EmailAddresses: []string{"target@example.com"}, State: "ok"}},
			credentials: strictCredentials{"target@example.com": "secret"},
			wantCode:    transport.CodeUnsupportedProvider,
			wantText:    "Supported providers:",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveAccountEmailFromCatalog(test.accounts, "TARGET-ACCOUNT", test.credentials)
			if got != test.wantEmail {
				t.Fatalf("email = %q, want %q; error = %v", got, test.wantEmail, err)
			}
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("error = %v, want nil", err)
				}
				return
			}
			if errorCodeForTest(err) != test.wantCode || !containsText(err, test.wantText) {
				t.Fatalf("error = %v, code = %q, want %q containing %q", err, errorCodeForTest(err), test.wantCode, test.wantText)
			}
			if test.name == "all references preserve categories" {
				var corrupt *mailref.AccountReferenceError
				if !errors.As(err, &corrupt) {
					t.Fatalf("error = %v, want preserved DecodeAccount cause", err)
				}
				if containsText(err, "acct_not-valid") {
					t.Fatalf("error = %v, leaked raw account reference", err)
				}
				if !containsText(err, "catalog entry 1") || !containsText(err, "catalog entry 2") || !containsText(err, "ref sha256:") {
					t.Fatalf("error = %v, want safe catalog entry fingerprints", err)
				}
			}
		})
	}
}

func TestResolveAccountIdentityBindingUsesConfiguredAliasWithoutHistory(t *testing.T) {
	accountRef, err := mailref.EncodeAccount("TARGET-ACCOUNT")
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	sender, credential, err := resolveAccountIdentityFromCatalog(
		[]mail.Account{{Ref: accountRef, State: "ok", ConfiguredSenderAliases: []string{"alias@icloud.com"}}},
		"TARGET-ACCOUNT",
		strictCredentials{"login@icloud.com": "secret"},
		mail.AccountBindingFile{
			Version:  mail.AccountBindingVersion,
			Bindings: []mail.AccountBinding{{AccountID: "TARGET-ACCOUNT", SenderAliases: []string{"alias@icloud.com"}, CredentialAccount: "login@icloud.com"}},
		},
	)
	if err != nil || sender != "alias@icloud.com" || credential != "login@icloud.com" {
		t.Fatalf("resolveAccountIdentityFromCatalog() = sender:%q credential:%q error:%v", sender, credential, err)
	}
}

func TestResolveAccountIdentityBindingRejectsRemovedAccount(t *testing.T) {
	_, _, err := resolveAccountIdentityFromCatalog(
		nil,
		"REMOVED-ACCOUNT",
		strictCredentials{"login@icloud.com": "secret"},
		mail.AccountBindingFile{
			Version:  mail.AccountBindingVersion,
			Bindings: []mail.AccountBinding{{AccountID: "REMOVED-ACCOUNT", SenderAliases: []string{"alias@icloud.com"}, CredentialAccount: "login@icloud.com"}},
		},
	)
	if errorCodeForTest(err) != accountBindingStaleCode {
		t.Fatalf("resolveAccountIdentityFromCatalog() error = %v, want %s", err, accountBindingStaleCode)
	}
}

func TestResolveAccountIdentityBindingPreservesDegradedAccountState(t *testing.T) {
	accountRef, err := mailref.EncodeAccount("DEGRADED-ACCOUNT")
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	_, _, err = resolveAccountIdentityFromCatalog(
		[]mail.Account{{Ref: accountRef, State: "degraded", DegradedReason: "mailbox_cache_unreadable", ConfiguredSenderAliases: []string{"alias@icloud.com"}}},
		"DEGRADED-ACCOUNT",
		strictCredentials{"login@icloud.com": "secret"},
		mail.AccountBindingFile{
			Version:  mail.AccountBindingVersion,
			Bindings: []mail.AccountBinding{{AccountID: "DEGRADED-ACCOUNT", SenderAliases: []string{"alias@icloud.com"}, CredentialAccount: "login@icloud.com"}},
		},
	)
	if errorCodeForTest(err) != "account_degraded" {
		t.Fatalf("resolveAccountIdentityFromCatalog() error = %v, want account_degraded", err)
	}
}

func containsText(err error, text string) bool {
	return err != nil && text != "" && strings.Contains(err.Error(), text)
}
