package mailstore

import (
	"context"

	"mailcli/internal/mail"
)

func (c *Client) ListAccountCatalog(ctx context.Context) (mail.AccountCatalog, error) {
	if c.store != nil {
		return c.store.ListAccountCatalog(ctx)
	}
	if c.fallback == nil {
		return mail.AccountCatalog{}, c.readUnavailableError()
	}
	accounts, err := c.fallback.ListAccounts(ctx)
	if err != nil {
		return mail.AccountCatalog{}, err
	}
	for index := range accounts {
		if accounts[index].IdentityCoverage.State != "" {
			continue
		}
		accounts[index].IdentityCoverage = mail.SenderIdentityCoverage{
			Source: mail.SenderIdentityCoverageSourceUnknown,
			State:  mail.SenderIdentityCoverageStateUnavailable,
		}
	}
	return mail.AccountCatalog{Accounts: accounts, Complete: true}, nil
}
