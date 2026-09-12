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

// BindingValidationCatalogReader is an optional capability that lists the
// account catalog for send-time binding validation. Bound accounts skip the
// Sent-history sender scan because their senders validate against configured
// aliases; unbound accounts keep the full identity resolution.
type BindingValidationCatalogReader interface {
	ListBindingValidationCatalog(context.Context) (AccountCatalog, error)
}
