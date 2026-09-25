package mailstore

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
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
	sender, credential, binding, err := resolveAccountIdentityFromCatalog(
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
	if binding == nil || binding.CredentialAccount != "login@icloud.com" {
		t.Fatalf("resolveAccountIdentityFromCatalog() binding = %+v, want credential login@icloud.com", binding)
	}
}

func TestResolveAccountIdentityBindingRejectsRemovedAccount(t *testing.T) {
	_, _, _, err := resolveAccountIdentityFromCatalog(
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
	_, _, _, err = resolveAccountIdentityFromCatalog(
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

func TestMutationAccountResolutionLoadsOnlyTargetAccount(t *testing.T) {
	fixture := newMutationIdentityFixture(t)
	store, client, counters, bindings, credentials := openMutationIdentityClient(t, fixture)
	messageRefs := mutationTargetReferences(t, store, fixture.searchFixtureData, 100)

	sender, credential, binding, err := client.resolveAccountIdentity(context.Background(), testAccountID)
	if err != nil || sender != "identity@gmail.com" || credential != "identity@gmail.com" {
		t.Fatalf("resolveAccountIdentity() = sender:%q credential:%q error:%v", sender, credential, err)
	}
	if binding == nil || binding.AccountID != testAccountID {
		t.Fatalf("resolveAccountIdentity() binding = %+v, want target account binding", binding)
	}
	if got := counters.fullCatalogBuilds.Load(); got != 0 {
		t.Fatalf("full catalog builds = %d, want 0", got)
	}

	for _, test := range []struct {
		name  string
		items int
	}{
		{name: "one mutation", items: 1},
		{name: "one hundred mutations", items: 100},
	} {
		t.Run(test.name, func(t *testing.T) {
			counters.fullCatalogBuilds.Store(0)
			counters.sentScanQueries.Store(0)
			bindings.loadCalls.Store(0)
			credentials.loadCalls.Store(0)
			for _, messageRef := range messageRefs[:test.items] {
				target, err := client.resolveImapTargetForMutation(context.Background(), messageRef)
				if err != nil || target.accountID != testAccountID || target.uid == 0 || target.uidvalidity == 0 {
					t.Fatalf("resolve mutation target: target=%+v error=%v", target, err)
				}
			}
			if got := counters.fullCatalogBuilds.Load(); got != 0 {
				t.Fatalf("full catalog builds = %d, want 0", got)
			}
			if got, want := counters.sentScanQueries.Load(), int64(2*test.items); got != want {
				t.Fatalf("Sent scan queries = %d, want %d for the target account only", got, want)
			}
			if got, want := bindings.loadCalls.Load(), int64(2*test.items); got != want {
				t.Fatalf("binding loads = %d, want %d across account and client freshness boundaries", got, want)
			}
			if got, want := credentials.loadCalls.Load(), int64(2*test.items); got != want {
				t.Fatalf("credential loads = %d, want %d across account resolution and IMAP setup", got, want)
			}
		})
	}
}

func TestMutationAccountResolutionPreservesDegradedTarget(t *testing.T) {
	fixture := newMutationIdentityFixture(t)
	cachePath := filepath.Join(fixture.mailRoot, "V10", testAccountID, ".mboxCache.plist")
	if err := os.WriteFile(cachePath, []byte("not a plist"), 0o600); err != nil {
		t.Fatalf("corrupt generated target mailbox cache: %v", err)
	}
	_, client, counters, _, _ := openMutationIdentityClient(t, fixture)
	_, _, _, err := client.resolveAccountIdentity(context.Background(), testAccountID)
	if errorCodeForTest(err) != "account_degraded" {
		t.Fatalf("resolveAccountIdentity() error = %v, want account_degraded", err)
	}
	if got := counters.fullCatalogBuilds.Load(); got != 0 {
		t.Fatalf("full catalog builds = %d, want 0 for a degraded active target", got)
	}
}

func TestMutationAccountResolutionPreservesSentSenderEvidence(t *testing.T) {
	fixture := newMutationIdentityFixture(t)
	store, client, counters, _, credentials := openMutationIdentityClient(t, fixture)
	databasePath := filepath.Join(fixture.mailRoot, "V10", "MailData", envelopeIndexName)
	database := openTestWriter(t, databasePath)
	if _, err := database.Exec(`INSERT INTO addresses(ROWID,address,comment) VALUES(3,'history@gmail.com','History')`); err != nil {
		closeTestResourceNow(t, database, "generated Sent identity fixture database")
		t.Fatalf("insert generated Sent sender: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO messages(ROWID,message_id,global_message_id,sender,subject,summary,date_sent,date_received,mailbox,flags,read,flagged,deleted,size,conversation_id,type,display_date,flag_color) VALUES(701,1701,2701,3,1,1,900,900,4,0,0,0,0,100,701,0,900,0)`); err != nil {
		closeTestResourceNow(t, database, "generated Sent identity fixture database")
		t.Fatalf("insert generated Sent message: %v", err)
	}
	closeTestResourceNow(t, database, "generated Sent identity fixture database")

	emptyBindings := &countedMutationBindings{AccountBindingStore: mail.NewAccountBindingStore(filepath.Join(t.TempDir(), "empty-bindings.json"))}
	store.accountBindings = emptyBindings
	client.send.AccountBindings = emptyBindings
	credentials.strictCredentials["history@gmail.com"] = "generated-test-password"

	accounts, err := store.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts() for Sent evidence: %v", err)
	}
	bindingFile, err := emptyBindings.LoadAccountBindings()
	if err != nil {
		t.Fatalf("LoadAccountBindings() for Sent evidence: %v", err)
	}
	wantSender, wantCredential, wantBinding, err := resolveAccountIdentityFromCatalog(accounts, testAccountID, credentials, bindingFile)
	if err != nil || wantSender != "history@gmail.com" || wantCredential != "history@gmail.com" || wantBinding != nil {
		t.Fatalf("full-catalog identity = sender:%q credential:%q binding:%+v error:%v", wantSender, wantCredential, wantBinding, err)
	}

	counters.fullCatalogBuilds.Store(0)
	counters.sentScanQueries.Store(0)
	emptyBindings.loadCalls.Store(0)
	credentials.loadCalls.Store(0)
	sender, credential, binding, err := client.resolveAccountIdentity(context.Background(), testAccountID)
	if err != nil || sender != wantSender || credential != wantCredential || binding != nil {
		t.Fatalf("targeted identity = sender:%q credential:%q binding:%+v error:%v; want full-catalog identity", sender, credential, binding, err)
	}
	if got := counters.fullCatalogBuilds.Load(); got != 0 {
		t.Fatalf("full catalog builds = %d, want 0", got)
	}
	if got := counters.sentScanQueries.Load(); got != 2 {
		t.Fatalf("Sent scan queries = %d, want 2 for the target account", got)
	}
	if got := emptyBindings.loadCalls.Load(); got != 2 {
		t.Fatalf("binding loads = %d, want 2 across account and client freshness boundaries", got)
	}
	if got := credentials.loadCalls.Load(); got != 1 {
		t.Fatalf("credential loads = %d, want 1 for identity verification", got)
	}
}

func TestMutationAccountResolutionFallsBackForInactiveTarget(t *testing.T) {
	fixture := newMutationIdentityFixture(t)
	_, client, counters, bindings, _ := openMutationIdentityClient(t, fixture)
	if err := bindings.UpsertAccountBinding(mail.AccountBinding{
		AccountID: "INACTIVE-ACCOUNT", SenderAliases: []string{"inactive@gmail.com"}, CredentialAccount: "inactive@gmail.com",
	}); err != nil {
		t.Fatalf("add inactive account binding: %v", err)
	}
	_, _, _, err := client.resolveAccountIdentity(context.Background(), "INACTIVE-ACCOUNT")
	if errorCodeForTest(err) != accountBindingStaleCode {
		t.Fatalf("resolveAccountIdentity() error = %v, want %s", err, accountBindingStaleCode)
	}
	if got := counters.fullCatalogBuilds.Load(); got != 1 {
		t.Fatalf("full catalog builds = %d, want 1 fallback for an inactive target", got)
	}
}

func TestMutationAccountResolutionPreservesCrossAccountError(t *testing.T) {
	fixture := newMutationIdentityFixture(t)
	store, client, counters, _, _ := openMutationIdentityClient(t, fixture)
	messageRefs := mutationTargetReferences(t, store, fixture.searchFixtureData, 1)
	destinationAccountRef, err := mailref.EncodeAccount(secondaryCatalogAccountID)
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	destinations, err := store.ListMailboxes(context.Background(), mail.ListMailboxesRequest{AccountRef: destinationAccountRef})
	if err != nil || len(destinations) == 0 {
		t.Fatalf("ListMailboxes() returned %d mailboxes, error = %v", len(destinations), err)
	}
	_, err = client.TransferMessage(context.Background(), mail.TransferMessageRequest{
		Ref: messageRefs[0], DestinationMailbox: destinations[0].Ref, Copy: true,
	})
	if errorCodeForTest(err) != transport.CodeIMAPMutationFailed || !containsText(err, "cross-account") {
		t.Fatalf("TransferMessage() error = %v, want cross-account %s", err, transport.CodeIMAPMutationFailed)
	}
	if got := counters.fullCatalogBuilds.Load(); got != 0 {
		t.Fatalf("full catalog builds = %d, want 0 before rejecting a cross-account transfer", got)
	}
}

func containsText(err error, text string) bool {
	return err != nil && text != "" && strings.Contains(err.Error(), text)
}
