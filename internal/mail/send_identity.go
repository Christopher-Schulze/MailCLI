package mail

import (
	"context"
	"fmt"
	stdmail "net/mail"
	"strings"

	"mailcli/internal/mailref"
)

type resolvedSendIdentity struct {
	Sender     string
	Credential string
	Binding    *AccountBinding
}

func resolveTransportIdentity(send SendTransport, draft Draft) (resolvedSendIdentity, error) {
	sender, err := sendSender(draft.From)
	if err != nil {
		return resolvedSendIdentity{}, err
	}
	accountID, err := accountIDForDraft(draft.AccountRef)
	if err != nil {
		return resolvedSendIdentity{}, err
	}
	if send.AccountBindings == nil {
		if accountID != "" {
			return resolvedSendIdentity{}, accountBindingMissingError(sender, accountID)
		}
		return resolvedSendIdentity{Sender: sender, Credential: sender}, nil
	}
	document, err := send.AccountBindings.LoadAccountBindings()
	if err != nil {
		return resolvedSendIdentity{}, err
	}
	binding, found, err := ResolveAccountBinding(document, sender, accountID)
	if err != nil {
		return resolvedSendIdentity{}, err
	}
	if !found {
		if accountID != "" {
			return resolvedSendIdentity{}, accountBindingMissingError(sender, accountID)
		}
		return resolvedSendIdentity{Sender: sender, Credential: sender}, nil
	}
	return resolvedSendIdentity{Sender: sender, Credential: binding.CredentialAccount, Binding: &binding}, nil
}

func accountIDForDraft(accountRef string) (string, error) {
	accountRef = strings.TrimSpace(accountRef)
	if accountRef == "" {
		return "", nil
	}
	ref, err := mailref.DecodeAccount(accountRef)
	if err != nil {
		return "", &OperationError{
			Code:    "account_reference_invalid",
			Message: fmt.Sprintf("account ref %q cannot be decoded: %v", accountRef, err),
		}
	}
	return ref.AccountID, nil
}

func accountBindingMissingError(sender, accountID string) error {
	message := "sender " + sender + " has no configured account binding"
	if accountID != "" {
		message = fmt.Sprintf("sender %s is not bound to account %s", sender, accountID)
	}
	return &OperationError{Code: "account_binding_missing", Message: message + "; run 'mailcli send setup --from " + sender + " --account <account-ref>'"}
}

func (s *Service) resolveSendIdentity(ctx context.Context, draft Draft) (resolvedSendIdentity, error) {
	identity, err := resolveTransportIdentity(s.send, draft)
	if err != nil {
		return resolvedSendIdentity{}, err
	}
	if identity.Binding == nil || s.gateway == nil {
		return identity, nil
	}
	if err := s.validateBindingCatalog(ctx, draft.AccountRef, *identity.Binding, identity.Sender); err != nil {
		return resolvedSendIdentity{}, err
	}
	return identity, nil
}

func (s *Service) validateBindingCatalog(
	ctx context.Context,
	accountRef string,
	binding AccountBinding,
	sender string,
) error {
	accountID, err := accountIDForDraft(accountRef)
	if err != nil {
		return err
	}
	if accountID == "" {
		accountID = binding.AccountID
	}
	var accounts []Account
	if reader, ok := s.gateway.(BindingValidationCatalogReader); ok {
		catalog, err := reader.ListBindingValidationCatalog(ctx)
		if err != nil {
			return &OperationError{
				Code:    "account_catalog_incomplete",
				Message: fmt.Sprintf("cannot validate account binding %s before send: %v", binding.AccountID, err),
			}
		}
		accounts = catalog.Accounts
	} else {
		accounts, err = s.ListAccounts(ctx)
		if err != nil {
			return &OperationError{
				Code:    "account_catalog_incomplete",
				Message: fmt.Sprintf("cannot validate account binding %s before send: %v", binding.AccountID, err),
			}
		}
	}
	for _, account := range accounts {
		ref, decodeErr := mailref.DecodeAccount(account.Ref)
		if decodeErr != nil || !strings.EqualFold(ref.AccountID, accountID) {
			continue
		}
		if account.State == "disabled" {
			return &OperationError{Code: "account_binding_stale", Message: fmt.Sprintf("account binding %s refers to a disabled account", binding.AccountID)}
		}
		if account.State == "degraded" {
			return &OperationError{Code: "account_binding_account_degraded", Message: fmt.Sprintf("account binding %s refers to a degraded account: %s", binding.AccountID, account.DegradedReason)}
		}
		if !accountContainsAddress(account, sender) {
			return &OperationError{Code: "account_binding_stale", Message: fmt.Sprintf("sender %s is no longer permitted by account binding %s", sender, binding.AccountID)}
		}
		return nil
	}
	return &OperationError{Code: "account_binding_stale", Message: fmt.Sprintf("account binding %s refers to an account that is no longer enabled in Mail.app", binding.AccountID)}
}

func accountContainsAddress(account Account, address string) bool {
	sender, err := stdmail.ParseAddress(address)
	if err != nil {
		return false
	}
	for _, group := range [][]string{
		account.EmailAddresses, account.DiscoveredSenderIdentities, account.ConfiguredSenderAliases,
	} {
		for _, candidate := range group {
			parsed, err := stdmail.ParseAddress(candidate)
			if err == nil && strings.EqualFold(parsed.Address, sender.Address) {
				return true
			}
		}
	}
	return false
}
