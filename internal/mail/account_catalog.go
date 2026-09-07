package mail

import "context"

// AccountCatalog is the account listing plus explicit completeness evidence.
// Complete is false when one or more configured accounts are listed in a
// degraded state.
type AccountCatalog struct {
	Accounts []Account `json:"accounts"`
	Complete bool      `json:"complete"`
}

// AccountCatalogReader is an optional richer account-listing capability. The
// legacy Gateway.ListAccounts method remains available for existing callers.
type AccountCatalogReader interface {
	ListAccountCatalog(context.Context) (AccountCatalog, error)
}
