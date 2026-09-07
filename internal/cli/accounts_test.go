package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"mailcli/internal/mail"
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
