package mail

import (
	"context"
	"testing"

	"mailcli/internal/mailref"
)

type bindingValidationGatewayStub struct {
	gatewayStub
	listAccountsCalled bool
	validationCalls    int
	validationCatalog  AccountCatalog
}

func (g *bindingValidationGatewayStub) ListAccounts(ctx context.Context) ([]Account, error) {
	g.listAccountsCalled = true
	return g.gatewayStub.ListAccounts(ctx)
}

func (g *bindingValidationGatewayStub) ListBindingValidationCatalog(
	context.Context,
) (AccountCatalog, error) {
	g.validationCalls++
	return g.validationCatalog, nil
}

func TestValidateBindingCatalogPrefersValidationCatalog(t *testing.T) {
	accountRef, err := mailref.EncodeAccount("ACCOUNT-1")
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	gateway := &bindingValidationGatewayStub{
		validationCatalog: AccountCatalog{Accounts: []Account{{
			Ref: accountRef, State: "ok",
			ConfiguredSenderAliases: []string{"alias@icloud.com"},
			EmailAddresses:          []string{"alias@icloud.com"},
		}}},
	}
	service := NewService(gateway)
	binding := AccountBinding{
		AccountID: "ACCOUNT-1", SenderAliases: []string{"alias@icloud.com"},
		CredentialAccount: "login@icloud.com",
	}
	if err := service.validateBindingCatalog(
		context.Background(), accountRef, binding, "alias@icloud.com",
	); err != nil {
		t.Fatalf("validateBindingCatalog() error = %v", err)
	}
	if gateway.validationCalls != 1 || gateway.listAccountsCalled {
		t.Fatalf(
			"validationCalls=%d listAccountsCalled=%v, want validation catalog only",
			gateway.validationCalls, gateway.listAccountsCalled,
		)
	}
}

func TestValidateBindingCatalogFallsBackToListAccounts(t *testing.T) {
	accountRef, err := mailref.EncodeAccount("ACCOUNT-2")
	if err != nil {
		t.Fatalf("EncodeAccount() error = %v", err)
	}
	gateway := &gatewayStub{accounts: []Account{{
		Ref: accountRef, State: "ok",
		ConfiguredSenderAliases: []string{"alias@icloud.com"},
	}}}
	service := NewService(gateway)
	binding := AccountBinding{
		AccountID: "ACCOUNT-2", SenderAliases: []string{"alias@icloud.com"},
		CredentialAccount: "login@icloud.com",
	}
	if err := service.validateBindingCatalog(
		context.Background(), accountRef, binding, "alias@icloud.com",
	); err != nil {
		t.Fatalf("validateBindingCatalog() error = %v", err)
	}
}
