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
	return mail.AccountCatalog{Accounts: accounts, Complete: true}, nil
}
