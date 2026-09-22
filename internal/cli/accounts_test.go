package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"mailcli/internal/mail"
	"mailcli/internal/mailref"
)

type accountCatalogGateway struct {
	testGateway
	catalog mail.AccountCatalog
}

func (g accountCatalogGateway) ListAccountCatalog(context.Context) (mail.AccountCatalog, error) {
	return g.catalog, nil
}

func TestAccountsListJSONReportsPartialCatalogAndRemediation(t *testing.T) {
	service := mail.NewService(accountCatalogGateway{catalog: mail.AccountCatalog{
		Accounts: []mail.Account{{
			Ref: "acct_ref", Name: "Account", State: "degraded",
			DegradedReason:      "sender_identity_unreadable",
			DegradedRemediation: "run `mailcli doctor` and repair the affected Sent data before retrying",
		}},
		Complete: false,
	}})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := Run(context.Background(), service, []string{"accounts", "list", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if !response.OK || response.Data.Complete == nil || *response.Data.Complete ||
		response.Data.IdentityCoverageComplete == nil || *response.Data.IdentityCoverageComplete {
		t.Fatalf("response = %+v, want complete=false", response)
	}
	if response.Data.Accounts == nil || len(*response.Data.Accounts) != 1 {
		t.Fatalf("accounts = %+v, want one account", response.Data.Accounts)
	}
	account := (*response.Data.Accounts)[0]
	if account.DegradedReason != "sender_identity_unreadable" || account.DegradedRemediation == "" {
		t.Fatalf("degraded account = %+v", account)
	}
}

func TestAccountsListHumanOutputMatchesPartialCompleteness(t *testing.T) {
	service := mail.NewService(accountCatalogGateway{catalog: mail.AccountCatalog{
		Accounts: []mail.Account{{
			Ref: "acct_ref", Name: "Account", State: "degraded",
			DegradedReason:      "mailbox_cache_unreadable",
			DegradedRemediation: "run `mailcli doctor` and let Mail.app rebuild the affected mailbox cache",
		}},
		Complete: false,
	}})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := Run(context.Background(), service, []string{"accounts", "list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{
		"complete\tfalse",
		"mailbox_cache_unreadable",
		"run `mailcli doctor` and let Mail.app rebuild the affected mailbox cache",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("stdout = %q, want %q", output, want)
		}
	}
}

func TestAccountsListHonorsIncompleteCatalogEvidence(t *testing.T) {
	service := mail.NewService(accountCatalogGateway{catalog: mail.AccountCatalog{
		Accounts: []mail.Account{{Ref: "acct_ref", Name: "Account", State: "ok"}},
		Complete: false,
	}})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := Run(context.Background(), service, []string{"accounts", "list", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if response.Data.Complete == nil || *response.Data.Complete {
		t.Fatalf("response = %+v, want complete=false", response)
	}
}

func TestAccountsListJSONReportsBoundedIdentityCoverage(t *testing.T) {
	service := mail.NewService(accountCatalogGateway{catalog: mail.AccountCatalog{
		Accounts: []mail.Account{{
			Ref: "acct_ref", Name: "Account", State: "ok",
			IdentityCoverage: mail.SenderIdentityCoverage{
				Source:           mail.SenderIdentityCoverageSourceSentHistory,
				State:            mail.SenderIdentityCoverageStateBounded,
				ObservedMessages: 2000,
				Limit:            2000,
				MoreAvailable:    true,
			},
		}},
		Complete: true,
	}})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := Run(context.Background(), service, []string{"accounts", "list", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if response.Data.IdentityCoverageComplete == nil || *response.Data.IdentityCoverageComplete {
		t.Fatalf("response = %+v, want incomplete identity coverage", response)
	}
	if response.Data.Accounts == nil || len(*response.Data.Accounts) != 1 ||
		(*response.Data.Accounts)[0].IdentityCoverage.State != mail.SenderIdentityCoverageStateBounded {
		t.Fatalf("accounts = %+v, want bounded coverage", response.Data.Accounts)
	}
}

func TestAccountsListReportsDirectOpsAnnotation(t *testing.T) {
	boundRef, err := mailref.EncodeAccount("ACCOUNT-1")
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	bindings := mail.NewAccountBindingStore(filepath.Join(t.TempDir(), "account-bindings.json"))
	if err := bindings.UpsertAccountBinding(mail.AccountBinding{
		AccountID: "ACCOUNT-1", SenderAliases: []string{"user@corp.example"}, CredentialAccount: "user@corp.example",
		SMTPHost: "smtp.corp.example", SMTPPort: 587,
		IMAPHost: "imap.corp.example", IMAPPort: 993,
	}); err != nil {
		t.Fatalf("UpsertAccountBinding() error = %v", err)
	}
	service := mail.NewServiceWithTransport(accountCatalogGateway{catalog: mail.AccountCatalog{
		Accounts: []mail.Account{
			{Ref: boundRef, Name: "Bound", Type: mail.AccountTypeIMAP, State: "ok", EmailAddresses: []string{"user@corp.example"}},
			{Ref: "acct_plain", Name: "Plain", State: "ok", EmailAddresses: []string{"user@gmail.com"}},
			{Ref: "acct_other", Name: "Other", State: "ok", EmailAddresses: []string{"user@other.example"}},
		},
		Complete: true,
	}}, "", mail.SendTransport{AccountBindings: bindings})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := Run(context.Background(), service, []string{"accounts", "list", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if response.Data.Accounts == nil || len(*response.Data.Accounts) != 3 {
		t.Fatalf("accounts = %+v", response.Data.Accounts)
	}
	bound, plain, other := (*response.Data.Accounts)[0], (*response.Data.Accounts)[1], (*response.Data.Accounts)[2]
	if !bound.DirectOpsSupported || bound.DirectOpsReason != mail.DirectOpsReasonBindingHosts {
		t.Fatalf("bound account annotation = %t,%s want true,binding_hosts", bound.DirectOpsSupported, bound.DirectOpsReason)
	}
	if !plain.DirectOpsSupported || plain.DirectOpsReason != mail.DirectOpsReasonProviderSupported {
		t.Fatalf("plain account annotation = %t,%s want true,provider_supported", plain.DirectOpsSupported, plain.DirectOpsReason)
	}
	if other.DirectOpsSupported || other.DirectOpsReason != mail.DirectOpsReasonUnsupportedProvider {
		t.Fatalf("other account annotation = %t,%s want false,unsupported_provider", other.DirectOpsSupported, other.DirectOpsReason)
	}
}

func TestAccountsListHumanOutputReportsDirectOpsAnnotation(t *testing.T) {
	service := mail.NewService(accountCatalogGateway{catalog: mail.AccountCatalog{
		Accounts: []mail.Account{
			{Ref: "acct_one", Name: "A", State: "ok", EmailAddresses: []string{"user@gmail.com"}},
			{Ref: "acct_two", Name: "B", State: "ok", EmailAddresses: []string{"user@corp.example"}},
		},
		Complete: true,
	}})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := Run(context.Background(), service, []string{"accounts", "list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"yes:provider_supported", "no:unsupported_provider"} {
		if !strings.Contains(output, want) {
			t.Fatalf("stdout = %q, want %q", output, want)
		}
	}
}

func TestAccountsListHumanOutputReportsBoundedIdentityCoverage(t *testing.T) {
	service := mail.NewService(accountCatalogGateway{catalog: mail.AccountCatalog{
		Accounts: []mail.Account{{
			Ref: "acct_ref", Name: "Account", State: "ok",
			IdentityCoverage: mail.SenderIdentityCoverage{
				Source:           mail.SenderIdentityCoverageSourceSentHistory,
				State:            mail.SenderIdentityCoverageStateBounded,
				ObservedMessages: 2000,
				Limit:            2000,
				MoreAvailable:    true,
			},
		}},
		Complete: true,
	}})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := Run(context.Background(), service, []string{"accounts", "list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{
		"identity_coverage_complete\tfalse",
		"bounded:2000/2000+",
		"sender identity coverage is bounded",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("stdout = %q, want %q", output, want)
		}
	}
}
