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
			got, err := resolveAccountEmailFromCatalog(context.Background(), test.accounts, "TARGET-ACCOUNT", test.credentials)
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
		context.Background(),
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
		context.Background(),
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
		context.Background(),
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
	store, client, bindings, credentials := openMutationIdentityClient(t, fixture)
	messageRefs := mutationTargetReferences(t, store, fixture.searchFixtureData, 100)

	initialInvocation := client.WithAccountBindingSnapshot(context.Background())
	sender, credential, binding, err := client.resolveAccountIdentity(initialInvocation, testAccountID)
	if err != nil || sender != "identity@gmail.com" || credential != "identity@gmail.com" {
		t.Fatalf("resolveAccountIdentity() = sender:%q credential:%q error:%v", sender, credential, err)
	}
	if binding == nil || binding.AccountID != testAccountID {
		t.Fatalf("resolveAccountIdentity() binding = %+v, want target account binding", binding)
	}
	assertMutationMailboxRows(t, initialInvocation, client, testAccountID, 3)

	for _, test := range []struct {
		name  string
		items int
	}{
		{name: "one mutation", items: 1},
		{name: "one hundred mutations", items: 100},
	} {
		t.Run(test.name, func(t *testing.T) {
			invocationCtx := client.WithAccountBindingSnapshot(context.Background())
			bindings.loadCalls.Store(0)
			credentials.loadCalls.Store(0)
			for _, messageRef := range messageRefs[:test.items] {
				target, err := client.resolveImapTargetForMutation(invocationCtx, messageRef)
				if err != nil || target.accountID != testAccountID || target.uid == 0 || target.uidvalidity == 0 {
					t.Fatalf("resolve mutation target: target=%+v error=%v", target, err)
				}
			}
			if got := bindings.loadCalls.Load(); got != 1 {
				t.Fatalf("binding loads = %d, want 1 shared load for the invocation", got)
			}
			assertMutationMailboxRows(t, invocationCtx, client, testAccountID, 3)
			if got, want := credentials.loadCalls.Load(), int64(2*test.items); got != want {
				t.Fatalf("credential loads = %d, want %d across account resolution and IMAP setup", got, want)
			}
		})
	}
}

func TestMutationAccountBindingSnapshotIsFreshPerInvocation(t *testing.T) {
	fixture := newMutationIdentityFixture(t)
	_, client, bindings, _ := openMutationIdentityClient(t, fixture)
	firstInvocation := client.WithAccountBindingSnapshot(context.Background())
	if sender, _, _, err := client.resolveAccountIdentity(firstInvocation, testAccountID); err != nil || sender != "identity@gmail.com" {
		t.Fatalf("first invocation identity = %q, error = %v", sender, err)
	}
	if err := mail.NewAccountBindingStore(fixture.bindingPath).UpsertAccountBinding(mail.AccountBinding{
		AccountID: testAccountID, SenderAliases: []string{"identity@gmail.com"}, CredentialAccount: "changed@gmail.com",
	}); err != nil {
		t.Fatalf("change binding for next invocation: %v", err)
	}
	if sender, _, _, err := client.resolveAccountIdentity(firstInvocation, testAccountID); err != nil || sender != "identity@gmail.com" {
		t.Fatalf("same-invocation identity = %q, error = %v; want original coherent snapshot", sender, err)
	}
	if got := bindings.loadCalls.Load(); got != 1 {
		t.Fatalf("same-invocation binding loads = %d, want 1", got)
	}

	secondInvocation := client.WithAccountBindingSnapshot(context.Background())
	if _, _, _, err := client.resolveAccountIdentity(secondInvocation, testAccountID); errorCodeForTest(err) != accountIdentityMissingCode {
		t.Fatalf("next-invocation error = %v, want fresh binding failure %s", err, accountIdentityMissingCode)
	}
	if got := bindings.loadCalls.Load(); got != 2 {
		t.Fatalf("binding loads across two invocations = %d, want 2", got)
	}
}

func assertMutationMailboxRows(
	tb testing.TB,
	ctx context.Context,
	client *Client,
	accountID string,
	wantRows int,
) {
	tb.Helper()
	snapshot, ok := ctx.Value(accountBindingSnapshotContextKey{}).(*accountBindingSnapshot)
	if !ok || snapshot == nil || snapshot.client != client {
		tb.Fatal("invocation context has no account-binding snapshot for the client")
	}
	accountRoot := ""
	for _, account := range client.store.activeAccounts {
		if account.AccountID == accountID {
			accountRoot = account.rootKey()
			break
		}
	}
	if accountRoot == "" {
		tb.Fatalf("account %s is not active in the generated fixture", accountID)
	}
	snapshot.mailboxRecordsMu.Lock()
	cached, exists := snapshot.mailboxRecords[accountRoot]
	snapshot.mailboxRecordsMu.Unlock()
	if !exists {
		tb.Fatalf("invocation has no mailbox-row result for account root %s", accountRoot)
	}
	if cached.err != nil {
		tb.Fatalf("mailbox-row query for account root %s: %v", accountRoot, cached.err)
	}
	if len(cached.records) != wantRows {
		tb.Fatalf("mailbox rows for account root %s = %d, want %d", accountRoot, len(cached.records), wantRows)
	}
	for _, record := range cached.records {
		if record.Location.rootKey() != accountRoot {
			tb.Fatalf("mailbox row %q belongs to root %s, want only %s", record.URL, record.Location.rootKey(), accountRoot)
		}
	}
}

func TestMutationAccountResolutionDoesNotReadOtherAccountMailboxRows(t *testing.T) {
	fixture := newMutationIdentityFixture(t)
	corruptSecondaryMailboxURL(t, fixture)
	_, client, bindings, _ := openMutationIdentityClient(t, fixture)
	invocationCtx := client.WithAccountBindingSnapshot(context.Background())
	sender, _, _, err := client.resolveAccountIdentity(invocationCtx, testAccountID)
	if err != nil || sender != "identity@gmail.com" {
		t.Fatalf("target identity = %q, error = %v; malformed secondary mailbox must stay outside the target query", sender, err)
	}
	assertMutationMailboxRows(t, invocationCtx, client, testAccountID, 3)
	if got := bindings.loadCalls.Load(); got != 1 {
		t.Fatalf("binding loads = %d, want 1", got)
	}
}

func TestMutationAccountResolutionInactiveTargetPreservesCatalogFailure(t *testing.T) {
	fixture := newMutationIdentityFixture(t)
	corruptSecondaryMailboxURL(t, fixture)
	_, client, bindings, _ := openMutationIdentityClient(t, fixture)
	_, _, _, err := client.resolveAccountIdentity(context.Background(), "INACTIVE-ACCOUNT")
	if errorCodeForTest(err) != "account_catalog_incomplete" {
		t.Fatalf("inactive target error = %v, want account_catalog_incomplete from full-catalog fallback", err)
	}
	if got := bindings.loadCalls.Load(); got != 1 {
		t.Fatalf("binding loads = %d, want 1 even when full-catalog fallback fails", got)
	}
}

func corruptSecondaryMailboxURL(tb testing.TB, fixture mutationIdentityFixture) {
	tb.Helper()
	databasePath := filepath.Join(fixture.mailRoot, "V10", "MailData", envelopeIndexName)
	database := openTestWriter(tb, databasePath)
	oldURL := "imap://" + secondaryCatalogAccountID + "/Sent"
	result, err := database.Exec(
		`UPDATE mailboxes SET url = ? WHERE url = ?`,
		"imap://"+secondaryCatalogAccountID+"/broken%ZZ", oldURL,
	)
	if err != nil {
		closeTestResourceNow(tb, database, "malformed secondary mailbox fixture database")
		tb.Fatalf("corrupt secondary mailbox URL: %v", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil || rowsAffected != 1 {
		closeTestResourceNow(tb, database, "malformed secondary mailbox fixture database")
		tb.Fatalf("corrupt secondary mailbox rows = %d, error = %v; want exactly 1 row", rowsAffected, err)
	}
	closeTestResourceNow(tb, database, "malformed secondary mailbox fixture database")
}

func TestMutationAccountResolutionPreservesDegradedTarget(t *testing.T) {
	fixture := newMutationIdentityFixture(t)
	cachePath := filepath.Join(fixture.mailRoot, "V10", testAccountID, ".mboxCache.plist")
	if err := os.WriteFile(cachePath, []byte("not a plist"), 0o600); err != nil {
		t.Fatalf("corrupt generated target mailbox cache: %v", err)
	}
	_, client, _, _ := openMutationIdentityClient(t, fixture)
	_, _, _, err := client.resolveAccountIdentity(context.Background(), testAccountID)
	if errorCodeForTest(err) != "account_degraded" {
		t.Fatalf("resolveAccountIdentity() error = %v, want account_degraded", err)
	}
}

func TestMutationAccountResolutionPreservesSentSenderEvidence(t *testing.T) {
	fixture := newMutationIdentityFixture(t)
	store, client, _, credentials := openMutationIdentityClient(t, fixture)
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
	wantSender, wantCredential, wantBinding, err := resolveAccountIdentityFromCatalog(context.Background(), accounts, testAccountID, credentials, bindingFile)
	if err != nil || wantSender != "history@gmail.com" || wantCredential != "history@gmail.com" || wantBinding != nil {
		t.Fatalf("full-catalog identity = sender:%q credential:%q binding:%+v error:%v", wantSender, wantCredential, wantBinding, err)
	}

	emptyBindings.loadCalls.Store(0)
	credentials.loadCalls.Store(0)
	sender, credential, binding, err := client.resolveAccountIdentity(context.Background(), testAccountID)
	if err != nil || sender != wantSender || credential != wantCredential || binding != nil {
		t.Fatalf("targeted identity = sender:%q credential:%q binding:%+v error:%v; want full-catalog identity", sender, credential, binding, err)
	}
	if got := emptyBindings.loadCalls.Load(); got != 1 {
		t.Fatalf("binding loads = %d, want 1 shared load for the invocation", got)
	}
	if got := credentials.loadCalls.Load(); got != 1 {
		t.Fatalf("credential loads = %d, want 1 for identity verification", got)
	}
}

func TestMutationAccountResolutionFallsBackForInactiveTarget(t *testing.T) {
	fixture := newMutationIdentityFixture(t)
	_, client, bindings, _ := openMutationIdentityClient(t, fixture)
	if err := bindings.UpsertAccountBinding(mail.AccountBinding{
		AccountID: "INACTIVE-ACCOUNT", SenderAliases: []string{"inactive@gmail.com"}, CredentialAccount: "inactive@gmail.com",
	}); err != nil {
		t.Fatalf("add inactive account binding: %v", err)
	}
	_, _, _, err := client.resolveAccountIdentity(context.Background(), "INACTIVE-ACCOUNT")
	if errorCodeForTest(err) != accountBindingStaleCode {
		t.Fatalf("resolveAccountIdentity() error = %v, want %s", err, accountBindingStaleCode)
	}
	if got := bindings.loadCalls.Load(); got != 1 {
		t.Fatalf("binding loads = %d, want 1 shared load for the inactive fallback", got)
	}
}

func TestMutationAccountResolutionPreservesCrossAccountError(t *testing.T) {
	fixture := newMutationIdentityFixture(t)
	store, client, _, _ := openMutationIdentityClient(t, fixture)
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
}

func containsText(err error, text string) bool {
	return err != nil && text != "" && strings.Contains(err.Error(), text)
}
